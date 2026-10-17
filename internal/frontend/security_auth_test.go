package frontend

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// newAuthorizePolicy 构造一个只关心认证判定的策略（跳过链路校验）。
func newAuthorizePolicy(t *testing.T, requireToken, loopbackAnonymous bool) (*accessPolicy, *tokenStore) {
	t.Helper()
	store := newTestTokenStore(t)
	if _, err := store.ensure(); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	return &accessPolicy{
		requireToken:      requireToken,
		loopbackAnonymous: loopbackAnonymous,
		tokens:            store,
	}, store
}

func authRequest(t *testing.T, path, authorization string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:5322"+path, nil)
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	return req
}

// 默认配置：环回免认证，局域网要 token。
func TestAuthorizeLoopbackAnonymous(t *testing.T) {
	policy, _ := newAuthorizePolicy(t, true, true)

	if err := policy.authorize(authRequest(t, "/mcp", ""), true); err != nil {
		t.Fatalf("环回免认证应放行，got %v", err)
	}
	// 非环回且无 token → 401
	if err := policy.authorize(authRequest(t, "/mcp", ""), false); !errors.Is(err, errUnauthorized) {
		t.Fatalf("非环回无 token 应拒绝，got %v", err)
	}
}

// 关闭环回免认证后，本机也需要 token。
func TestAuthorizeLoopbackRequiresToken(t *testing.T) {
	policy, store := newAuthorizePolicy(t, true, false)
	token, err := store.current()
	if err != nil {
		t.Fatalf("current: %v", err)
	}

	if err := policy.authorize(authRequest(t, "/mcp", ""), true); !errors.Is(err, errUnauthorized) {
		t.Fatalf("应要求 token，got %v", err)
	}
	if err := policy.authorize(authRequest(t, "/mcp", "Bearer "+string(token)), true); err != nil {
		t.Fatalf("正确 token 应放行，got %v", err)
	}
	if err := policy.authorize(authRequest(t, "/mcp", "Bearer wrong"), true); !errors.Is(err, errUnauthorized) {
		t.Fatalf("错误 token 应拒绝，got %v", err)
	}
}

// requireToken=false 时不做任何认证：用于用户主动关闭认证的场景。
func TestAuthorizeTokenDisabled(t *testing.T) {
	policy, _ := newAuthorizePolicy(t, false, false)
	if err := policy.authorize(authRequest(t, "/mcp", ""), false); err != nil {
		t.Fatalf("关闭认证后应放行，got %v", err)
	}
}

// /health 与 /version 无论配置如何都免认证。
func TestAuthorizeExemptPaths(t *testing.T) {
	policy, _ := newAuthorizePolicy(t, true, false)
	for _, path := range []string{"/health", "/version"} {
		if err := policy.authorize(authRequest(t, path, ""), false); err != nil {
			t.Fatalf("%s 应免认证，got %v", path, err)
		}
	}
	// 对照：同样条件下 /mcp 必须要求凭证。
	if err := policy.authorize(authRequest(t, "/mcp", ""), false); !errors.Is(err, errUnauthorized) {
		t.Fatalf("/mcp 应要求认证，got %v", err)
	}
}

// 配置要求认证但 token 存储缺失时必须拒绝，不能退化成裸奔。
func TestAuthorizeMissingTokenStoreDenies(t *testing.T) {
	policy := &accessPolicy{requireToken: true, loopbackAnonymous: false}
	if err := policy.authorize(authRequest(t, "/mcp", ""), false); !errors.Is(err, errTokenMissing) {
		t.Fatalf("token 存储缺失应拒绝，got %v", err)
	}
}
