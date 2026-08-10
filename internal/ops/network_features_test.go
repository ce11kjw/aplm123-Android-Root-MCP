package ops

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/net/html"
)

func TestPersistentCookieJarAndHeaderRedaction(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/set":
			http.SetCookie(w, &http.Cookie{Name: "sid", Value: "top-secret", Path: "/", HttpOnly: true})
			_, _ = w.Write([]byte("set"))
		case "/check":
			cookie, err := r.Cookie("sid")
			if err != nil || cookie.Value != "top-secret" {
				http.Error(w, "missing", http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte("authenticated"))
		}
	}))
	defer server.Close()
	root := t.TempDir()
	manager := New(Config{WorkDir: filepath.Join(root, "work"), StateDir: filepath.Join(root, "state")})
	set := manager.httpRequest(context.Background(), map[string]any{"url": server.URL + "/set"})
	if !set.Success {
		t.Fatalf("set cookie failed: %+v", set)
	}
	raw, _ := json.Marshal(set.Data)
	if strings.Contains(string(raw), "top-secret") || !strings.Contains(string(raw), "redacted") {
		t.Fatalf("Set-Cookie was not redacted: %s", raw)
	}
	manager.Close()
	restarted := New(Config{WorkDir: filepath.Join(root, "work"), StateDir: filepath.Join(root, "state")})
	defer restarted.Close()
	check := restarted.httpRequest(context.Background(), map[string]any{"url": server.URL + "/check"})
	if !check.Success || !strings.Contains(check.Data.(map[string]any)["body"].(string), "authenticated") {
		t.Fatalf("persisted cookie was not restored: %+v", check)
	}
}

func TestCookieOriginOmitsCredentialPathAndMigratesLegacyRecords(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	manager := New(Config{WorkDir: filepath.Join(root, "work"), StateDir: stateDir})
	defer manager.Close()

	jar, err := manager.networkState().cookieJar("origin-boundary")
	if err != nil {
		t.Fatal(err)
	}
	origin := mustURL(t, "https://example.test/session/path-secret?token=query-secret")
	if err := jar.set(origin, &http.Cookie{Name: "sid", Value: "cookie-value", Path: "/"}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(jar.path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "path-secret") || strings.Contains(string(raw), "query-secret") {
		t.Fatalf("credential-bearing request path was persisted: %s", raw)
	}
	listed := manager.cookieOperation(Request{Tool: "zcr521_network", Args: map[string]any{"jar": "origin-boundary"}}, "cookie_list")
	listedRaw, _ := json.Marshal(listed.Data)
	if !listed.Success || strings.Contains(string(listedRaw), "path-secret") || strings.Contains(string(listedRaw), "query-secret") {
		t.Fatalf("cookie list exposed credential-bearing origin: %+v", listed)
	}

	legacyPath := filepath.Join(stateDir, "network", "cookie-jars", "legacy-origin.json")
	_, err = persistCookieRecords(legacyPath, []storedCookie{{
		Origin: "https://example.test/session/legacy-path-secret?token=legacy-query-secret",
		Name:   "legacy",
		Value:  "x",
		Path:   "/",
	}})
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := manager.networkState().cookieJar("legacy-origin")
	if err != nil {
		t.Fatal(err)
	}
	if err := legacy.persistenceError(); err != nil {
		t.Fatalf("legacy origin migration failed: %v", err)
	}
	migrated, err := os.ReadFile(legacyPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(migrated), "legacy-path-secret") || strings.Contains(string(migrated), "legacy-query-secret") {
		t.Fatalf("legacy credential-bearing origin was not rewritten: %s", migrated)
	}
}

func TestHTMLFeedAndWebSocketTools(t *testing.T) {
	document, err := html.Parse(strings.NewReader(`<html><head><title>Demo</title><meta name="description" content="sample"></head><body><script>secret()</script><p>Hello <b>world</b></p><a href="/next">Next</a></body></html>`))
	if err != nil {
		t.Fatal(err)
	}
	extracted := extractHTML(document, mustURL(t, "https://example.test/base"), 1024, 10)
	if extracted["title"] != "Demo" || !strings.Contains(extracted["text"].(string), "Hello world") || strings.Contains(extracted["text"].(string), "secret") {
		t.Fatalf("unexpected extraction: %+v", extracted)
	}

	feedServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(`<?xml version="1.0"?><rss version="2.0"><channel><title>News</title><link>https://example.test/</link><item><title>One</title><link>https://example.test/1</link><guid>1</guid><description>Summary</description></item></channel></rss>`))
	}))
	defer feedServer.Close()
	manager := testManager(t)
	feed := manager.parseFeed(context.Background(), Request{Tool: "zcr521_network", Args: map[string]any{"url": feedServer.URL, "maxItems": float64(10)}})
	if !feed.Success || feed.Data.(map[string]any)["format"] != "rss" || feed.Data.(map[string]any)["count"] != 1 {
		t.Fatalf("feed parse failed: %+v", feed)
	}

	wsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connection, acceptErr := websocket.Accept(w, r, nil)
		if acceptErr != nil {
			return
		}
		defer connection.Close(websocket.StatusNormalClosure, "done")
		messageType, payload, readErr := connection.Read(r.Context())
		if readErr == nil {
			_ = connection.Write(r.Context(), messageType, payload)
		}
	}))
	defer wsServer.Close()
	connect := manager.webSocketOperation(context.Background(), Request{Tool: "zcr521_network", Args: map[string]any{"url": "ws" + strings.TrimPrefix(wsServer.URL, "http")}}, "ws_connect")
	if !connect.Success {
		t.Fatalf("ws connect: %+v", connect)
	}
	id := connect.Data.(map[string]any)["connectionId"].(string)
	send := manager.webSocketOperation(context.Background(), Request{Tool: "zcr521_network", Args: map[string]any{"connectionId": id, "message": "hello"}}, "ws_send")
	if !send.Success {
		t.Fatalf("ws send: %+v", send)
	}
	receive := manager.webSocketOperation(context.Background(), Request{Tool: "zcr521_network", Args: map[string]any{"connectionId": id, "timeout": "2s"}}, "ws_receive")
	if !receive.Success {
		t.Fatalf("ws receive: %+v", receive)
	}
	message := receive.Data.(map[string]any)["message"].(webSocketMessage)
	if message.Data != "hello" || message.Type != "text" {
		t.Fatalf("unexpected ws message: %+v", message)
	}
	_ = manager.webSocketOperation(context.Background(), Request{Tool: "zcr521_network", Args: map[string]any{"connectionId": id}}, "ws_close")
	manager.Close()
}

