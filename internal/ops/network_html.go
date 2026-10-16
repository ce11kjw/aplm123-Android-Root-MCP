package ops

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"

	"golang.org/x/net/html"
	"golang.org/x/net/html/charset"
)

func (m *Manager) webExtract(ctx context.Context, req Request) Result {
	maxChars, err := argInt64(req.Args, min64(m.cfg.ResultPreviewBytes, 64<<10), "maxChars")
	if err != nil || maxChars < 1024 || maxChars > 256<<10 {
		return invalid("maxChars 必须在 1024..262144 之间")
	}
	maxLinks, err := argInt64(req.Args, 100, "maxItems")
	if err != nil || maxLinks < 0 || maxLinks > 1000 {
		return invalid("maxItems 必须在 0..1000 之间")
	}
	if _, exists := req.Args["maxResponseBytes"]; !exists {
		req.Args["maxResponseBytes"] = int64(2 << 20)
	}
	fetched, err := m.fetchHTTP(ctx, req.Args, 2<<20)
	if err != nil {
		return fail("NETWORK_ERROR", "网页读取失败", err, "html5_extract")
	}
	if fetched.Status < 200 || fetched.Status >= 400 {
		return fail("HTTP_ERROR", "网页返回错误状态", httpStatusError(fetched.Status), "html5_extract")
	}
	decoded, err := charset.NewReader(bytes.NewReader(fetched.Body), fetched.Headers.Get("Content-Type"))
	if err != nil {
		return fail("PARSE_ERROR", "HTML 字符集转换失败", err, "html_charset_decode")
	}
	document, err := html.Parse(decoded)
	if err != nil {
		return fail("PARSE_ERROR", "HTML 解析失败", err, "html5_extract")
	}
	base, _ := url.Parse(fetched.FinalURL)
	extracted := extractHTML(document, base, int(maxChars), int(maxLinks))
	extracted["url"] = displayHTTPURL(fetched.RequestedURL)
	extracted["finalUrl"] = displayHTTPURL(fetched.FinalURL)
	extracted["status"] = fetched.Status
	extracted["cookieJar"] = fetched.Jar
	extracted["responseTruncated"] = fetched.Truncated
	extracted["renderMode"] = "static_html"
	extracted["javascriptExecuted"] = false
	extracted = boundHTMLExtraction(extracted, int(m.cfg.ResultPreviewBytes))
	return ok("网页内容提取成功", extracted, "html5_extract")
}

func boundHTMLExtraction(source map[string]any, budget int) map[string]any {
	if budget < 4096 {
		budget = 4096
	}
	result := make(map[string]any, len(source)+2)
	for key, value := range source {
		if key != "text" && key != "links" && key != "meta" && key != "title" {
			result[key] = value
		}
	}
	result["title"] = truncateUTF8Bytes(fmt.Sprint(source["title"]), minInt(512, budget/16))
	text, _ := source["text"].(string)
	textLimit := budget / 2
	if len(text) > textLimit {
		text = truncateUTF8Bytes(text, textLimit)
		result["textTruncated"] = true
	}
	result["text"] = text

	linkBudget := budget / 4
	linkBytes := 0
	boundedLinks := make([]map[string]string, 0)
	if links, ok := source["links"].([]map[string]string); ok {
		for _, link := range links {
			candidate := map[string]string{"url": truncateUTF8Bytes(link["url"], 2048), "text": truncateUTF8Bytes(link["text"], 256)}
			raw, _ := json.Marshal(candidate)
			if linkBytes+len(raw) > linkBudget {
				result["linksTruncated"] = true
				break
			}
			boundedLinks = append(boundedLinks, candidate)
			linkBytes += len(raw)
		}
		if len(boundedLinks) < len(links) {
			result["linksTruncated"] = true
		}
	}
	result["links"] = boundedLinks

	metaBudget := budget / 8
	metaBytes := 0
	boundedMeta := make(map[string]string)
	if metadata, ok := source["meta"].(map[string]string); ok {
		keys := make([]string, 0, len(metadata))
		for key := range metadata {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			boundedKey := truncateUTF8Bytes(key, 128)
			boundedValue := truncateUTF8Bytes(metadata[key], 256)
			entryBytes := len(boundedKey) + len(boundedValue) + 8
			if metaBytes+entryBytes > metaBudget {
				result["metaTruncated"] = true
				break
			}
			boundedMeta[boundedKey] = boundedValue
			metaBytes += entryBytes
		}
	}
	result["meta"] = boundedMeta
	result["inlineLimitBytes"] = budget
	fitHTMLResultData(result, budget)
	return result
}

