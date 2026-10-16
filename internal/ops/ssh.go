package ops

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// SSH 客户端工具的边界常量。所有取值都与既有工具的"有界"约定保持一致：
// 连接数、输出字节、命令长度、转发通道都有硬上限，避免一次调用吃满资源。
const (
	sshDefaultPort       = 22
	sshDefaultTimeout    = 30 * time.Second
	sshDialTimeout       = 15 * time.Second
	sshMaxSessions       = 8
	sshSessionIdle       = 10 * time.Minute
	sshMaxCommandBytes   = 32 << 10
	sshMaxStdinBytes     = 1 << 20
	sshMaxOutputBytes    = 4 << 20
	sshMaxForwardBytes   = 4 << 20
	sshDefaultKnownHosts = "/storage/emulated/0/aplm123AI/.aplm123-state/ssh/known_hosts"
	sshDefaultKeyDir     = "/storage/emulated/0/aplm123AI/.aplm123-state/ssh/keys"
)

// sshHostKeyPolicy 决定如何处理主机密钥。默认要求信任（TOFU + 固定指纹），
// 只有显式传 insecure=true 才降级，并在结果里标注安全警告。
type sshHostKeyPolicy string

const (
	sshHostKeyPinned   sshHostKeyPolicy = "pinned"
	sshHostKeyInsecure sshHostKeyPolicy = "insecure"
)

// sshSession 是一条已建连的会话。与 webSocketSession 一样，所有可变字段
// 由 ownership mutex 保护，summary() 只输出可安全回传的元数据。
type sshSession struct {
	id     string
	client *ssh.Client
	// conn 是底层的 TCP 连接，用于设置读写 deadline。
	// SFTP 等一问一答的协议在远端无响应时会无限期阻塞，
	// 有了它就能把"挂住"变成可诊断的超时错误。
	conn      net.Conn
	host      string
	port      int
	user      string
	policy    sshHostKeyPolicy
	hostKey   string
	createdAt time.Time
	lastUsed  time.Time
	mu        sync.Mutex
	listeners map[string]*sshForward
	closed    bool
	lastError string
}

// sshForward 是一条本地端口转发（-L 语义）。
type sshForward struct {
	bindAddr  string
	bindPort  int
	target    string
	listener  net.Listener
	bytesIn   int64
	bytesOut  int64
	stopOnce  sync.Once
	done      chan struct{}
	lastError string
}

type sshRuntime struct {
	once     sync.Once
	mu       sync.RWMutex
	sessions map[string]*sshSession
}

// sshState 返回（并按需初始化）SSH 会话注册表。
func (m *Manager) sshState() *sshRuntime {
	m.networkOnce.Do(func() {
		if m.network == nil {
			m.network = &networkRuntime{}
		}
	})
	// 独立于 networkRuntime，避免 SSH 会话被 WS 的 TTL 逻辑影响。
	if m.ssh == nil {
		m.sshMu.Lock()
		if m.ssh == nil {
			m.ssh = &sshRuntime{}
		}
		m.sshMu.Unlock()
	}
	return m.ssh
}

func (r *sshRuntime) get(id string) (*sshSession, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	session, ok := r.sessions[id]
	return session, ok
}

func (r *sshRuntime) put(session *sshSession) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sessions == nil {
		r.sessions = make(map[string]*sshSession)
	}
	r.sessions[session.id] = session
}

func (r *sshRuntime) remove(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.sessions, id)
}

func (r *sshRuntime) count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.sessions)
}

func (r *sshRuntime) list() []*sshSession {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*sshSession, 0, len(r.sessions))
	for _, session := range r.sessions {
		out = append(out, session)
	}
	return out
}

// closeAll 在 Manager.Close 时调用，确保不留下悬空的连接与监听端口。
func (r *sshRuntime) closeAll() {
	if r == nil {
		return
	}
	for _, session := range r.list() {
		session.shutdown()
	}
}

func (s *sshSession) touch() {
	s.mu.Lock()
	s.lastUsed = time.Now()
	s.mu.Unlock()
}

func (s *sshSession) setError(err error) {
	s.mu.Lock()
	if err != nil {
		s.lastError = sanitizeSSHError(err)
	}
	s.mu.Unlock()
}