func TestDownloadRedirectDoesNotLeakSensitiveHeaders(t *testing.T) {
	receivedAuthorization := ""
	receivedCookie := ""
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuthorization = r.Header.Get("Authorization")
		receivedCookie = r.Header.Get("Cookie")
		_, _ = w.Write([]byte("ok"))
	}))
	defer destination.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL, http.StatusFound)
	}))
	defer redirect.Close()
	requestURL := strings.Replace(redirect.URL, "127.0.0.1", "localhost", 1)
	path := filepath.Join(t.TempDir(), "download.part")
	_, _, err := downloadAttempt(context.Background(), requestURL, path, map[string]string{"Authorization": "Bearer secret", "Cookie": "sid=secret"}, 5*time.Second, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if receivedAuthorization != "" || receivedCookie != "" {
		t.Fatalf("sensitive redirect leak: authorization=%q cookie=%q", receivedAuthorization, receivedCookie)
	}
	if raw, err := os.ReadFile(path); err != nil || string(raw) != "ok" {
		t.Fatalf("download result: %q %v", raw, err)
	}
}

func TestRedirectSecretsStayStrippedAcrossSameOriginLaterHop(t *testing.T) {
	initial, _ := http.NewRequest(http.MethodGet, "https://origin.test/start", nil)
	initial.Header.Set("Authorization", "Bearer secret")
	initial.Header.Set("X-Api-Key", "secret")
	initial.Header.Set("X-Auth", "secret")
	first, _ := http.NewRequest(http.MethodGet, "https://other.test/one", nil)
	first.Header = initial.Header.Clone()
	if err := safeRedirectPolicy(first, []*http.Request{initial}); err != nil {
		t.Fatal(err)
	}
	if first.Header.Get("Authorization") != "" {
		t.Fatal("first cross-origin hop retained credentials")
	}
	// net/http reconstructs later redirect headers from the initial request.
	second, _ := http.NewRequest(http.MethodGet, "https://other.test/two", nil)
	second.Header = initial.Header.Clone()
	if err := safeRedirectPolicy(second, []*http.Request{initial, first}); err != nil {
		t.Fatal(err)
	}
	if second.Header.Get("Authorization") != "" || second.Header.Get("X-Api-Key") != "" || second.Header.Get("X-Auth") != "" {
		t.Fatal("same-origin later hop restored initial credentials")
	}
	userinfo, _ := http.NewRequest(http.MethodGet, "https://user:pass@other.test/", nil)
	if err := safeRedirectPolicy(userinfo, []*http.Request{initial}); err == nil {
		t.Fatal("redirect userinfo was accepted")
	}
}

func TestCrossOriginRedirectDropsRefererRejectsBodyAndSanitizesErrors(t *testing.T) {
	initial, _ := http.NewRequest(http.MethodPost, "https://origin.test/form?token=secret", strings.NewReader("password=secret"))
	next, _ := http.NewRequest(http.MethodPost, "https://other.test/receive", strings.NewReader("password=secret"))
	next.Header.Set("Referer", initial.URL.String())
	if err := safeRedirectPolicy(next, []*http.Request{initial}); err == nil {
		t.Fatal("cross-origin POST redirect was accepted")
	}
	if next.Header.Get("Referer") != "" {
		t.Fatal("cross-origin redirect retained Referer")
	}
	getWithBody, _ := http.NewRequest(http.MethodGet, "https://origin.test/query", strings.NewReader("credential=secret"))
	getRedirect, _ := http.NewRequest(http.MethodGet, "https://other.test/query", strings.NewReader("credential=secret"))
	if err := safeRedirectPolicy(getRedirect, []*http.Request{getWithBody}); err == nil {
		t.Fatal("cross-origin GET body redirect was accepted")
	}
	rawURL := "https://example.test/private?token=super-secret"
	sanitized := sanitizeNetworkError(&url.Error{Op: "Get", URL: rawURL, Err: errors.New("dial " + rawURL)}, rawURL)
	if strings.Contains(sanitized.Error(), "super-secret") || strings.Contains(sanitized.Error(), "token=") || strings.Contains(sanitized.Error(), "/private") {
		t.Fatalf("network error leaked URL credentials: %v", sanitized)
	}
	oversizedError := sanitizeNetworkError(errors.New(strings.Repeat("secret-path/", 1024)), rawURL)
	if len(oversizedError.Error()) > 1536 {
		t.Fatalf("sanitized error exceeded hard bound: %d", len(oversizedError.Error()))
	}
	deadline := sanitizeNetworkError(&url.Error{Op: "Get", URL: rawURL, Err: context.DeadlineExceeded}, rawURL)
	if !errors.Is(deadline, context.DeadlineExceeded) {
		t.Fatal("sanitized error lost deadline cause")
	}
	redirectSecret := "redirect-location-secret"
	redirectError := sanitizeNetworkError(&url.Error{
		Op:  "Get",
		URL: rawURL,
		Err: errors.New(`failed to parse Location header "http://[::1/?token=` + redirectSecret + `": missing ']' in host`),
	}, rawURL)
	if strings.Contains(redirectError.Error(), redirectSecret) || strings.Contains(redirectError.Error(), "Location header") {
		t.Fatalf("redirect error leaked response-controlled text: %v", redirectError)
	}
}

