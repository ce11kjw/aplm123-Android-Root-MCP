package ops

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zcr521/android-ai-mcp/internal/atomicfile"
	"golang.org/x/net/publicsuffix"
)

var jarNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

const (
	maxStoredCookieBytes = 4096
	maxCookiesPerJar     = 1024
	maxCookieJarBytes    = 2 << 20
	maxActiveCookieJars  = 64
)

var errCookieJarUntrusted = errors.New("cookie jar persistent state is untrusted")

type storedCookie struct {
	Origin   string    `json:"origin"`
	Name     string    `json:"name"`
	Value    string    `json:"value"`
	Path     string    `json:"path,omitempty"`
	Domain   string    `json:"domain,omitempty"`
	Expires  time.Time `json:"expires,omitempty"`
	MaxAge   int       `json:"maxAge,omitempty"`
	Secure   bool      `json:"secure,omitempty"`
	HTTPOnly bool      `json:"httpOnly,omitempty"`
	SameSite int       `json:"sameSite,omitempty"`
}

type cookieJarState struct {
	mu      sync.Mutex
	name    string
	path    string
	jar     *cookiejar.Jar
	records []storedCookie
	lastErr string
}

// requestCookieJar scopes Set-Cookie persistence failures to the request that
// caused them. Invalid server cookies are ignored like net/http/cookiejar and
// cannot poison unrelated later requests through shared sticky error state.
type requestCookieJar struct {
	state *cookieJarState
	mu    sync.Mutex
	err   error
}

func (j *requestCookieJar) Cookies(origin *url.URL) []*http.Cookie {
	return j.state.Cookies(origin)
}

func (j *requestCookieJar) SetCookies(origin *url.URL, cookies []*http.Cookie) {
	if err := j.state.applyCookies(origin, cookies, false); err != nil {
		j.mu.Lock()
		if j.err == nil {
			j.err = err
		}
		j.mu.Unlock()
	}
}

func (j *requestCookieJar) persistenceError() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.err
}

func (n *networkRuntime) cookieJar(name string) (*cookieJarState, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "default"
	}
	if !jarNamePattern.MatchString(name) {
		return nil, errors.New("jar 必须是 1..64 位字母、数字、点、下划线或连字符")
	}
	n.jarsMu.Lock()
	defer n.jarsMu.Unlock()
	if state := n.jars[name]; state != nil {
		return state, nil
	}
	if len(n.jars) >= maxActiveCookieJars {
		return nil, fmt.Errorf("active cookie jar limit reached (%d)", maxActiveCookieJars)
	}
	jar, err := cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
	if err != nil {
		return nil, err
	}
	state := &cookieJarState{name: name, path: filepath.Join(n.manager.cfg.StateDir, "network", "cookie-jars", name+".json"), jar: jar}
	state.load()
	n.jars[name] = state
	return state, nil
}

func (s *cookieJarState) load() {
	if info, err := os.Stat(s.path); err == nil && info.Size() > maxCookieJarBytes {
		s.lastErr = "cookie jar exceeds the 2 MiB storage limit"
		return
	}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if !os.IsNotExist(err) {
			s.lastErr = err.Error()
		}
		return
	}
	var loaded []storedCookie
	if err := json.Unmarshal(raw, &loaded); err != nil {
		s.records = nil
		s.lastErr = "cookie jar JSON is corrupt: " + err.Error()
		return
	}
	s.records = loaded
	originalCount := len(loaded)
	if len(s.records) > maxCookiesPerJar {
		s.records = nil
		s.lastErr = "cookie jar exceeds the 1024-cookie limit"
		return
	}
	now := time.Now()
	kept := s.records[:0]
	changed := false
	for _, record := range s.records {
		if storedCookieBytes(record) > maxStoredCookieBytes {
			changed = true
			continue
		}
		if !record.Expires.IsZero() && record.Expires.Before(now) {
			changed = true
			continue
		}
		origin, err := url.Parse(record.Origin)
		cookie := record.httpCookie()
		if err != nil || origin.Host == "" || validateCookieForOrigin(origin, cookie) != nil {
			changed = true
			continue
		}
		normalizedOrigin := canonicalCookieOrigin(origin)
		if record.Origin != normalizedOrigin {
			record.Origin = normalizedOrigin
			changed = true
		}
		s.jar.SetCookies(origin, []*http.Cookie{cookie})
		kept = append(kept, record)
	}
	s.records = kept
	if len(kept) != originalCount {
		changed = true
	}
	if changed {
		sorted, err := persistCookieRecords(s.path, s.records)
		if err != nil {
			s.lastErr = "cookie jar migration failed: " + err.Error()
		} else {
			s.records = sorted
		}
	}
}

