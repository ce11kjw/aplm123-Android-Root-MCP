package ops

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/pkg/sftp"
)

// SFTP 单次传输上限。大文件应走分块或 transfer_export 链路。
const (
	sshMaxTransferBytes = 64 << 20
	sshSFTPTimeout      = 10 * time.Minute

	// SFTP 单次交互的等待上限。
	// 下限防止用户把 timeout 设得过短误杀正常传输；
	// 上限防止对端彻底无响应时无限期挂住。
	sftpMinIdleTimeout = 30 * time.Second
	sftpMaxIdleTimeout = 2 * time.Minute
	// 关闭 SFTP 会话时的等待上限。取值要够断开完成，又不能拖住调用返回。
	sftpCloseTimeout = 3 * time.Second
)

// sftpSession 包装一条已建立的 SFTP 通道。
type sftpSession struct {
	client  *sftp.Client
	session *sshSession
}

func (s *sftpSession) Close() {
	// 关闭前把 deadline 收紧到一个很短的值。
	//
	// 两个方向都要避免：
	//   - 保留原有较长 deadline：断开握手若写不出去，会一直等到超时
	//     （实测白白多等 30 秒才返回）。
	//   - 清除 deadline（传 0）：连接彻底无响应时会无限期挂住。
	// 因此这里给一个"足够断开、又不会拖住调用"的短上限。
	if s.session != nil {
		s.session.setDeadline(sftpCloseTimeout)
	}
	if s.client != nil {
		_ = s.client.Close()
	}
	if s.session != nil {
		// 断开握手可能仍会短暂等待对端确认；放到后台收尾，
		// 不让它拖住已经完成的传输结果返回给调用方。
		done := make(chan struct{})
		go func() {
			s.session.shutdown()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(sftpCloseTimeout):
			// 超时也继续返回：连接资源由 shutdown 内部继续清理。
		}
	}
}

// openSFTP 建立 SSH 连接并升级为 SFTP 子系统。
func (m *Manager) openSFTP(ctx context.Context, conn sshConnectionArgs) (*sftpSession, error) {
	session, err := m.dialSSH(ctx, conn)
	if err != nil {
		return nil, err
	}
	client, err := sftp.NewClient(session.client)
	if err != nil {
		session.shutdown()
		return nil, fmt.Errorf("启动 SFTP 子系统失败：%v", sanitizeSSHError(err))
	}
	// 给底层连接设置读写 deadline。
	//
	// 原因：SFTP 是按请求-响应一问一答的协议。若远端在收到请求后
	// 不再回复（网络中断、服务端异常、中间设备丢包），Read 会无限期阻塞，
	// 表现为工具调用一直挂住而不返回错误。设置 deadline 后，
	// 阻塞会变成可诊断的 i/o timeout。
	//
	// 每次成功交互后刷新 deadline，因此长传输不会被整体掐断，
	// 只有**单次无响应**才会超时。
	if deadline := sftpDeadlineFor(conn); deadline > 0 {
		session.setDeadline(deadline)
	}
	return &sftpSession{client: client, session: session}, nil
}

// sftpDeadlineFor 计算单次 SFTP 交互的等待上限。
// 以连接超时为基准并给出下限，避免用户把 timeout 设得很短时误杀正常传输。
func sftpDeadlineFor(conn sshConnectionArgs) time.Duration {
	base := conn.Timeout
	if base <= 0 {
		base = sshDialTimeout
	}
	if base < sftpMinIdleTimeout {
		base = sftpMinIdleTimeout
	}
	if base > sftpMaxIdleTimeout {
		base = sftpMaxIdleTimeout
	}
	return base
}

