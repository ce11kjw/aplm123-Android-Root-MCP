package frontend

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mustRequest 构造一个仅带 Authorization 头的请求，用于测试凭据提取。
func mustRequest(t *testing.T, authorization string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:5322/mcp", nil)
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	return req
}

func newTestTokenStore(t *testing.T) *tokenStore {
	t.Helper()
	dir := t.TempDir()
	store, err := newTokenStore(filepath.Join(dir, "token"))
	if err != nil {
		t.Fatalf("newTokenStore: %v", err)
	}
	return store
}

// 首次调用应生成 token，再次调用返回同一个值。
func TestTokenStoreGeneratesAndPersists(t *testing.T) {
	store := newTestTokenStore(t)

	first, err := store.ensure()
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if len(first) != tokenBytes*2 {
		t.Fatalf("token 长度 = %d, want %d", len(first), tokenBytes*2)
	}

	// 新实例读同一个文件，应拿到相同 token。
	reopened, err := newTokenStore(store.path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	second, err := reopened.ensure()
	if err != nil {
		t.Fatalf("ensure after reopen: %v", err)
	}
	if first != second {
		t.Fatalf("token 未持久化: %q != %q", first, second)
	}
}

// token 文件必须是 0600，避免同机其它用户读取。
func TestTokenStoreFileMode(t *testing.T) {
	store := newTestTokenStore(t)
	if _, err := store.ensure(); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	info, err := os.Stat(store.path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != tokenFileMode {
		t.Fatalf("token 权限 = %o, want %o", perm, tokenFileMode)
	}
}

// 轮换后旧 token 必须立即失效。
func TestTokenStoreRotateInvalidatesOld(t *testing.T) {
	store := newTestTokenStore(t)
	old, err := store.ensure()
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if !store.verify(old) {
		t.Fatal("新生成的 token 应能通过校验")
	}

	next, err := store.rotate()
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if next == old {
		t.Fatal("轮换后 token 应改变")
	}
	if store.verify(old) {
		t.Fatal("轮换后旧 token 必须失效")
	}
	if !store.verify(next) {
		t.Fatal("轮换后新 token 应通过校验")
	}
}

// 空文件（上次生成中途失败的残留）应重新生成而不是返回空 token。
func TestTokenStoreRecoversFromEmptyFile(t *testing.T) {
	store := newTestTokenStore(t)
	if err := os.MkdirAll(filepath.Dir(store.path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(store.path, nil, tokenFileMode); err != nil {
		t.Fatalf("write empty: %v", err)
	}

	token, err := store.ensure()
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if token == "" {
		t.Fatal("空文件场景应重新生成非空 token")
	}
}

func TestBearerFromRequest(t *testing.T) {
	cases := []struct {
		header string
		want   string
	}{
		{"Bearer abc123", "abc123"},
		{"bearer abc123", "abc123"}, // 大小写不敏感
		{"BEARER abc123", "abc123"},
		{"Bearer  abc123 ", "abc123"},
		{"", ""},
		{"Basic abc123", ""},
		{"Bearer", ""},
		{"Bearer ", ""},
	}
	for _, tc := range cases {
		req := mustRequest(t, tc.header)
		if got := bearerFromRequest(req); got != tc.want {
			t.Fatalf("header %q → %q, want %q", tc.header, got, tc.want)
		}
	}
}

// verify 必须拒绝空串与前缀，避免被空 token 绕过。
func TestTokenStoreVerifyRejectsEmpty(t *testing.T) {
	store := newTestTokenStore(t)
	if _, err := store.ensure(); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if store.verify("") {
		t.Fatal("空 token 不应通过")
	}
	if store.verify(strings.Repeat("0", 64)) {
		t.Fatal("伪造 token 不应通过")
	}
}

// prefix 用于界面核对，不能返回完整 token。
func TestTokenStorePrefix(t *testing.T) {
	store := newTestTokenStore(t)
	token, err := store.ensure()
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	got := store.prefix(8)
	if got != token[:8] {
		t.Fatalf("prefix = %q, want %q", got, token[:8])
	}
	if got == token {
		t.Fatal("prefix 不应返回完整 token")
	}
}