func fitHTMLResultData(data map[string]any, budget int) {
	if budget < 1024 || jsonEncodedSize(data) <= budget {
		return
	}
	data["truncated"] = true
	if metadata, ok := data["meta"].(map[string]string); ok {
		keys := make([]string, 0, len(metadata))
		for key := range metadata {
			keys = append(keys, key)
		}
		sort.Sort(sort.Reverse(sort.StringSlice(keys)))
		for _, key := range keys {
			if jsonEncodedSize(data) <= budget {
				break
			}
			delete(metadata, key)
			data["metaTruncated"] = true
		}
	}
	if links, ok := data["links"].([]map[string]string); ok {
		for len(links) > 0 && jsonEncodedSize(data) > budget {
			links = links[:len(links)-1]
			data["links"] = links
			data["linksTruncated"] = true
		}
	}
	if jsonEncodedSize(data) <= budget {
		return
	}
	text, _ := data["text"].(string)
	if jsonEncodedSize(data) > budget {
		data["textTruncated"] = true
		if fitJSONStringField(data, "text", text, budget) {
			return
		}
	}
	title := fmt.Sprint(data["title"])
	if jsonEncodedSize(data) > budget && fitJSONStringField(data, "title", title, budget) {
		return
	}
	if raw, ok := data["url"].(string); ok {
		data["url"] = displayHTTPAuthority(raw)
	}
	if raw, ok := data["finalUrl"].(string); ok {
		data["finalUrl"] = displayHTTPAuthority(raw)
	}
	if jsonEncodedSize(data) <= budget {
		return
	}
	status := data["status"]
	for key := range data {
		delete(data, key)
	}
	data["status"] = status
	data["text"] = ""
	data["truncated"] = true
	data["inlineLimitBytes"] = budget
}

func minInt(left, right int) int {
	if left < right {
		return left
	}
	return right
}

func extractHTML(root *html.Node, base *url.URL, maxChars, maxLinks int) map[string]any {
	var title string
	textParts := make([]string, 0)
	links := make([]map[string]string, 0)
	meta := make(map[string]string)
	var walk func(*html.Node, bool)
	walk = func(node *html.Node, suppressed bool) {
		if node.Type == html.ElementNode {
			tag := strings.ToLower(node.Data)
			if tag == "script" || tag == "style" || tag == "noscript" || tag == "svg" || tag == "template" {
				suppressed = true
			}
			if tag == "meta" {
				name := attr(node, "name")
				if name == "" {
					name = attr(node, "property")
				}
				content := strings.TrimSpace(attr(node, "content"))
				if name != "" && content != "" && len(meta) < 64 {
					meta[name] = truncateRunes(content, 1024)
				}
			}
			if tag == "a" && len(links) < maxLinks {
				href := strings.TrimSpace(attr(node, "href"))
				if href != "" {
					if parsed, err := url.Parse(href); err == nil {
						if base != nil {
							parsed = base.ResolveReference(parsed)
						}
						if parsed.Scheme == "http" || parsed.Scheme == "https" {
							links = append(links, map[string]string{"url": parsed.String(), "text": truncateRunes(nodeText(node), 300)})
						}
					}
				}
			}
		}
		if !suppressed && node.Type == html.TextNode {
			text := cleanSpace(node.Data)
			if text != "" {
				if node.Parent != nil && strings.EqualFold(node.Parent.Data, "title") && title == "" {
					title = text
				} else {
					textParts = append(textParts, text)
				}
			}
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child, suppressed)
		}
	}
	walk(root, false)
	text := cleanSpace(strings.Join(textParts, " "))
	truncated := len([]rune(text)) > maxChars
	if truncated {
		text = string([]rune(text)[:maxChars])
	}
	return map[string]any{"title": title, "text": text, "textTruncated": truncated, "links": links, "meta": meta}
}

func attr(node *html.Node, key string) string {
	for _, attribute := range node.Attr {
		if strings.EqualFold(attribute.Key, key) {
			return attribute.Val
		}
	}
	return ""
}

func nodeText(node *html.Node) string {
	parts := make([]string, 0)
	var walk func(*html.Node)
	walk = func(current *html.Node) {
		if current.Type == html.TextNode {
			parts = append(parts, current.Data)
		}
		for child := current.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(node)
	return cleanSpace(strings.Join(parts, " "))
}

func cleanSpace(value string) string {
	return strings.TrimSpace(strings.Join(strings.FieldsFunc(value, unicode.IsSpace), " "))
}

func truncateRunes(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit]) + "…"
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