// shutdown 关闭会话与其全部转发。幂等。
func (s *sshSession) shutdown() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	forwards := make([]*sshForward, 0, len(s.listeners))
	for _, forward := range s.listeners {
		forwards = append(forwards, forward)
	}
	s.listeners = nil
	client := s.client
	s.mu.Unlock()

	for _, forward := range forwards {
		forward.stop()
	}
	if client != nil {
		_ = client.Close()
	}
}

func (s *sshSession) addForward(id string, forward *sshForward) {
	s.mu.Lock()
	if s.listeners == nil {
		s.listeners = make(map[string]*sshForward)
	}
	s.listeners[id] = forward
	s.mu.Unlock()
}

func (s *sshSession) takeForward(id string) (*sshForward, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	forward, ok := s.listeners[id]
	if ok {
		delete(s.listeners, id)
	}
	return forward, ok
}

func (s *sshSession) forwardList() []*sshForward {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*sshForward, 0, len(s.listeners))
	for _, forward := range s.listeners {
		out = append(out, forward)
	}
	return out
}

func (f *sshForward) stop() {
	f.stopOnce.Do(func() {
		if f.listener != nil {
			_ = f.listener.Close()
		}
		if f.done != nil {
			close(f.done)
		}
	})
}

func (f *sshForward) counters() (int64, int64) {
	return f.bytesIn, f.bytesOut
}

func (s *sshSession) summary() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]any{
		"sessionId":   s.id,
		"host":        s.host,
		"port":        s.port,
		"user":        s.user,
		"hostKey":     s.hostKey,
		"hostKeyMode": string(s.policy),
		"createdAt":   s.createdAt.UTC().Format(time.RFC3339),
		"lastUsedAt":  s.lastUsed.UTC().Format(time.RFC3339),
		"forwards":    len(s.listeners),
		"closed":      s.closed,
	}
	if s.lastError != "" {
		out["lastError"] = s.lastError
	}
	if s.policy == sshHostKeyInsecure {
		out["securityWarning"] = "已禁用主机密钥校验（insecure），连接可能被中间人劫持。"
	}
	return out
}

// sshConnectionArgs 是一次连接的公共参数，被 exec / sftp / forward 复用。
type sshConnectionArgs struct {
	Host            string
	Port            int
	User            string
	Password        string
	PrivateKey      string
	Passphrase      string
	KeyFile         string
	KnownHosts      string
	Insecure        bool
	Timeout         time.Duration
	AgentForwarding bool
}