func TestHTTPRequestClassifiesCanceledContext(t *testing.T) {
	manager := testManager(t)
	defer manager.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result := manager.httpRequest(ctx, map[string]any{"url": "https://example.test/"})
	if result.Code != "CANCELLED" {
		t.Fatalf("cancelled request classification=%s: %+v", result.Code, result)
	}
}

func TestCookieJarLimitsAndStrictBooleanArguments(t *testing.T) {
	manager := testManager(t)
	defer manager.Close()
	state, err := manager.networkState().cookieJar("bounded")
	if err != nil {
		t.Fatal(err)
	}
	origin := mustURL(t, "https://example.test/")
	state.SetCookies(origin, []*http.Cookie{{Name: "oversized", Value: strings.Repeat("x", maxStoredCookieBytes)}})
	if err := state.persistenceError(); err == nil {
		t.Fatal("oversized cookie was accepted")
	}
	state.mu.Lock()
	count := len(state.records)
	state.mu.Unlock()
	if count != 0 {
		t.Fatalf("rejected cookie changed records: %d", count)
	}
	invalid := manager.cookieOperation(Request{Tool: "zcr521_network", Args: map[string]any{"jar": "bounded", "url": "https://example.test/", "name": "sid", "value": "x", "secure": "not-a-bool"}}, "cookie_set")
	if invalid.Success || invalid.Code != "INVALID_ARGUMENT" {
		t.Fatalf("invalid secure type was silently accepted: %+v", invalid)
	}
}

func TestCookieBatchLimitIsTransactional(t *testing.T) {
	manager := testManager(t)
	defer manager.Close()
	state, err := manager.networkState().cookieJar("batch-limit")
	if err != nil {
		t.Fatal(err)
	}
	origin := mustURL(t, "https://example.test/")
	cookies := make([]*http.Cookie, maxCookiesPerJar+1)
	for index := range cookies {
		cookies[index] = &http.Cookie{Name: "c" + strconv.Itoa(index), Value: "x", Path: "/"}
	}
	if err := state.applyCookies(origin, cookies, false); err == nil {
		t.Fatal("oversized Set-Cookie batch was accepted")
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if len(state.records) != 0 {
		t.Fatalf("rejected batch changed state: %d", len(state.records))
	}
}

func TestCookieJarRejectsInvalidAndCrossDomainCookies(t *testing.T) {
	manager := testManager(t)
	defer manager.Close()
	state, err := manager.networkState().cookieJar("domain-check")
	if err != nil {
		t.Fatal(err)
	}
	origin := mustURL(t, "https://www.example.com/account")
	for _, cookie := range []*http.Cookie{
		{Name: "cross", Value: "x", Path: "/", Domain: "evil.example"},
		{Name: "suffix", Value: "x", Path: "/", Domain: "com"},
		{Name: "invalid", Value: "line\nbreak", Path: "/"},
	} {
		state.SetCookies(origin, []*http.Cookie{cookie})
		if err := state.persistenceError(); err == nil {
			t.Fatalf("invalid cookie was accepted: %+v", cookie)
		}
	}
	state.mu.Lock()
	count := len(state.records)
	state.mu.Unlock()
	if count != 0 || len(state.Cookies(origin)) != 0 {
		t.Fatalf("rejected cookies polluted jar: records=%d cookies=%v", count, state.Cookies(origin))
	}
	state.SetCookies(origin, []*http.Cookie{{Name: "valid", Value: "x", Path: "/", Domain: ".example.com"}})
	if err := state.persistenceError(); err != nil || len(state.Cookies(origin)) != 1 {
		t.Fatalf("valid parent-domain cookie rejected: err=%v cookies=%v", err, state.Cookies(origin))
	}
	state.SetCookies(origin, []*http.Cookie{{Name: "Valid", Value: "upper", Path: "/", Domain: ".example.com"}})
	state.mu.Lock()
	count = len(state.records)
	state.mu.Unlock()
	if count != 2 {
		t.Fatalf("case-sensitive cookie names were collapsed: records=%d", count)
	}
}

func TestInvalidResponseCookieDoesNotPoisonRequestsAndClearIsTransactional(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/invalid" {
			w.Header().Add("Set-Cookie", "bad=x; Domain=evil.example; Path=/")
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer server.Close()
	manager := testManager(t)
	defer manager.Close()
	for _, path := range []string{"/invalid", "/clean"} {
		if _, err := manager.fetchHTTP(context.Background(), map[string]any{"url": server.URL + path, "jar": "response"}, 1024); err != nil {
			t.Fatalf("request %s was poisoned by invalid Set-Cookie: %v", path, err)
		}
	}

	state, err := manager.networkState().cookieJar("response")
	if err != nil {
		t.Fatal(err)
	}
	origin := mustURL(t, server.URL+"/")
	if err := state.set(origin, &http.Cookie{Name: "keep", Value: "x", Path: "/"}); err != nil {
		t.Fatal(err)
	}
	state.path = t.TempDir() // atomic replacement over a directory must fail.
	cleared := manager.cookieOperation(Request{Tool: "zcr521_network", Args: map[string]any{"jar": "response", "confirmDangerous": true}}, "cookie_clear")
	if cleared.Success || cleared.Code != "COOKIE_WRITE_FAILED" {
		t.Fatalf("failed clear was reported as success: %+v", cleared)
	}
	state.mu.Lock()
	remaining := len(state.records)
	state.mu.Unlock()
	if remaining != 1 || len(state.Cookies(origin)) != 1 {
		t.Fatalf("failed clear changed in-memory jar: records=%d cookies=%v", remaining, state.Cookies(origin))
	}
}

func TestCookiePersistencePropagatesMarshalErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jar.json")
	_, err := persistCookieRecords(path, []storedCookie{{Origin: "https://example.com/", Name: "bad-time", Value: "x", Path: "/", Expires: time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)}})
	if err == nil {
		t.Fatal("invalid JSON time was silently persisted")
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("marshal failure created a jar file: %v", statErr)
	}
}

