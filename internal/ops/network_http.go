package ops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

type httpFetchResult struct {
	RequestedURL string
	FinalURL     string
	Status       int
	StatusText   string
	Headers      http.Header
	Body         []byte
	Truncated    bool
	DurationMS   int64
	Jar          string
}

func parseHTTPURL(rawURL string) (*url.URL, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil {
		return nil, errors.New("url 必须是无内嵌凭据的有效 HTTP/HTTPS 地址")
	}
	return parsed, nil
}

func (m *Manager) fetchHTTP(ctx context.Context, args map[string]any, hardMax int64) (httpFetchResult, error) {
	rawURL, err := argString(args, "url")
	if err != nil {
		return httpFetchResult{}, err
	}
	if _, err := parseHTTPURL(rawURL); err != nil {
		return httpFetchResult{}, err
	}
	method, err := argOptionalString(args, http.MethodGet, "method")
	if err != nil {
		return httpFetchResult{}, err
	}
	body, err := argOptionalString(args, "", "body")
	if err != nil {
		return httpFetchResult{}, err
	}
	headers, err := argStringMap(args, "headers")
	if err != nil {
		return httpFetchResult{}, err
	}
	timeout, err := argDuration(args, 30*time.Second)
	if err != nil {
		return httpFetchResult{}, err
	}
	maxBytes, err := argInt64(args, hardMax, "maxResponseBytes")
	if err != nil || maxBytes <= 0 || maxBytes > hardMax {
		return httpFetchResult{}, fmt.Errorf("maxResponseBytes 必须在 1 到 %d 之间", hardMax)
	}
	jarName, err := argOptionalString(args, "default", "jar")
	if err != nil {
		return httpFetchResult{}, err
	}
	jar, err := m.networkState().cookieJar(jarName)
	if err != nil {
		return httpFetchResult{}, err
	}
	if err := jar.persistenceError(); err != nil {
		return httpFetchResult{}, errCookieJarUntrusted
	}
	requestContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var requestBody io.Reader
	if body != "" {
		requestBody = strings.NewReader(body)
	}
	request, err := http.NewRequestWithContext(requestContext, strings.ToUpper(method), rawURL, requestBody)
	if err != nil {
		return httpFetchResult{}, sanitizeNetworkError(err, rawURL)
	}
	request.Header.Set("User-Agent", "ZCR521-Android-AI-MCP/2")
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	started := time.Now()
	transport := boundedHTTPTransport()
	defer transport.CloseIdleConnections()
	requestJar := &requestCookieJar{state: jar}
	client := &http.Client{Timeout: timeout, Jar: requestJar, CheckRedirect: safeRedirectPolicy, Transport: transport}
	response, err := client.Do(request)
	if err != nil {
		return httpFetchResult{}, sanitizeNetworkError(err, rawURL)
	}
	defer response.Body.Close()
	content, err := io.ReadAll(io.LimitReader(response.Body, maxBytes+1))
	if err != nil {
		return httpFetchResult{}, sanitizeNetworkError(err, rawURL)
	}
	truncated := int64(len(content)) > maxBytes
	if truncated {
		content = content[:maxBytes]
	}
	if err := requestJar.persistenceError(); err != nil {
		return httpFetchResult{}, fmt.Errorf("persist cookies: %w", err)
	}
	return httpFetchResult{RequestedURL: rawURL, FinalURL: response.Request.URL.String(), Status: response.StatusCode, StatusText: response.Status, Headers: response.Header.Clone(), Body: content, Truncated: truncated, DurationMS: time.Since(started).Milliseconds(), Jar: jar.name}, nil
}

// safeRedirectPolicy rejects transport-security downgrades. net/http already
// withholds sensitive headers from unrelated hosts, but intentionally keeps
// them for the same host; without this guard an HTTPS endpoint could redirect
// Authorization or Cookie onto plaintext HTTP.
func safeRedirectPolicy(request *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("重定向次数超过 10")
	}
	if len(via) == 0 {
		return nil
	}
	if request.URL.User != nil {
		return errors.New("拒绝含内嵌凭据的重定向地址")
	}
	previous := strings.ToLower(via[len(via)-1].URL.Scheme)
	next := strings.ToLower(request.URL.Scheme)
	if (previous == "https" || previous == "wss") && (next == "http" || next == "ws") {
		return errors.New("拒绝从加密连接重定向到明文连接")
	}
	// net/http rebuilds every redirect request from the initial headers. Always
	// compare with the original origin so A -> B -> B cannot restore secrets.
	if redirectOrigin(via[0].URL) != redirectOrigin(request.URL) {
		request.Header.Del("Referer")
		for name := range request.Header {
			if !safeCrossOriginRedirectHeader(name) {
				request.Header.Del(name)
			}
		}
		originalMethod := strings.ToUpper(via[0].Method)
		if originalMethod != http.MethodGet && originalMethod != http.MethodHead || requestHasBody(via[0]) || requestHasBody(request) {
			return errors.New("拒绝跨源重定向重放请求正文")
		}
	}
	return nil
}

