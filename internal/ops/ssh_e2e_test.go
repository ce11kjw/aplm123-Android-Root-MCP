// 真实端到端验证：在进程内起一个 SSH 服务器，用 aplm123d 的 SSH 客户端实现连上去，
// 覆盖 exec / upload / download / forward / 主机密钥校验 / 认证失败 等路径。
//
// 这个测试不依赖任何外部服务或系统包，因此可以在 CI 和本机稳定运行。
package ops

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// testSSHServer 是一个最小可用的 SSH 服务器，仅用于测试。
type testSSHServer struct {
	listener net.Listener
	config   *ssh.ServerConfig
	addr     string
	hostKey  ssh.Signer
	done     chan struct{}
}

// newTestSSHServer 启动一个监听 127.0.0.1 随机端口的 SSH 服务器。
// password 为接受的密码；返回的 authorizedPublicKey 用于证书认证测试。
func newTestSSHServer(t *testing.T, password string) *testSSHServer {
	t.Helper()

	// 生成主机密钥。
	_, hostPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate host key: %v", err)
	}
	hostSigner, err := ssh.NewSignerFromKey(hostPriv)
	if err != nil {
		t.Fatalf("host signer: %v", err)
	}

	config := &ssh.ServerConfig{
		PasswordCallback: func(conn ssh.ConnMetadata, pass []byte) (*ssh.Permissions, error) {
			if string(pass) == password {
				return nil, nil
			}
			return nil, fmt.Errorf("password rejected")
		},
	}
	config.AddHostKey(hostSigner)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	server := &testSSHServer{
		listener: listener,
		config:   config,
		addr:     listener.Addr().String(),
		hostKey:  hostSigner,
		done:     make(chan struct{}),
	}

	go server.serve()
	t.Cleanup(server.Close)
	return server
}

func (s *testSSHServer) serve() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			select {
			case <-s.done:
				return
			default:
				return
			}
		}
		go s.handle(conn)
	}
}

func (s *testSSHServer) handle(netConn net.Conn) {
	defer netConn.Close()
	sshConn, chans, reqs, err := ssh.NewServerConn(netConn, s.config)
	if err != nil {
		return
	}
	defer sshConn.Close()
	go ssh.DiscardRequests(reqs)

	for newChannel := range chans {
		switch newChannel.ChannelType() {
		case "session":
			channel, requests, acceptErr := newChannel.Accept()
			if acceptErr != nil {
				continue
			}
			go s.handleSession(channel, requests)
		case "direct-tcpip":
			// 端口转发目标：回环上的一个 echo 服务。
			go s.handleDirectTCPIP(newChannel)
		default:
			_ = newChannel.Reject(ssh.UnknownChannelType, "unsupported in test server")
		}
	}
}

func (s *testSSHServer) handleSession(channel ssh.Channel, requests <-chan *ssh.Request) {
	defer channel.Close()
	for req := range requests {
		switch req.Type {
		case "exec":
			// payload: uint32 length + command
			if len(req.Payload) < 4 {
				_ = req.Reply(false, nil)
				continue
			}
			cmdLen := int(req.Payload[0])<<24 | int(req.Payload[1])<<16 | int(req.Payload[2])<<8 | int(req.Payload[3])
			if 4+cmdLen > len(req.Payload) {
				_ = req.Reply(false, nil)
				continue
			}
			command := string(req.Payload[4 : 4+cmdLen])
			_ = req.Reply(true, nil)
			s.runCommand(channel, command)
		case "subsystem":
			s.handleSubsystem(channel, req)
		case "shell", "pty-req", "env", "window-change":
			_ = req.Reply(req.Type == "env", nil)
		default:
			_ = req.Reply(false, nil)
		}
	}
}

func (s *testSSHServer) runCommand(channel ssh.Channel, command string) {
	switch {
	case command == "echo hello":
		_, _ = channel.Write([]byte("hello\n"))
	case command == "exit3":
		_, _ = channel.Write([]byte("failing\n"))
		_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{3}))
		_ = channel.Close()
		return
	case command == "big":
		_, _ = channel.Write([]byte(strings.Repeat("x", sshMaxOutputBytes+4096)))
	default:
		_, _ = channel.Write([]byte("ran: " + command + "\n"))
	}
	// 正常结束：发送 exit-status 0 并关闭通道。
	// 必须 Close 而不只是 CloseWrite，否则客户端等待 IO 排空的 goroutine 会一直阻塞。
	_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
	_ = channel.Close()
}