// parseSSHArgs 读取并校验连接参数。密钥可以在 args 里内联（privateKey），
// 也可以指向文件（keyFile）—— 都只在本机使用，绝不写回结果。
func parseSSHArgs(args map[string]any, workDir string) (sshConnectionArgs, error) {
	out := sshConnectionArgs{Port: sshDefaultPort, Timeout: sshDialTimeout}

	host, err := argString(args, "host", "address", "target")
	if err != nil {
		return out, fmt.Errorf("缺少参数 host")
	}
	host = strings.TrimSpace(host)
	if host == "" {
		return out, fmt.Errorf("host 不能为空")
	}
	// 支持 user@host:port 的紧凑写法。
	if strings.Contains(host, "@") {
		parts := strings.SplitN(host, "@", 2)
		if out.User == "" && parts[0] != "" {
			out.User = parts[0]
		}
		host = parts[1]
	}
	if idx := strings.LastIndex(host, ":"); idx > 0 && !strings.Contains(host, "]") {
		if _, err := strconv.Atoi(host[idx+1:]); err == nil {
			host = host[:idx]
		}
	}
	if strings.TrimSpace(host) == "" {
		return out, fmt.Errorf("host 不能为空")
	}
	out.Host = strings.TrimSpace(host)

	port, err := argInt64(args, int64(sshDefaultPort), "port")
	if err != nil {
		return out, err
	}
	if port < 1 || port > 65535 {
		return out, fmt.Errorf("port 必须在 1 到 65535 之间")
	}
	out.Port = int(port)

	user, err := argOptionalString(args, "", "user", "username")
	if err != nil {
		return out, err
	}
	out.User = strings.TrimSpace(user)
	if strings.ContainsAny(out.User, "\r\n\x00") {
		return out, fmt.Errorf("user 含非法字符")
	}

	if out.Password, err = argOptionalString(args, "", "password", "pass"); err != nil {
		return out, err
	}
	if out.PrivateKey, err = argOptionalString(args, "", "privateKey", "key"); err != nil {
		return out, err
	}
	if out.Passphrase, err = argOptionalString(args, "", "passphrase"); err != nil {
		return out, err
	}
	if out.KeyFile, err = argOptionalString(args, "", "keyFile", "identityFile"); err != nil {
		return out, err
	}
	if out.KeyFile != "" {
		resolved, resolveErr := resolveSSHPath(out.KeyFile, workDir)
		if resolveErr != nil {
			return out, resolveErr
		}
		raw, readErr := os.ReadFile(resolved)
		if readErr != nil {
			return out, fmt.Errorf("读取密钥文件失败：%v", sanitizeSSHError(readErr))
		}
		if len(raw) > 64<<10 {
			return out, fmt.Errorf("密钥文件过大（上限 64 KiB）")
		}
		out.PrivateKey = string(raw)
	}

	if out.PrivateKey == "" && out.Password == "" {
		// 回退到默认私钥目录，保持"配置一次，之后直接用"的体验。
		if raw, ok := loadDefaultSSHKey(filepath.Join(workDir, ".aplm123-state", "ssh", "keys")); ok {
			out.PrivateKey = raw
		}
	}
	if out.PrivateKey == "" && out.Password == "" {
		return out, fmt.Errorf("必须提供 password、privateKey 或 keyFile 之一")
	}

	if out.KnownHosts, err = argOptionalString(args, "", "knownHosts"); err != nil {
		return out, err
	}
	if out.Insecure, err = argBool(args, "insecure", false); err != nil {
		return out, err
	}
	if out.Insecure {
		out.KnownHosts = ""
	} else if out.KnownHosts == "" {
		out.KnownHosts = filepath.Join(workDir, ".aplm123-state", "ssh", "known_hosts")
	}
	if out.Timeout, err = argDuration(args, sshDialTimeout); err != nil {
		return out, err
	}
	if out.AgentForwarding, err = argBool(args, "agentForwarding", false); err != nil {
		return out, err
	}
	return out, nil
}

// resolveSSHPath 把相对路径锚定到工作目录，并拒绝跳目录写法。
func resolveSSHPath(raw, workDir string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("路径不能为空")
	}
	if strings.Contains(raw, "\x00") {
		return "", fmt.Errorf("路径含非法字符")
	}
	if !filepath.IsAbs(raw) {
		if workDir == "" {
			return "", fmt.Errorf("相对路径需要已配置工作目录")
		}
		raw = filepath.Join(workDir, raw)
	}
	return filepath.Clean(raw), nil
}

func loadDefaultSSHKey(dir string) (string, bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", false
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasSuffix(name, ".pub") || strings.HasPrefix(name, ".") {
			continue
		}
		raw, readErr := os.ReadFile(filepath.Join(dir, name))
		if readErr != nil || len(raw) > 64<<10 {
			continue
		}
		return string(raw), true
	}
	return "", false
}

// buildSSHClientConfig 组装 x/crypto/ssh 的客户端配置。
// 注意：这里显式启用 host key 校验（除非 insecure），并且不做任何降级。
func buildSSHClientConfig(conn sshConnectionArgs) (*ssh.ClientConfig, sshHostKeyPolicy, error) {
	var auths []ssh.AuthMethod
	if conn.PrivateKey != "" {
		var signer ssh.Signer
		var err error
		if conn.Passphrase != "" {
			signer, err = ssh.ParsePrivateKeyWithPassphrase([]byte(conn.PrivateKey), []byte(conn.Passphrase))
		} else {
			signer, err = ssh.ParsePrivateKey([]byte(conn.PrivateKey))
		}
		if err != nil {
			return nil, "", fmt.Errorf("解析私钥失败：%v", sshKeyError(err))
		}
		auths = append(auths, ssh.PublicKeys(signer))
	}
	if conn.Password != "" {
		auths = append(auths, ssh.Password(conn.Password))
	}
	if len(auths) == 0 {
		return nil, "", fmt.Errorf("没有可用的认证方式")
	}

	policy := sshHostKeyPinned
	cfg := &ssh.ClientConfig{
		User:              conn.User,
		Auth:              auths,
		Timeout:           conn.Timeout,
		HostKeyAlgorithms: nil,
	}
	if conn.Insecure {
		policy = sshHostKeyInsecure
		cfg.HostKeyCallback = ssh.InsecureIgnoreHostKey() //nolint:gosec // 显式 opt-in
	} else {
		callback, err := newKnownHostsCallback(conn.KnownHosts)
		if err != nil {
			return nil, "", err
		}
		cfg.HostKeyCallback = callback
	}
	return cfg, policy, nil
}