type uiHierarchy struct {
	Nodes []uiNode `xml:"node"`
}

type uiNode struct {
	Text        string   `xml:"text,attr"`
	ContentDesc string   `xml:"content-desc,attr"`
	ResourceID  string   `xml:"resource-id,attr"`
	Class       string   `xml:"class,attr"`
	Bounds      string   `xml:"bounds,attr"`
	Children    []uiNode `xml:"node"`

	// Attrs 捕获完整属性表，供 aplm_ui 读取 clickable / enabled / package 等
	// 可选字段（--compressed dump 不保证这些属性存在）。
	Attrs map[string]string `xml:"-"`
}

// UnmarshalXML 在解析标准属性之外，额外把全部属性存进 Attrs。
// 这样既保持既有字段的零改动，又能让元素级自动化拿到完整的节点信息。
func (n *uiNode) UnmarshalXML(decoder *xml.Decoder, start xml.StartElement) error {
	type plain uiNode // 避免递归调用自身
	var raw plain
	raw.Attrs = make(map[string]string, len(start.Attr))
	for _, attr := range start.Attr {
		raw.Attrs[attr.Name.Local] = attr.Value
	}
	if err := decoder.DecodeElement(&raw, &start); err != nil {
		return err
	}
	*n = uiNode(raw)
	if n.Attrs == nil {
		n.Attrs = map[string]string{}
	}
	return nil
}

func (m *Manager) browserCapture(ctx context.Context, req Request) Result {
	if result := requireAndroid(); result != nil {
		return *result
	}
	rawURL, err := argString(req.Args, "url")
	if err != nil {
		return invalid(err.Error())
	}
	if _, err := parseHTTPURL(rawURL); err != nil {
		return invalid(err.Error())
	}
	user, err := m.resolveAndroidUser(ctx, req.Args, false)
	if err != nil {
		return fail("USER_RESOLUTION_FAILED", "无法确定 Android 用户", err, "android_user_probe")
	}
	browserPackage, err := m.resolveBrowserPackage(ctx, user)
	if err != nil {
		return fail("BROWSER_NOT_FOUND", "无法解析目标用户的默认浏览器", err, "android_browser_handler_resolution")
	}
	launch := m.runAndroid(ctx, commandVariant{Name: "am", Args: []string{"start", "-W", "--user", user, "-a", "android.intent.action.VIEW", "-c", "android.intent.category.BROWSABLE", "-p", browserPackage, "-d", rawURL}, Strategy: "android_browser_intent_explicit_package"})
	if err := androidSemanticError(launch); err != nil {
		return fail("BROWSER_LAUNCH_FAILED", "系统浏览器无法打开 URL", sanitizeNetworkError(err, rawURL), "android_browser_intent_explicit_package")
	}
	waitMS, err := argInt64(req.Args, 1500, "renderWaitMs")
	if err != nil || waitMS < 0 || waitMS > 30000 {
		return invalid("renderWaitMs 必须在 0..30000 之间")
	}
	if waitMS > 0 {
		timer := time.NewTimer(time.Duration(waitMS) * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fail("CANCELLED", "浏览器捕获已取消", ctx.Err(), "android_browser_capture")
		case <-timer.C:
		}
	}
	stamp := browserArtifactStem(time.Now().UTC())
	// Browser artifacts must be reachable by the dropped frontend UID. StateDir
	// is intentionally private to root, while WorkDir/output is the established
	// artifact publication boundary used by screenshots and task exports.
	dir := filepath.Join(m.cfg.WorkDir, "output", "browser")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fail("IO_ERROR", "无法创建浏览器产物目录", err, "android_browser_capture")
	}
	pngPath := filepath.Join(dir, "browser-"+stamp+".png")
	xmlPath := filepath.Join(dir, "browser-"+stamp+".xml")
	screen := m.runAndroid(ctx, commandVariant{Name: "screencap", Args: []string{"-p", pngPath}, Strategy: "screencap"})
	if !screen.Success {
		return screen
	}
	if err := validatePNGArtifact(pngPath); err != nil {
		return fail("SCREENSHOT_INVALID", "浏览器截图未生成有效 PNG", err, "screencap_verify")
	}
	dump := m.runAndroid(ctx, commandVariant{Name: "uiautomator", Args: []string{"dump", "--compressed", xmlPath}, Strategy: "uiautomator_dump"})
	visible := make([]map[string]string, 0)
	hierarchyError := ""
	if dump.Success {
		if raw, readErr := readBoundedArtifact(xmlPath, 2<<20); readErr == nil {
			var hierarchy uiHierarchy
			if xml.Unmarshal(raw, &hierarchy) == nil {
				budget := int(m.cfg.ResultPreviewBytes / 3)
				collectUINodes(hierarchy.Nodes, &visible, 500, &budget)
			} else {
				hierarchyError = "UI hierarchy XML is invalid"
			}
		} else {
			hierarchyError = readErr.Error()
			dump.Success = false
		}
	}
	artifacts := []string{pngPath}
	if dump.Success {
		artifacts = append(artifacts, xmlPath)
	}
	result := ok("浏览器已打开并完成屏幕捕获", map[string]any{"url": displayHTTPURL(rawURL), "user": user, "browserPackage": browserPackage, "renderMode": "android_system_browser", "cookieJarApplied": false, "cookieNote": "系统浏览器使用自身认证存储；MCP Cookie jar 仅用于 HTTP/HTML/Feed/WS。", "screenshot": pngPath, "hierarchy": map[bool]string{true: xmlPath, false: ""}[dump.Success], "visibleNodes": visible, "uiautomatorAvailable": dump.Success, "hierarchyError": hierarchyError}, "android_browser_screencap_uiautomator")
	result.Artifacts = artifacts
	return result
}

