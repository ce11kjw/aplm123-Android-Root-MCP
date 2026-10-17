package frontend

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aplm123/android-ai-mcp/internal/webui"
)

const (
	defaultPort            = 5322
	defaultWorkDir         = "/storage/emulated/0/aplm123AI"
	defaultMaxRequestBytes = int64(8 << 20)
	defaultMaxUploadBytes  = int64(8 << 30)
	defaultTransferChunk   = int64(4 << 20)
	defaultMaxConcurrent   = 32
	// 流式连接单独给配额，默认比普通调用宽松一些：
	// MCP 客户端经常会同时挂多个流，而它们几乎不消耗 CPU。
	defaultMaxStreams = 64
	// 流空闲判定阈值：WebUI 用它标注“疑似僵尸连接”并允许清理。
	defaultStreamIdleTimeout = 10 * time.Minute
	anonymousRootLANWarning  = "安全警告：此服务在局域网内匿名提供完整 Root 能力；同一网络中的任何设备都可能控制本机。请勿连接不可信 Wi-Fi，且不要将端口映射到公网。"
)

// Server is the complete public HTTP frontend.
type Server struct {
	options   Options
	mux       *http.ServeMux
	access    *accessPolicy
	tokens    *tokenStore
	slots     chan struct{}
	transfers *TransferManager

	// streamSlots 是长连接（Streamable HTTP / SSE）的独立配额。
	//
	// 历史问题：长短连接共用一个 slots 池时，客户端不主动关闭的流式连接
	// 会把 32 个槽位全部占满，之后连 /health 这种瞬时请求都会返回
	// CONCURRENCY_LIMIT，只能重启进程才能恢复。把两者分开后，
	// 流连接再多也不会阻塞普通工具调用。
	streamSlots chan struct{}
	streams     *streamRegistry

	// applySecurity 由 service 层注入：把面板上的三个开关写回配置文件。
	// 为 nil 表示当前运行模式不支持在线修改（例如测试构造的 Server）。
	applySecurity func(SecurityChange) error
	// mu 保护 access 里的认证开关，使其能在运行期被面板修改。
	mu sync.Mutex
}

// SecurityChange 是面板可修改的三个安全开关；nil 表示不改该项。
type SecurityChange struct {
	ListenLAN         *bool
	RequireToken      *bool
	LoopbackAnonymous *bool
}

// SetSecurityApplier 注入配置写回实现。由 cmd 层在构造后调用，
// 使面板开关能把改动持久化；未注入时面板会返回 SECURITY_UPDATE_UNAVAILABLE。
func (s *Server) SetSecurityApplier(fn func(SecurityChange) error) {
	s.mu.Lock()
	s.applySecurity = fn
	s.mu.Unlock()
}

