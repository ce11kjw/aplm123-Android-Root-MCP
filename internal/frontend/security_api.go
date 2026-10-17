package frontend

// 安全相关控制端点。
//
// 设计取舍：监听地址与认证开关在服务启动时绑定（监听器不能热切换），
// 因此这里只负责**读写配置**并明确告知需要重启，不做「假装立即生效」。
// token 轮换是例外——它只影响校验逻辑，可以立即生效。

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// decodeBody 解析请求体。
// 安全控制端点的请求很小，限制读取量避免被超大 body 拖住。
func decodeBody(r *http.Request, target any) error {
	const maxBody = 64 << 10
	decoder := json.NewDecoder(io.LimitReader(r.Body, maxBody))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	return nil
}

// securityApplier 安全地取出配置写回函数。
func (s *Server) securityApplier() func(SecurityChange) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.applySecurity
}

// securityDispatch 按方法分派：GET 读状态，POST 改开关。
func (s *Server) securityDispatch(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleSecurityGet(w, r)
	case http.MethodPost:
		s.handleSecurityUpdate(w, r)
	default:
		w.Header().Set("Allow", "GET, POST")
		writeAPIError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "仅支持 GET 或 POST")
	}
}

// securityViewLimits 控制面板上展示的字段，避免把完整配置暴露给前端。
type securityView struct {
	ListenLAN         bool   `json:"listenLan"`
	RequireToken      bool   `json:"requireToken"`
	LoopbackAnonymous bool   `json:"loopbackAnonymous"`
	TokenPrefix       string `json:"tokenPrefix"`
	TokenPresent      bool   `json:"tokenPresent"`
	// RestartRequired 表示本次修改需要重启服务才能完全生效。
	RestartRequired bool `json:"restartRequired"`
}

func (s *Server) securitySnapshot() securityView {
	view := securityView{
		ListenLAN:         true,
		RequireToken:      s.options.RequireToken,
		LoopbackAnonymous: s.options.LoopbackAnonymous,
	}
	if s.access != nil {
		view.RequireToken = s.access.requireToken
		view.LoopbackAnonymous = s.access.loopbackAnonymous
	}
	if s.tokens != nil {
		view.TokenPrefix = s.tokens.prefix(8)
		view.TokenPresent = view.TokenPrefix != ""
	}
	return view
}

// handleSecurityGet 返回当前安全状态。
func (s *Server) handleSecurityGet(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAPIError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "仅支持 GET")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"security": s.securitySnapshot(),
	})
}

// handleSecurityUpdate 接受三个开关的新值并写回配置。
//
// 只接受这三个字段，其余配置一律由 aplm_config 工具管理：
// 面板是「应急开关」而不是完整配置编辑器，缩小误操作面。
func (s *Server) handleSecurityUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAPIError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "仅支持 POST")
		return
	}
	applier := s.securityApplier()
	if applier == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "SECURITY_UPDATE_UNAVAILABLE", "当前运行模式不支持在线修改安全配置")
		return
	}
	var body struct {
		ListenLAN         *bool `json:"listenLan"`
		RequireToken      *bool `json:"requireToken"`
		LoopbackAnonymous *bool `json:"loopbackAnonymous"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "INVALID_BODY", err.Error())
		return
	}
	change := SecurityChange{
		ListenLAN:         body.ListenLAN,
		RequireToken:      body.RequireToken,
		LoopbackAnonymous: body.LoopbackAnonymous,
	}
	if change.ListenLAN == nil && change.RequireToken == nil && change.LoopbackAnonymous == nil {
		writeAPIError(w, http.StatusBadRequest, "NO_CHANGE", "至少提供一个要修改的字段")
		return
	}
	if err := applier(change); err != nil {
		writeAPIError(w, http.StatusBadRequest, "SECURITY_UPDATE_FAILED", err.Error())
		return
	}
	// 立刻反映到内存中的策略，使认证开关立即生效；
	// 监听地址仍需重启（监听器已绑定）。
	s.mu.Lock()
	if change.RequireToken != nil && s.access != nil {
		s.access.requireToken = *change.RequireToken
	}
	if change.LoopbackAnonymous != nil && s.access != nil {
		s.access.loopbackAnonymous = *change.LoopbackAnonymous
	}
	s.mu.Unlock()

	view := s.securitySnapshot()
	// 监听范围变更必然需要重启；认证开关已即时生效。
	if change.ListenLAN != nil {
		view.RestartRequired = true
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"security":  view,
		"restarted": change.ListenLAN == nil,
		"message":   securityChangeMessage(change),
	})
}

// handleTokenRotate 生成新 token；旧 token 立即失效。
func (s *Server) handleTokenRotate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAPIError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "仅支持 POST")
		return
	}
	if s.tokens == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "TOKEN_UNAVAILABLE", "当前运行模式未启用 token")
		return
	}
	token, err := s.tokens.rotate()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "TOKEN_ROTATE_FAILED", err.Error())
		return
	}
	// 完整 token 只在这一次返回：调用方必须自己保存，之后只能看到前缀。
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"token":   token,
		"message": "新 token 已生效，旧 token 立即失效。请立即保存，此值不会再显示。",
	})
}

func securityChangeMessage(change SecurityChange) string {
	var parts []string
	if change.ListenLAN != nil {
		if *change.ListenLAN {
			parts = append(parts, "已允许局域网访问（重启后生效）")
		} else {
			parts = append(parts, "已关闭局域网访问（重启后仅本机可达）")
		}
	}
	if change.RequireToken != nil {
		if *change.RequireToken {
			parts = append(parts, "局域网访问需要 token（已生效）")
		} else {
			parts = append(parts, "已关闭 token 要求（已生效，请注意风险）")
		}
	}
	if change.LoopbackAnonymous != nil {
		if *change.LoopbackAnonymous {
			parts = append(parts, "本机免认证（已生效）")
		} else {
			parts = append(parts, "本机也需要 token（已生效）")
		}
	}
	return strings.Join(parts, "；")
}