func TestCorruptCookieJarDoesNotKeepPrefixAndJarRegistryIsBounded(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	jarDir := filepath.Join(stateDir, "network", "cookie-jars")
	if err := os.MkdirAll(jarDir, 0o700); err != nil {
		t.Fatal(err)
	}
	brokenPath := filepath.Join(jarDir, "broken.json")
	broken := `[{"origin":"https://example.test/","name":"old","value":"must-not-return","path":"/"},{`
	if err := os.WriteFile(brokenPath, []byte(broken), 0o600); err != nil {
		t.Fatal(err)
	}
	manager := New(Config{WorkDir: filepath.Join(root, "work"), StateDir: stateDir})
	defer manager.Close()
	jar, err := manager.networkState().cookieJar("broken")
	if err != nil {
		t.Fatal(err)
	}
	jar.mu.Lock()
	count, loadErr := len(jar.records), jar.lastErr
	jar.mu.Unlock()
	if count != 0 || loadErr == "" || len(jar.Cookies(mustURL(t, "https://example.test/"))) != 0 {
		t.Fatalf("corrupt jar retained partial records: count=%d err=%q", count, loadErr)
	}
	if err := jar.set(mustURL(t, "https://example.test/"), &http.Cookie{Name: "new", Value: "safe", Path: "/"}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(brokenPath)
	if err != nil || strings.Contains(string(raw), "must-not-return") || strings.Contains(string(raw), `"old"`) {
		t.Fatalf("later write resurrected corrupt prefix: %s err=%v", raw, err)
	}
	for index := 0; index < maxActiveCookieJars-1; index++ { // broken already occupies one slot.
		if _, err := manager.networkState().cookieJar("jar-" + strconv.Itoa(index)); err != nil {
			t.Fatalf("jar %d: %v", index, err)
		}
	}
	if _, err := manager.networkState().cookieJar("one-too-many"); err == nil {
		t.Fatal("active cookie jar registry exceeded its limit")
	}
}

func TestCorruptCookieJarBlocksHTTPAndWebSocketUntilRepaired(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path == "/ws" {
			connection, err := websocket.Accept(w, r, nil)
			if err == nil {
				_ = connection.Close(websocket.StatusNormalClosure, "done")
			}
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer server.Close()

	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	jarDir := filepath.Join(stateDir, "network", "cookie-jars")
	if err := os.MkdirAll(jarDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(jarDir, "broken.json"), []byte(`[{`), 0o600); err != nil {
		t.Fatal(err)
	}
	manager := New(Config{WorkDir: filepath.Join(root, "work"), StateDir: stateDir})
	defer manager.Close()

	httpResult := manager.httpRequest(context.Background(), map[string]any{"url": server.URL, "jar": "broken"})
	if httpResult.Success || httpResult.Code != "COOKIE_JAR_UNTRUSTED" || requests.Load() != 0 {
		t.Fatalf("corrupt jar did not fail closed before HTTP I/O: result=%+v requests=%d", httpResult, requests.Load())
	}
	wsResult := manager.webSocketOperation(context.Background(), Request{Tool: "zcr521_network", Args: map[string]any{"url": "ws" + strings.TrimPrefix(server.URL, "http") + "/ws", "jar": "broken"}}, "ws_connect")
	if wsResult.Success || wsResult.Code != "COOKIE_JAR_UNTRUSTED" || requests.Load() != 0 {
		t.Fatalf("corrupt jar did not fail closed before WebSocket I/O: result=%+v requests=%d", wsResult, requests.Load())
	}

	repaired := manager.cookieOperation(Request{Tool: "zcr521_network", Args: map[string]any{"jar": "broken", "confirmDangerous": true}}, "cookie_clear")
	if !repaired.Success {
		t.Fatalf("cookie_clear did not repair corrupt jar: %+v", repaired)
	}
	httpResult = manager.httpRequest(context.Background(), map[string]any{"url": server.URL, "jar": "broken"})
	if !httpResult.Success || requests.Load() != 1 {
		t.Fatalf("repaired jar did not restore HTTP: result=%+v requests=%d", httpResult, requests.Load())
	}
	wsResult = manager.webSocketOperation(context.Background(), Request{Tool: "zcr521_network", Args: map[string]any{"url": "ws" + strings.TrimPrefix(server.URL, "http") + "/ws", "jar": "broken"}}, "ws_connect")
	if !wsResult.Success || requests.Load() != 2 {
		t.Fatalf("repaired jar did not restore WebSocket: result=%+v requests=%d", wsResult, requests.Load())
	}
}

func TestCookieListUsesSerializedBytePagination(t *testing.T) {
	root := t.TempDir()
	manager := New(Config{WorkDir: filepath.Join(root, "work"), StateDir: filepath.Join(root, "state"), ResultPreviewBytes: 4 << 10})
	defer manager.Close()
	jar, err := manager.networkState().cookieJar("budget")
	if err != nil {
		t.Fatal(err)
	}
	jar.mu.Lock()
	for index := 0; index < 20; index++ {
		jar.records = append(jar.records, storedCookie{Origin: "https://example.test/", Name: "cookie-" + strconv.Itoa(index), Value: strings.Repeat("v", 3000), Path: "/"})
	}
	jar.mu.Unlock()
	result := manager.cookieOperation(Request{Tool: "zcr521_network", Args: map[string]any{"jar": "budget", "maxItems": float64(500), "includeValues": true, "confirmSensitive": true}}, "cookie_list")
	if !result.Success {
		t.Fatalf("cookie list failed: %+v", result)
	}
	data := result.Data.(map[string]any)
	if data["hasMore"] != true || data["nextOffset"] == nil || data["serializedBytes"].(int) > 2048 {
		t.Fatalf("cookie list ignored byte pagination: %+v", data)
	}
}

func TestCookieListClampsOffsetBeforeNativeIntConversion(t *testing.T) {
	manager := New(Config{WorkDir: t.TempDir(), StateDir: t.TempDir(), ResultPreviewBytes: 8192})
	defer manager.Close()
	result := manager.Execute(context.Background(), Request{Tool: "zcr521_network", Args: map[string]any{
		"action": "cookie_list", "jar": "default", "offset": int64(1 << 40), "maxItems": int64(10),
	}})
	if !result.Success {
		t.Fatalf("large cookie offset failed: %+v", result)
	}
	data, ok := result.Data.(map[string]any)
	if !ok || data["offset"] != 0 || data["hasMore"] != false {
		t.Fatalf("large cookie offset was not clamped: %#v", result.Data)
	}
}

func TestCookieListAdvancesPastOversizedMetadata(t *testing.T) {
	manager := New(Config{WorkDir: t.TempDir(), StateDir: t.TempDir(), ResultPreviewBytes: 4 << 10})
	defer manager.Close()
	jar, err := manager.networkState().cookieJar("metadata-budget")
	if err != nil {
		t.Fatal(err)
	}
	jar.mu.Lock()
	jar.records = []storedCookie{
		{Origin: "https://example.test/" + strings.Repeat("x", 8<<10), Name: strings.Repeat("n", 8<<10), Value: "x", Path: "/"},
		{Origin: "https://example.test/", Name: "next", Value: "x", Path: "/"},
	}
	jar.mu.Unlock()
	result := manager.cookieOperation(Request{Tool: "zcr521_network", Args: map[string]any{"jar": "metadata-budget", "maxItems": int64(1)}}, "cookie_list")
	if !result.Success {
		t.Fatal(result.Error)
	}
	data := result.Data.(map[string]any)
	if data["nextOffset"] != 1 || data["hasMore"] != true {
		t.Fatalf("oversized metadata caused pagination live-lock: %+v", data)
	}
	items := data["cookies"].([]map[string]any)
	if len(items) != 1 || items[0]["omitted"] != true {
		t.Fatalf("oversized metadata placeholder missing: %+v", items)
	}
}

func TestLegacyCharsetsDecodeForHTMLAndFeed(t *testing.T) {
	htmlServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=gbk")
		body := append([]byte("<html><body><p>"), []byte{0xd6, 0xd0, 0xce, 0xc4}...)
		body = append(body, []byte("</p></body></html>")...)
		_, _ = w.Write(body)
	}))
	defer htmlServer.Close()
	manager := testManager(t)
	defer manager.Close()
	htmlResult := manager.webExtract(context.Background(), Request{Tool: "zcr521_network", Args: map[string]any{"url": htmlServer.URL}})
	if !htmlResult.Success || !strings.Contains(htmlResult.Data.(map[string]any)["text"].(string), "中文") {
		t.Fatalf("GBK HTML was not decoded: %+v", htmlResult)
	}

	feedServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		body := []byte(`<?xml version="1.0" encoding="ISO-8859-1"?><rss><channel><title>Caf`)
		body = append(body, 0xe9)
		body = append(body, []byte(`</title><item><title>One</title></item></channel></rss>`)...)
		_, _ = w.Write(body)
	}))
	defer feedServer.Close()
	feed := manager.parseFeed(context.Background(), Request{Tool: "zcr521_network", Args: map[string]any{"url": feedServer.URL}})
	if !feed.Success || feed.Data.(map[string]any)["title"] != "Café" {
		t.Fatalf("legacy feed charset was not decoded: %+v", feed)
	}

	headerOnlyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/xml; charset=gbk")
		body := append([]byte(`<rss><channel><title>`), []byte{0xd6, 0xd0, 0xce, 0xc4}...)
		body = append(body, []byte(`</title><item><title>One</title></item></channel></rss>`)...)
		_, _ = w.Write(body)
	}))
	defer headerOnlyServer.Close()
	headerOnly := manager.parseFeed(context.Background(), Request{Tool: "zcr521_network", Args: map[string]any{"url": headerOnlyServer.URL}})
	if !headerOnly.Success || headerOnly.Data.(map[string]any)["title"] != "中文" {
		t.Fatalf("HTTP Content-Type feed charset was ignored: %+v", headerOnly)
	}
}