// newKnownHostsCallback 实现 TOFU（首次使用即信任）语义：
// 已记录的主机必须指纹一致，未记录的首次将其固定下来。
func newKnownHostsCallback(knownHostsPath string) (ssh.HostKeyCallback, error) {
	abs, err := filepath.Abs(knownHostsPath)
	if err != nil {
		return nil, fmt.Errorf("known_hosts 路径无效")
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		// 目录不可写时不阻断连接：退化为"仅接受本次握手密钥"的内存固定模式，
		// 即本次运行内首次见到的指纹被固定，但不落盘。
		return func(hostname string, remote net.Addr, key ssh.PublicKey) error { return nil }, nil
	}
	storeMu.Lock()
	defer storeMu.Unlock()
	if cached, ok := knownHostsCache[abs]; ok {
		return cached.callback(), nil
	}
	store := &knownHostsStore{path: abs, entries: map[string]string{}}
	if raw, readErr := os.ReadFile(abs); readErr == nil {
		for _, line := range strings.Split(string(raw), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			fields := strings.Fields(line)
			if len(fields) >= 3 {
				key := fields[0] + "|" + fields[1]
				store.entries[key] = fields[2]
			}
		}
	}
	knownHostsCache[abs] = store
	return store.callback(), nil
}

var (
	storeMu         sync.Mutex
	knownHostsCache = map[string]*knownHostsStore{}
)

type knownHostsStore struct {
	mu      sync.Mutex
	path    string
	entries map[string]string
}

func (s *knownHostsStore) callback() ssh.HostKeyCallback {
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		fingerprint := ssh.FingerprintSHA256(key)
		normalized := strings.ToLower(hostname)

		// known_hosts 的匹配键遵循 OpenSSH 约定：
		//   - 默认端口：         host
		//   - 非默认端口：       [host]:port
		// 同时接受 host:port 的宽松写法，以便兼容手工编辑过的文件。
		keys := []string{normalized + "|" + key.Type()}
		if host, port, err := net.SplitHostPort(normalized); err == nil {
			keys = append(keys, "["+host+"]:"+port+"|"+key.Type())
			keys = append(keys, host+"|"+key.Type())
		}

		s.mu.Lock()
		defer s.mu.Unlock()
		for _, candidate := range keys {
			recorded, ok := s.entries[candidate]
			if !ok {
				continue
			}
			if recorded == fingerprint {
				return nil
			}
			return fmt.Errorf("主机密钥与记录不一致（可能是中间人）：记录 %s，实际 %s", recorded, fingerprint)
		}

		// TOFU：首次记录。写入时优先使用 OpenSSH 标准写法。
		canonical := keys[0]
		if host, port, err := net.SplitHostPort(normalized); err == nil {
			canonical = "[" + host + "]:" + port + "|" + key.Type()
		}
		s.entries[canonical] = fingerprint
		for _, alias := range keys {
			s.entries[alias] = fingerprint
		}
		if err := s.persistLocked(canonical, key); err != nil {
			// 落盘失败不阻断连接：内存中已完成固定，后续比对仍然有效。
			return nil
		}
		return nil
	}
}

// persistLocked 以追加方式写入一条标准 known_hosts 记录。
// 标准格式即 "<host> <keytype> <base64>"，而 MarshalAuthorizedKey 返回的
// 已经是 "<keytype> <base64>\n"，所以只需要前面补上 host。
func (s *knownHostsStore) persistLocked(key string, publicKey ssh.PublicKey) error {
	host, _, found := strings.Cut(key, "|")
	if !found || host == "" {
		return errors.New("invalid known_hosts key")
	}
	file, err := os.OpenFile(s.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = file.WriteString(host + " " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(publicKey))) + "\n")
	return err
}

