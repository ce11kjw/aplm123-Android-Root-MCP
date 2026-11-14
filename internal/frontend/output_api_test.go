package frontend

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newOutputServer(mode string) *Server {
	return &Server{options: Options{ResultTextMode: mode}}
}

func TestOutputGetShowsInitialMode(t *testing.T) {
	srv := newOutputServer("off")
	rec := httptest.NewRecorder()
	srv.handleOutputGet(rec, httptest.NewRequest(http.MethodGet, "/api/output", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 %d", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, `"compactResultText":"off"`) {
		t.Fatalf("响应缺少形态字段: %s", body)
	}
}

func TestOutputUpdateAppliesImmediately(t *testing.T) {
	srv := newOutputServer("auto")
	var applied OutputChange
	srv.SetOutputApplier(func(change OutputChange) (string, error) {
		applied = change
		return "", nil
	})
	rec := httptest.NewRecorder()
	srv.handleOutputUpdate(rec, httptest.NewRequest(http.MethodPost, "/api/output",
		strings.NewReader(`{"compactResultText":"on"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 %d，body=%s", rec.Code, rec.Body.String())
	}
	if applied.CompactResultText == nil || *applied.CompactResultText != "on" {
		t.Fatalf("applier 收到的值不对: %+v", applied)
	}
	if srv.resultTextMode != "on" {
		t.Fatalf("运行期形态应即时更新，得到 %q", srv.resultTextMode)
	}
	if got := srv.outputSnapshot().CompactResultText; got != "on" {
		t.Fatalf("快照应为 on，得到 %q", got)
	}
}

func TestOutputUpdateRejectsUnknownValue(t *testing.T) {
	srv := newOutputServer("auto")
	called := false
	srv.SetOutputApplier(func(OutputChange) (string, error) { called = true; return "", nil })
	rec := httptest.NewRecorder()
	srv.handleOutputUpdate(rec, httptest.NewRequest(http.MethodPost, "/api/output",
		strings.NewReader(`{"compactResultText":"loud"}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("应返回 400，得到 %d", rec.Code)
	}
	if called {
		t.Fatal("非法取值不应写回配置")
	}
}

func TestOutputUpdateWithoutApplier(t *testing.T) {
	srv := newOutputServer("auto")
	rec := httptest.NewRecorder()
	srv.handleOutputUpdate(rec, httptest.NewRequest(http.MethodPost, "/api/output",
		strings.NewReader(`{"compactResultText":"off"}`)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("未注入 applier 时应返回 503，得到 %d", rec.Code)
	}
}

func TestOutputUpdateRejectsUnknownField(t *testing.T) {
	srv := newOutputServer("auto")
	srv.SetOutputApplier(func(OutputChange) (string, error) { return "", nil })
	rec := httptest.NewRecorder()
	srv.handleOutputUpdate(rec, httptest.NewRequest(http.MethodPost, "/api/output",
		strings.NewReader(`{"foo":1}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("未知字段应返回 400，得到 %d", rec.Code)
	}
}
