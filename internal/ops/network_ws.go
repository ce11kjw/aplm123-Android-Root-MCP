package ops

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"
)

const (
	maxWebSocketMessageBytes = int64(1 << 20)
	maxWebSocketQueueBytes   = int64(4 << 20)
	maxWebSocketQueueItems   = 32
	webSocketIdleTTL         = 15 * time.Minute
	webSocketClosedRetention = 30 * time.Second
)

type webSocketMessage struct {
	Type       string    `json:"type"`
	Data       string    `json:"data"`
	Bytes      int       `json:"bytes"`
	ReceivedAt time.Time `json:"receivedAt"`
	Truncated  bool      `json:"truncated,omitempty"`
}

type webSocketSession struct {
	id            string
	url           string
	subprotocol   string
	createdAt     time.Time
	lastActivity  time.Time
	conn          *websocket.Conn
	maxBytes      int64
	queueCapacity int
	messages      chan webSocketMessage
	done          chan struct{}
	activity      chan struct{}
	cancel        context.CancelFunc
	idleTTL       time.Duration
	closeFunc     func(websocket.StatusCode, string) error
	closeOnce     sync.Once
	mu            sync.RWMutex
	lastError     string
	closeErr      error
}

func (s *webSocketSession) closeContext(ctx context.Context, code websocket.StatusCode, reason string) error {
	s.closeOnce.Do(func() {
		if s.cancel != nil {
			s.cancel()
		}
		go func() {
			var err error
			if s.closeFunc != nil {
				err = s.closeFunc(code, truncateUTF8Bytes(reason, 123))
			} else if s.conn != nil {
				err = s.conn.Close(code, truncateUTF8Bytes(reason, 123))
			} else {
				err = errors.New("WebSocket connection is unavailable")
			}
			s.mu.Lock()
			s.closeErr = err
			s.mu.Unlock()
			close(s.done)
		}()
	})
	select {
	case <-s.done:
		s.mu.RLock()
		err := s.closeErr
		s.mu.RUnlock()
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *webSocketSession) abort(err error) {
	s.closeOnce.Do(func() {
		s.setError(err)
		if s.cancel != nil {
			s.cancel()
		}
		if s.conn != nil {
			_ = s.conn.CloseNow()
		}
		s.mu.Lock()
		s.closeErr = err
		s.mu.Unlock()
		close(s.done)
	})
}

func (s *webSocketSession) setError(err error) {
	if err == nil {
		return
	}
	s.mu.Lock()
	s.lastError = err.Error()
	s.mu.Unlock()
}

func (s *webSocketSession) summary() map[string]any {
	s.mu.RLock()
	lastError := s.lastError
	lastActivity := s.lastActivity
	s.mu.RUnlock()
	return map[string]any{"connectionId": s.id, "url": s.url, "subprotocol": truncateUTF8Bytes(s.subprotocol, 256), "createdAt": s.createdAt, "lastActivity": lastActivity, "idleTtlSeconds": int64(s.idleTTL.Seconds()), "queuedMessages": len(s.messages), "queueCapacity": s.queueCapacity, "maxMessageBytes": s.maxBytes, "lastError": truncateUTF8Bytes(lastError, 512)}
}

func (s *webSocketSession) touch(at time.Time) {
	s.mu.Lock()
	s.lastActivity = at
	s.mu.Unlock()
	select {
	case s.activity <- struct{}{}:
	default:
	}
}

func (n *networkRuntime) readWebSocket(ctx context.Context, session *webSocketSession) {
	for {
		typ, raw, err := session.conn.Read(ctx)
		if err != nil {
			session.abort(sanitizeNetworkError(err, session.url))
			return
		}
		message := webSocketMessage{Type: "text", Data: string(raw), Bytes: len(raw), ReceivedAt: time.Now().UTC()}
		if typ == websocket.MessageBinary {
			message.Type = "binary"
			message.Data = base64.StdEncoding.EncodeToString(raw)
		}
		session.touch(message.ReceivedAt)
		select {
		case session.messages <- message:
		default:
			overflow := errors.New("WebSocket receive queue exceeded its bounded capacity")
			session.setError(overflow)
			session.abort(overflow)
			return
		}
	}
}

func parseWebSocketURL(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "ws" && parsed.Scheme != "wss") || parsed.User != nil {
		return errors.New("url 必须是无内嵌凭据的有效 ws/wss 地址")
	}
	return nil
}

func displayWebSocketURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "[invalid]"
	}
	// Paths and queries commonly contain temporary credentials. Session
	// diagnostics only need the transport and endpoint authority.
	return truncateUTF8Bytes(strings.ToLower(parsed.Scheme)+"://"+parsed.Host, 512)
}