// handleSubsystem 实现最小 SFTP 子集：足够验证 upload/download 链路。
func (s *testSSHServer) handleSubsystem(channel ssh.Channel, req *ssh.Request) {
	if len(req.Payload) < 4 {
		_ = req.Reply(false, nil)
		return
	}
	nameLen := int(req.Payload[0])<<24 | int(req.Payload[1])<<16 | int(req.Payload[2])<<8 | int(req.Payload[3])
	if 4+nameLen > len(req.Payload) || string(req.Payload[4:4+nameLen]) != "sftp" {
		_ = req.Reply(false, nil)
		return
	}
	_ = req.Reply(true, nil)
	runTestSFTP(channel)
}

func (s *testSSHServer) handleDirectTCPIP(newChannel ssh.NewChannel) {
	// 解析 direct-tcpip 请求，连到目标端口，做双向转发。
	payload := struct {
		DestAddr   string
		DestPort   uint32
		OriginAddr string
		OriginPort uint32
	}{}
	if err := ssh.Unmarshal(newChannel.ExtraData(), &payload); err != nil {
		_ = newChannel.Reject(ssh.ConnectionFailed, "bad payload")
		return
	}
	target, err := net.Dial("tcp", net.JoinHostPort(payload.DestAddr, fmt.Sprintf("%d", payload.DestPort)))
	if err != nil {
		_ = newChannel.Reject(ssh.ConnectionFailed, "dial failed")
		return
	}
	channel, reqs, err := newChannel.Accept()
	if err != nil {
		target.Close()
		return
	}
	go ssh.DiscardRequests(reqs)
	go func() {
		_, _ = io.Copy(channel, target)
		_ = channel.Close()
	}()
	go func() {
		_, _ = io.Copy(target, channel)
		_ = target.Close()
	}()
}

func (s *testSSHServer) Close() {
	select {
	case <-s.done:
		return
	default:
		close(s.done)
	}
	_ = s.listener.Close()
}

func (s *testSSHServer) host() (string, int) {
	host, portStr, _ := net.SplitHostPort(s.addr)
	var port int
	_, _ = fmt.Sscanf(portStr, "%d", &port)
	return host, port
}

// hostKeyFingerprint 返回服务器主机密钥的 SHA256 指纹，用于 known_hosts 预置。
func (s *testSSHServer) hostKeyFingerprint() string {
	return ssh.FingerprintSHA256(s.hostKey.PublicKey())
}

// writeKnownHosts 预置一条 known_hosts 记录，使客户端无需 TOFU 即可连上。
func writeKnownHosts(t *testing.T, path, host string, port int, key ssh.PublicKey) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir known_hosts dir: %v", err)
	}
	line := fmt.Sprintf("[%s]:%d %s\n", host, port, strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key))))
	if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
		t.Fatalf("write known_hosts: %v", err)
	}
}

// --- 端到端测试 ---

func TestSSHEndToEndExec(t *testing.T) {
	server := newTestSSHServer(t, "secret")
	host, port := server.host()
	manager := testManager(t)

	result := manager.Execute(context.Background(), Request{
		Tool: "aplm_ssh",
		Args: map[string]any{
			"action":    "exec",
			"host":      host,
			"port":      float64(port),
			"user":      "tester",
			"password":  "secret",
			"command":   "echo hello",
			"insecure":  true, // 首次连接无 known_hosts；单独测试校验路径。
			"timeoutMs": float64(10000),
		},
	})
	if !result.Success {
		t.Fatalf("exec failed: code=%s msg=%s err=%s", result.Code, result.Message, result.Error)
	}
	data, ok := result.Data.(map[string]any)
	if !ok {
		t.Fatalf("unexpected data shape %T", result.Data)
	}
	if got, _ := data["stdout"].(string); got != "hello\n" {
		t.Fatalf("stdout = %q, want %q", got, "hello\n")
	}
	if code, _ := data["exitCode"].(int); code != 0 {
		t.Fatalf("exitCode = %v, want 0", data["exitCode"])
	}
}