// sshSFTP 处理 upload / download / sftp 三种 action。
func (m *Manager) sshSFTP(ctx context.Context, req Request, action string) Result {
	conn, err := parseSSHArgs(req.Args, m.cfg.WorkDir)
	if err != nil {
		return fail("SSH_ARGUMENT_INVALID", err.Error(), nil, "SSH_ARGUMENT_INVALID")
	}
	timeout, err := argDuration(req.Args, sshSFTPTimeout)
	if err != nil {
		return invalid(err.Error())
	}

	switch action {
	case "upload":
		localPath, err := argString(req.Args, "localPath", "local", "src")
		if err != nil {
			return invalid("缺少参数 localPath")
		}
		remotePath, err := argString(req.Args, "remotePath", "remote", "dest", "destination")
		if err != nil {
			return invalid("缺少参数 remotePath")
		}
		return m.sftpUpload(ctx, conn, localPath, remotePath, timeout)

	case "download":
		remotePath, err := argString(req.Args, "remotePath", "remote", "src", "source")
		if err != nil {
			return invalid("缺少参数 remotePath")
		}
		localPath, err := argString(req.Args, "localPath", "local", "dest", "destination")
		if err != nil {
			return invalid("缺少参数 localPath")
		}
		return m.sftpDownload(ctx, conn, remotePath, localPath, timeout)

	case "sftp":
		// sftp 是 upload/download 的显式别名分发。
		op, err := argString(req.Args, "operation", "op", "mode")
		if err != nil {
			return invalid("sftp 需要 operation 参数（upload/download）")
		}
		switch op {
		case "upload":
			return m.sshSFTP(ctx, req, "upload")
		case "download":
			return m.sshSFTP(ctx, req, "download")
		default:
			return invalid("operation 必须是 upload 或 download")
		}
	}
	return invalidAction("aplm_ssh", action)
}

func (m *Manager) sftpUpload(ctx context.Context, conn sshConnectionArgs, localPath, remotePath string, timeout time.Duration) Result {
	started := time.Now()
	resolved, err := resolveSSHPath(localPath, m.cfg.WorkDir)
	if err != nil {
		return invalid(err.Error())
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return withData(fail("SSH_LOCAL_FILE_UNAVAILABLE", "本地文件不可用："+sanitizeSSHError(err), errors.New("本地文件不可用："+sanitizeSSHError(err)), "SSH_LOCAL_FILE_UNAVAILABLE"), map[string]any{"localPath": resolved})
	}
	if info.IsDir() {
		return invalid("localPath 是目录；请先通过 aplm_archive 压缩后再上传")
	}
	if info.Size() > sshMaxTransferBytes {
		return invalid(fmt.Sprintf("文件 %d 字节超过单次上限 %d 字节", info.Size(), sshMaxTransferBytes))
	}

	localHash, err := hashFileSHA256(resolved)
	if err != nil {
		return fail("SSH_HASH_FAILED", "本地文件哈希计算失败", nil, "SSH_HASH_FAILED")
	}

	session, err := m.openSFTP(ctx, conn)
	if err != nil {
		return fail("SSH_CONNECT_FAILED", err.Error(), nil, "SSH_CONNECT_FAILED")
	}
	defer session.Close()

	dst := remotePath
	if strings.HasSuffix(remotePath, "/") || remotePath == "." {
		dst = path.Join(remotePath, filepath.Base(resolved))
	}
	if err := session.client.MkdirAll(path.Dir(dst)); err != nil {
		// 远端目录可能已存在或不允许创建，继续尝试直接写入。
		_ = err
	}

	var written int64
	err = runWithTimeout(ctx, timeout, func() error {
		src, openErr := os.Open(resolved)
		if openErr != nil {
			return openErr
		}
		defer src.Close()
		// 每次交互前刷新 deadline：长传输不会被整体掐断，
		// 只有单次无响应才会超时。
		idle := sftpDeadlineFor(conn)
		session.session.setDeadline(idle)
		dstFile, createErr := session.client.Create(dst)
		if createErr != nil {
			return createErr
		}
		defer dstFile.Close()
		session.session.setDeadline(idle)
		var copyErr error
		written, copyErr = io.Copy(dstFile, src)
		return copyErr
	})
	if err != nil {
		return withData(
			fail("SSH_UPLOAD_FAILED", "上传失败："+sanitizeSSHError(err), errors.New("上传失败："+sanitizeSSHError(err)), "SSH_UPLOAD_FAILED"),
			map[string]any{
				"localPath": resolved, "remotePath": dst, "bytesWritten": written, "durationMs": time.Since(started).Milliseconds()},
		)
	}

	// 写后读回：远端大小必须与本地一致，否则明确报错而不是假装成功。
	if sshDebugEnabled() {
		fmt.Fprintf(os.Stderr, "[aplm_ssh] write 完成 written=%d，准备 Stat %s\n", written, dst)
	}
	// 读回前重置 deadline：前面的写入可能已消耗掉大半时间。
	session.session.setDeadline(sftpDeadlineFor(conn))
	remoteInfo, statErr := session.client.Stat(dst)
	if sshDebugEnabled() {
		fmt.Fprintf(os.Stderr, "[aplm_ssh] Stat 返回 err=%v\n", statErr)
	}
	data := map[string]any{
		"localPath":    resolved,
		"remotePath":   dst,
		"host":         conn.Host,
		"bytesWritten": written,
		"localSHA256":  localHash,
		"durationMs":   time.Since(started).Milliseconds(),
		"strategy":     "sftp_write_then_readback",
	}
	if statErr != nil {
		data["readback"] = "unavailable"
		return withData(fail("SSH_UPLOAD_UNVERIFIED", "上传完成但无法读回远端大小："+sanitizeSSHError(statErr), errors.New("上传完成但无法读回远端大小："+sanitizeSSHError(statErr)), "SSH_UPLOAD_UNVERIFIED"), data)
	}
	data["remoteSize"] = remoteInfo.Size()
	if remoteInfo.Size() != info.Size() {
		return withData(fail("SSH_UPLOAD_SIZE_MISMATCH", fmt.Sprintf("上传后远端大小 %d 与本地 %d 不一致", remoteInfo.Size(), info.Size()), errors.New(fmt.Sprintf("上传后远端大小 %d 与本地 %d 不一致", remoteInfo.Size(), info.Size())), "SSH_UPLOAD_SIZE_MISMATCH"), data)
	}
	return ok("上传完成并已校验", data, "sftp_write_verified")
}

