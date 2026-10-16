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
	// sshMaxSessions 是**保留会话**（keep=true / 复用 sessionId）的数量上限。
	// 取 32 与 frontend 的 maxConnections 对齐：会话内部复用同一条 TCP 连接，
	// 不会额外放大服务端连接数。注意一次性 exec（不传 sessionId）不进池。
	sshMaxSessions       = 32
	// sshSessionIdle 决定空闲会话多久被自动回收。之前只定义未使用，
	// 导致调用方忘记 close 时会话永久占用名额；现在由 evictIdle 真正执行。
	sshSessionIdle       = 10 * time.Minute
	sshMaxCommandBytes   = 32 << 10
	sshMaxStdinBytes     = 1 << 20
	sshMaxOutputBytes    = 4 << 20
	sshMaxForwardBytes   = 4 << 20
	// sshMaxBatch 限制单次 exec 展开的命令/目标数量，避免一次调用吃满资源。
	sshMaxBatch          = 32
	// sshBatchConcurrency 是 targets/jobs 扇出时的并发上限，与服务端 totalTasks 对齐。
	sshBatchConcurrency  = 8
	// sshSessionSweepInterval 是后台清扫空闲会话的间隔。
	sshSessionSweepInterval = time.Minute
	// sshLimitMessageMaxSessions 限制上限报错里列出的会话条数，
	// 避免批量场景下错误信息过长、把大量内网地址写进日志。
	sshLimitMessageMaxSessions = 5
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
			// 后台周期回收空闲会话：调用方忘记 close 时也能自愈。
			m.ssh.startSweeper(m.closed)
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

// evictIdle 回收空闲超时的会话，返回被回收的数量。
//
// 为什么需要：会话只在显式 close 时释放，调用方一旦忘记 close，
// 名额就被永久占用，表现为「SSH 会话数已达上限」却看不到活跃连接。
// 有了它，即使调用方不 close 也能自愈。
//
// 保护规则：
//   - 带活跃端口转发的会话**不回收** —— 转发是长期存在的，
//     杀掉会断掉用户正在使用的隧道，与「建完就挂着」的语义冲突。
//   - 已关闭的会话直接清理出表。
func (r *sshRuntime) evictIdle(ttl time.Duration) int {
	if r == nil || ttl <= 0 {
		return 0
	}
	now := time.Now()
	evicted := 0
	for _, session := range r.list() {
		if session.idle(now) < ttl {
			continue
		}
		if session.hasForwards() {
			continue
		}
		session.shutdown()
		r.remove(session.id)
		evicted++
	}
	return evicted
}

// admit 在建立新会话前腾出名额：先回收空闲会话，再判断是否真的满了。
// 返回错误时说明即使回收后也没有可用名额。
func (r *sshRuntime) admit() error {
	if r.count() < sshMaxSessions {
		return nil
	}
	if r.evictIdle(sshSessionIdle) > 0 && r.count() < sshMaxSessions {
		return nil
	}
	return fmt.Errorf("%s", r.limitMessage())
}

// limitMessage 说明当前为何无法再建会话，并列出可关闭的候选。
// 只说「请先关闭不用的会话」会让调用方无从下手，所以附上会话清单。
//
// 清单只列前若干个：批量场景下会话可能很多，全部拼接会让错误信息过长，
// 也会把大量内网地址写进日志。
func (r *sshRuntime) limitMessage() string {
	sessions := r.list()
	var b strings.Builder
	fmt.Fprintf(&b, "SSH 会话数已达上限 %d，且没有可自动回收的空闲会话", sshMaxSessions)
	if len(sessions) == 0 {
		return b.String()
	}
	now := time.Now()
	b.WriteString("。当前会话：")
	for i, session := range sessions {
		if i >= sshLimitMessageMaxSessions {
			fmt.Fprintf(&b, "…等共 %d 条", len(sessions))
			break
		}
		if i > 0 {
			b.WriteString("；")
		}
		idle := session.idle(now).Truncate(time.Second)
		fmt.Fprintf(&b, "%s(%s:%d 空闲 %s)", session.id, session.host, session.port, idle)
	}
	b.WriteString("。可用 aplm_ssh close 关闭指定会话，或传 all=true 全部关闭。")
	return b.String()
}