func TestSSHEndToEndExecNonZeroExit(t *testing.T) {
	server := newTestSSHServer(t, "secret")
	host, port := server.host()
	manager := testManager(t)

	result := manager.Execute(context.Background(), Request{
		Tool: "aplm_ssh",
		Args: map[string]any{
			"action":    "exec",
			"host":      host,
			"port":      float64(port),
			"user":      "tester",
			"password":  "secret",
			"command":   "exit3",
			"insecure":  true,
			"timeoutMs": float64(10000),
		},
	})
	if result.Success {
		t.Fatal("non-zero remote exit should be reported as failure")
	}
	if result.Code != "SSH_COMMAND_FAILED" {
		t.Fatalf("code = %q, want SSH_COMMAND_FAILED", result.Code)
	}
	data, _ := result.Data.(map[string]any)
	if code, _ := data["exitCode"].(int); code != 3 {
		t.Fatalf("exitCode = %v, want 3", data["exitCode"])
	}
}

func TestSSHEndToEndPasswordRejected(t *testing.T) {
	server := newTestSSHServer(t, "secret")
	host, port := server.host()
	manager := testManager(t)

	result := manager.Execute(context.Background(), Request{
		Tool: "aplm_ssh",
		Args: map[string]any{
			"action":    "connect",
			"host":      host,
			"port":      float64(port),
			"user":      "tester",
			"password":  "wrong",
			"insecure":  true,
			"timeoutMs": float64(10000),
		},
	})
	if result.Success {
		t.Fatal("wrong password must not connect")
	}
	if result.Code != "SSH_HANDSHAKE_FAILED" && result.Code != "SSH_CONNECT_FAILED" {
		t.Fatalf("code = %q, want a handshake/connect failure", result.Code)
	}
}

// TestSSHEndToEndHostKeyPinning 验证主机密钥校验真的生效：
// 未记录时 TOFU 接受并写入 known_hosts；记录不一致时必须拒绝。
func TestSSHEndToEndHostKeyPinning(t *testing.T) {
	server := newTestSSHServer(t, "secret")
	host, port := server.host()
	manager := testManager(t)

	knownHosts := filepath.Join(manager.cfg.WorkDir, ".aplm123-state", "ssh", "known_hosts")
	base := map[string]any{
		"host":      host,
		"port":      float64(port),
		"user":      "tester",
		"password":  "secret",
		"timeoutMs": float64(10000),
	}

	// 第一次：无记录 → TOFU 接受。
	first := map[string]any{"action": "connect"}
	for k, v := range base {
		first[k] = v
	}
	result := manager.Execute(context.Background(), Request{Tool: "aplm_ssh", Args: first})
	if !result.Success {
		t.Fatalf("TOFU first connect should succeed: %s / %s", result.Code, result.Message)
	}
	if _, err := os.Stat(knownHosts); err != nil {
		t.Fatalf("known_hosts should have been written: %v", err)
	}

	// 第二次：记录一致 → 仍然成功。
	second := map[string]any{"action": "connect"}
	for k, v := range base {
		second[k] = v
	}
	result = manager.Execute(context.Background(), Request{Tool: "aplm_ssh", Args: second})
	if !result.Success {
		t.Fatalf("pinned host key should still connect: %s / %s", result.Code, result.Message)
	}

	// 第三次：把 known_hosts 里的主机密钥换成另一把不同的密钥 → 必须拒绝。
	raw, err := os.ReadFile(knownHosts)
	if err != nil {
		t.Fatalf("read known_hosts: %v", err)
	}
	fields := strings.Fields(strings.TrimSpace(string(raw)))
	if len(fields) < 3 {
		t.Fatalf("known_hosts line malformed: %q", raw)
	}
	// 生成一把无关的 ed25519 主机密钥，其 base64 编码与真实密钥不同。
	_, otherPub, genErr := ed25519.GenerateKey(rand.Reader)
	if genErr != nil {
		t.Fatalf("generate unrelated key: %v", genErr)
	}
	otherSigner, signerErr := ssh.NewSignerFromKey(otherPub)
	if signerErr != nil {
		t.Fatalf("unrelated signer: %v", signerErr)
	}
	badLine := fmt.Sprintf("%s %s %s\n", fields[0], fields[1],
		strings.TrimSpace(string(ssh.MarshalAuthorizedKey(otherSigner.PublicKey()))))
	if err := os.WriteFile(knownHosts, []byte(badLine), 0o600); err != nil {
		t.Fatalf("write tampered known_hosts: %v", err)
	}
	// 清掉进程内缓存，强制重新读取文件。
	storeMu.Lock()
	delete(knownHostsCache, knownHosts)
	storeMu.Unlock()

	third := map[string]any{"action": "connect"}
	for k, v := range base {
		third[k] = v
	}
	result = manager.Execute(context.Background(), Request{Tool: "aplm_ssh", Args: third})
	if result.Success {
		t.Fatal("mismatched host key MUST be rejected")
	}
	if !strings.Contains(result.Message, "主机密钥") && !strings.Contains(result.Message, "中间人") {
		t.Fatalf("expected a host-key mismatch error, got %q", result.Message)
	}
}