func redirectOrigin(value *url.URL) string {
	if value == nil {
		return ""
	}
	scheme := strings.ToLower(value.Scheme)
	host := strings.ToLower(value.Hostname())
	port := value.Port()
	if port == "" {
		switch scheme {
		case "http", "ws":
			port = "80"
		case "https", "wss":
			port = "443"
		}
	}
	return scheme + "://" + host + ":" + port
}

func safeCrossOriginRedirectHeader(name string) bool {
	switch http.CanonicalHeaderKey(name) {
	case "Accept", "Accept-Encoding", "Accept-Language", "Cache-Control", "Pragma", "Range", "User-Agent":
		return true
	default:
		return false
	}
}

func requestHasBody(request *http.Request) bool {
	if request == nil {
		return false
	}
	return request.ContentLength != 0 || request.Body != nil && request.Body != http.NoBody || request.GetBody != nil && request.Body != http.NoBody
}

func (m *Manager) httpRequest(ctx context.Context, args map[string]any) Result {
	fetched, err := m.fetchHTTP(ctx, args, 4*1024*1024)
	if err != nil {
		code := "NETWORK_ERROR"
		if errors.Is(err, errCookieJarUntrusted) {
			code = "COOKIE_JAR_UNTRUSTED"
		} else if errors.Is(err, context.DeadlineExceeded) {
			code = "TIMEOUT"
		} else if errors.Is(err, context.Canceled) {
			code = "CANCELLED"
		}
		return fail(code, "HTTP 请求失败", err, "net_http_cookie_jar")
	}
	headerBudget := int(m.cfg.ResultPreviewBytes / 4)
	headers, headersTruncated := boundedHTTPHeaders(fetched.Headers, headerBudget)
	headerRaw, _ := json.Marshal(headers)
	bodyBudget := int(m.cfg.ResultPreviewBytes) - len(headerRaw) - 2048
	if bodyBudget < 0 {
		bodyBudget = 0
	}
	body := fetched.Body
	inlineTruncated := fetched.Truncated || len(body) > bodyBudget
	if len(body) > bodyBudget {
		body = body[:bodyBudget]
	}
	data := map[string]any{"url": displayHTTPURL(fetched.RequestedURL), "finalUrl": displayHTTPURL(fetched.FinalURL), "status": fetched.Status, "statusText": truncateUTF8Bytes(fetched.StatusText, 256), "headers": headers, "headersTruncated": headersTruncated, "body": strings.ToValidUTF8(string(body), "\uFFFD"), "bodyBytesRead": len(fetched.Body), "inlineLimitBytes": m.cfg.ResultPreviewBytes, "truncated": inlineTruncated, "durationMs": fetched.DurationMS, "cookieJar": fetched.Jar}
	if fitHTTPResultData(data, int(m.cfg.ResultPreviewBytes)) {
		inlineTruncated = true
	}
	if fetched.Status < 200 || fetched.Status >= 400 {
		result := fail("HTTP_ERROR", "HTTP 服务返回错误状态", httpStatusError(fetched.Status), "net_http_cookie_jar")
		result.Data = data
		return result
	}
	message := "HTTP 请求成功"
	if inlineTruncated || headersTruncated {
		message = "HTTP 请求成功；内联结果已按 Token 预算截断，完整大文件请使用 zcr521_download"
	}
	return ok(message, data, "net_http_cookie_jar")
}

func httpStatusError(status int) error {
	return fmt.Errorf("HTTP status %d", status)
}

func sanitizedHTTPHeaders(headers http.Header) http.Header {
	copy := headers.Clone()
	for name := range copy {
		lower := strings.ToLower(name)
		redactURL := lower == "location" || lower == "content-location" || lower == "link" || lower == "refresh"
		redactUnlisted := !safeResponseHeader(name)
		if redactURL || redactUnlisted {
			count := len(copy.Values(name))
			copy.Del(name)
			reason := "non-allowlisted header"
			if redactURL {
				reason = "URL-bearing header"
			}
			copy.Set(name, fmt.Sprintf("[redacted %s: %d value(s)]", reason, count))
		}
	}
	return copy
}

// safeResponseHeader deliberately uses an allowlist. Credential-bearing
// response headers are not standardized, so a denylist inevitably misses
// names such as X-Password or X-Private-Key. These fields contain transport or
// representation metadata and are sufficient for HTTP diagnostics.
func safeResponseHeader(name string) bool {
	switch http.CanonicalHeaderKey(name) {
	case "Accept-Ranges", "Age", "Allow", "Cache-Control", "Content-Encoding", "Content-Language", "Content-Length", "Content-Range", "Content-Type", "Date", "Expires", "Last-Modified", "Retry-After", "Vary":
		return true
	default:
		return false
	}
}