// startSweeper 启动后台清扫，周期回收空闲会话。
// 传入的 done 通道关闭后协程退出（由 Manager.Close 触发）。
func (r *sshRuntime) startSweeper(done <-chan struct{}) {
	if r == nil || done == nil {
		return
	}
	r.once.Do(func() {
		go func() {
			ticker := time.NewTicker(sshSessionSweepInterval)
			defer ticker.Stop()
			for {
				select {
				case <-done:
					return
				case <-ticker.C:
					r.evictIdle(sshSessionIdle)
				}
			}
		}()
	})
}

func (s *sshSession) touch() {
	s.mu.Lock()
	s.lastUsed = time.Now()
	s.mu.Unlock()
}

// idle 返回距今多久没被使用过。
func (s *sshSession) idle(now time.Time) time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastUsed.IsZero() {
		return now.Sub(s.createdAt)
	}
	return now.Sub(s.lastUsed)
}

// hasForwards 判断会话上是否挂着活跃的端口转发。
func (s *sshSession) hasForwards() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.listeners) > 0
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
// findByTarget 找出指向同一目标的存活会话。
// 批量场景下反复 connect 同一台机器很常见，复用可以避免占满名额。
func (r *sshRuntime) findByTarget(host string, port int, user string) (*sshSession, bool) {
	for _, session := range r.list() {
		if session.matches(host, port, user) {
			return session, true
		}
	}
	return nil, false
}

// matches 判断会话是否指向给定目标（host 不区分大小写）。
func (s *sshSession) matches(host string, port int, user string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	return strings.EqualFold(s.host, host) && s.port == port && s.user == user
}

