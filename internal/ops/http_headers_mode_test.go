package ops

import (
	"net/http"
	"strings"
	"testing"
)

func TestSanitizedHTTPHeadersSafeModeRedacts(t *testing.T) {
	headers := http.Header{}
	headers.Set("Content-Type", "application/json")
	headers.Set("Location", "https://example.com/next?token=abc")
	headers.Set("Mcp-Session-Id", "SESSION123")
	headers.Set("Set-Cookie", "sid=secret")

	safe := sanitizedHTTPHeadersMode(headers, "safe")
	if got := safe.Get("Content-Type"); got != "application/json" {
		t.Fatalf("白名单头应保留，得到 %q", got)
	}
	for _, name := range []string{"Location", "Mcp-Session-Id", "Set-Cookie"} {
		value := strings.Join(safe.Values(name), ",")
		if !strings.Contains(value, "redacted") {
			t.Fatalf("%s 在 safe 模式应被打码，得到 %q", name, value)
		}
		if strings.Contains(value, "SESSION123") || strings.Contains(value, "token=abc") {
			t.Fatalf("%s 不应泄露原值：%q", name, value)
		}
	}
}

func TestSanitizedHTTPHeadersFullModeKeepsSemanticHeaders(t *testing.T) {
	headers := http.Header{}
	headers.Set("Location", "https://example.com/next")
	headers.Set("Mcp-Session-Id", "SESSION123")
	headers.Set("ETag", "W/\"abc\"")
	headers.Set("Set-Cookie", "sid=secret")

	full := sanitizedHTTPHeadersMode(headers, "full")
	if got := full.Get("Location"); got != "https://example.com/next" {
		t.Fatalf("full 模式应保留 Location，得到 %q", got)
	}
	if got := full.Get("Mcp-Session-Id"); got != "SESSION123" {
		t.Fatalf("full 模式应保留 Mcp-Session-Id，得到 %q", got)
	}
	if got := full.Get("ETag"); got != "W/\"abc\"" {
		t.Fatalf("full 模式应保留 ETag，得到 %q", got)
	}
	// Cookie 值即使 full 也要打码：它是凭据而不是语义元数据。
	if cookie := full.Get("Set-Cookie"); strings.Contains(cookie, "secret") {
		t.Fatalf("full 模式仍须打码 Set-Cookie 的值，得到 %q", cookie)
	}
}

func TestResponseHeaderModeArg(t *testing.T) {
	cases := []struct {
		args map[string]any
		want string
	}{
		{nil, "safe"},
		{map[string]any{}, "safe"},
		{map[string]any{"headersMode": "full"}, "full"},
		{map[string]any{"headersMode": "FULL "}, "full"},
		{map[string]any{"headersMode": "bogus"}, "safe"},
		{map[string]any{"headersMode": 123}, "safe"},
	}
	for _, tc := range cases {
		if got := responseHeaderMode(tc.args); got != tc.want {
			t.Fatalf("args=%v 期望 %q，得到 %q", tc.args, tc.want, got)
		}
	}
}

func TestAutoBackgroundSecondsParse(t *testing.T) {
	cases := []struct {
		args map[string]any
		want int
		ok   bool
	}{
		{nil, 0, false},
		{map[string]any{}, 0, false},
		{map[string]any{"autoBackgroundSeconds": float64(30)}, 30, true},
		{map[string]any{"autoBackgroundSeconds": 5}, 5, true},
		{map[string]any{"autoBackgroundSeconds": float64(0)}, 0, false},
		{map[string]any{"autoBackgroundSeconds": "30"}, 0, false},
	}
	for _, tc := range cases {
		got, ok := autoBackgroundSeconds(tc.args)
		if got != tc.want || ok != tc.ok {
			t.Fatalf("args=%v 期望 (%d,%v)，得到 (%d,%v)", tc.args, tc.want, tc.ok, got, ok)
		}
	}
}