func truncateUTF8Bytes(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(value) <= limit {
		return value
	}
	value = value[:limit]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

func webSocketQueueCapacity(maxBytes int64) int {
	// Binary messages are retained as Base64, so budget for the encoded size.
	encodedWorstCase := ((maxBytes + 2) / 3) * 4
	capacity := int(maxWebSocketQueueBytes / encodedWorstCase)
	if capacity < 1 {
		return 1
	}
	if capacity > maxWebSocketQueueItems {
		return maxWebSocketQueueItems
	}
	return capacity
}

func (m *Manager) webSocketOperation(ctx context.Context, req Request, action string) Result {
	runtime := m.networkState()
	runtime.pruneWebSockets()
	switch action {
	case "ws_connect":
		rawURL, err := argString(req.Args, "url")
		if err != nil {
			return invalid(err.Error())
		}
		if err := parseWebSocketURL(rawURL); err != nil {
			return invalid(err.Error())
		}
		defaultMax := m.cfg.ResultPreviewBytes
		if defaultMax > 64<<10 {
			defaultMax = 64 << 10
		}
		inlineMax := (m.cfg.ResultPreviewBytes - 2048) * 3 / 4
		if inlineMax > maxWebSocketMessageBytes {
			inlineMax = maxWebSocketMessageBytes
		}
		if inlineMax < 1024 {
			inlineMax = 1024
		}
		if defaultMax > inlineMax {
			defaultMax = inlineMax
		}
		maxBytes, err := argInt64(req.Args, defaultMax, "maxMessageBytes")
		if err != nil || maxBytes < 1024 || maxBytes > inlineMax {
			return invalid("maxMessageBytes 超出当前内联结果字节预算")
		}
		timeout, err := argDuration(req.Args, 15*time.Second)
		if err != nil {
			return invalid(err.Error())
		}
		headers, err := argStringMap(req.Args, "headers")
		if err != nil {
			return invalid(err.Error())
		}
		jarName, _ := argOptionalString(req.Args, "default", "jar")
		jar, err := runtime.cookieJar(jarName)
		if err != nil {
			return invalid(err.Error())
		}
		if err := jar.persistenceError(); err != nil {
			return fail("COOKIE_JAR_UNTRUSTED", "Cookie jar 持久状态不可用，请先用 cookie_set 或 cookie_clear 修复", errCookieJarUntrusted, "persistent_cookie_jar")
		}
		subprotocols, err := optionalStringSlice(req.Args, "subprotocols")
		if err != nil {
			return invalid(err.Error())
		}
		httpHeaders := make(http.Header, len(headers))
		for key, value := range headers {
			httpHeaders.Set(key, value)
		}
		connectContext, cancelConnect := context.WithTimeout(ctx, timeout)
		defer cancelConnect()
		requestJar := &requestCookieJar{state: jar}
		transport := boundedHTTPTransport()
		defer transport.CloseIdleConnections()
		connection, response, err := websocket.Dial(connectContext, rawURL, &websocket.DialOptions{HTTPClient: &http.Client{Jar: requestJar, CheckRedirect: safeRedirectPolicy, Transport: transport}, HTTPHeader: httpHeaders, Subprotocols: subprotocols})
		if err != nil {
			status := 0
			if response != nil {
				status = response.StatusCode
			}
			result := fail("WS_CONNECT_FAILED", "WebSocket 连接失败", sanitizeNetworkError(err, rawURL), "rfc6455_client")
			result.Data = map[string]any{"httpStatus": status}
			return result
		}
		if err := requestJar.persistenceError(); err != nil {
			_ = connection.CloseNow()
			return fail("COOKIE_WRITE_FAILED", "WebSocket 握手 Cookie 持久化失败", err, "persistent_cookie_jar")
		}
		connection.SetReadLimit(maxBytes)
		runtime.wsMu.Lock()
		if len(runtime.ws) >= 16 {
			runtime.wsMu.Unlock()
			_ = connection.CloseNow()
			return fail("LIMIT_EXCEEDED", "WebSocket 会话上限为 16", errors.New("session limit"), "rfc6455_client")
		}
		sessionContext, cancelSession := context.WithCancel(runtime.ctx)
		queueCapacity := webSocketQueueCapacity(maxBytes)
		now := time.Now().UTC()
		session := &webSocketSession{id: randomTaskID(), url: displayWebSocketURL(rawURL), subprotocol: connection.Subprotocol(), createdAt: now, lastActivity: now, conn: connection, maxBytes: maxBytes, queueCapacity: queueCapacity, messages: make(chan webSocketMessage, queueCapacity), done: make(chan struct{}), activity: make(chan struct{}, 1), cancel: cancelSession, idleTTL: webSocketIdleTTL}
		runtime.ws[session.id] = session
		runtime.wsMu.Unlock()
		go runtime.readWebSocket(sessionContext, session)
		go runtime.monitorWebSocketIdle(session)
		go runtime.removeWebSocketWhenDone(session)
		return ok("WebSocket 已连接", session.summary(), "rfc6455_client")
	case "ws_list":
		runtime.wsMu.RLock()
		items := make([]map[string]any, 0, len(runtime.ws))
		for _, session := range runtime.ws {
			candidate := append(items, session.summary())
			if jsonEncodedSize(map[string]any{"connections": candidate, "limit": 16}) > int(m.cfg.ResultPreviewBytes) {
				break
			}
			items = candidate
		}
		runtime.wsMu.RUnlock()
		return ok("WebSocket 会话读取成功", map[string]any{"connections": items, "limit": 16}, "rfc6455_session_registry")
	case "ws_send", "ws_receive", "ws_ping", "ws_close":
		id, err := argString(req.Args, "connectionId")
		if err != nil {
			return invalid(err.Error())
		}
		runtime.wsMu.RLock()
		session := runtime.ws[id]
		runtime.wsMu.RUnlock()
		if session == nil {
			return fail("NOT_FOUND", "WebSocket 会话不存在", errors.New("session not found"), "rfc6455_session_registry")
		}
		timeout, err := argDuration(req.Args, 30*time.Second)
		if err != nil {
			return invalid(err.Error())
		}
		opContext, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		switch action {
		case "ws_send":
			payloadType := websocket.MessageText
			messageValue, hasMessage := req.Args["message"]
			binaryValue, hasBinary := req.Args["binaryBase64"]
			if hasMessage == hasBinary {
				return invalid("message 与 binaryBase64 必须且只能提供一个")
			}
			payload := ""
			if hasMessage {
				var ok bool
				payload, ok = messageValue.(string)
				if !ok {
					return invalid("message 必须是字符串")
				}
			}
			if hasBinary {
				encoded, ok := binaryValue.(string)
				if !ok {
					return invalid("binaryBase64 必须是字符串")
				}
				decoded, decodeErr := base64.StdEncoding.DecodeString(encoded)
				if decodeErr != nil {
					return invalid("binaryBase64 必须是合法 Base64")
				}
				payloadType = websocket.MessageBinary
				payload = string(decoded)
			}
			if int64(len(payload)) > session.maxBytes {
				return fail("LIMIT_EXCEEDED", "WebSocket 消息超过会话上限", errors.New("message too large"), "rfc6455_message_limit")
			}
			if err := session.conn.Write(opContext, payloadType, []byte(payload)); err != nil {
				safeErr := sanitizeNetworkError(err, session.url)
				session.setError(safeErr)
				return fail("WS_WRITE_FAILED", "WebSocket 消息发送失败", safeErr, "rfc6455_client")
			}
			session.touch(time.Now().UTC())
			return ok("WebSocket 消息已发送", map[string]any{"connectionId": id, "bytes": len(payload), "type": map[bool]string{true: "binary", false: "text"}[payloadType == websocket.MessageBinary]}, "rfc6455_client")
		case "ws_receive":
			select {
			case message := <-session.messages:
				return m.webSocketReceiveResult(runtime, id, session, message)
			default:
			}
			select {
			case message := <-session.messages:
				return m.webSocketReceiveResult(runtime, id, session, message)
			case <-session.done:
				return fail("WS_CLOSED", "WebSocket 会话已关闭", errors.New("connection closed"), "rfc6455_client")
			case <-opContext.Done():
				return fail("TIMEOUT", "等待 WebSocket 消息超时", opContext.Err(), "rfc6455_bounded_queue")
			}
		case "ws_ping":
			if err := session.conn.Ping(opContext); err != nil {
				return fail("WS_PING_FAILED", "WebSocket Ping 失败", sanitizeNetworkError(err, session.url), "rfc6455_client")
			}
			session.touch(time.Now().UTC())
			return ok("WebSocket Ping/Pong 成功", map[string]any{"connectionId": id}, "rfc6455_client")
		case "ws_close":
			reason, _ := argOptionalString(req.Args, "client close", "reason")
			closeErr := session.closeContext(opContext, websocket.StatusNormalClosure, reason)
			if closeErr != nil {
				return fail("WS_CLOSE_FAILED", "WebSocket 会话关闭失败", sanitizeNetworkError(closeErr, session.url), "rfc6455_close_timeout")
			}
			return ok("WebSocket 会话已关闭", map[string]any{"connectionId": id}, "rfc6455_client")
		}
	}
	return invalidAction(req.Tool, action, "ws_close", "ws_connect", "ws_list", "ws_ping", "ws_receive", "ws_send")
}

func (m *Manager) webSocketReceiveResult(runtime *networkRuntime, id string, session *webSocketSession, message webSocketMessage) Result {
	data, truncated := boundedWebSocketReceiveData(id, message, int(m.cfg.ResultPreviewBytes))
	text := "WebSocket 消息已接收"
	if truncated {
		text = "WebSocket 消息已接收；返回内容已按字节预算截断"
	}
	runtime.removeWebSocketIfDrained(session)
	return ok(text, data, "rfc6455_bounded_queue")
}

func boundedWebSocketReceiveData(id string, message webSocketMessage, budget int) (map[string]any, bool) {
	data := map[string]any{"connectionId": id, "message": message}
	if budget < 1024 || jsonEncodedSize(data) <= budget {
		return data, false
	}
	original := strings.ToValidUTF8(message.Data, "\uFFFD")
	message.Truncated = true
	message.Data = ""
	data["message"] = message
	low, high := 0, len(original)
	best := ""
	for low <= high {
		middle := low + (high-low)/2
		message.Data = truncateUTF8Bytes(original, middle)
		data["message"] = message
		if jsonEncodedSize(data) <= budget {
			best = message.Data
			low = middle + 1
		} else {
			high = middle - 1
		}
	}
	message.Data = best
	data["message"] = message
	return data, true
}

func (n *networkRuntime) removeWebSocketWhenDone(session *webSocketSession) {
	<-session.done
	if n.removeWebSocketIfDrained(session) {
		return
	}
	timer := time.NewTimer(webSocketClosedRetention)
	defer timer.Stop()
	select {
	case <-n.ctx.Done():
	case <-timer.C:
	}
	n.removeWebSocket(session)
}

func (n *networkRuntime) removeWebSocketIfDrained(session *webSocketSession) bool {
	select {
	case <-session.done:
		if len(session.messages) == 0 {
			n.removeWebSocket(session)
			return true
		}
	default:
	}
	return false
}

func (n *networkRuntime) removeWebSocket(session *webSocketSession) {
	n.wsMu.Lock()
	if n.ws[session.id] == session {
		delete(n.ws, session.id)
	}
	n.wsMu.Unlock()
}

func (n *networkRuntime) monitorWebSocketIdle(session *webSocketSession) {
	ttl := session.idleTTL
	if ttl <= 0 {
		ttl = webSocketIdleTTL
	}
	timer := time.NewTimer(ttl)
	defer timer.Stop()
	reset := func(wait time.Duration) {
		if wait <= 0 {
			wait = time.Millisecond
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(wait)
	}
	for {
		select {
		case <-n.ctx.Done():
			return
		case <-session.done:
			return
		case <-session.activity:
			session.mu.RLock()
			remaining := ttl - time.Since(session.lastActivity)
			session.mu.RUnlock()
			reset(remaining)
		case <-timer.C:
			session.mu.RLock()
			remaining := ttl - time.Since(session.lastActivity)
			session.mu.RUnlock()
			if remaining > 0 {
				timer.Reset(remaining)
				continue
			}
			session.abort(errors.New("WebSocket idle TTL exceeded"))
			n.removeWebSocketIfDrained(session)
			return
		}
	}
}

func (n *networkRuntime) pruneWebSockets() {
	n.wsMu.Lock()
	now := time.Now().UTC()
	stale := make([]*webSocketSession, 0)
	for id, session := range n.ws {
		select {
		case <-session.done:
			if len(session.messages) == 0 {
				delete(n.ws, id)
			}
		default:
			session.mu.RLock()
			idle := now.Sub(session.lastActivity) > webSocketIdleTTL
			session.mu.RUnlock()
			if idle {
				stale = append(stale, session)
			}
		}
	}
	n.wsMu.Unlock()
	for _, session := range stale {
		session.abort(errors.New("WebSocket idle TTL exceeded"))
		n.removeWebSocketIfDrained(session)
	}
}

func optionalStringSlice(args map[string]any, key string) ([]string, error) {
	value, ok := args[key]
	if !ok {
		return nil, nil
	}
	raw, ok := value.([]any)
	if !ok {
		if direct, ok := value.([]string); ok {
			return direct, nil
		}
		return nil, errors.New(key + " 必须是字符串数组")
	}
	values := make([]string, 0, len(raw))
	for _, item := range raw {
		text, ok := item.(string)
		if !ok || strings.TrimSpace(text) == "" {
			return nil, errors.New(key + " 必须是非空字符串数组")
		}
		values = append(values, text)
	}
	return values, nil
}