// New validates all mandatory integration points and constructs a frontend.
func New(options Options) (*Server, error) {
	if options.Broker == nil {
		return nil, errors.New("frontend: BrokerClient 未配置")
	}
	if options.MCP.SDKStreamable == nil {
		return nil, errors.New("frontend: 官方 SDK Streamable HTTP handler 未配置")
	}
	if options.MCP.SDKCurrent == nil {
		return nil, errors.New("frontend: 官方 SDK 2026 Streamable HTTP handler 未配置")
	}
	if options.EnableLegacySSE &&
		(options.MCP.LegacySSE == nil || options.MCP.LegacyMessages == nil) {
		return nil, errors.New("frontend: legacy SSE 已启用但 handler 不完整")
	}
	applyDefaults(&options)
	transfers, err := NewTransferManager(TransferOptions{
		Directory:      options.TransferDir,
		MaxUploadBytes: options.MaxUploadBytes,
		MaxChunkBytes:  options.TransferChunkBytes,
		UploadTTL:      options.UploadTTL,
		ArtifactTTL:    options.ArtifactTTL,
	})
	if err != nil {
		return nil, fmt.Errorf("frontend: 初始化传输目录: %w", err)
	}
	server := &Server{
		options:   options,
		mux:       http.NewServeMux(),
		access:    nil, // 下面按需构造（可能带 token 存储）
		slots:     make(chan struct{}, options.MaxConcurrent),
		transfers: transfers,

		streamSlots: make(chan struct{}, options.MaxStreams),
		streams:     newStreamRegistry(options.StreamIdleTimeout),
	}
	// token 存储按配置构造：TokenPath 为空时不启用（测试用）。
	var tokens *tokenStore
	if strings.TrimSpace(options.TokenPath) != "" {
		store, err := newTokenStore(options.TokenPath)
		if err != nil {
			return nil, fmt.Errorf("frontend: 初始化 token 存储: %w", err)
		}
		// 首次启动生成 token；失败则拒绝启动，避免「要求认证却没有凭证」。
		if _, err := store.ensure(); err != nil {
			return nil, fmt.Errorf("frontend: 准备 bearer token: %w", err)
		}
		tokens = store
	}
	server.tokens = tokens
	server.access = newAccessPolicy(options.ListenAddr, options.AllowedOrigins, SecurityPolicy{
		RequireToken:      options.RequireToken,
		LoopbackAnonymous: options.LoopbackAnonymous,
		Tokens:            tokens,
	})
	server.routes()
	return server, nil
}

func applyDefaults(options *Options) {
	if options.Port == 0 {
		options.Port = defaultPort
	}
	if options.ListenAddr == "" {
		options.ListenAddr = "0.0.0.0"
	}
	if options.WorkDir == "" {
		options.WorkDir = defaultWorkDir
	}
	if options.TransferDir == "" {
		options.TransferDir = options.WorkDir + "/uploads/.transfer"
	}
	if options.MaxRequestBytes <= 0 {
		options.MaxRequestBytes = defaultMaxRequestBytes
	}
	if options.MaxUploadBytes <= 0 {
		options.MaxUploadBytes = defaultMaxUploadBytes
	}
	if options.TransferChunkBytes <= 0 {
		options.TransferChunkBytes = defaultTransferChunk
	}
	if options.MaxConcurrent <= 0 {
		options.MaxConcurrent = defaultMaxConcurrent
	}
	if options.MaxStreams <= 0 {
		options.MaxStreams = defaultMaxStreams
	}
	if options.StreamIdleTimeout <= 0 {
		options.StreamIdleTimeout = defaultStreamIdleTimeout
	}
	if options.UploadTTL <= 0 {
		options.UploadTTL = 24 * time.Hour
	}
	if options.ArtifactTTL <= 0 {
		options.ArtifactTTL = time.Hour
	}
}

func (s *Server) routes() {
	s.mux.HandleFunc("/health", exactPath("/health", s.health))
	s.mux.HandleFunc("/version", exactPath("/version", s.version))
	s.mux.HandleFunc("/status", exactPath("/status", s.status))
	// /mcp 外面套一层 2026-07-28 协议的 _meta 兼容层：
	// 客户端只发 Mcp-Protocol-Version 头、不在请求体里重复 _meta 时，
	// 由它补齐后再交给 SDK handler，避免能力探测阶段就被 400 拒绝。
	s.mux.HandleFunc("/mcp", exactPath("/mcp", normalizeNewProtocolMeta(s.mcp)))
	s.mux.HandleFunc("/sse", exactPath("/sse", s.legacySSE))
	s.mux.HandleFunc("/messages", exactPath("/messages", s.legacyMessages))
	s.mux.HandleFunc("/transfer/upload", exactPath("/transfer/upload", s.transfers.handleCreateUpload))
	s.mux.HandleFunc("/transfer/upload/", s.transfers.handleUpload)
	s.mux.HandleFunc("/transfer/download/", s.transfers.handleDownload)
	// Compatibility aliases never appear in newly generated URLs.
	s.mux.HandleFunc("/transfer/uploads", exactPath("/transfer/uploads", s.transfers.handleCreateUpload))
	// WebUI 面板与控制端点。全部位于 exemptPaths 白名单，
	// 因此在连接池耗尽时仍可访问——用于诊断并一键释放。
	s.mux.HandleFunc("/ui", s.uiPage)
	s.mux.HandleFunc("/api/ui", exactPath("/api/ui", s.uiSummary))
	// /api/streams 同时支持 GET（列表）与 POST/DELETE（断开），
	// 这里按方法分派，避免落到兜底静态处理器变成 405。
	s.mux.HandleFunc("/api/streams", exactPath("/api/streams", s.uiStreamsDispatch))
	// 安全开关：面板上的应急控制。改动写入配置，认证项即时生效、
	// 监听范围需重启（监听器已绑定，不能热切换）。
	s.mux.HandleFunc("/api/security", exactPath("/api/security", s.securityDispatch))
	s.mux.HandleFunc("/api/token/rotate", exactPath("/api/token/rotate", s.handleTokenRotate))
	s.mux.HandleFunc("/transfer/uploads/", s.transfers.handleUpload)
	s.mux.HandleFunc("/transfer/files/", s.transfers.handleDownload)
	s.mux.Handle("/", webui.New())
}