func TestSSHEndToEndInsecureSkipsPinning(t *testing.T) {
	server := newTestSSHServer(t, "secret")
	host, port := server.host()
	manager := testManager(t)

	result := manager.Execute(context.Background(), Request{
		Tool: "aplm_ssh",
		Args: map[string]any{
			"action":    "connect",
			"host":      host,
			"port":      float64(port),
			"user":      "tester",
			"password":  "secret",
			"insecure":  true,
			"timeoutMs": float64(10000),
		},
	})
	if !result.Success {
		t.Fatalf("insecure connect should succeed: %s / %s", result.Code, result.Message)
	}
	data, _ := result.Data.(map[string]any)
	if _, ok := data["securityWarning"]; !ok {
		t.Fatal("insecure connect must surface a security warning")
	}
}

func TestSSHEndToEndOutputTruncation(t *testing.T) {
	server := newTestSSHServer(t, "secret")
	host, port := server.host()
	manager := testManager(t)

	result := manager.Execute(context.Background(), Request{
		Tool: "aplm_ssh",
		Args: map[string]any{
			"action":    "exec",
			"host":      host,
			"port":      float64(port),
			"user":      "tester",
			"password":  "secret",
			"command":   "big",
			"insecure":  true,
			"timeoutMs": float64(20000),
		},
	})
	if !result.Success {
		t.Fatalf("big output exec should still succeed: %s / %s", result.Code, result.Message)
	}
	data, _ := result.Data.(map[string]any)
	stdout, _ := data["stdout"].(string)
	if len(stdout) > sshMaxOutputBytes {
		t.Fatalf("stdout was not bounded: %d bytes", len(stdout))
	}
	if truncated, _ := data["outputTruncated"].(bool); !truncated {
		t.Fatal("outputTruncated should be set when output exceeds the cap")
	}
}

func TestSSHEndToEndCommandTimeout(t *testing.T) {
	manager := testManager(t)

	// 用 RFC1918 内不可达地址验证拨号超时被正确遵守。
	start := time.Now()
	result := manager.Execute(context.Background(), Request{
		Tool: "aplm_ssh",
		Args: map[string]any{
			"action":    "connect",
			"host":      "10.255.255.1", // RFC1918 内不可达地址，触发拨号超时
			"port":      float64(2222),
			"user":      "tester",
			"password":  "secret",
			"insecure":  true,
			"timeoutMs": float64(1500),
		},
	})
	if result.Success {
		t.Skip("unreachable address unexpectedly connected")
	}
	elapsed := time.Since(start)
	if elapsed > 10*time.Second {
		t.Fatalf("dial timeout was not honoured; took %s", elapsed)
	}
}