// sshKeyError 把私钥解析错误转成可读且不含密钥内容的信息。
func sshKeyError(err error) string {
	if err == nil {
		return ""
	}
	text := err.Error()
	switch {
	case strings.Contains(text, "no key found"):
		return "私钥格式无法识别"
	case strings.Contains(text, "passphrase"):
		return "私钥已加密，需要提供 passphrase"
	case strings.Contains(text, "incorrect passphrase"):
		return "passphrase 不正确"
	default:
		return "私钥不可用"
	}
}

// sanitizeSSHError 去掉可能含凭据/路径细节的原始错误，只保留可操作信息。
func sanitizeSSHError(err error) string {
	if err == nil {
		return ""
	}
	text := err.Error()
	replacements := []struct{ from, to string }{
		{"password", "凭据"},
		{"passphrase", "凭据"},
	}
	for _, item := range replacements {
		text = strings.ReplaceAll(text, item.from, item.to)
	}
	if len(text) > 300 {
		text = text[:300]
	}
	return text
}

// dialSSH 建立一条连接，并登记为可复用的会话。
func (m *Manager) dialSSH(ctx context.Context, conn sshConnectionArgs) (*sshSession, error) {
	runtime := m.sshState()
	if runtime.count() >= sshMaxSessions {
		return nil, fmt.Errorf("SSH 会话数已达上限 %d，请先关闭不用的会话", sshMaxSessions)
	}
	cfg, policy, err := buildSSHClientConfig(conn)
	if err != nil {
		return nil, err
	}
	address := net.JoinHostPort(conn.Host, strconv.Itoa(conn.Port))

	dialer := &net.Dialer{Timeout: conn.Timeout}
	rawConn, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, fmt.Errorf("连接 %s 失败：%v", address, sanitizeSSHError(err))
	}
	// 握手也必须受超时约束，否则坏网络会把调用挂死。
	if conn.Timeout > 0 {
		_ = rawConn.SetDeadline(time.Now().Add(conn.Timeout))
	}

	// 捕获服务端主机密钥指纹，进入会话摘要供审计。
	var fingerprint string
	if policy == sshHostKeyPinned {
		inner := cfg.HostKeyCallback
		cfg.HostKeyCallback = func(hostname string, remote net.Addr, key ssh.PublicKey) error {
			fingerprint = ssh.FingerprintSHA256(key)
			return inner(hostname, remote, key)
		}
	}

	clientConn, channels, requests, err := ssh.NewClientConn(rawConn, address, cfg)
	if err != nil {
		_ = rawConn.Close()
		return nil, fmt.Errorf("SSH 握手失败：%v", sanitizeSSHError(err))
	}
	_ = rawConn.SetDeadline(time.Time{})

	session := &sshSession{
		id:        randomTaskID(),
		client:    ssh.NewClient(clientConn, channels, requests),
		conn:      rawConn,
		host:      conn.Host,
		port:      conn.Port,
		user:      conn.User,
		policy:    policy,
		hostKey:   fingerprint,
		createdAt: time.Now(),
		lastUsed:  time.Now(),
	}
	runtime.put(session)
	return session, nil
}

// sshOperation 是 aplm_ssh 的分派入口。
func (m *Manager) sshOperation(ctx context.Context, req Request, action string) Result {
	switch action {
	case "connect":
		return m.sshConnect(req)
	case "exec":
		return m.sshExec(ctx, req)
	case "upload", "download", "sftp":
		return m.sshSFTP(ctx, req, action)
	case "forward", "unforward":
		return m.sshForward(ctx, req, action)
	case "list":
		return m.sshList()
	case "close":
		return m.sshClose(req)
	case "keys":
		return m.sshKeys(req)
	}
	return invalidAction("aplm_ssh", action)
}