func browserArtifactStem(now time.Time) string {
	return now.UTC().Format("20060102-150405.000") + "-" + randomTaskID()
}

func (m *Manager) resolveBrowserPackage(ctx context.Context, user string) (string, error) {
	roleArgs := []string{"role", "get-role-holders", "--user", user, "android.app.role.BROWSER"}
	role := m.runAndroid(ctx, commandVariant{Name: "cmd", Args: roleArgs, Strategy: "cmd_role_browser"})
	if role.Success {
		if pkg := browserPackageFromOutput(role.Stdout); pkg != "" {
			return pkg, nil
		}
	}
	probeURL := "https://www.example.com/"
	base := []string{"package", "resolve-activity", "--brief", "--components", "--user", user, "-a", "android.intent.action.VIEW", "-c", "android.intent.category.BROWSABLE", probeURL}
	resolved := m.runAndroid(ctx,
		commandVariant{Name: "cmd", Args: base, Strategy: "cmd_package_browser_resolve"},
		commandVariant{Name: "pm", Args: base[1:], Strategy: "pm_browser_resolve"},
	)
	if resolved.Success {
		if pkg := browserPackageFromOutput(resolved.Stdout); pkg != "" && pkg != "android" {
			return pkg, nil
		}
	}
	return "", errors.New("default browser role and generic BROWSABLE resolver returned no concrete package")
}

func browserPackageFromOutput(output string) string {
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		for _, field := range fields {
			candidate := strings.Trim(field, "{}[](),")
			if slash := strings.Index(candidate, "/"); slash > 0 {
				candidate = candidate[:slash]
			}
			if strings.Contains(candidate, ".") && looksLikePackage(candidate) {
				return candidate
			}
		}
	}
	return ""
}

func validatePNGArtifact(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	signature := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}
	if !info.Mode().IsRegular() || info.Size() < int64(len(signature)) || info.Size() > 64<<20 {
		return fmt.Errorf("screenshot size %d is outside the bounded PNG range", info.Size())
	}
	raw := make([]byte, len(signature))
	if _, err := io.ReadFull(file, raw); err != nil {
		return err
	}
	if !bytes.Equal(raw, signature) {
		return errors.New("screenshot has no PNG signature")
	}
	return nil
}

func readBoundedArtifact(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > limit {
		return nil, fmt.Errorf("artifact size %d is outside 1..%d", info.Size(), limit)
	}
	return io.ReadAll(io.LimitReader(file, limit+1))
}

func collectUINodes(nodes []uiNode, output *[]map[string]string, limit int, budget *int) {
	for _, node := range nodes {
		if len(*output) >= limit || *budget <= 0 {
			return
		}
		if node.Text != "" || node.ContentDesc != "" {
			item := map[string]string{"text": truncateUTF8Bytes(node.Text, 512), "contentDescription": truncateUTF8Bytes(node.ContentDesc, 512), "resourceId": truncateUTF8Bytes(node.ResourceID, 256), "class": truncateUTF8Bytes(node.Class, 256), "bounds": truncateUTF8Bytes(node.Bounds, 64)}
			raw, _ := json.Marshal(item)
			if len(raw) > *budget {
				return
			}
			*output = append(*output, item)
			*budget -= len(raw)
		}
		collectUINodes(node.Children, output, limit, budget)
	}
}