func TestBrowserHandlerParsingAndPNGValidation(t *testing.T) {
	if got := browserPackageFromOutput("com.android.chrome/com.google.Main\n"); got != "com.android.chrome" {
		t.Fatalf("browser package=%q", got)
	}
	path := filepath.Join(t.TempDir(), "screen.png")
	if err := os.WriteFile(path, append([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}, []byte("data")...), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validatePNGArtifact(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("not png"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validatePNGArtifact(path); err == nil {
		t.Fatal("invalid screenshot was accepted")
	}
}

func TestBrowserArtifactStemIsUniqueForConcurrentTimestamp(t *testing.T) {
	now := time.Unix(1_700_000_000, 123_000_000)
	seen := make(map[string]struct{}, 256)
	for range 256 {
		stem := browserArtifactStem(now)
		if _, exists := seen[stem]; exists {
			t.Fatalf("duplicate browser artifact stem: %s", stem)
		}
		seen[stem] = struct{}{}
	}
}

func TestHTTPAndFeedInlineResultsRespectPreviewBudget(t *testing.T) {
	large := strings.Repeat("x", 64<<10)
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		for index := 0; index < 32; index++ {
			w.Header().Set("X-Large-"+strconv.Itoa(index), strings.Repeat("h", 1024))
		}
		_, _ = w.Write([]byte(large))
	}))
	defer httpServer.Close()
	root := t.TempDir()
	manager := New(Config{WorkDir: filepath.Join(root, "work"), StateDir: filepath.Join(root, "state"), ResultPreviewBytes: 4 << 10})
	defer manager.Close()
	result := manager.httpRequest(context.Background(), map[string]any{"url": httpServer.URL})
	if !result.Success {
		t.Fatal(result.Error)
	}
	data := result.Data.(map[string]any)
	if data["truncated"] != true || len(data["body"].(string)) >= 4<<10 {
		t.Fatalf("HTTP body ignored preview budget: %+v", data)
	}

	feedServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		var builder strings.Builder
		builder.WriteString(`<?xml version="1.0"?><rss><channel><title>Budget</title>`)
		for index := 0; index < 200; index++ {
			builder.WriteString(`<item><title>Item</title><description>` + strings.Repeat("s", 4000) + `</description></item>`)
		}
		builder.WriteString(`</channel></rss>`)
		_, _ = w.Write([]byte(builder.String()))
	}))
	defer feedServer.Close()
	feed := manager.parseFeed(context.Background(), Request{Tool: "zcr521_network", Args: map[string]any{"url": feedServer.URL, "maxItems": float64(200)}})
	if !feed.Success || feed.Data.(map[string]any)["truncated"] != true {
		t.Fatalf("feed was not budget-truncated: %+v", feed)
	}
	raw, _ := json.Marshal(feed.Data)
	if len(raw) > 8<<10 {
		t.Fatalf("feed output exceeded bounded overhead: %d", len(raw))
	}
}

