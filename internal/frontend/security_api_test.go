package frontend

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newSecurityTestServer 构造带 token 与安全端点的最小 Server。
func newSecurityTestServer(t *testing.T) (*Server, *tokenStore) {
	t.Helper()
	store := newTestTokenStore(t)
	if _, err := store.ensure(); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	srv := &Server{
		access: &accessPolicy{requireToken: true, loopbackAnonymous: true, tokens: store},
		tokens: store,
	}
	return srv, store
}

func TestSecurityGetReturnsState(t *testing.T) {
	srv, _ := newSecurityTestServer(t)
	rec := httptest.NewRecorder()
	srv.handleSecurityGet(rec, httptest.NewRequest(http.MethodGet, "/api/security", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var payload struct {
		OK       bool         `json:"ok"`
		Security securityView `json:"security"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !payload.OK {
		t.Fatal("ok 应为 true")
	}
	if !payload.Security.RequireToken || !payload.Security.LoopbackAnonymous {
		t.Fatalf("安全状态不符: %+v", payload.Security)
	}
	if !payload.Security.TokenPresent || payload.Security.TokenPrefix == "" {
		t.Fatal("应报告 token 存在及前缀")
	}
}

// GET 端点不接受 POST，避免误用。
func TestSecurityGetRejectsPost(t *testing.T) {
	srv, _ := newSecurityTestServer(t)
	rec := httptest.NewRecorder()
	srv.handleSecurityGet(rec, httptest.NewRequest(http.MethodPost, "/api/security", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

// 未注入 applySecurity 时，更新应返回 503 而不是静默成功。
func TestSecurityUpdateWithoutApplier(t *testing.T) {
	srv, _ := newSecurityTestServer(t)
	body := strings.NewReader(`{"requireToken":false}`)
	rec := httptest.NewRecorder()
	srv.handleSecurityUpdate(rec, httptest.NewRequest(http.MethodPost, "/api/security", body))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

// 认证开关修改后应即时反映到策略上。
func TestSecurityUpdateAppliesTokenSwitch(t *testing.T) {
	srv, _ := newSecurityTestServer(t)
	srv.applySecurity = func(SecurityChange) error { return nil }

	body := strings.NewReader(`{"requireToken":false}`)
	rec := httptest.NewRecorder()
	srv.handleSecurityUpdate(rec, httptest.NewRequest(http.MethodPost, "/api/security", body))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if srv.access.requireToken {
		t.Fatal("requireToken 应被更新为 false")
	}
}

// 修改监听范围必须标记需要重启。
func TestSecurityUpdateListenLANRequiresRestart(t *testing.T) {
	srv, _ := newSecurityTestServer(t)
	srv.applySecurity = func(SecurityChange) error { return nil }

	body := strings.NewReader(`{"listenLan":false}`)
	rec := httptest.NewRecorder()
	srv.handleSecurityUpdate(rec, httptest.NewRequest(http.MethodPost, "/api/security", body))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var payload struct {
		Security  securityView `json:"security"`
		Restarted bool         `json:"restarted"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !payload.Security.RestartRequired {
		t.Fatal("修改 listenLan 应标记需要重启")
	}
	if payload.Restarted {
		t.Fatal("修改 listenLan 不应声称已即时生效")
	}
}

// 空 body 应被拒绝，而不是当成「什么都不改」静默通过。
func TestSecurityUpdateRejectsEmptyChange(t *testing.T) {
	srv, _ := newSecurityTestServer(t)
	srv.applySecurity = func(SecurityChange) error { return nil }

	rec := httptest.NewRecorder()
	srv.handleSecurityUpdate(rec, httptest.NewRequest(http.MethodPost, "/api/security", strings.NewReader(`{}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// 未知字段必须拒绝：面板只允许改三个开关。
func TestSecurityUpdateRejectsUnknownField(t *testing.T) {
	srv, _ := newSecurityTestServer(t)
	srv.applySecurity = func(SecurityChange) error { return nil }

	rec := httptest.NewRecorder()
	srv.handleSecurityUpdate(rec, httptest.NewRequest(http.MethodPost, "/api/security",
		strings.NewReader(`{"port":9999}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// 轮换返回完整 token，且旧 token 立即失效。
func TestTokenRotateEndpoint(t *testing.T) {
	srv, store := newSecurityTestServer(t)
	old, err := store.current()
	if err != nil {
		t.Fatalf("current: %v", err)
	}

	rec := httptest.NewRecorder()
	srv.handleTokenRotate(rec, httptest.NewRequest(http.MethodPost, "/api/token/rotate", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var payload struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload.Token == "" || payload.Token == string(old) {
		t.Fatal("应返回新的非空 token")
	}
	if store.verify(string(old)) {
		t.Fatal("旧 token 必须失效")
	}
	if !store.verify(payload.Token) {
		t.Fatal("新 token 应可校验通过")
	}
}