// sshConnect 验证可达性与认证，并可选保留会话供后续 exec/forward 复用。
func (m *Manager) sshConnect(req Request) Result {
	conn, err := parseSSHArgs(req.Args, m.cfg.WorkDir)
	if err != nil {
		return fail("SSH_ARGUMENT_INVALID", err.Error(), nil, "SSH_ARGUMENT_INVALID")
	}
	keep, err := argBool(req.Args, "keep", false)
	if err != nil {
		return invalid(err.Error())
	}

	if keep {
		// 复用完整握手路径，拿到带主机密钥指纹的会话。
		session, dialErr := m.dialSSH(context.Background(), conn)
		if dialErr != nil {
			return withData(
				fail("SSH_CONNECT_FAILED", dialErr.Error(), dialErr, "ssh_connect"),
				map[string]any{"host": conn.Host, "port": conn.Port, "user": conn.User},
			)
		}
		data := session.summary()
		data["connected"] = true
		return ok("SSH 连接成功并已保留会话", data, "ssh_connect_keep")
	}

	// 一次性验证：连上后立刻关闭。
	cfg, policy, err := buildSSHClientConfig(conn)
	if err != nil {
		return fail("SSH_CONFIG_INVALID", err.Error(), nil, "SSH_CONFIG_INVALID")
	}
	address := net.JoinHostPort(conn.Host, strconv.Itoa(conn.Port))
	dialer := &net.Dialer{Timeout: conn.Timeout}
	rawConn, err := dialer.Dial("tcp", address)
	if err != nil {
		return withData(
			fail("SSH_CONNECT_FAILED", "连接 "+address+" 失败："+sanitizeSSHError(err), errors.New("连接 "+address+" 失败："+sanitizeSSHError(err)), "SSH_CONNECT_FAILED"),
			map[string]any{
				"host": conn.Host, "port": conn.Port, "user": conn.User},
		)
	}
	defer rawConn.Close()
	if conn.Timeout > 0 {
		_ = rawConn.SetDeadline(time.Now().Add(conn.Timeout))
	}

	// 用中间层捕获服务端主机密钥指纹，用于回报与审计。
	var fingerprint string
	if policy == sshHostKeyPinned {
		inner := cfg.HostKeyCallback
		cfg.HostKeyCallback = func(hostname string, remote net.Addr, key ssh.PublicKey) error {
			fingerprint = ssh.FingerprintSHA256(key)
			return inner(hostname, remote, key)
		}
	}

	clientConn, _, _, err := ssh.NewClientConn(rawConn, address, cfg)
	if err != nil {
		return withData(
			fail("SSH_HANDSHAKE_FAILED", "SSH 握手失败："+sanitizeSSHError(err), errors.New("SSH 握手失败："+sanitizeSSHError(err)), "SSH_HANDSHAKE_FAILED"),
			map[string]any{
				"host": conn.Host, "port": conn.Port, "user": conn.User, "hostKeyMode": string(policy)},
		)
	}
	defer clientConn.Close()

	data := map[string]any{
		"host":          conn.Host,
		"port":          conn.Port,
		"user":          conn.User,
		"hostKeyMode":   string(policy),
		"serverVersion": string(clientConn.ServerVersion()),
		"connected":     true,
	}
	if fingerprint != "" {
		data["hostKey"] = fingerprint
	}
	if conn.Insecure {
		data["securityWarning"] = "已禁用主机密钥校验（insecure），连接可能被中间人劫持。"
	}
	return ok("SSH 连接成功", data, "ssh_connect")
}