func exactPath(path string, handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != path {
			http.NotFound(w, r)
			return
		}
		handler(w, r)
	}
}

// ServeHTTP applies the public security boundary before endpoint routing.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w.Header())
	if r.Method == http.MethodOptions {
		w.Header().Set("Allow", "GET, HEAD, POST, PUT, DELETE")
		writeAPIError(w, http.StatusMethodNotAllowed, "CORS_DISABLED", "不提供跨域访问")
		return
	}
	if err := s.access.validate(r); err != nil {
		// 认证失败与来源策略失败要分开报：401 提示客户端补凭证，
		// 403 表示来源本身不被接受，前者可重试、后者重试无意义。
		if errors.Is(err, errUnauthorized) || errors.Is(err, errTokenMissing) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="aplm123"`)
			writeAPIError(w, http.StatusUnauthorized, "AUTH_REQUIRED", "需要有效的 bearer token")
			return
		}
		writeAPIError(w, http.StatusForbidden, "NETWORK_POLICY_DENIED", err.Error())
		return
	}
	// 连接准入：管理端点免占配额，长连接与短请求分开计数。
	// 这样即使大量流式连接挂着不关，/health 与普通工具调用仍可正常工作
	//（此前共用单池时会被流连接占满，只能重启恢复）。
	release, admitted := s.admit(w, r)
	if !admitted {
		return
	}
	defer release()
	if r.Body != nil && r.Method != http.MethodGet && r.Method != http.MethodHead {
		limit := s.options.MaxRequestBytes
		if r.Method == http.MethodPut &&
			(strings.HasPrefix(r.URL.Path, "/transfer/upload/") ||
				strings.HasPrefix(r.URL.Path, "/transfer/uploads/")) {
			limit = s.options.TransferChunkBytes
		}
		if r.ContentLength > limit {
			writeAPIError(w, http.StatusRequestEntityTooLarge, "BODY_TOO_LARGE", "请求体超过大小限制")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, limit)
	}
	s.mux.ServeHTTP(w, r)
}

func setSecurityHeaders(header http.Header) {
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("X-Frame-Options", "DENY")
	header.Set("Referrer-Policy", "no-referrer")
	header.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
	header.Set("Cross-Origin-Resource-Policy", "same-origin")
	header.Set("X-aplm123-Anonymous-Root-Warning", "enabled")
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w, "GET, HEAD")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"service": "running",
	})
}

func (s *Server) version(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w, "GET, HEAD")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"name":              "aplm123 Root MCP",
		"author":            "@aplm123",
		"version":           s.options.Version,
		"protocolCurrent":   s.options.ProtocolCurrent,
		"protocolPrevious":  s.options.ProtocolPrevious,
		"protocolLegacy":    s.options.ProtocolLegacy,
		"streamableHTTP":    "/mcp",
		"legacySSE":         s.options.EnableLegacySSE,
		"legacySSEPath":     enabledPath(s.options.EnableLegacySSE, "/sse"),
		"legacyMessagePath": enabledPath(s.options.EnableLegacySSE, "/messages"),
	})
}

func enabledPath(enabled bool, path string) any {
	if !enabled {
		return nil
	}
	return path
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w, "GET, HEAD")
		return
	}
	status, err := s.options.Broker.Status(r.Context())
	if err != nil {
		writeAPIError(w, http.StatusServiceUnavailable, "BROKER_UNAVAILABLE", err.Error())
		return
	}
	payload := cloneMap(status)
	payload["service"] = "running"
	payload["version"] = s.options.Version
	payload["protocolCurrent"] = s.options.ProtocolCurrent
	payload["protocolPrevious"] = s.options.ProtocolPrevious
	payload["protocolLegacy"] = s.options.ProtocolLegacy
	payload["mcpAddress"] = requestBaseURL(r) + "/mcp"
	payload["workDir"] = s.options.WorkDir
	payload["anonymousRootLAN"] = true
	payload["securityWarning"] = anonymousRootLANWarning
	// 连接池水位：让 WebUI 与排障一眼看出「卡死」到底卡在哪一层。
	payload["connections"] = s.connectionStatus()
	if degraded, _ := payload["securityDegraded"].(bool); degraded {
		reason, _ := payload["securityDegradationReason"].(string)
		payload["securityWarning"] = anonymousRootLANWarning + " frontend 降权失败，当前以 Root 运行：" + reason
	}
	writeJSON(w, http.StatusOK, payload)
}

// connectionStatus 汇报两类连接配额的实时占用情况。
func (s *Server) connectionStatus() map[string]any {
	streams := s.streams.snapshot()
	return map[string]any{
		"shortRequests": map[string]any{
			"inUse": len(s.slots),
			"limit": cap(s.slots),
		},
		"streams": map[string]any{
			"inUse":        len(s.streamSlots),
			"limit":        cap(s.streamSlots),
			"registered":   len(streams),
			"idleTimeoutS": int(s.options.StreamIdleTimeout.Seconds()),
		},
		// 免占配额的管理端点，用于过载时自证存活。
		"exempt": []string{"/health", "/status", "/version", "/ui", "/api/ui", "/api/streams"},
	}
}

func cloneMap(source map[string]any) map[string]any {
	target := make(map[string]any, len(source)+8)
	for key, value := range source {
		target[key] = value
	}
	return target
}

func requestBaseURL(r *http.Request) string {
	return requestScheme(r) + "://" + r.Host
}

func (s *Server) mcp(w http.ResponseWriter, r *http.Request) {
	w.Header().Add("Vary", "MCP-Protocol-Version")
	handler, err := s.selectMCPHandler(r)
	if err != nil {
		var maxBytesError *http.MaxBytesError
		if errors.As(err, &maxBytesError) {
			writeAPIError(w, http.StatusRequestEntityTooLarge, "BODY_TOO_LARGE", "MCP 请求体超过大小限制")
			return
		}
		writeAPIError(w, http.StatusBadRequest, "PROTOCOL_ROUTING_FAILED", err.Error())
		return
	}
	handler.ServeHTTP(w, r)
}

func (s *Server) selectMCPHandler(r *http.Request) (http.Handler, error) {
	version := strings.TrimSpace(r.Header.Get("MCP-Protocol-Version"))
	if version == s.options.ProtocolCurrent && version != "" {
		if s.options.MCP.Compatibility == nil || r.Method != http.MethodPost || r.Body == nil {
			return s.options.MCP.SDKCurrent, nil
		}
		return s.selectCurrentExtension(r)
	}
	if r.Method != http.MethodPost || r.Body == nil {
		return s.options.MCP.SDKStreamable, nil
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, fmt.Errorf("读取 MCP 请求: %w", err)
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	var envelope mcpRoutingEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		// The selected protocol handler owns JSON-RPC parse errors.
		return s.options.MCP.SDKStreamable, nil
	}
	currentClaim := envelope.claimsCurrentProtocol(s.options.ProtocolCurrent)
	if envelope.usesTaskCompatibility(currentClaim) && s.options.MCP.Compatibility != nil {
		return s.options.MCP.Compatibility, nil
	}
	if envelope.Method == "server/discover" ||
		currentClaim {
		return s.options.MCP.SDKCurrent, nil
	}
	return s.options.MCP.SDKStreamable, nil
}

func (s *Server) selectCurrentExtension(r *http.Request) (http.Handler, error) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, fmt.Errorf("读取 MCP 请求: %w", err)
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	var envelope mcpRoutingEnvelope
	if json.Unmarshal(body, &envelope) == nil && envelope.usesTaskCompatibility(true) {
		return s.options.MCP.Compatibility, nil
	}
	return s.options.MCP.SDKCurrent, nil
}

type mcpRoutingEnvelope struct {
	Method string `json:"method"`
	Params struct {
		ProtocolVersion string         `json:"protocolVersion"`
		Name            string         `json:"name"`
		Arguments       map[string]any `json:"arguments"`
		Meta            map[string]any `json:"_meta"`
	} `json:"params"`
}

func (e mcpRoutingEnvelope) claimsCurrentProtocol(current string) bool {
	if e.Params.ProtocolVersion == current {
		return true
	}
	version, _ := e.Params.Meta["io.modelcontextprotocol/protocolVersion"].(string)
	if version == "" {
		// Compatibility with early release-candidate clients.
		version, _ = e.Params.Meta["protocolVersion"].(string)
	}
	return version == current
}

func (e mcpRoutingEnvelope) usesTaskCompatibility(current bool) bool {
	if !current {
		return false
	}
	if strings.HasPrefix(e.Method, "tasks/") {
		return true
	}
	if e.Method != "tools/call" {
		return false
	}
	background, _ := e.Params.Arguments["background"].(bool)
	if !background {
		return false
	}
	capabilities, _ := e.Params.Meta["io.modelcontextprotocol/clientCapabilities"].(map[string]any)
	extensions, _ := capabilities["extensions"].(map[string]any)
	_, declared := extensions["io.modelcontextprotocol/tasks"]
	return declared
}

func (s *Server) legacySSE(w http.ResponseWriter, r *http.Request) {
	if !s.options.EnableLegacySSE {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w, "GET")
		return
	}
	// The SDK derives the endpoint event from the request URL. Internally
	// presenting /messages makes the event point to the documented POST path
	// while the client still opens the public GET /sse endpoint.
	clone := r.Clone(r.Context())
	urlCopy := *r.URL
	urlCopy.Path = "/messages"
	clone.URL = &urlCopy
	s.options.MCP.LegacySSE.ServeHTTP(w, clone)
}

func (s *Server) legacyMessages(w http.ResponseWriter, r *http.Request) {
	if !s.options.EnableLegacySSE {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w, "POST")
		return
	}
	s.options.MCP.LegacyMessages.ServeHTTP(w, r)
}

func methodNotAllowed(w http.ResponseWriter, allow string) {
	w.Header().Set("Allow", allow)
	writeAPIError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "请求方法不受支持")
}

// PublishFile creates an unguessable, expiring download URL without exposing a
// device path. The source is streamed by the HTTP server and is never buffered
// in memory.
func (s *Server) PublishFile(path, name, sha256 string, ttl time.Duration) (TransferArtifact, error) {
	return s.transfers.PublishFile(path, name, sha256, ttl)
}

// Transfers exposes the transfer manager to the daemon integration layer.
func (s *Server) Transfers() *TransferManager {
	return s.transfers
}

// ListenAddress returns the configured address without starting a listener.
func (s *Server) ListenAddress() string {
	return net.JoinHostPort(s.options.ListenAddr, strconv.Itoa(s.options.Port))
}