func (s *cookieJarState) Cookies(origin *url.URL) []*http.Cookie {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.jar.Cookies(origin)
}

func (s *cookieJarState) SetCookies(origin *url.URL, cookies []*http.Cookie) {
	err := s.applyCookies(origin, cookies, true)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.lastErr = err.Error()
	} else {
		s.lastErr = ""
	}
}

func (s *cookieJarState) applyCookies(origin *url.URL, cookies []*http.Cookie, strict bool) error {
	if len(cookies) > maxCookiesPerJar {
		return errors.New("Set-Cookie batch exceeds the 1024-cookie limit")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	prospective := append([]storedCookie(nil), s.records...)
	accepted := 0
	for _, cookie := range cookies {
		if err := validateCookieForOrigin(origin, cookie); err != nil {
			if strict {
				return err
			}
			continue
		}
		accepted++
		record := cookieRecord(origin, cookie)
		if storedCookieBytes(record) > maxStoredCookieBytes {
			return errors.New("cookie exceeds the 4096-byte storage limit")
		}
		prospective = upsertCookieRecord(prospective, record)
		if len(prospective) > maxCookiesPerJar {
			return errors.New("cookie jar exceeds the 1024-cookie limit")
		}
	}
	if accepted == 0 {
		return nil
	}
	if len(prospective) > maxCookiesPerJar {
		return errors.New("cookie jar exceeds the 1024-cookie limit")
	}
	preview, err := json.MarshalIndent(prospective, "", "  ")
	if err != nil {
		return err
	}
	if len(preview) > maxCookieJarBytes {
		return errors.New("cookie jar exceeds the 2 MiB storage limit")
	}
	replacement, _ := cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
	for _, record := range prospective {
		recordOrigin, err := url.Parse(record.Origin)
		if err != nil {
			return errors.New("cookie jar contains an invalid origin")
		}
		replacement.SetCookies(recordOrigin, []*http.Cookie{record.httpCookie()})
	}
	sorted, err := persistCookieRecords(s.path, prospective)
	if err != nil {
		return err
	}
	s.jar = replacement
	s.records = sorted
	return nil
}

func (s *cookieJarState) persistenceError() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastErr == "" {
		return nil
	}
	return errors.New(s.lastErr)
}

func (r storedCookie) httpCookie() *http.Cookie {
	return &http.Cookie{Name: r.Name, Value: r.Value, Path: r.Path, Domain: r.Domain, Expires: r.Expires, MaxAge: r.MaxAge, Secure: r.Secure, HttpOnly: r.HTTPOnly, SameSite: http.SameSite(r.SameSite)}
}

func (s *cookieJarState) set(origin *url.URL, cookie *http.Cookie) error {
	err := s.applyCookies(origin, []*http.Cookie{cookie}, true)
	s.mu.Lock()
	if err != nil {
		s.lastErr = err.Error()
	} else {
		s.lastErr = ""
	}
	s.mu.Unlock()
	return err
}

func cookieRecord(origin *url.URL, cookie *http.Cookie) storedCookie {
	path := cookie.Path
	if path == "" || !strings.HasPrefix(path, "/") {
		path = defaultCookiePath(origin.EscapedPath())
	}
	expires := cookie.Expires
	maxAge := cookie.MaxAge
	if maxAge > 0 {
		expires = time.Now().Add(time.Duration(maxAge) * time.Second)
		maxAge = 0
	}
	return storedCookie{Origin: canonicalCookieOrigin(origin), Name: cookie.Name, Value: cookie.Value, Path: path, Domain: cookie.Domain, Expires: expires, MaxAge: maxAge, Secure: cookie.Secure, HTTPOnly: cookie.HttpOnly, SameSite: int(cookie.SameSite)}
}