func (m *Manager) dialSSH(ctx context.Context, conn sshConnectionArgs) (*sshSession, error) {
	runtime := m.sshState()
	// 先回收空闲会话再判断名额，避免调用方忘记 close 导致的永久占用。
	if err := runtime.admit(); err != nil {
		return nil, err
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
		// 同一目标已有存活会话时直接复用，避免反复 connect 占满名额。
		// 批量场景（同一台机器跑多次 connect）下这一条能省掉大量槽位。
		if existing, found := m.sshState().findByTarget(conn.Host, conn.Port, conn.User); found {
			existing.touch()
			data := existing.summary()
			data["connected"] = true
			data["reused"] = true
			return ok("SSH 会话已存在，复用现有会话", data, "ssh_connect_reuse")
		}
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

// sshExecRequest 是一次远端执行的全部参数（已解析）。
// 抽出来是为了让 commands/targets/jobs 三种批量形态共用同一条执行路径。
type sshExecRequest struct {
	command string
	timeout time.Duration
	stdin   string
	env     map[string]string
	cwd     string
}

// sshExec 是 aplm_ssh exec 的入口，支持四种形态：
//
//  1. 单条一次性执行（默认）：连上、跑完、断开，不进会话池。
//  2. 复用会话：传 sessionId，在已有会话上执行（不新建连接）。
//  3. 多条命令：传 commands:[...]，一次连接顺序跑完。
//  4. 扇出：
//     - targets:[sessionId...] + command  → 多个会话跑同一条命令
//     - jobs:[{sessionId,command}...]     → 每个会话跑各自的命令
//
// 形态 3/4 的存在意义：把「N 台机器 × M 条命令」收敛成一次 MCP 调用，
// 既省掉大量往返，也避免调用方反复 connect 把会话名额占满。
func (m *Manager) sshExec(ctx context.Context, req Request) Result {
	base, err := m.parseExecBase(req)
	if err != nil {
		return invalid(err.Error())
	}

	// 形态 4b：jobs 优先，指定每个会话跑什么。
	if raw, ok := req.Args["jobs"]; ok && raw != nil {
		return m.execJobs(ctx, base, raw)
	}
	// 形态 4a：targets 复用同一命令扇出。
	if raw, ok := req.Args["targets"]; ok && raw != nil {
		return m.execTargets(ctx, base, raw)
	}
	// 形态 3：多条命令在同一个连接上顺序执行。
	if raw, ok := req.Args["commands"]; ok && raw != nil {
		return m.execCommands(ctx, req, base, raw)
	}

	// 形态 1/2：单条命令。有 sessionId 就复用，否则新建一次性连接。
	if base.command == "" {
		return invalid("缺少参数 command")
	}
	if len(base.command) > sshMaxCommandBytes {
		return invalid(fmt.Sprintf("command 超过 %d 字节上限", sshMaxCommandBytes))
	}

	if id, ok := execSessionID(req.Args); ok {
		session, found := m.sshState().get(id)
		if !found {
			return fail("SSH_SESSION_NOT_FOUND", "SSH 会话不存在："+id, nil, "SSH_SESSION_NOT_FOUND")
		}
		one := m.runOnSession(ctx, session, base)
		return one.result
	}

	conn, err := parseSSHArgs(req.Args, m.cfg.WorkDir)
	if err != nil {
		return fail("SSH_ARGUMENT_INVALID", err.Error(), nil, "SSH_ARGUMENT_INVALID")
	}
	session, err := m.dialSSH(ctx, conn)
	if err != nil {
		return fail("SSH_CONNECT_FAILED", err.Error(), nil, "SSH_CONNECT_FAILED")
	}
	// 一次性执行：跑完立刻断开，并从会话池移除。
	// 只 shutdown 不移除会让僵尸条目永久占用名额。
	defer func() {
		session.shutdown()
		m.sshState().remove(session.id)
	}()
	return m.runOnSession(ctx, session, base).result
}

// parseExecBase 解析四种形态共用的参数。
// 注意这里不校验 command 是否为空 —— commands/targets/jobs 形态允许不传单条 command。
func (m *Manager) parseExecBase(req Request) (sshExecRequest, error) {
	out := sshExecRequest{timeout: m.cfg.ShellTimeout}
	command, err := argOptionalString(req.Args, "", "command", "cmd")
	if err != nil {
		return out, err
	}
	out.command = command

	timeout, err := argDuration(req.Args, m.cfg.ShellTimeout)
	if err != nil {
		return out, err
	}
	out.timeout = timeout

	stdin, err := argOptionalString(req.Args, "", "stdin", "input")
	if err != nil {
		return out, err
	}
	if len(stdin) > sshMaxStdinBytes {
		return out, fmt.Errorf("stdin 超过 %d 字节上限", sshMaxStdinBytes)
	}
	out.stdin = stdin

	envMap, err := argStringMap(req.Args, "env")
	if err != nil {
		return out, err
	}
	out.env = envMap

	cwd, err := argOptionalString(req.Args, "", "cwd", "workdir")
	if err != nil {
		return out, err
	}
	out.cwd = cwd
	return out, nil
}

// execSessionID 从参数里取出可选的会话 ID。
func execSessionID(args map[string]any) (string, bool) {
	for _, key := range []string{"sessionId", "session"} {
		if value, ok := args[key]; ok {
			if text, ok := value.(string); ok {
				if trimmed := strings.TrimSpace(text); trimmed != "" {
					return trimmed, true
				}
			}
		}
	}
	return "", false
}

// execOutcome 是单次远端执行的结果，便于批量形态汇总。
type execOutcome struct {
	result   Result
	exitCode int
}

// runOnSession 在给定会话上执行一条命令。
//
// 与旧实现的关键差别：**不再关闭会话**（由调用方决定生命周期），
// 并在成功/失败后刷新 lastUsed，让空闲回收能正确判断活跃度。
func (m *Manager) runOnSession(ctx context.Context, session *sshSession, spec sshExecRequest) execOutcome {
	started := time.Now()
	session.touch()

	sshSession, err := session.client.NewSession()
	if err != nil {
		session.setError(err)
		return execOutcome{result: fail("SSH_SESSION_FAILED", "创建远端会话失败："+sanitizeSSHError(err), nil, "SSH_SESSION_FAILED")}
	}
	defer sshSession.Close()

	for name, value := range spec.env {
		if strings.ContainsAny(name, "=\x00") {
			return execOutcome{result: invalid("env 变量名非法")}
		}
		_ = sshSession.Setenv(name, value)
	}

	var stdout, stderr boundedSSHWriter
	stdout.limit = sshMaxOutputBytes
	stderr.limit = sshMaxOutputBytes
	sshSession.Stdout = &stdout
	sshSession.Stderr = &stderr
	if spec.stdin != "" {
		sshSession.Stdin = strings.NewReader(spec.stdin)
	}

	full := spec.command
	if strings.TrimSpace(spec.cwd) != "" {
		// cwd 走远端 shell 语义，必须单引号转义，避免注入。
		full = "cd " + shellSingleQuote(spec.cwd) + " && " + spec.command
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
		return execOutcome{result: withData(
			fail("SSH_EXEC_CANCELLED", "SSH 命令被取消", errors.New("SSH 命令被取消"), "SSH_EXEC_CANCELLED"),
			map[string]any{
				"host": session.host, "durationMs": time.Since(started).Milliseconds()},
		)}
	case <-time.After(spec.timeout):
		_ = sshSession.Signal(ssh.SIGKILL)
		_ = sshSession.Close()
		<-done
		return execOutcome{result: withData(
			fail("SSH_EXEC_TIMEOUT", fmt.Sprintf("SSH 命令超时（%s）", spec.timeout), errors.New(fmt.Sprintf("SSH 命令超时（%s）", spec.timeout)), "SSH_EXEC_TIMEOUT"),
			map[string]any{
				"host": session.host, "code": "SSH_EXEC_TIMEOUT", "stdout": stdout.String(), "stderr": stderr.String(), "durationMs": time.Since(started).Milliseconds()},
		)}
	case result := <-done:
		runErr = result.err
	}

	duration := time.Since(started).Milliseconds()
	session.touch()
	exitCode := 0
	if runErr != nil {
		var exitErr *ssh.ExitError
		if errors.As(runErr, &exitErr) {
			exitCode = exitErr.ExitStatus()
		} else {
			// 连接层错误：会话已不可信，标记后由调用方回收。
			session.setError(runErr)
			return execOutcome{
				exitCode: -1,
				result: withData(
					fail("SSH_EXEC_FAILED", "SSH 命令执行失败："+sanitizeSSHError(runErr), errors.New("SSH 命令执行失败："+sanitizeSSHError(runErr)), "SSH_EXEC_FAILED"),
					map[string]any{
						"stdout": stdout.String(), "stderr": stderr.String(), "host": session.host, "durationMs": duration},
				)}
		}
	}

	data := map[string]any{
		"host":       session.host,
		"port":       session.port,
		"user":       session.user,
		"sessionId":  session.id,
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
		return execOutcome{exitCode: exitCode, result: withData(
			fail("SSH_COMMAND_FAILED", fmt.Sprintf("远端命令退出码 %d", exitCode), errors.New(fmt.Sprintf("远端命令退出码 %d", exitCode)), "SSH_COMMAND_FAILED"), data)}
	}
	return execOutcome{exitCode: 0, result: ok("SSH 命令执行成功", data, "ssh_exec")}
}

// shellSingleQuote 做安全的 shell 单引号转义。
// execCommands 在同一个连接上顺序执行多条命令。
// 相比调用方循环发起 N 次 exec，这里只建一次远端会话，
// 既省掉往返开销，也保证了命令间的顺序与同机上下文一致。
func (m *Manager) execCommands(ctx context.Context, req Request, base sshExecRequest, raw any) Result {
	commands, err := toCommandList(raw)
	if err != nil {
		return invalid(err.Error())
	}
	if len(commands) == 0 {
		return invalid("commands 不能为空")
	}
	if len(commands) > sshMaxBatch {
		return invalid(fmt.Sprintf("commands 最多 %d 条", sshMaxBatch))
	}
	for i, command := range commands {
		if strings.TrimSpace(command) == "" {
			return invalid(fmt.Sprintf("commands[%d] 不能为空", i))
		}
		if len(command) > sshMaxCommandBytes {
			return invalid(fmt.Sprintf("commands[%d] 超过 %d 字节上限", i, sshMaxCommandBytes))
		}
	}

	sessionID, _ := execSessionID(req.Args)
	// 有 sessionId 就复用，否则新建一次性连接。
	session, cleanup, err := m.execSession(ctx, req, sessionID)
	if err != nil {
		return fail("SSH_CONNECT_FAILED", err.Error(), nil, "SSH_CONNECT_FAILED")
	}
	defer cleanup()

	started := time.Now()
	results := make([]map[string]any, 0, len(commands))
	failed := 0
	for i, command := range commands {
		spec := base
		spec.command = command
		outcome := m.runOnSession(ctx, session, spec)
		item := resultData(outcome.result)
		if item == nil {
			item = map[string]any{"error": outcome.result.Message}
		}
		item["index"] = i
		item["command"] = command
		results = append(results, item)
		if outcome.exitCode != 0 {
			failed++
		}
	}

	data := map[string]any{
		"host":       session.host,
		"port":       session.port,
		"user":       session.user,
		"sessionId":  session.id,
		"count":      len(commands),
		"failed":     failed,
		"results":    results,
		"durationMs": time.Since(started).Milliseconds(),
	}
	if failed > 0 {
		return withData(fail("SSH_COMMAND_FAILED", fmt.Sprintf("批量执行完成，%d/%d 条命令失败", failed, len(commands)), nil, "ssh_exec_commands"), data)
	}
	return ok(fmt.Sprintf("批量执行完成，共 %d 条命令", len(commands)), data, "ssh_exec_commands")
}

// execTargets 让多个会话并行执行同一条命令。
// 典型用途：一批服务器同时跑 uptime / 拉日志。
func (m *Manager) execTargets(ctx context.Context, base sshExecRequest, raw any) Result {
	targets, err := toCommandList(raw)
	if err != nil {
		return invalid("targets 必须是 sessionId 数组：" + err.Error())
	}
	if len(targets) == 0 {
		return invalid("targets 不能为空")
	}
	if len(targets) > sshMaxBatch {
		return invalid(fmt.Sprintf("targets 最多 %d 个", sshMaxBatch))
	}
	if strings.TrimSpace(base.command) == "" {
		return invalid("targets 形态必须提供 command")
	}
	if len(base.command) > sshMaxCommandBytes {
		return invalid(fmt.Sprintf("command 超过 %d 字节上限", sshMaxCommandBytes))
	}
	return m.fanOut(ctx, base, targets, nil)
}

// execJobs 让每个会话跑各自的命令（jobs:[{sessionId,command}...]）。
func (m *Manager) execJobs(ctx context.Context, base sshExecRequest, raw any) Result {
	jobs, err := toJobList(raw)
	if err != nil {
		return invalid(err.Error())
	}
	if len(jobs) == 0 {
		return invalid("jobs 不能为空")
	}
	if len(jobs) > sshMaxBatch {
		return invalid(fmt.Sprintf("jobs 最多 %d 条", sshMaxBatch))
	}
	targets := make([]string, 0, len(jobs))
	commands := make([]string, 0, len(jobs))
	for i, job := range jobs {
		if job.SessionID == "" {
			return invalid(fmt.Sprintf("jobs[%d] 缺少 sessionId", i))
		}
		if strings.TrimSpace(job.Command) == "" {
			return invalid(fmt.Sprintf("jobs[%d] 缺少 command", i))
		}
		if len(job.Command) > sshMaxCommandBytes {
			return invalid(fmt.Sprintf("jobs[%d] command 超过 %d 字节上限", i, sshMaxCommandBytes))
		}
		targets = append(targets, job.SessionID)
		commands = append(commands, job.Command)
	}
	return m.fanOut(ctx, base, targets, commands)
}

// fanOut 在多个会话上并发执行；commands 为 nil 时所有目标共用 base.command。
// 并发上限 sshBatchConcurrency，与服务端 totalTasks 对齐，避免打爆任务槽。
func (m *Manager) fanOut(ctx context.Context, base sshExecRequest, targets []string, commands []string) Result {
	type slotResult struct {
		index int
		data  map[string]any
	}
	started := time.Now()
	// results 每个 goroutine 只写自己的索引，索引互不重叠，无需额外加锁。
	results := make([]map[string]any, len(targets))

	sem := make(chan struct{}, sshBatchConcurrency)
	var wg sync.WaitGroup
	for i, target := range targets {
		wg.Add(1)
		go func(idx int, sessionID string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			item := map[string]any{"sessionId": sessionID, "index": idx}
			session, found := m.sshState().get(sessionID)
			if !found {
				item["error"] = "SSH 会话不存在：" + sessionID
				results[idx] = item
				return
			}
			spec := base
			if commands != nil {
				spec.command = commands[idx]
			}
			outcome := m.runOnSession(ctx, session, spec)
			inner := resultData(outcome.result)
			if inner == nil {
				inner = map[string]any{"error": outcome.result.Message}
			}
			for key, value := range inner {
				item[key] = value
			}
			item["index"] = idx
			item["sessionId"] = sessionID
			results[idx] = item
		}(i, target)
	}
	wg.Wait()

	failed := 0
	for _, item := range results {
		if item == nil {
			failed++
			continue
		}
		if _, bad := item["error"]; bad {
			failed++
			continue
		}
		if code, ok := item["exitCode"].(int); ok && code != 0 {
			failed++
		}
	}

	data := map[string]any{
		"count":       len(targets),
		"failed":      failed,
		"results":     results,
		"durationMs":  time.Since(started).Milliseconds(),
		"concurrency": sshBatchConcurrency,
	}
	if failed > 0 {
		return withData(fail("SSH_COMMAND_FAILED", fmt.Sprintf("扇出执行完成，%d/%d 个目标失败", failed, len(targets)), nil, "ssh_exec_fanout"), data)
	}
	return ok(fmt.Sprintf("扇出执行完成，共 %d 个目标", len(targets)), data, "ssh_exec_fanout")
}

// execSession 取出 exec 要用的会话：有 sessionId 就复用，否则新建一次性连接。
// 返回的 cleanup 在新建连接时需要调用（关闭会话），复用时为空操作。
func (m *Manager) execSession(ctx context.Context, req Request, sessionID string) (*sshSession, func(), error) {
	if sessionID != "" {
		session, found := m.sshState().get(sessionID)
		if !found {
			return nil, nil, fmt.Errorf("SSH 会话不存在：%s", sessionID)
		}
		return session, func() {}, nil
	}
	conn, err := parseSSHArgs(req.Args, m.cfg.WorkDir)
	if err != nil {
		return nil, nil, err
	}
	session, err := m.dialSSH(ctx, conn)
	if err != nil {
		return nil, nil, err
	}
	// 一次性连接：用完关闭并从池中移除，否则僵尸条目会占用名额。
	return session, func() {
		session.shutdown()
		m.sshState().remove(session.id)
	}, nil
}

// sshJob 是 jobs 形态里的一项。
type sshJob struct {
	SessionID string
	Command   string
}

// toCommandList 把任意形态的字符串数组参数规整成 []string。
func toCommandList(raw any) ([]string, error) {
	switch typed := raw.(type) {
	case []string:
		return append([]string(nil), typed...), nil
	case []any:
		out := make([]string, 0, len(typed))
		for i, item := range typed {
			text, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("第 %d 项必须是字符串", i)
			}
			out = append(out, text)
		}
		return out, nil
	case string:
		if strings.TrimSpace(typed) == "" {
			return nil, nil
		}
		return []string{typed}, nil
	default:
		return nil, fmt.Errorf("必须是字符串数组")
	}
}

// toJobList 解析 jobs 形态。
func toJobList(raw any) ([]sshJob, error) {
	list, ok := raw.([]any)
	if !ok {
		if typed, ok2 := raw.([]map[string]any); ok2 {
			out := make([]sshJob, 0, len(typed))
			for _, item := range typed {
				out = append(out, sshJob{SessionID: stringOf(item["sessionId"]), Command: stringOf(item["command"])})
			}
			return out, nil
		}
		return nil, fmt.Errorf("jobs 必须是对象数组")
	}
	out := make([]sshJob, 0, len(list))
	for i, item := range list {
		obj, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("jobs[%d] 必须是对象", i)
		}
		job := sshJob{SessionID: stringOf(obj["sessionId"]), Command: stringOf(obj["command"])}
		if job.SessionID == "" {
			job.SessionID = stringOf(obj["session"])
		}
		out = append(out, job)
	}
	return out, nil
}

func stringOf(value any) string {
	text, _ := value.(string)
	return strings.TrimSpace(text)
}

// resultData 取出 Result.Data 里的 map，供批量结果聚合。
func resultData(r Result) map[string]any {
	if r.Data == nil {
		return nil
	}
	if typed, ok := r.Data.(map[string]any); ok {
		return typed
	}
	return nil
}

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
