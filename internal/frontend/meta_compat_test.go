package frontend

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 这些测试验证 2026-07-28 协议（SEP-2575）的 _meta 兼容层。
//
// 背景：SDK 要求声明该协议的请求在 _meta 中重复 protocolVersion 并带上
// clientCapabilities，否则 400。多数客户端只发 HTTP 头，因此需要服务端补齐。

// echoBody 把实际到达下游的请求体回传，用于断言补全结果。
func echoBody(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

func runCompat(t *testing.T, headerVersion string, body string) map[string]any {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:5322/mcp",
		strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	if headerVersion != "" {
		request.Header.Set("Mcp-Protocol-Version", headerVersion)
	}
	recorder := httptest.NewRecorder()
	normalizeNewProtocolMeta(echoBody)(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("状态码 = %d: %s", recorder.Code, recorder.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应不是 JSON: %s", recorder.Body.String())
	}
	return out
}

// metaOf 从 **params 内部** 取 _meta。
// 这是 SEP-2575 的实际布局：SDK 用 extractRequestMeta(req.Params) 读取。
func metaOf(t *testing.T, message map[string]any) map[string]any {
	t.Helper()
	params, ok := message["params"].(map[string]any)
	if !ok {
		t.Fatalf("params 缺失或类型错误: %#v", message["params"])
	}
	meta, ok := params["_meta"].(map[string]any)
	if !ok {
		t.Fatalf("params._meta 缺失或类型错误: %#v", params["_meta"])
	}
	return meta
}

// hasMeta 判断 params 内是否存在 _meta。
func hasMeta(message map[string]any) bool {
	params, ok := message["params"].(map[string]any)
	if !ok {
		return false
	}
	_, ok = params["_meta"]
	return ok
}

// TestOnlyMissingCapabilitiesIsPatched 是核心断言：
// 当请求体已声明 2026 协议（_meta.protocolVersion 存在）但缺
// clientCapabilities 时，必须补齐——这是 SDK 唯一会拒绝的情形。
func TestOnlyMissingCapabilitiesIsPatched(t *testing.T) {
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{` +
		`"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}}}`
	out := runCompat(t, "2026-07-28", body)

	meta := metaOf(t, out)
	if _, ok := meta[metaKeyClientCapabilities]; !ok {
		t.Fatal("clientCapabilities 未被补齐")
	}
}

// TestBodyWithoutMetaIsPatched 验证带 2026 协议头但无 _meta 的请求会被补齐。
// SDK 在 streamable.go 中强制要求 _meta.protocolVersion 存在，缺失即 400，
// 因此这是必需行为而非可选优化。
func TestBodyWithoutMetaIsPatched(t *testing.T) {
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`
	out := runCompat(t, "2026-07-28", body)
	meta := metaOf(t, out)
	if got, _ := meta[metaKeyProtocolVersion].(string); got != "2026-07-28" {
		t.Fatalf("protocolVersion = %q，期望 2026-07-28", got)
	}
	if _, ok := meta[metaKeyClientCapabilities]; !ok {
		t.Fatal("clientCapabilities 未被补齐")
	}
}

// TestNonProtocolKeysPassThrough 验证补齐不会影响请求的其它字段。
func TestNonProtocolKeysPassThrough(t *testing.T) {
	body := `{"jsonrpc":"2.0","id":42,"method":"tools/call",` +
		`"params":{"name":"aplm_status","arguments":{"action":"get"}}}`
	out := runCompat(t, "2026-07-28", body)
	if id, _ := out["id"].(float64); id != 42 {
		t.Fatalf("id 被破坏: %v", out["id"])
	}
	if method, _ := out["method"].(string); method != "tools/call" {
		t.Fatalf("method 被破坏: %v", out["method"])
	}
	params, _ := out["params"].(map[string]any)
	if params["name"] != "aplm_status" {
		t.Fatalf("params.name 被破坏: %v", params["name"])
	}
	args, _ := params["arguments"].(map[string]any)
	if args["action"] != "get" {
		t.Fatalf("params.arguments 被破坏: %v", args)
	}
}

// TestNewerProtocolVersionAlsoPatched 验证更高协议版本同样适用。
func TestNewerProtocolVersionAlsoPatched(t *testing.T) {
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{` +
		`"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-09-01"}}}`
	out := runCompat(t, "2026-09-01", body)
	meta := metaOf(t, out)
	if _, ok := meta[metaKeyClientCapabilities]; !ok {
		t.Fatal("clientCapabilities 未被补齐")
	}
}

// TestExistingMetaIsPreserved 验证客户端已提供的值不会被改写。
func TestExistingMetaIsPreserved(t *testing.T) {
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{` +
		`"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28",` +
		`"io.modelcontextprotocol/clientCapabilities":{"roots":{}}}}}`
	out := runCompat(t, "2026-07-28", body)

	meta := metaOf(t, out)
	caps, ok := meta[metaKeyClientCapabilities].(map[string]any)
	if !ok {
		t.Fatalf("clientCapabilities 类型被破坏: %#v", meta[metaKeyClientCapabilities])
	}
	// 客户端明确提供的能力必须原样保留，不能被空对象覆盖。
	if _, ok := caps["roots"]; !ok {
		t.Fatalf("客户端提供的 clientCapabilities 被覆盖: %#v", caps)
	}
}

// TestMismatchedVersionIsNotSilentlyFixed 验证协议不一致时不做"修复"。
// 头说 2026-07-28、_meta 说别的版本，属于真实不一致，应交由 SDK 判定。
func TestMismatchedVersionIsNotSilentlyFixed(t *testing.T) {
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{` +
		`"_meta":{"io.modelcontextprotocol/protocolVersion":"2025-06-18"}}}`
	out := runCompat(t, "2026-07-28", body)

	meta := metaOf(t, out)
	if got, _ := meta[metaKeyProtocolVersion].(string); got != "2025-06-18" {
		t.Fatalf("客户端声明的版本被改写为 %q，不应发生", got)
	}
	// clientCapabilities 仍应被补齐（缺的是它，不是版本）
	if _, ok := meta[metaKeyClientCapabilities]; !ok {
		t.Fatal("clientCapabilities 应被补齐")
	}
}

// TestOldProtocolPassesThrough 验证旧协议不受影响。
func TestOldProtocolPassesThrough(t *testing.T) {
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`
	out := runCompat(t, "2025-11-25", body)
	if hasMeta(out) {
		t.Fatal("旧协议不应被注入 _meta")
	}
}

// TestNoHeaderPassesThrough 验证不带协议头时原样透传。
func TestNoHeaderPassesThrough(t *testing.T) {
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`
	out := runCompat(t, "", body)
	if hasMeta(out) {
		t.Fatal("无协议头时不应注入 _meta")
	}
}

// TestBatchRequestsAreNormalized 验证批量请求逐条补全。
func TestBatchRequestsAreNormalized(t *testing.T) {
	body := `[{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{` +
		`"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}}},` +
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{` +
		`"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}}}]`
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:5322/mcp",
		strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Mcp-Protocol-Version", "2026-07-28")
	recorder := httptest.NewRecorder()
	normalizeNewProtocolMeta(echoBody)(recorder, request)

	var batch []map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &batch); err != nil {
		t.Fatalf("批量响应解析失败: %s", recorder.Body.String())
	}
	if len(batch) != 2 {
		t.Fatalf("批量条数 = %d，期望 2", len(batch))
	}
	for i, item := range batch {
		params, ok := item["params"].(map[string]any)
		if !ok {
			t.Fatalf("第 %d 条 params 丢失", i)
		}
		meta, ok := params["_meta"].(map[string]any)
		if !ok {
			t.Fatalf("第 %d 条 params._meta 缺失", i)
		}
		if _, ok := meta[metaKeyClientCapabilities]; !ok {
			t.Fatalf("第 %d 条 clientCapabilities 未补齐", i)
		}
	}
}

// TestNotificationAlsoNormalized 验证通知（无 id）同样被补全。
func TestNotificationAlsoNormalized(t *testing.T) {
	body := `{"jsonrpc":"2.0","method":"notifications/initialized","params":{` +
		`"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}}}`
	out := runCompat(t, "2026-07-28", body)
	meta := metaOf(t, out)
	if _, ok := meta[metaKeyClientCapabilities]; !ok {
		t.Fatal("通知应同样补齐 clientCapabilities")
	}
}

// TestNonObjectMetaPassesThrough 验证 _meta 类型异常时原样透传。
// 无法判定协议版本，最小干预原则下不做改写，交由 SDK 报错。
func TestNonObjectMetaPassesThrough(t *testing.T) {
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":"broken"}}`
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:5322/mcp",
		strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Mcp-Protocol-Version", "2026-07-28")
	recorder := httptest.NewRecorder()
	normalizeNewProtocolMeta(echoBody)(recorder, request)
	if got := recorder.Body.String(); got != body {
		t.Fatalf("_meta 类型异常时请求体被改写：\n got %s\nwant %s", got, body)
	}
}

// TestBodyIsRestoredForDownstream 验证 body 被正确恢复，下游能完整读取。
func TestBodyIsRestoredForDownstream(t *testing.T) {
	body := `{"jsonrpc":"2.0","id":7,"method":"tools/list","params":{}}`
	out := runCompat(t, "2026-07-28", body)
	if id, _ := out["id"].(float64); id != 7 {
		t.Fatalf("下游拿到的 id = %v，请求体可能被破坏", out["id"])
	}
	if method, _ := out["method"].(string); method != "tools/list" {
		t.Fatalf("下游拿到的 method = %v", out["method"])
	}
}

// TestOversizedBodyIsNotFullyBuffered 验证超大请求跳过补全。
func TestOversizedBodyIsNotFullyBuffered(t *testing.T) {
	// ContentLength 远超上限时，中间件不应读取 body。
	big := bytes.Repeat([]byte("x"), 64)
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:5322/mcp",
		bytes.NewReader(big))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Mcp-Protocol-Version", "2026-07-28")
	request.ContentLength = metaCompatMaxBytes + 1

	var seen int
	handler := normalizeNewProtocolMeta(func(w http.ResponseWriter, r *http.Request) {
		seen = int(r.ContentLength)
		w.WriteHeader(http.StatusOK)
	})
	recorder := httptest.NewRecorder()
	handler(recorder, request)
	if seen != metaCompatMaxBytes+1 {
		t.Fatalf("下游看到的 ContentLength = %d，应保持原值", seen)
	}
}