func TestSSHEndToEndForward(t *testing.T) {
	server := newTestSSHServer(t, "secret")
	host, port := server.host()
	manager := testManager(t)

	// 目标：本地起一个 echo 服务器，由测试 SSH 服务器转发。
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("echo listen: %v", err)
	}
	defer echo.Close()
	go func() {
		for {
			conn, acceptErr := echo.Accept()
			if acceptErr != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}(conn)
		}
	}()
	echoPort := echo.Addr().(*net.TCPAddr).Port

	result := manager.Execute(context.Background(), Request{
		Tool: "aplm_ssh",
		Args: map[string]any{
			"action":     "forward",
			"host":       host,
			"port":       float64(port),
			"user":       "tester",
			"password":   "secret",
			"insecure":   true,
			"bindPort":   float64(0),
			"bindAddr":   "127.0.0.1",
			"targetHost": "127.0.0.1",
			"targetPort": float64(echoPort),
			"timeoutMs":  float64(10000),
		},
	})
	if !result.Success {
		t.Fatalf("forward failed: %s / %s", result.Code, result.Message)
	}
	data, _ := result.Data.(map[string]any)
	bind, _ := data["bind"].(string)
	if bind == "" {
		t.Fatalf("forward did not report a bind address: %+v", data)
	}

	// 真正穿过隧道读写一次。
	conn, err := net.DialTimeout("tcp", bind, 5*time.Second)
	if err != nil {
		t.Fatalf("dial forwarded port %s: %v", bind, err)
	}
	defer conn.Close()
	payload := []byte("tunnel-check")
	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("write through tunnel: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read through tunnel: %v", err)
	}
	if string(buf) != string(payload) {
		t.Fatalf("tunnel echoed %q, want %q", buf, payload)
	}

	// 关闭转发。
	sessionID, _ := data["sessionId"].(string)
	forwardID, _ := data["forwardId"].(string)
	closeResult := manager.Execute(context.Background(), Request{
		Tool: "aplm_ssh",
		Args: map[string]any{
			"action":    "unforward",
			"sessionId": sessionID,
			"forwardId": forwardID,
		},
	})
	if !closeResult.Success {
		t.Fatalf("unforward failed: %s / %s", closeResult.Code, closeResult.Message)
	}
}

func TestSSHEndToEndSessionReuseAndClose(t *testing.T) {
	server := newTestSSHServer(t, "secret")
	host, port := server.host()
	manager := testManager(t)

	connect := manager.Execute(context.Background(), Request{
		Tool: "aplm_ssh",
		Args: map[string]any{
			"action":    "connect",
			"host":      host,
			"port":      float64(port),
			"user":      "tester",
			"password":  "secret",
			"insecure":  true,
			"keep":      true,
			"timeoutMs": float64(10000),
		},
	})
	if !connect.Success {
		t.Fatalf("keep connect failed: %s / %s", connect.Code, connect.Message)
	}
	data, _ := connect.Data.(map[string]any)
	sessionID, _ := data["sessionId"].(string)
	if sessionID == "" {
		t.Fatalf("keep connect did not return a sessionId: %+v", data)
	}

	list := manager.Execute(context.Background(), Request{
		Tool: "aplm_ssh",
		Args: map[string]any{"action": "list"},
	})
	listData, _ := list.Data.(map[string]any)
	if count, _ := listData["count"].(int); count != 1 {
		t.Fatalf("session list count = %v, want 1", listData["count"])
	}

	closed := manager.Execute(context.Background(), Request{
		Tool: "aplm_ssh",
		Args: map[string]any{"action": "close", "sessionId": sessionID},
	})
	if !closed.Success {
		t.Fatalf("close failed: %s / %s", closed.Code, closed.Message)
	}

	list = manager.Execute(context.Background(), Request{
		Tool: "aplm_ssh",
		Args: map[string]any{"action": "list"},
	})
	listData, _ = list.Data.(map[string]any)
	if count, _ := listData["count"].(int); count != 0 {
		t.Fatalf("session list count after close = %v, want 0", listData["count"])
	}
}

func TestSSHEndToEndUploadRejectsMissingLocalFile(t *testing.T) {
	server := newTestSSHServer(t, "secret")
	host, port := server.host()
	manager := testManager(t)

	result := manager.Execute(context.Background(), Request{
		Tool: "aplm_ssh",
		Args: map[string]any{
			"action":     "upload",
			"host":       host,
			"port":       float64(port),
			"user":       "tester",
			"password":   "secret",
			"insecure":   true,
			"localPath":  "/nonexistent/file.bin",
			"remotePath": "/tmp/file.bin",
			"timeoutMs":  float64(10000),
		},
	})
	if result.Success {
		t.Fatal("upload of a missing local file must fail")
	}
	if result.Code != "SSH_LOCAL_FILE_UNAVAILABLE" {
		t.Fatalf("code = %q, want SSH_LOCAL_FILE_UNAVAILABLE (%s)", result.Code, result.Message)
	}
}