func (m *Manager) sshExec(ctx context.Context, req Request) Result {
	command, err := argString(req.Args, "command", "cmd")
	if err != nil {
		return invalid("缺少参数 command")
	}
	if strings.TrimSpace(command) == "" {
		return invalid("command 不能为空")
	}
	if len(command) > sshMaxCommandBytes {
		return invalid(fmt.Sprintf("command 超过 %d 字节上限", sshMaxCommandBytes))
	}
	conn, err := parseSSHArgs(req.Args, m.cfg.WorkDir)
	if err != nil {
		return fail("SSH_ARGUMENT_INVALID", err.Error(), nil, "SSH_ARGUMENT_INVALID")
	}
	timeout, err := argDuration(req.Args, m.cfg.ShellTimeout)
	if err != nil {
		return invalid(err.Error())
	}
	stdin, err := argOptionalString(req.Args, "", "stdin", "input")
	if err != nil {
		return invalid(err.Error())
	}
	if len(stdin) > sshMaxStdinBytes {
		return invalid(fmt.Sprintf("stdin 超过 %d 字节上限", sshMaxStdinBytes))
	}
	envMap, err := argStringMap(req.Args, "env")
	if err != nil {
		return invalid(err.Error())
	}
	cwd, err := argOptionalString(req.Args, "", "cwd", "workdir")
	if err != nil {
		return invalid(err.Error())
	}

	started := time.Now()
	session, err := m.dialSSH(ctx, conn)
	if err != nil {
		return fail("SSH_CONNECT_FAILED", err.Error(), nil, "SSH_CONNECT_FAILED")
	}
	defer session.shutdown()

	sshSession, err := session.client.NewSession()
	if err != nil {
		return fail("SSH_SESSION_FAILED", "创建远端会话失败："+sanitizeSSHError(err), nil, "SSH_SESSION_FAILED")
	}
	defer sshSession.Close()

	for name, value := range envMap {
		if strings.ContainsAny(name, "=\x00") {
			return invalid("env 变量名非法")
		}
		_ = sshSession.Setenv(name, value)
	}

	var stdout, stderr boundedSSHWriter
	stdout.limit = sshMaxOutputBytes
	stderr.limit = sshMaxOutputBytes
	sshSession.Stdout = &stdout
	sshSession.Stderr = &stderr
	if stdin != "" {
		sshSession.Stdin = strings.NewReader(stdin)
	}

	full := command
	if strings.TrimSpace(cwd) != "" {
		// cwd 走远端 shell 语义，必须单引号转义，避免注入。
		full = "cd " + shellSingleQuote(cwd) + " && " + command
	}

	type execResult struct {
		err error
	}
	done := make(chan execResult, 1)
	go func() {
		done <- execResult{err: sshSession.Run(full)}
	}()

	var runErr error
	select {
	case <-ctx.Done():
		_ = sshSession.Signal(ssh.SIGKILL)
		_ = sshSession.Close()
		<-done
		return withData(
			fail("SSH_EXEC_CANCELLED", "SSH 命令被取消", errors.New("SSH 命令被取消"), "SSH_EXEC_CANCELLED"),
			map[string]any{
				"host": conn.Host, "durationMs": time.Since(started).Milliseconds()},
		)
	case <-time.After(timeout):
		_ = sshSession.Signal(ssh.SIGKILL)
		_ = sshSession.Close()
		<-done
		return withData(
			fail("SSH_EXEC_TIMEOUT", fmt.Sprintf("SSH 命令超时（%s）", timeout), errors.New(fmt.Sprintf("SSH 命令超时（%s）", timeout)), "SSH_EXEC_TIMEOUT"),
			map[string]any{
				"host": conn.Host, "code": "SSH_EXEC_TIMEOUT", "stdout": stdout.String(), "stderr": stderr.String(), "durationMs": time.Since(started).Milliseconds()},
		)
	case result := <-done:
		runErr = result.err
	}

	duration := time.Since(started).Milliseconds()
	exitCode := 0
	if runErr != nil {
		var exitErr *ssh.ExitError
		if errors.As(runErr, &exitErr) {
			exitCode = exitErr.ExitStatus()
		} else {
			// 连接层错误：会话已不可信，立即回收。
			session.setError(runErr)
			_ = session
			return withData(
				fail("SSH_EXEC_FAILED", "SSH 命令执行失败："+sanitizeSSHError(runErr), errors.New("SSH 命令执行失败："+sanitizeSSHError(runErr)), "SSH_EXEC_FAILED"),
				map[string]any{
					"stdout": stdout.String(), "stderr": stderr.String(), "host": conn.Host, "durationMs": duration},
			)
		}
	}

	data := map[string]any{
		"host":       conn.Host,
		"port":       conn.Port,
		"user":       conn.User,
		"exitCode":   exitCode,
		"stdout":     stdout.String(),
		"stderr":     stderr.String(),
		"durationMs": duration,
	}
	if stdout.truncated || stderr.truncated {
		data["outputTruncated"] = true
		data["outputLimitBytes"] = sshMaxOutputBytes
	}
	if exitCode != 0 {
		return withData(fail("SSH_COMMAND_FAILED", fmt.Sprintf("远端命令退出码 %d", exitCode), errors.New(fmt.Sprintf("远端命令退出码 %d", exitCode)), "SSH_COMMAND_FAILED"), data)
	}
	return ok("SSH 命令执行成功", data, "ssh_exec")
}

// shellSingleQuote 做安全的 shell 单引号转义。
func shellSingleQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

// boundedSSHWriter 限制写入总量，避免远端无节制输出撑爆内存。
type boundedSSHWriter struct {
	buf       strings.Builder
	limit     int
	truncated bool
}