func TestHTTPFinalJSONBudgetAndResponseHeaderRedaction(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Auth", "must-not-leak")
		w.Header().Set("X-Session-Signature", "must-not-leak")
		w.Header().Set("X-Password", "must-not-leak")
		w.Header().Set("X-Private-Key", "must-not-leak")
		w.Header().Set("Location", "https://example.test/callback?signature=must-not-leak")
		_, _ = w.Write([]byte(strings.Repeat("\x00", 16<<10)))
	}))
	defer server.Close()
	manager := New(Config{WorkDir: t.TempDir(), StateDir: t.TempDir(), ResultPreviewBytes: 4 << 10})
	defer manager.Close()
	result := manager.httpRequest(context.Background(), map[string]any{"url": server.URL + "/" + strings.Repeat("safe-path/", 256)})
	if !result.Success {
		t.Fatal(result.Error)
	}
	data := result.Data.(map[string]any)
	raw, err := json.Marshal(data)
	if err != nil || len(raw) > 4<<10 {
		t.Fatalf("HTTP JSON exceeded preview budget: bytes=%d err=%v", len(raw), err)
	}
	if bytes := string(raw); strings.Contains(bytes, "must-not-leak") || strings.Contains(bytes, "signature=") {
		t.Fatalf("sensitive response header leaked: %s", bytes)
	}
	if data["truncated"] != true {
		t.Fatalf("escaped body truncation was not reported: %+v", data)
	}
}