func boundedHTTPHeaders(headers http.Header, limit int) (http.Header, bool) {
	if limit < 512 {
		limit = 512
	}
	safe := sanitizedHTTPHeaders(headers)
	keys := make([]string, 0, len(safe))
	for key := range safe {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	bounded := make(http.Header)
	truncated := false
	for _, key := range keys {
		values := safe.Values(key)
		for index := range values {
			if len(values[index]) > 512 {
				values[index] = truncateUTF8Bytes(values[index], 512)
				truncated = true
			}
		}
		bounded[key] = values
		raw, _ := json.Marshal(bounded)
		if len(raw) > limit {
			delete(bounded, key)
			truncated = true
			break
		}
	}
	return bounded, truncated || len(bounded) < len(safe)
}

func boundedHTTPTransport() *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxResponseHeaderBytes = 1 << 20
	return transport
}

func displayHTTPURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "[invalid]"
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.ForceQuery = false
	parsed.Fragment = ""
	parsed.RawFragment = ""
	return truncateUTF8Bytes(parsed.String(), 512)
}

func displayHTTPAuthority(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "[invalid]"
	}
	return truncateUTF8Bytes(strings.ToLower(parsed.Scheme)+"://"+parsed.Host, 512)
}

func sanitizeNetworkError(err error, rawURL string) error {
	if err == nil {
		return nil
	}
	safeURL := displayHTTPAuthority(rawURL)
	if strings.HasPrefix(strings.ToLower(rawURL), "ws://") || strings.HasPrefix(strings.ToLower(rawURL), "wss://") {
		safeURL = displayWebSocketURL(rawURL)
	}
	var urlError *url.Error
	if errors.As(err, &urlError) {
		copy := *urlError
		copy.Op = "request"
		copy.URL = truncateUTF8Bytes(safeURL, 256)
		if urlError.Err != nil {
			copy.Err = &sanitizedNetworkCause{message: safeNetworkCauseMessage(urlError.Err), cause: urlError.Err}
		}
		return &copy
	}
	return &sanitizedNetworkCause{message: safeNetworkCauseMessage(err), cause: err}
}

func safeNetworkCauseMessage(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "network operation timed out"
	case errors.Is(err, context.Canceled):
		return "network operation cancelled"
	default:
		// Network libraries may include redirect targets, response headers or
		// request URLs in Error(). Keep the cause only for errors.Is/errors.As;
		// never expose its uncontrolled text to MCP clients.
		return "network operation failed"
	}
}

type sanitizedNetworkCause struct {
	message string
	cause   error
}

func (e *sanitizedNetworkCause) Error() string { return e.message }
func (e *sanitizedNetworkCause) Unwrap() error { return e.cause }

func fitHTTPResultData(data map[string]any, budget int) bool {
	if budget < 1024 || jsonEncodedSize(data) <= budget {
		return false
	}
	body, _ := data["body"].(string)
	data["truncated"] = true
	if fitJSONStringField(data, "body", body, budget) {
		return true
	}
	data["headers"] = http.Header{}
	data["headersTruncated"] = true
	if fitJSONStringField(data, "body", body, budget) {
		return true
	}
	if raw, ok := data["url"].(string); ok {
		data["url"] = displayHTTPAuthority(raw)
	}
	if raw, ok := data["finalUrl"].(string); ok {
		data["finalUrl"] = displayHTTPAuthority(raw)
	}
	data["statusText"] = truncateUTF8Bytes(fmt.Sprint(data["statusText"]), 128)
	if fitJSONStringField(data, "body", body, budget) {
		return true
	}
	status := data["status"]
	for key := range data {
		delete(data, key)
	}
	data["status"] = status
	data["body"] = ""
	data["truncated"] = true
	data["inlineLimitBytes"] = budget
	return true
}

func fitJSONStringField(data map[string]any, key, value string, budget int) bool {
	data[key] = ""
	if jsonEncodedSize(data) > budget {
		return false
	}
	value = strings.ToValidUTF8(value, "\uFFFD")
	low, high := 0, len(value)
	best := ""
	for low <= high {
		middle := low + (high-low)/2
		candidate := truncateUTF8Bytes(value, middle)
		data[key] = candidate
		if jsonEncodedSize(data) <= budget {
			best = candidate
			low = middle + 1
		} else {
			high = middle - 1
		}
	}
	data[key] = best
	return true
}

func jsonEncodedSize(value any) int {
	raw, err := json.Marshal(value)
	if err != nil {
		return int(^uint(0) >> 1)
	}
	return len(raw)
}