func canonicalCookieOrigin(origin *url.URL) string {
	if origin == nil {
		return ""
	}
	clean := *origin
	clean.User = nil
	clean.Opaque = ""
	clean.Path = "/"
	clean.RawPath = ""
	clean.RawQuery = ""
	clean.ForceQuery = false
	clean.Fragment = ""
	clean.RawFragment = ""
	return clean.String()
}

func defaultCookiePath(requestPath string) string {
	if requestPath == "" || requestPath[0] != '/' {
		return "/"
	}
	last := strings.LastIndex(requestPath, "/")
	if last <= 0 {
		return "/"
	}
	return requestPath[:last]
}

func upsertCookieRecord(records []storedCookie, candidate storedCookie) []storedCookie {
	deleteCookie := candidate.MaxAge < 0 || (!candidate.Expires.IsZero() && candidate.Expires.Before(time.Now()))
	filtered := records[:0]
	for _, existing := range records {
		if sameCookie(existing, candidate) {
			continue
		}
		filtered = append(filtered, existing)
	}
	if !deleteCookie {
		filtered = append(filtered, candidate)
	}
	return filtered
}

func storedCookieBytes(record storedCookie) int {
	return len(record.Name) + len(record.Value) + len(record.Path) + len(record.Domain) + len(record.Origin) + 128
}

func sameCookie(a, b storedCookie) bool {
	return a.Name == b.Name && strings.EqualFold(cookieDomain(a), cookieDomain(b)) && a.Path == b.Path
}

func cookieDomain(record storedCookie) string {
	if record.Domain != "" {
		return strings.TrimPrefix(strings.ToLower(record.Domain), ".")
	}
	origin, err := url.Parse(record.Origin)
	if err != nil {
		return ""
	}
	return strings.ToLower(origin.Hostname())
}

func persistCookieRecords(path string, records []storedCookie) ([]storedCookie, error) {
	sorted := append([]storedCookie(nil), records...)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Origin+sorted[i].Domain+sorted[i].Path+sorted[i].Name < sorted[j].Origin+sorted[j].Domain+sorted[j].Path+sorted[j].Name
	})
	raw, err := json.MarshalIndent(sorted, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := atomicfile.Write(path, raw, 0o600); err != nil {
		return nil, err
	}
	return sorted, nil
}

func validateCookieForOrigin(origin *url.URL, cookie *http.Cookie) error {
	if origin == nil || origin.Host == "" || origin.User != nil || (origin.Scheme != "http" && origin.Scheme != "https") {
		return errors.New("cookie origin must be HTTP or HTTPS")
	}
	if cookie == nil {
		return errors.New("cookie is nil")
	}
	if err := cookie.Valid(); err != nil {
		return fmt.Errorf("invalid cookie: %w", err)
	}
	probeCookie := *cookie
	if probeCookie.Path == "" || !strings.HasPrefix(probeCookie.Path, "/") {
		probeCookie.Path = defaultCookiePath(origin.EscapedPath())
	}
	// Deletion and expired cookies must still prove that their Domain belongs
	// to the response origin. Validate a non-deleting copy in a temporary jar.
	probeCookie.MaxAge = 0
	probeCookie.Expires = time.Time{}
	probeJar, _ := cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
	probeJar.SetCookies(origin, []*http.Cookie{&probeCookie})
	probeURL := *origin
	if probeCookie.Secure {
		probeURL.Scheme = "https"
	}
	probeURL.Path = probeCookie.Path
	probeURL.RawPath = ""
	probeURL.RawQuery = ""
	probeURL.Fragment = ""
	for _, candidate := range probeJar.Cookies(&probeURL) {
		if candidate.Name == probeCookie.Name && candidate.Value == probeCookie.Value {
			return nil
		}
	}
	return errors.New("cookie Domain is not valid for the origin or is a public suffix")
}