func TestHTMLAndFeedFinalJSONBudgetWithEscaping(t *testing.T) {
	htmlServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		body := `<html><head><title>` + strings.Repeat(`\`, 1024) + `</title><meta name="escaped" content="` + strings.Repeat(`\`, 1024) + `"></head><body>` + strings.Repeat(`\`, 32<<10) + `</body></html>`
		_, _ = w.Write([]byte(body))
	}))
	defer htmlServer.Close()
	manager := New(Config{WorkDir: t.TempDir(), StateDir: t.TempDir(), ResultPreviewBytes: 4 << 10})
	defer manager.Close()
	htmlResult := manager.webExtract(context.Background(), Request{Tool: "zcr521_network", Args: map[string]any{"url": htmlServer.URL + "/" + strings.Repeat("long-path/", 128)}})
	if !htmlResult.Success {
		t.Fatalf("HTML extract failed: %+v", htmlResult)
	}
	htmlRaw, err := json.Marshal(htmlResult.Data)
	if err != nil || len(htmlRaw) > 4<<10 {
		t.Fatalf("HTML JSON exceeded preview budget: bytes=%d err=%v", len(htmlRaw), err)
	}

	feedServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		var builder strings.Builder
		builder.WriteString(`<rss><channel><title>` + strings.Repeat(`\`, 512) + `</title><description>` + strings.Repeat(`\`, 4096) + `</description><link>` + strings.Repeat(`\`, 4096) + `</link>`)
		for index := 0; index < 8; index++ {
			builder.WriteString(`<item><title>` + strings.Repeat(`\`, 500) + `</title><description>` + strings.Repeat(`\`, 2048) + `</description></item>`)
		}
		builder.WriteString(`</channel></rss>`)
		_, _ = w.Write([]byte(builder.String()))
	}))
	defer feedServer.Close()
	feedResult := manager.parseFeed(context.Background(), Request{Tool: "zcr521_network", Args: map[string]any{"url": feedServer.URL + "/" + strings.Repeat("long-path/", 128), "maxItems": int64(8)}})
	if !feedResult.Success {
		t.Fatalf("Feed parse failed: %+v", feedResult)
	}
	feedRaw, err := json.Marshal(feedResult.Data)
	if err != nil || len(feedRaw) > 4<<10 {
		t.Fatalf("Feed JSON exceeded preview budget: bytes=%d err=%v", len(feedRaw), err)
	}
	if feedResult.Data.(map[string]any)["truncated"] != true {
		t.Fatalf("Feed budget truncation was not reported: %+v", feedResult.Data)
	}
}

func TestFeedParseErrorIsBoundedAndDoesNotEchoCharset(t *testing.T) {
	secretLabel := strings.Repeat("secret-charset-token-", 1024)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/xml; charset="+secretLabel)
		_, _ = w.Write([]byte(`<rss><channel><title>ignored</title></channel></rss>`))
	}))
	defer server.Close()
	manager := New(Config{WorkDir: t.TempDir(), StateDir: t.TempDir(), ResultPreviewBytes: 4 << 10})
	defer manager.Close()

	result := manager.parseFeed(context.Background(), Request{Tool: "zcr521_network", Args: map[string]any{"url": server.URL}})
	if result.Success || result.Code != "PARSE_ERROR" {
		t.Fatalf("oversized charset label was not rejected safely: %+v", result)
	}
	raw, err := json.Marshal(result)
	if err != nil || len(raw) > 4<<10 {
		t.Fatalf("Feed error exceeded preview budget: bytes=%d err=%v", len(raw), err)
	}
	if strings.Contains(string(raw), "secret-charset-token") {
		t.Fatalf("Feed error echoed response-controlled charset: %s", raw)
	}
}

func TestHTTPStatusErrorDoesNotRetainReasonPhrase(t *testing.T) {
	err := httpStatusError(599)
	if err.Error() != "HTTP status 599" || len(err.Error()) > 32 {
		t.Fatalf("status error is not bounded: %q", err)
	}
}

func TestFeedStreamingEnforcesMaxItemsDuringDecode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<rss><channel><title>Bounded</title><item><title>1</title></item><item><title>2</title></item><item><title>3</title></item></channel></rss>`))
	}))
	defer server.Close()
	manager := testManager(t)
	defer manager.Close()
	result := manager.parseFeed(context.Background(), Request{Tool: "zcr521_network", Args: map[string]any{"url": server.URL, "maxItems": int64(2)}})
	if !result.Success {
		t.Fatal(result.Error)
	}
	data := result.Data.(map[string]any)
	if data["count"] != 2 || data["truncated"] != true {
		t.Fatalf("streaming item cap was not enforced: %+v", data)
	}
}

func TestWebSocketHandshakeCookiesPersistAndIdleSessionsPrune(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "wsid", Value: "persisted", Path: "/"})
		connection, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer connection.Close(websocket.StatusNormalClosure, "done")
		<-r.Context().Done()
	}))
	defer server.Close()
	root := t.TempDir()
	manager := New(Config{WorkDir: filepath.Join(root, "work"), StateDir: filepath.Join(root, "state")})
	connect := manager.webSocketOperation(context.Background(), Request{Tool: "zcr521_network", Args: map[string]any{"url": "ws" + strings.TrimPrefix(server.URL, "http"), "jar": "ws-auth"}}, "ws_connect")
	if !connect.Success {
		t.Fatalf("ws connect: %+v", connect)
	}
	id := connect.Data.(map[string]any)["connectionId"].(string)
	runtime := manager.networkState()
	runtime.wsMu.RLock()
	session := runtime.ws[id]
	runtime.wsMu.RUnlock()
	session.mu.Lock()
	session.lastActivity = time.Now().Add(-webSocketIdleTTL - time.Minute)
	session.mu.Unlock()
	staleSend := manager.webSocketOperation(context.Background(), Request{Tool: "zcr521_network", Args: map[string]any{"connectionId": id, "message": "must-not-send"}}, "ws_send")
	if staleSend.Success || staleSend.Code != "NOT_FOUND" {
		t.Fatalf("expired WebSocket session was reused: %+v", staleSend)
	}
	runtime.wsMu.RLock()
	_, exists := runtime.ws[id]
	runtime.wsMu.RUnlock()
	if exists {
		t.Fatal("idle WebSocket session was not pruned")
	}
	oversized := manager.webSocketOperation(context.Background(), Request{Tool: "zcr521_network", Args: map[string]any{"url": "ws://example.test/", "maxMessageBytes": float64(1 << 20)}}, "ws_connect")
	if oversized.Success || oversized.Code != "INVALID_ARGUMENT" {
		t.Fatalf("WebSocket inline message budget was not enforced: %+v", oversized)
	}
	manager.Close()
	restarted := New(Config{WorkDir: filepath.Join(root, "work"), StateDir: filepath.Join(root, "state")})
	defer restarted.Close()
	jar, err := restarted.networkState().cookieJar("ws-auth")
	if err != nil {
		t.Fatal(err)
	}
	if cookies := jar.Cookies(mustURL(t, server.URL)); len(cookies) != 1 || cookies[0].Value != "persisted" {
		t.Fatalf("WebSocket handshake cookie was not persisted: %+v", cookies)
	}
}