func (m *Manager) sftpDownload(ctx context.Context, conn sshConnectionArgs, remotePath, localPath string, timeout time.Duration) Result {
	started := time.Now()
	resolved, err := resolveSSHPath(localPath, m.cfg.WorkDir)
	if err != nil {
		return invalid(err.Error())
	}

	session, err := m.openSFTP(ctx, conn)
	if err != nil {
		return fail("SSH_CONNECT_FAILED", err.Error(), nil, "SSH_CONNECT_FAILED")
	}
	defer session.Close()

	session.session.setDeadline(sftpDeadlineFor(conn))
	remoteInfo, err := session.client.Stat(remotePath)
	if err != nil {
		return withData(fail("SSH_REMOTE_FILE_UNAVAILABLE", "远端文件不可用："+sanitizeSSHError(err), errors.New("远端文件不可用："+sanitizeSSHError(err)), "SSH_REMOTE_FILE_UNAVAILABLE"), map[string]any{"remotePath": remotePath})
	}
	if remoteInfo.IsDir() {
		return invalid("remotePath 是目录；目录请先在远端压缩为单个文件")
	}
	if remoteInfo.Size() > sshMaxTransferBytes {
		return invalid(fmt.Sprintf("远端文件 %d 字节超过单次上限 %d 字节", remoteInfo.Size(), sshMaxTransferBytes))
	}

	dst := resolved
	if strings.HasSuffix(localPath, "/") {
		dst = filepath.Join(resolved, path.Base(remotePath))
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fail("SSH_LOCAL_DIR_FAILED", "本地目录创建失败", nil, "SSH_LOCAL_DIR_FAILED")
	}

	var written int64
	err = runWithTimeout(ctx, timeout, func() error {
		src, openErr := session.client.Open(remotePath)
		if openErr != nil {
			return openErr
		}
		defer src.Close()
		idle := sftpDeadlineFor(conn)
		session.session.setDeadline(idle)
		dstFile, createErr := os.Create(dst)
		if createErr != nil {
			return createErr
		}
		defer dstFile.Close()
		session.session.setDeadline(idle)
		var copyErr error
		written, copyErr = io.Copy(dstFile, src)
		return copyErr
	})
	if err != nil {
		return withData(
			fail("SSH_DOWNLOAD_FAILED", "下载失败："+sanitizeSSHError(err), errors.New("下载失败："+sanitizeSSHError(err)), "SSH_DOWNLOAD_FAILED"),
			map[string]any{
				"remotePath": remotePath, "localPath": dst, "bytesWritten": written, "durationMs": time.Since(started).Milliseconds()},
		)
	}

	session.session.setDeadline(sftpDeadlineFor(conn))
	remoteHash, hashErr := sftpHash(session.client, remotePath)
	localHash, localHashErr := hashFileSHA256(dst)

	data := map[string]any{
		"remotePath":   remotePath,
		"localPath":    dst,
		"host":         conn.Host,
		"bytesWritten": written,
		"remoteSize":   remoteInfo.Size(),
		"durationMs":   time.Since(started).Milliseconds(),
		"strategy":     "sftp_read_then_hash",
	}
	if hashErr == nil {
		data["remoteSHA256"] = remoteHash
	}
	if localHashErr == nil {
		data["localSHA256"] = localHash
	}
	if hashErr == nil && localHashErr == nil && remoteHash != localHash {
		return withData(fail("SSH_DOWNLOAD_CHECKSUM_MISMATCH", "下载后 SHA-256 与远端不一致", errors.New("下载后 SHA-256 与远端不一致"), "SSH_DOWNLOAD_CHECKSUM_MISMATCH"), data)
	}
	return ok("下载完成并已校验", data, "sftp_read_verified")
}