// TestRequestDeclaresNewProtocol 单元测试版本判定。
func TestRequestDeclaresNewProtocol(t *testing.T) {
	cases := map[string]bool{
		"":             false,
		"2025-11-25":   false,
		"2026-06-30":   false,
		"2026-07-28":   true,
		"2026-09-01":   true,
		" 2026-07-28 ": true,
	}
	for value, want := range cases {
		if got := requestDeclaresNewProtocol(value); got != want {
			t.Errorf("requestDeclaresNewProtocol(%q) = %v，期望 %v", value, got, want)
		}
	}
}

// TestStandardHeadersAreInjected 验证 Mcp-Method / Mcp-Name 被补齐。
// 2026-07-28 起 SDK 要求 Mcp-Method 必须存在且与请求体一致。
func TestStandardHeadersAreInjected(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:5322/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call",`+
			`"params":{"name":"aplm_status","arguments":{}}}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Mcp-Protocol-Version", "2026-07-28")

	var gotMethod, gotName string
	handler := normalizeNewProtocolMeta(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Header.Get(headerMcpMethod)
		gotName = r.Header.Get(headerMcpName)
		w.WriteHeader(http.StatusOK)
	})
	handler(httptest.NewRecorder(), request)

	if gotMethod != "tools/call" {
		t.Fatalf("Mcp-Method = %q，期望 tools/call", gotMethod)
	}
	if gotName != "aplm_status" {
		t.Fatalf("Mcp-Name = %q，期望 aplm_status", gotName)
	}
}

// TestProvidedHeadersAreNotOverwritten 验证客户端已提供的头不被覆盖。
// 头与请求体不一致属于真实协议错误，应交由 SDK 报错。
func TestProvidedHeadersAreNotOverwritten(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:5322/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Mcp-Protocol-Version", "2026-07-28")
	request.Header.Set(headerMcpMethod, "prompts/list") // 故意与 body 不符

	var gotMethod string
	handler := normalizeNewProtocolMeta(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Header.Get(headerMcpMethod)
		w.WriteHeader(http.StatusOK)
	})
	handler(httptest.NewRecorder(), request)

	if gotMethod != "prompts/list" {
		t.Fatalf("已提供的 Mcp-Method 被覆盖为 %q，不应发生", gotMethod)
	}
}

// TestListMethodsHaveNoNameHeader 验证非 tools/call 不注入 Mcp-Name。
func TestListMethodsHaveNoNameHeader(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:5322/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Mcp-Protocol-Version", "2026-07-28")

	var gotName string
	handler := normalizeNewProtocolMeta(func(w http.ResponseWriter, r *http.Request) {
		gotName = r.Header.Get(headerMcpName)
		w.WriteHeader(http.StatusOK)
	})
	handler(httptest.NewRecorder(), request)

	if gotName != "" {
		t.Fatalf("tools/list 不应注入 Mcp-Name，实际为 %q", gotName)
	}
}

// TestOldProtocolSkipsHeaderInjection 验证旧协议不补标准头。
func TestOldProtocolSkipsHeaderInjection(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:5322/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Mcp-Protocol-Version", "2025-11-25")

	var gotMethod string
	handler := normalizeNewProtocolMeta(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Header.Get(headerMcpMethod)
		w.WriteHeader(http.StatusOK)
	})
	handler(httptest.NewRecorder(), request)

	if gotMethod != "" {
		t.Fatalf("旧协议不应注入 Mcp-Method，实际为 %q", gotMethod)
	}
}

// TestExtractMethodAndName 单元测试提取逻辑。
func TestExtractMethodAndName(t *testing.T) {
	cases := []struct {
		body       string
		wantMethod string
		wantName   string
	}{
		{`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`, "tools/list", ""},
		{`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"aplm_ui"}}`, "tools/call", "aplm_ui"},
		{`{"jsonrpc":"2.0","method":"notifications/initialized"}`, "notifications/initialized", ""},
		{`[{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}]`, "tools/list", ""},
		{`not json`, "", ""},
		{``, "", ""},
	}
	for _, tc := range cases {
		method, name := extractMethodAndName([]byte(tc.body))
		if method != tc.wantMethod || name != tc.wantName {
			t.Errorf("extractMethodAndName(%q) = (%q, %q)，期望 (%q, %q)",
				tc.body, method, name, tc.wantMethod, tc.wantName)
		}
	}
}