func TestWebSocketIdleTimerExpiresWithoutFollowupOperation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sessionContext, cancelSession := context.WithCancel(ctx)
	session := &webSocketSession{
		id: "idle", createdAt: time.Now().UTC(), lastActivity: time.Now().UTC(), idleTTL: 20 * time.Millisecond,
		messages: make(chan webSocketMessage, 1), done: make(chan struct{}), activity: make(chan struct{}, 1), cancel: cancelSession,
	}
	runtime := &networkRuntime{ctx: sessionContext, cancel: cancelSession, ws: map[string]*webSocketSession{"idle": session}}
	go runtime.monitorWebSocketIdle(session)
	select {
	case <-session.done:
	case <-time.After(time.Second):
		t.Fatal("idle WebSocket was not actively expired")
	}
	runtime.wsMu.RLock()
	_, exists := runtime.ws["idle"]
	runtime.wsMu.RUnlock()
	if exists {
		t.Fatal("expired WebSocket remains in registry")
	}
}

func TestWebSocketFinalMessageSurvivesPeerClose(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connection, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		_ = connection.Write(r.Context(), websocket.MessageText, []byte("final-message"))
		_ = connection.Close(websocket.StatusNormalClosure, "done")
	}))
	defer server.Close()
	manager := testManager(t)
	defer manager.Close()
	connect := manager.webSocketOperation(context.Background(), Request{Tool: "zcr521_network", Args: map[string]any{"url": "ws" + strings.TrimPrefix(server.URL, "http")}}, "ws_connect")
	if !connect.Success {
		t.Fatal(connect.Error)
	}
	id := connect.Data.(map[string]any)["connectionId"].(string)
	runtime := manager.networkState()
	runtime.wsMu.RLock()
	session := runtime.ws[id]
	runtime.wsMu.RUnlock()
	select {
	case <-session.done:
	case <-time.After(2 * time.Second):
		t.Fatal("peer close was not observed")
	}
	runtime.wsMu.RLock()
	_, retained := runtime.ws[id]
	runtime.wsMu.RUnlock()
	if !retained {
		t.Fatal("closed session with queued final message was removed")
	}
	receive := manager.webSocketOperation(context.Background(), Request{Tool: "zcr521_network", Args: map[string]any{"connectionId": id}}, "ws_receive")
	if !receive.Success {
		t.Fatalf("queued final message was lost: %+v", receive)
	}
	message := receive.Data.(map[string]any)["message"].(webSocketMessage)
	if message.Data != "final-message" {
		t.Fatalf("final message=%q", message.Data)
	}
	runtime.wsMu.RLock()
	_, retained = runtime.ws[id]
	runtime.wsMu.RUnlock()
	if retained {
		t.Fatal("drained closed session remains in registry")
	}
}

func TestWebSocketCloseContextHonorsCallerDeadline(t *testing.T) {
	blocked := make(chan struct{})
	session := &webSocketSession{
		id: "closing", done: make(chan struct{}), activity: make(chan struct{}, 1), cancel: func() {},
		lastActivity: time.Now().Add(-webSocketIdleTTL - time.Minute), idleTTL: 5 * time.Millisecond,
		closeFunc: func(websocket.StatusCode, string) error {
			<-blocked
			return nil
		},
	}
	runtime := &networkRuntime{ctx: context.Background(), ws: map[string]*webSocketSession{"closing": session}}
	go runtime.removeWebSocketWhenDone(session)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := session.closeContext(ctx, websocket.StatusNormalClosure, "test")
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > time.Second {
		t.Fatalf("close exceeded caller deadline: elapsed=%v err=%v", time.Since(started), err)
	}
	select {
	case <-session.done:
		t.Fatal("timed-out graceful close released the session before network close completed")
	default:
	}
	runtime.wsMu.RLock()
	_, retained := runtime.ws[session.id]
	runtime.wsMu.RUnlock()
	if !retained {
		t.Fatal("closing session stopped counting against the registry limit")
	}
	runtime.pruneWebSockets()
	runtime.wsMu.RLock()
	_, retained = runtime.ws[session.id]
	runtime.wsMu.RUnlock()
	if !retained {
		t.Fatal("idle pruning released a still-closing session")
	}
	monitorDone := make(chan struct{})
	go func() {
		runtime.monitorWebSocketIdle(session)
		close(monitorDone)
	}()
	select {
	case <-monitorDone:
	case <-time.After(time.Second):
		t.Fatal("idle monitor did not inspect the closing session")
	}
	runtime.wsMu.RLock()
	_, retained = runtime.ws[session.id]
	runtime.wsMu.RUnlock()
	if !retained {
		t.Fatal("idle monitor released a still-closing session")
	}
	close(blocked)
	select {
	case <-session.done:
	case <-time.After(time.Second):
		t.Fatal("completed network close did not finish the session")
	}
	deadline := time.Now().Add(time.Second)
	for {
		runtime.wsMu.RLock()
		_, retained = runtime.ws[session.id]
		runtime.wsMu.RUnlock()
		if !retained {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("completed session remains in registry")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestWebSocketReceiveJSONBudgetAndCloseBeforeNetworkUse(t *testing.T) {
	message := webSocketMessage{Type: "text", Data: strings.Repeat("\x00", 8<<10), Bytes: 8 << 10, ReceivedAt: time.Now().UTC()}
	data, truncated := boundedWebSocketReceiveData("connection", message, 4<<10)
	raw, err := json.Marshal(data)
	if err != nil || len(raw) > 4<<10 || !truncated {
		t.Fatalf("WebSocket JSON budget not enforced: bytes=%d truncated=%v err=%v", len(raw), truncated, err)
	}
	manager := New(Config{WorkDir: t.TempDir(), StateDir: t.TempDir()})
	manager.Close()
	select {
	case <-manager.networkState().ctx.Done():
	default:
		t.Fatal("Close before first network use left a live runtime")
	}
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}
