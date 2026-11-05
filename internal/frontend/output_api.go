package frontend

// 输出形态控制端点。
//
// 与安全开关不同，这里改的是「工具结果怎么拼」，只影响之后发出的响应，
// 不需要重启：写回配置的同时立即更新内存中的形态。

import (
	"net/http"
	"strings"
)

// 结果文本形态取值，与 config 里的 ResultText* 常量保持一致。
const (
	resultTextAuto = "auto"
	resultTextOn   = "on"
	resultTextOff  = "off"
)

// normalizeResultTextMode 把任意输入收敛到三个合法取值之一。
func normalizeResultTextMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case resultTextOn:
		return resultTextOn
	case resultTextOff:
		return resultTextOff
	default:
		return resultTextAuto
	}
}

// OutputChange 是面板可修改的输出设置；nil 表示不改该项。
type OutputChange struct {
	CompactResultText *string
}

// SetOutputApplier 注入配置写回实现。未注入时面板返回 OUTPUT_UPDATE_UNAVAILABLE。
// 返回的 warning 非空表示改动只在本次运行生效（例如主配置不可写）。
func (s *Server) SetOutputApplier(fn func(OutputChange) (string, error)) {
	s.mu.Lock()
	s.applyOutput = fn
	s.mu.Unlock()
}

// outputView 是面板展示的输出状态。
type outputView struct {
	CompactResultText string `json:"compactResultText"`
	// Effective 用一句话说明该取值对当前调用的实际含义，
	// 避免用户以为「切了就一定变短」却看不到变化。
	Effective string `json:"effective"`
}

func (s *Server) outputSnapshot() outputView {
	s.mu.Lock()
	mode := s.resultTextMode
	s.mu.Unlock()
	if mode == "" {
		mode = normalizeResultTextMode(s.options.ResultTextMode)
	}
	view := outputView{CompactResultText: mode}
	switch mode {
	case resultTextOn:
		view.Effective = "所有客户端只收到「OK: 消息」短文本；完整结果仍在 structuredContent"
	case resultTextOff:
		view.Effective = "所有客户端都收到完整 JSON 文本"
	default:
		view.Effective = "仅最新协议客户端收到短文本，其余收到完整 JSON"
	}
	return view
}

func (s *Server) outputDispatch(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleOutputGet(w, r)
	case http.MethodPost:
		s.handleOutputUpdate(w, r)
	default:
		w.Header().Set("Allow", "GET, POST")
		writeAPIError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "仅支持 GET 或 POST")
	}
}

func (s *Server) handleOutputGet(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAPIError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "仅支持 GET")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "output": s.outputSnapshot()})
}

// handleOutputUpdate 接受 compactResultText 的新值并写回配置。
func (s *Server) handleOutputUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAPIError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "仅支持 POST")
		return
	}
	s.mu.Lock()
	applier := s.applyOutput
	s.mu.Unlock()
	if applier == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "OUTPUT_UPDATE_UNAVAILABLE", "当前运行模式不支持在线修改输出形态")
		return
	}
	var body struct {
		CompactResultText *string `json:"compactResultText"`
	}
	if err := decodeSingleJSON(r, &body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "INVALID_BODY", err.Error())
		return
	}
	if body.CompactResultText == nil {
		writeAPIError(w, http.StatusBadRequest, "NO_CHANGE", "至少提供 compactResultText")
		return
	}
	requested := strings.ToLower(strings.TrimSpace(*body.CompactResultText))
	mode := normalizeResultTextMode(requested)
	if mode != requested {
		writeAPIError(w, http.StatusBadRequest, "INVALID_VALUE", "compactResultText 只能是 auto、on 或 off")
		return
	}
	warning, err := applier(OutputChange{CompactResultText: &mode})
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "OUTPUT_UPDATE_FAILED", err.Error())
		return
	}
	s.mu.Lock()
	s.resultTextMode = mode
	s.mu.Unlock()
	message := "已保存并立即生效"
	if warning != "" {
		message = "已生效，但" + warning
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"output":  s.outputSnapshot(),
		"message": message,
	})
}