func (m *Manager) cookieOperation(req Request, action string) Result {
	jarName, err := argOptionalString(req.Args, "default", "jar")
	if err != nil {
		return invalid(err.Error())
	}
	state, err := m.networkState().cookieJar(jarName)
	if err != nil {
		return invalid(err.Error())
	}
	switch action {
	case "cookie_list":
		includeValues, _ := argBool(req.Args, "includeValues", false)
		confirmed, _ := argBool(req.Args, "confirmSensitive", false)
		if includeValues && !confirmed {
			return fail("CONFIRMATION_REQUIRED", "显示 Cookie 值需要 confirmSensitive=true", errors.New("sensitive cookie values"), "cookie_privacy")
		}
		limit, parseErr := argInt64(req.Args, 100, "maxItems")
		if parseErr != nil || limit < 1 || limit > 500 {
			return invalid("maxItems 必须在 1..500 之间")
		}
		offset, parseErr := argInt64(req.Args, 0, "offset")
		if parseErr != nil || offset < 0 {
			return invalid("offset 必须大于等于 0")
		}
		state.mu.Lock()
		state.pruneExpiredLocked()
		total := len(state.records)
		start := 0
		if offset >= int64(total) {
			start = total
		} else {
			start = int(offset)
		}
		budget := int(m.cfg.ResultPreviewBytes) - 2048
		if budget < 1024 {
			budget = 1024
		}
		end := start
		used := 0
		items := make([]map[string]any, 0, minInt(int(limit), total-start))
		for end < total && end-start < int(limit) {
			record := state.records[end]
			value := "[hidden]"
			if includeValues {
				value = record.Value
			}
			item := map[string]any{"origin": displayHTTPAuthority(record.Origin), "name": record.Name, "value": value, "valueBytes": len(record.Value), "path": record.Path, "domain": record.Domain, "expires": record.Expires, "secure": record.Secure, "httpOnly": record.HTTPOnly}
			raw, _ := json.Marshal(item)
			if used+len(raw) > budget {
				if len(items) == 0 {
					item = map[string]any{
						"name":    truncateUTF8Bytes(record.Name, 128),
						"omitted": true,
						"reason":  "cookie metadata exceeds inline byte budget",
					}
					raw, _ = json.Marshal(item)
					items = append(items, item)
					used += len(raw)
					end++
				}
				break
			}
			items = append(items, item)
			used += len(raw)
			end++
		}
		loadError := state.lastErr
		state.mu.Unlock()
		var nextOffset any
		if end < total {
			nextOffset = end
		}
		return ok("Cookie jar 读取成功", map[string]any{"jar": state.name, "cookies": items, "valuesIncluded": includeValues, "total": total, "offset": start, "limit": limit, "nextOffset": nextOffset, "hasMore": end < total, "serializedBytes": used, "inlineLimitBytes": m.cfg.ResultPreviewBytes, "loadError": loadError}, "persistent_cookie_jar")
	case "cookie_set":
		rawURL, parseErr := argString(req.Args, "url")
		if parseErr != nil {
			return invalid(parseErr.Error())
		}
		origin, parseErr := parseHTTPURL(rawURL)
		if parseErr != nil {
			return invalid(parseErr.Error())
		}
		name, parseErr := argString(req.Args, "name")
		if parseErr != nil || !validCookieName(name) {
			return invalid("name 必须是合法 Cookie 名")
		}
		rawValue, exists := req.Args["value"]
		if !exists {
			return invalid("value 不能为空")
		}
		var value string
		switch typed := rawValue.(type) {
		case string:
			value = typed
		case bool:
			value = strconv.FormatBool(typed)
		case float64:
			value = strconv.FormatFloat(typed, 'f', -1, 64)
		case json.Number:
			value = typed.String()
		default:
			return invalid("value 必须是字符串、数字或布尔值")
		}
		path, _ := argOptionalString(req.Args, "/", "cookiePath")
		if !strings.HasPrefix(path, "/") {
			return invalid("cookiePath 必须以 / 开头")
		}
		domain, _ := argOptionalString(req.Args, "", "domain")
		secure, parseErr := argBool(req.Args, "secure", origin.Scheme == "https")
		if parseErr != nil {
			return invalid(parseErr.Error())
		}
		httpOnly, parseErr := argBool(req.Args, "httpOnly", true)
		if parseErr != nil {
			return invalid(parseErr.Error())
		}
		maxAge, parseErr := argInt64(req.Args, 0, "maxAge")
		if parseErr != nil || maxAge < 0 || maxAge > 31536000 {
			return invalid("maxAge 必须在 0..31536000 秒")
		}
		cookie := &http.Cookie{Name: name, Value: value, Path: path, Domain: domain, Secure: secure, HttpOnly: httpOnly, MaxAge: int(maxAge)}
		if maxAge > 0 {
			cookie.Expires = time.Now().Add(time.Duration(maxAge) * time.Second)
		}
		if err := state.set(origin, cookie); err != nil {
			return fail("COOKIE_WRITE_FAILED", "Cookie 持久化失败", err, "persistent_cookie_jar")
		}
		return ok("Cookie 已写入并持久化", map[string]any{"jar": state.name, "name": name, "valueBytes": len(value)}, "persistent_cookie_jar")
	case "cookie_delete":
		rawURL, parseErr := argString(req.Args, "url")
		if parseErr != nil {
			return invalid(parseErr.Error())
		}
		origin, parseErr := parseHTTPURL(rawURL)
		if parseErr != nil {
			return invalid(parseErr.Error())
		}
		name, parseErr := argString(req.Args, "name")
		if parseErr != nil {
			return invalid(parseErr.Error())
		}
		path, _ := argOptionalString(req.Args, "/", "cookiePath")
		if !strings.HasPrefix(path, "/") {
			return invalid("cookiePath 必须以 / 开头")
		}
		domain, _ := argOptionalString(req.Args, "", "domain")
		if err := state.set(origin, &http.Cookie{Name: name, Path: path, Domain: domain, MaxAge: -1}); err != nil {
			return fail("COOKIE_WRITE_FAILED", "Cookie 删除持久化失败", err, "persistent_cookie_jar")
		}
		return ok("Cookie 已删除", map[string]any{"jar": state.name, "name": name}, "persistent_cookie_jar")
	case "cookie_clear":
		confirmed, _ := argBool(req.Args, "confirmDangerous", false)
		if !confirmed {
			return fail("CONFIRMATION_REQUIRED", "清空 Cookie jar 需要 confirmDangerous=true", errors.New("confirmation required"), "cookie_privacy")
		}
		state.mu.Lock()
		count := len(state.records)
		_, err := persistCookieRecords(state.path, nil)
		if err == nil {
			state.records = nil
			state.jar, _ = cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
			state.lastErr = ""
		} else {
			state.lastErr = err.Error()
		}
		state.mu.Unlock()
		if err != nil {
			return fail("COOKIE_WRITE_FAILED", "Cookie jar 清空失败", err, "persistent_cookie_jar")
		}
		return ok("Cookie jar 已清空", map[string]any{"jar": state.name, "removed": count}, "persistent_cookie_jar")
	default:
		return invalidAction(req.Tool, action, "cookie_clear", "cookie_delete", "cookie_list", "cookie_set")
	}
}

func (s *cookieJarState) pruneExpiredLocked() {
	now := time.Now()
	kept := s.records[:0]
	for _, record := range s.records {
		if !record.Expires.IsZero() && !record.Expires.After(now) {
			continue
		}
		kept = append(kept, record)
	}
	s.records = kept
}

func validCookieName(name string) bool {
	if name == "" || len(name) > 256 {
		return false
	}
	for _, r := range name {
		if r <= 0x20 || r >= 0x7f || strings.ContainsRune("()<>@,;:\\\"/[]?={}\t", r) {
			return false
		}
	}
	return true
}
