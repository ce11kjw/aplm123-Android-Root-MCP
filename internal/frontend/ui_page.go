package frontend

import (
	_ "embed"
	"net/http"
)

// WebUI 面板：只读增强视图。
//
// 与 internal/webui 的旧状态页的区别：
//   - 数据源是 /api/ui 与 /api/streams，二者都在免配额白名单里，
//     因此**服务过载时面板依然打得开**——这正是它最大的价值。
//   - 增加了连接池水位可视化与“断开闲置长连接”操作，
//     用于在不重启进程的前提下恢复被流连接占满的服务。
//
//go:embed assets/panel.html
var panelHTML []byte

// uiPage 提供面板页面。带扩展名或非根路径时回退到内嵌静态资源。
func (s *Server) uiPage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		methodNotAllowed(w, "GET, HEAD")
		return
	}
	// /ui 与 /ui/ 都给面板；面板自身不再依赖其它静态文件（样式与脚本内联）。
	if r.URL.Path != "/ui" && r.URL.Path != "/ui/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'; "+
			"style-src 'unsafe-inline'; script-src 'unsafe-inline'; connect-src 'self'")
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	_, _ = w.Write(panelHTML)
}