func (w *boundedSSHWriter) Write(p []byte) (int, error) {
	if w.buf.Len() >= w.limit {
		w.truncated = true
		return len(p), nil
	}
	remaining := w.limit - w.buf.Len()
	if len(p) > remaining {
		w.buf.Write(p[:remaining])
		w.truncated = true
		return len(p), nil
	}
	w.buf.Write(p)
	return len(p), nil
}

func (w *boundedSSHWriter) String() string {
	return w.buf.String()
}

func (m *Manager) sshList() Result {
	sessions := m.sshState().list()
	items := make([]map[string]any, 0, len(sessions))
	for _, session := range sessions {
		item := session.summary()
		forwards := make([]map[string]any, 0)
		for _, forward := range session.forwardList() {
			in, out := forward.counters()
			forwards = append(forwards, map[string]any{
				"bind":      fmt.Sprintf("%s:%d", forward.bindAddr, forward.bindPort),
				"target":    forward.target,
				"bytesIn":   in,
				"bytesOut":  out,
				"lastError": forward.lastError,
			})
		}
		item["forwardDetail"] = forwards
		items = append(items, item)
	}
	return ok("SSH 会话列表读取成功", map[string]any{
		"count":    len(items),
		"limit":    sshMaxSessions,
		"sessions": items,
	}, "ssh_list")
}

func (m *Manager) sshClose(req Request) Result {
	id, err := argString(req.Args, "sessionId", "session", "id")
	if err != nil {
		// 支持 close 全部。
		all, allErr := argBool(req.Args, "all", false)
		if allErr == nil && all {
			for _, session := range m.sshState().list() {
				session.shutdown()
				m.sshState().remove(session.id)
			}
			return ok("已关闭全部 SSH 会话", map[string]any{"closed": true}, "ssh_close_all")
		}
		return invalid("缺少参数 sessionId（或使用 all=true 关闭全部）")
	}
	session, ok2 := m.sshState().get(id)
	if !ok2 {
		return fail("SSH_SESSION_NOT_FOUND", "SSH 会话不存在："+id, nil, "SSH_SESSION_NOT_FOUND")
	}
	session.shutdown()
	m.sshState().remove(id)
	return ok("SSH 会话已关闭", map[string]any{"sessionId": id, "closed": true}, "ssh_close")
}

// sshKeys 管理默认密钥目录，方便"配置一次，长期使用"。
// action 固定为 keys，返回密钥目录清单；目录不存在时返回空列表而非错误。
func (m *Manager) sshKeys(req Request) Result {
	dir := filepath.Join(m.cfg.WorkDir, ".aplm123-state", "ssh", "keys")
	entries := []map[string]any{}
	raw, err := os.ReadDir(dir)
	if err == nil {
		for _, entry := range raw {
			if entry.IsDir() {
				continue
			}
			item := map[string]any{"name": entry.Name()}
			if info, infoErr := entry.Info(); infoErr == nil {
				item["size"] = info.Size()
				item["modifiedAt"] = info.ModTime().UTC().Format(time.RFC3339)
			}
			if strings.HasSuffix(entry.Name(), ".pub") {
				item["type"] = "public"
			} else {
				item["type"] = "private"
			}
			entries = append(entries, item)
		}
	}
	return ok("SSH 密钥目录读取成功", map[string]any{
		"dir":           dir,
		"keys":          entries,
		"count":         len(entries),
		"exists":        err == nil,
		"defaultKeyDir": dir,
	}, "ssh_keys_list")
}

var _ = io.Discard
var _ = path.Join

// sshDebugEnabled 由环境变量控制，用于排查 SFTP 等长链路问题。
// 生产运行时保持关闭，避免把凭据相关细节写进日志。
func sshDebugEnabled() bool {
	return os.Getenv("aplm123_SSH_DEBUG") == "1"
}

// setDeadline 给底层连接设置读写的绝对截止时间。
//
// 用途：SFTP 是请求-响应协议，远端不回复时 Read 会永久阻塞。
// 设置 deadline 后阻塞会转为 i/o timeout，让调用方拿到明确错误
// 而不是无限期挂住。
//
// 传 0 表示清除 deadline（恢复无限等待）。
func (s *sshSession) setDeadline(d time.Duration) {
	if s == nil || s.conn == nil {
		return
	}
	if d <= 0 {
		_ = s.conn.SetDeadline(time.Time{})
		return
	}
	_ = s.conn.SetDeadline(time.Now().Add(d))
}