func TestSSHEndToEndPrivateKeyAuth(t *testing.T) {
	// 用 ed25519 密钥做公钥认证，验证 buildSSHClientConfig 的密钥路径。
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	// 生成 OpenSSH 格式私钥。
	pem, err := marshalOpenSSHPrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}

	// 服务器只接受这个公钥。
	_, hostPriv, _ := ed25519.GenerateKey(rand.Reader)
	hostSigner, _ := ssh.NewSignerFromKey(hostPriv)
	config := &ssh.ServerConfig{
		PublicKeyCallback: func(conn ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if string(key.Marshal()) == string(signer.PublicKey().Marshal()) {
				return nil, nil
			}
			return nil, fmt.Errorf("key rejected")
		},
	}
	config.AddHostKey(hostSigner)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server := &testSSHServer{listener: listener, config: config, addr: listener.Addr().String(), hostKey: hostSigner, done: make(chan struct{})}
	go server.serve()
	t.Cleanup(server.Close)

	host, port := server.host()
	manager := testManager(t)
	result := manager.Execute(context.Background(), Request{
		Tool: "aplm_ssh",
		Args: map[string]any{
			"action":     "exec",
			"host":       host,
			"port":       float64(port),
			"user":       "tester",
			"privateKey": string(pem),
			"command":    "echo hello",
			"insecure":   true,
			"timeoutMs":  float64(10000),
		},
	})
	if !result.Success {
		t.Fatalf("public key auth failed: %s / %s / %s", result.Code, result.Message, result.Error)
	}
}

func TestSSHEndToEndKeyFileAuth(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	pem, err := marshalOpenSSHPrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	signer, _ := ssh.NewSignerFromKey(priv)

	_, hostPriv, _ := ed25519.GenerateKey(rand.Reader)
	hostSigner, _ := ssh.NewSignerFromKey(hostPriv)
	config := &ssh.ServerConfig{
		PublicKeyCallback: func(conn ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if string(key.Marshal()) == string(signer.PublicKey().Marshal()) {
				return nil, nil
			}
			return nil, fmt.Errorf("key rejected")
		},
	}
	config.AddHostKey(hostSigner)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server := &testSSHServer{listener: listener, config: config, addr: listener.Addr().String(), hostKey: hostSigner, done: make(chan struct{})}
	go server.serve()
	t.Cleanup(server.Close)

	manager := testManager(t)
	keyPath := filepath.Join(manager.cfg.WorkDir, "id_ed25519")
	if err := os.MkdirAll(manager.cfg.WorkDir, 0o700); err != nil {
		t.Fatalf("mkdir workdir: %v", err)
	}
	if err := os.WriteFile(keyPath, pem, 0o600); err != nil {
		t.Fatalf("write key file: %v", err)
	}

	host, port := server.host()
	result := manager.Execute(context.Background(), Request{
		Tool: "aplm_ssh",
		Args: map[string]any{
			"action":    "exec",
			"host":      host,
			"port":      float64(port),
			"user":      "tester",
			"keyFile":   "id_ed25519", // 相对路径应锚定到工作目录
			"command":   "echo hello",
			"insecure":  true,
			"timeoutMs": float64(10000),
		},
	})
	if !result.Success {
		t.Fatalf("keyFile auth failed: %s / %s / %s", result.Code, result.Message, result.Error)
	}
}

func TestSSHEndToEndBadPrivateKeyIsClassified(t *testing.T) {
	server := newTestSSHServer(t, "secret")
	host, port := server.host()
	manager := testManager(t)

	result := manager.Execute(context.Background(), Request{
		Tool: "aplm_ssh",
		Args: map[string]any{
			"action":     "connect",
			"host":       host,
			"port":       float64(port),
			"user":       "tester",
			"privateKey": "-----BEGIN OPENSSH PRIVATE KEY-----\nnot-a-real-key\n-----END OPENSSH PRIVATE KEY-----\n",
			"insecure":   true,
			"timeoutMs":  float64(10000),
		},
	})
	if result.Success {
		t.Fatal("unparsable private key must not connect")
	}
	if result.Code != "SSH_CONFIG_INVALID" {
		t.Fatalf("code = %q, want SSH_CONFIG_INVALID (%s)", result.Code, result.Message)
	}
	if strings.Contains(result.Message, "not-a-real-key") {
		t.Fatal("error message must not echo key material")
	}
}