// sftpHash 在远端流式计算 SHA-256；远端被限制时不返回结果。
func sftpHash(client *sftp.Client, remotePath string) (string, error) {
	file, err := client.Open(remotePath)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hasher := sha256.New()
	if _, err := io.Copy(hasher, io.LimitReader(file, sshMaxTransferBytes)); err != nil {
		return "", err
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

// runWithTimeout 给一次传输套上超时；超时后返回明确错误而不是挂死。
func runWithTimeout(ctx context.Context, timeout time.Duration, fn func() error) error {
	if timeout <= 0 {
		return fn()
	}
	done := make(chan error, 1)
	go func() { done <- fn() }()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return errors.New("操作被取消")
	case <-timer.C:
		return fmt.Errorf("操作超时（%s）", timeout)
	}
}

// sshForward 处理 forward / unforward。
func (m *Manager) sshForward(ctx context.Context, req Request, action string) Result {
	switch action {
	case "forward":
		return m.sshStartForward(ctx, req)
	case "unforward":
		return m.sshStopForward(req)
	}
	return invalidAction("aplm_ssh", action)
}

func (m *Manager) sshStartForward(ctx context.Context, req Request) Result {
	// 先判断是否复用已有会话：复用时不要求重新提供连接参数，
	// 因为这些信息已经保存在会话里。
	sessionID, err := argOptionalString(req.Args, "", "sessionId", "session")
	if err != nil {
		return invalid(err.Error())
	}

	var session *sshSession
	if sessionID != "" {
		existing, found := m.sshState().get(sessionID)
		if !found {
			return fail("SSH_SESSION_NOT_FOUND", "SSH 会话不存在："+sessionID, nil, "SSH_SESSION_NOT_FOUND")
		}
		session = existing
	}

	// bindPort 是本地监听端口；target 是经 SSH 隧道访问的远端地址。
	bindPort, err := argInt64(req.Args, 0, "bindPort", "localPort")
	if err != nil {
		return invalid(err.Error())
	}
	if bindPort < 0 || bindPort > 65535 {
		return invalid("bindPort 必须在 0 到 65535 之间（0 表示随机端口）")
	}
	bindAddr, err := argOptionalString(req.Args, "127.0.0.1", "bindAddr", "bind")
	if err != nil {
		return invalid(err.Error())
	}
	targetHost, err := argString(req.Args, "targetHost", "targetAddress")
	if err != nil {
		return invalid("缺少参数 targetHost")
	}
	targetPort, err := argInt64(req.Args, 0, "targetPort")
	if err != nil {
		return invalid(err.Error())
	}
	if targetPort < 1 || targetPort > 65535 {
		return invalid("targetPort 必须在 1 到 65535 之间")
	}

	// 仅在需要新建连接时才解析完整的连接参数。
	if session == nil {
		conn, connErr := parseSSHArgs(req.Args, m.cfg.WorkDir)
		if connErr != nil {
			return fail("SSH_ARGUMENT_INVALID", connErr.Error(), connErr, "SSH_ARGUMENT_INVALID")
		}
		fresh, dialErr := m.dialSSH(ctx, conn)
		if dialErr != nil {
			return fail("SSH_CONNECT_FAILED", dialErr.Error(), dialErr, "SSH_CONNECT_FAILED")
		}
		session = fresh
	}

	listenAddr := net.JoinHostPort(bindAddr, strconv.Itoa(int(bindPort)))
	listener, err := net.Listen("tcp", listenAddr)
	if err != nil {
		if sessionID == "" {
			session.shutdown()
		}
		return withData(fail("SSH_FORWARD_BIND_FAILED", "本地监听失败："+sanitizeSSHError(err), errors.New("本地监听失败："+sanitizeSSHError(err)), "SSH_FORWARD_BIND_FAILED"), map[string]any{"bind": listenAddr})
	}
	actualPort := listener.Addr().(*net.TCPAddr).Port
	forwardID := randomTaskID()
	forward := &sshForward{
		bindAddr: bindAddr,
		bindPort: actualPort,
		target:   net.JoinHostPort(targetHost, strconv.Itoa(int(targetPort))),
		listener: listener,
		done:     make(chan struct{}),
	}
	session.addForward(forwardID, forward)
	go m.serveSSHForward(session, forward)

	localOnly := bindAddr == "127.0.0.1" || bindAddr == "::1" || bindAddr == "localhost"
	data := map[string]any{
		"forwardId":       forwardID,
		"sessionId":       session.id,
		"bind":            net.JoinHostPort(bindAddr, strconv.Itoa(actualPort)),
		"target":          forward.target,
		"host":            session.host,
		"mode":            "local_forward",
		"localOnly":       localOnly,
		"maxBytesPerConn": sshMaxForwardBytes,
	}
	if !localOnly {
		data["securityWarning"] = "转发绑定在非回环地址，同网段设备可经此隧道访问目标服务。"
	}
	return ok("SSH 本地端口转发已建立", data, "ssh_forward")
}

func (m *Manager) sshStopForward(req Request) Result {
	sessionID, err := argString(req.Args, "sessionId", "session")
	if err != nil {
		return invalid("缺少参数 sessionId")
	}
	forwardID, err := argString(req.Args, "forwardId", "forward")
	if err != nil {
		return invalid("缺少参数 forwardId")
	}
	session, found := m.sshState().get(sessionID)
	if !found {
		return fail("SSH_SESSION_NOT_FOUND", "SSH 会话不存在："+sessionID, nil, "SSH_SESSION_NOT_FOUND")
	}
	forward, found := session.takeForward(forwardID)
	if !found {
		return fail("SSH_FORWARD_NOT_FOUND", "转发不存在："+forwardID, nil, "SSH_FORWARD_NOT_FOUND")
	}
	forward.stop()
	in, out := forward.counters()
	return ok("SSH 转发已停止", map[string]any{
		"sessionId": sessionID, "forwardId": forwardID,
		"bytesIn": in, "bytesOut": out, "stopped": true,
	}, "ssh_unforward")
}

// serveSSHForward 接受本地连接并逐条经 SSH 隧道转发。
// 每个连接独立计数，且总字节受 sshMaxForwardBytes 约束，防止无限流量。
func (m *Manager) serveSSHForward(session *sshSession, forward *sshForward) {
	defer forward.stop()
	for {
		localConn, err := forward.listener.Accept()
		if err != nil {
			select {
			case <-forward.done:
				return
			default:
			}
			forward.lastError = sanitizeSSHError(err)
			return
		}
		go func(conn net.Conn) {
			defer conn.Close()
			remoteConn, dialErr := session.client.Dial("tcp", forward.target)
			if dialErr != nil {
				forward.lastError = sanitizeSSHError(dialErr)
				return
			}
			defer remoteConn.Close()

			done := make(chan struct{}, 2)
			go func() {
				written, copyErr := io.Copy(remoteConn, io.LimitReader(conn, sshMaxForwardBytes))
				forward.bytesIn += written
				if copyErr != nil {
					forward.lastError = sanitizeSSHError(copyErr)
				}
				done <- struct{}{}
			}()
			go func() {
				written, copyErr := io.Copy(conn, io.LimitReader(remoteConn, sshMaxForwardBytes))
				forward.bytesOut += written
				if copyErr != nil {
					forward.lastError = sanitizeSSHError(copyErr)
				}
				done <- struct{}{}
			}()
			<-done
			<-done
		}(localConn)
	}
}

var _ = errors.Is
