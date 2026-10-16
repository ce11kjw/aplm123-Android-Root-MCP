package ops

import (
	"context"
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// aplm_ui —— 基于无障碍层级的元素级自动化。
//
// 设计目标：把「先截图、肉眼算坐标、再 input tap」这条脆弱链路，
// 换成「按 text / resourceId / class 定位元素、由节点 bounds 反算坐标」。
//
// 与既有能力的关系：
//   - 层级解析复用 network_html.go 的 uiHierarchy / uiNode（uiautomator dump）。
//   - 底层动作仍走 /system/bin/input（aplm_input 已封装的同一批命令）。
//   - 坐标由 bounds 解算，因此换分辨率、换主题、系统更新都不受影响。
const (
	uiDefaultTimeout    = 10 * time.Second
	uiDefaultPollMs     = 300
	uiMaxNodes          = 2000
	uiMaxWaitMs         = 120000
	uiDefaultSwipes     = 8
	uiMaxSwipes         = 50
	uiMaxTextFieldBytes = 4 << 10
)

// uiSelector 是元素定位条件。多个字段之间是 AND 关系。
type uiSelector struct {
	Text         string `json:"text,omitempty"`
	TextContains string `json:"textContains,omitempty"`
	ContentDesc  string `json:"contentDescription,omitempty"`
	ResourceID   string `json:"resourceId,omitempty"`
	Class        string `json:"class,omitempty"`
	Package      string `json:"package,omitempty"`
	Index        int    `json:"index,omitempty"`
	Clickable    bool   `json:"clickable,omitempty"`
	HasIndex     bool   `json:"-"`
}

func (s uiSelector) empty() bool {
	return s.Text == "" && s.TextContains == "" && s.ContentDesc == "" &&
		s.ResourceID == "" && s.Class == "" && s.Package == "" && !s.HasIndex
}

func (s uiSelector) describe() string {
	parts := []string{}
	if s.Text != "" {
		parts = append(parts, "text="+s.Text)
	}
	if s.TextContains != "" {
		parts = append(parts, "textContains="+s.TextContains)
	}
	if s.ContentDesc != "" {
		parts = append(parts, "contentDescription="+s.ContentDesc)
	}
	if s.ResourceID != "" {
		parts = append(parts, "resourceId="+s.ResourceID)
	}
	if s.Class != "" {
		parts = append(parts, "class="+s.Class)
	}
	if s.Package != "" {
		parts = append(parts, "package="+s.Package)
	}
	if s.HasIndex {
		parts = append(parts, fmt.Sprintf("index=%d", s.Index))
	}
	if len(parts) == 0 {
		return "(未指定定位条件)"
	}
	return strings.Join(parts, " ")
}

// parseUISelector 从 args 读取定位条件。
// 兼容简写：selector{...} 对象，或直接把 text/resourceId 平铺在 args 顶层。
func parseUISelector(args map[string]any) (uiSelector, error) {
	var sel uiSelector

	if nested, ok := args["selector"]; ok && nested != nil {
		obj, isObj := nested.(map[string]any)
		if !isObj {
			return sel, fmt.Errorf("selector 必须是对象")
		}
		args = obj
	}

	if v, err := argOptionalString(args, "", "text"); err != nil {
		return sel, err
	} else {
		sel.Text = v
	}
	if v, err := argOptionalString(args, "", "textContains"); err != nil {
		return sel, err
	} else {
		sel.TextContains = v
	}
	if v, err := argOptionalString(args, "", "contentDescription", "contentDesc"); err != nil {
		return sel, err
	} else {
		sel.ContentDesc = v
	}
	if v, err := argOptionalString(args, "", "resourceId", "id"); err != nil {
		return sel, err
	} else {
		sel.ResourceID = v
	}
	if v, err := argOptionalString(args, "", "className", "class"); err != nil {
		return sel, err
	} else {
		sel.Class = v
	}
	if v, err := argOptionalString(args, "", "package", "packageName"); err != nil {
		return sel, err
	} else {
		sel.Package = v
	}
	if _, exists := args["index"]; exists {
		idx, err := argInt64(args, 0, "index")
		if err != nil {
			return sel, err
		}
		if idx < 0 {
			return sel, fmt.Errorf("index 必须是非负整数")
		}
		sel.Index = int(idx)
		sel.HasIndex = true
	}
	if v, err := argBool(args, "clickable", false); err != nil {
		return sel, err
	} else {
		sel.Clickable = v
	}
	if sel.empty() {
		return sel, fmt.Errorf("必须提供至少一个定位条件（text / textContains / contentDescription / resourceId / class / package / index）")
	}
	if sel.Text != "" && sel.TextContains != "" {
		return sel, fmt.Errorf("text 与 textContains 不能同时使用")
	}
	for name, value := range map[string]string{
		"text": sel.Text, "textContains": sel.TextContains,
		"contentDescription": sel.ContentDesc, "resourceId": sel.ResourceID,
		"class": sel.Class, "package": sel.Package,
	} {
		if len(value) > 256 {
			return sel, fmt.Errorf("%s 超过 256 字符上限", name)
		}
	}
	return sel, nil
}

// matchedNode 是命中元素及其解算出的屏幕坐标。
type matchedNode struct {
	Text        string `json:"text,omitempty"`
	ContentDesc string `json:"contentDescription,omitempty"`
	ResourceID  string `json:"resourceId,omitempty"`
	Class       string `json:"class,omitempty"`
	Package     string `json:"package,omitempty"`
	Bounds      string `json:"bounds"`
	CenterX     int    `json:"centerX"`
	CenterY     int    `json:"centerY"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
	Clickable   bool   `json:"clickable"`
	Enabled     bool   `json:"enabled"`
}

var uiBoundsPattern = regexp.MustCompile(`^\[(\d+),(\d+)\]\[(\d+),(\d+)\]$`)

// parseUIBounds 解析 uiautomator 的 bounds 字符串 "[x1,y1][x2,y2]"。
func parseUIBounds(raw string) (x1, y1, x2, y2 int, ok bool) {
	m := uiBoundsPattern.FindStringSubmatch(strings.TrimSpace(raw))
	if m == nil {
		return 0, 0, 0, 0, false
	}
	vals := make([]int, 4)
	for i := 0; i < 4; i++ {
		n, err := strconv.Atoi(m[i+1])
		if err != nil {
			return 0, 0, 0, 0, false
		}
		vals[i] = n
	}
	if vals[2] < vals[0] || vals[3] < vals[1] {
		return 0, 0, 0, 0, false
	}
	return vals[0], vals[1], vals[2], vals[3], true
}

// nodeMatches 判断节点是否满足定位条件。
func nodeMatches(node uiNode, sel uiSelector) bool {
	if sel.Text != "" && node.Text != sel.Text {
		return false
	}
	if sel.TextContains != "" && !strings.Contains(node.Text, sel.TextContains) {
		return false
	}
	if sel.ContentDesc != "" && node.ContentDesc != sel.ContentDesc {
		return false
	}
	if sel.ResourceID != "" && node.ResourceID != sel.ResourceID {
		return false
	}
	if sel.Class != "" {
		// 支持简写：android.widget.Button 也可写作 Button
		if node.Class != sel.Class && !strings.HasSuffix(node.Class, "."+sel.Class) {
			return false
		}
	}
	if sel.Package != "" && !strings.Contains(node.ResourceID, sel.Package) {
		return false
	}
	return true
}

// walkUINodes 深度优先遍历，把匹配的节点收集起来（带上坐标）。
func walkUINodes(nodes []uiNode, sel uiSelector, out *[]matchedNode, limit int) {
	for _, node := range nodes {
		if len(*out) >= limit {
			return
		}
		clickable, enabled, hasFlags := parseUIExtra(node)
		candidate := node
		if sel.Clickable && hasFlags && !clickable {
			// 仅当节点自身声明了 clickable=false 时跳过；
			// 未声明 clickable 的容器仍参与匹配（例如含文字的 FrameLayout）。
			goto children
		}
		if nodeMatches(candidate, sel) {
			x1, y1, x2, y2, ok := parseUIBounds(node.Bounds)
			if ok {
				item := matchedNode{
					Text:        truncateUTF8Bytes(node.Text, 512),
					ContentDesc: truncateUTF8Bytes(node.ContentDesc, 512),
					ResourceID:  truncateUTF8Bytes(node.ResourceID, 256),
					Class:       truncateUTF8Bytes(node.Class, 256),
					Bounds:      node.Bounds,
					CenterX:     (x1 + x2) / 2,
					CenterY:     (y1 + y2) / 2,
					Width:       x2 - x1,
					Height:      y2 - y1,
					Clickable:   clickable,
					Enabled:     enabled,
				}
				if !hasFlags {
					item.Clickable = true // 未声明时按可点击处理，交由上层决定
					item.Enabled = true
				}
				*out = append(*out, item)
			}
		}
	children:
		walkUINodes(node.Children, sel, out, limit)
	}
}

// parseUIExtra 读取 clickable/enabled/package 等可选属性。
// uiautomator 的 --compressed 只保留 text/content-desc/resource-id/class/bounds，
// 因此这些属性可能缺失；缺失时 hasFlags 为 false。
func parseUIExtra(node uiNode) (clickable, enabled, hasFlags bool) {
	if node.Attrs == nil {
		return false, false, false
	}
	hasFlags = true
	if v, ok := node.Attrs["clickable"]; ok {
		clickable = v == "true"
	} else {
		clickable = true
	}
	if v, ok := node.Attrs["enabled"]; ok {
		enabled = v == "true"
	} else {
		enabled = true
	}
	return clickable, enabled, hasFlags
}

// dumpUIHierarchy 执行 uiautomator dump 并把 XML 解析成节点树。
func (m *Manager) dumpUIHierarchy(ctx context.Context) ([]uiNode, string, error) {
	dir := filepath.Join(m.cfg.StateDir, "ui")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, "", fmt.Errorf("创建 UI 快照目录失败：%v", err)
	}
	xmlPath := filepath.Join(dir, "hierarchy-"+randomTaskID()+".xml")

	dump := m.runAndroid(ctx, commandVariant{
		Name: "uiautomator", Args: []string{"dump", "--compressed", xmlPath}, Strategy: "uiautomator_dump",
	})
	if !dump.Success {
		// 有些 ROM 的 uiautomator 不接受路径参数，退回标准输出写法。
		dump = m.runAndroid(ctx, commandVariant{
			Name: "uiautomator", Args: []string{"dump", "--compressed", "/dev/tty"}, Strategy: "uiautomator_dump_stdout",
		})
		if !dump.Success {
			return nil, "", fmt.Errorf("uiautomator dump 失败：%s", strings.TrimSpace(dump.Message))
		}
		raw := []byte(dump.Stdout)
		return parseUIHierarchyBytes(raw, xmlPath)
	}

	raw, err := readBoundedArtifact(xmlPath, 2<<20)
	if err != nil {
		return nil, xmlPath, err
	}
	nodes, path, err := parseUIHierarchyBytes(raw, xmlPath)
	return nodes, path, err
}

func parseUIHierarchyBytes(raw []byte, xmlPath string) ([]uiNode, string, error) {
	var hierarchy uiHierarchy
	if err := xml.Unmarshal(raw, &hierarchy); err != nil {
		return nil, xmlPath, fmt.Errorf("UI hierarchy XML 解析失败：%v", err)
	}
	return hierarchy.Nodes, xmlPath, nil
}

// findUINodes 是带重试的查找：等待元素出现直到超时。
func (m *Manager) findUINodes(ctx context.Context, sel uiSelector, timeout time.Duration, limit int) ([]matchedNode, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		nodes, _, err := m.dumpUIHierarchy(ctx)
		if err != nil {
			lastErr = err
		} else {
			matches := make([]matchedNode, 0)
			walkUINodes(nodes, sel, &matches, limit)
			if len(matches) > 0 {
				if sel.HasIndex {
					if sel.Index >= len(matches) {
						lastErr = fmt.Errorf("匹配到 %d 个元素，但没有第 %d 个（index 从 0 开始）", len(matches), sel.Index)
					} else {
						return []matchedNode{matches[sel.Index]}, nil
					}
				} else {
					return matches, nil
				}
			} else {
				lastErr = fmt.Errorf("未找到匹配元素：%s", sel.describe())
			}
		}
		if time.Now().After(deadline) {
			if lastErr == nil {
				lastErr = fmt.Errorf("等待元素超时：%s", sel.describe())
			}
			return nil, lastErr
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("操作已取消")
		case <-time.After(uiDefaultPollMs):
		}
	}
}

// uiOperation 是 aplm_ui 的分派入口。
func (m *Manager) uiOperation(ctx context.Context, req Request, action string) Result {
	if result := requireAndroidRoot(); result != nil {
		return *result
	}
	switch action {
	case "dump", "find":
		return m.uiFind(ctx, req, action)
	case "click":
		return m.uiClick(ctx, req)
	case "wait":
		return m.uiWait(ctx, req)
	case "scroll_to":
		return m.uiScrollTo(ctx, req)
	case "type":
		return m.uiType(ctx, req)
	case "exists":
		return m.uiExists(ctx, req)
	}
	return invalidAction("aplm_ui", action)
}

func (m *Manager) uiFind(ctx context.Context, req Request, action string) Result {
	sel, err := parseUISelector(req.Args)
	if err != nil {
		return invalid(err.Error())
	}
	limit, err := argInt64(req.Args, 200, "limit", "maxResults")
	if err != nil {
		return invalid(err.Error())
	}
	if limit < 1 || limit > uiMaxNodes {
		return invalid(fmt.Sprintf("limit 必须在 1 到 %d 之间", uiMaxNodes))
	}
	timeout, err := argDuration(req.Args, uiDefaultTimeout)
	if err != nil {
		return invalid(err.Error())
	}

	matches, err := m.findUINodes(ctx, sel, timeout, int(limit))
	if err != nil {
		return fail("UI_ELEMENT_NOT_FOUND", err.Error(), err, "uiautomator_find")
	}
	if action == "dump" {
		// dump 返回全部可见节点，便于 AI 了解当前界面。
		nodes, _, dumpErr := m.dumpUIHierarchy(ctx)
		if dumpErr != nil {
			return fail("UI_DUMP_FAILED", dumpErr.Error(), dumpErr, "uiautomator_dump")
		}
		all := make([]matchedNode, 0)
		walkUINodes(nodes, uiSelector{TextContains: ""}, &all, int(limit))
		return ok("UI 层级读取成功", map[string]any{
			"selector": sel.describe(),
			"matched":  len(all),
			"nodes":    all,
			"hint":     "可用 nodes 里的 text / resourceId 作为后续 click 的定位条件",
		}, "uiautomator_dump_nodes")
	}
	return ok("UI 元素查找成功", map[string]any{
		"selector": sel.describe(),
		"count":    len(matches),
		"elements": matches,
	}, "uiautomator_find")
}

func (m *Manager) uiExists(ctx context.Context, req Request) Result {
	sel, err := parseUISelector(req.Args)
	if err != nil {
		return invalid(err.Error())
	}
	timeout, err := argDuration(req.Args, 0)
	if err != nil {
		return invalid(err.Error())
	}
	matches, findErr := m.findUINodes(ctx, sel, timeout, 1)
	exists := findErr == nil && len(matches) > 0
	data := map[string]any{"selector": sel.describe(), "exists": exists}
	if exists {
		data["element"] = matches[0]
	}
	return ok("元素存在性检查完成", data, "uiautomator_exists")
}

func (m *Manager) uiWait(ctx context.Context, req Request) Result {
	sel, err := parseUISelector(req.Args)
	if err != nil {
		return invalid(err.Error())
	}
	timeout, err := argDuration(req.Args, uiDefaultTimeout)
	if err != nil {
		return invalid(err.Error())
	}
	if timeout > time.Duration(uiMaxWaitMs)*time.Millisecond {
		timeout = time.Duration(uiMaxWaitMs) * time.Millisecond
	}
	started := time.Now()
	matches, findErr := m.findUINodes(ctx, sel, timeout, 1)
	if findErr != nil {
		return fail("UI_WAIT_TIMEOUT", findErr.Error(), findErr, "uiautomator_wait")
	}
	return ok("元素已出现", map[string]any{
		"selector": sel.describe(),
		"waitedMs": time.Since(started).Milliseconds(),
		"element":  matches[0],
	}, "uiautomator_wait")
}

func (m *Manager) uiClick(ctx context.Context, req Request) Result {
	sel, err := parseUISelector(req.Args)
	if err != nil {
		return invalid(err.Error())
	}
	timeout, err := argDuration(req.Args, uiDefaultTimeout)
	if err != nil {
		return invalid(err.Error())
	}
	longPress, err := argBool(req.Args, "longPress", false)
	if err != nil {
		return invalid(err.Error())
	}
	duration, err := argInt64(req.Args, 800, "durationMs")
	if err != nil {
		return invalid(err.Error())
	}

	matches, findErr := m.findUINodes(ctx, sel, timeout, 1)
	if findErr != nil {
		return fail("UI_ELEMENT_NOT_FOUND", findErr.Error(), findErr, "uiautomator_click")
	}
	target := matches[0]
	if !target.Enabled {
		return fail("UI_ELEMENT_DISABLED", "目标元素处于禁用状态："+sel.describe(),
			fmt.Errorf("element disabled"), "uiautomator_click")
	}

	var args []string
	strategy := "input_tap_element"
	if longPress {
		if duration <= 0 {
			return invalid("longPress 的 durationMs 必须为正")
		}
		// input swipe 起止同点即长按。
		args = []string{
			"swipe",
			strconv.Itoa(target.CenterX), strconv.Itoa(target.CenterY),
			strconv.Itoa(target.CenterX), strconv.Itoa(target.CenterY),
			strconv.FormatInt(duration, 10),
		}
		strategy = "input_longpress_element"
	} else {
		args = []string{"tap", strconv.Itoa(target.CenterX), strconv.Itoa(target.CenterY)}
	}

	click := m.runAndroidRoot(ctx, commandVariant{Name: "input", Args: args, Strategy: strategy})
	if !click.Success {
		return withData(click, map[string]any{
			"selector": sel.describe(),
			"element":  target,
		})
	}
	return ok("已点击目标元素", map[string]any{
		"selector":  sel.describe(),
		"element":   target,
		"tapX":      target.CenterX,
		"tapY":      target.CenterY,
		"longPress": longPress,
	}, strategy)
}

func (m *Manager) uiType(ctx context.Context, req Request) Result {
	sel, err := parseUISelector(req.Args)
	if err != nil {
		return invalid(err.Error())
	}
	text, err := argString(req.Args, "text", "value")
	if err != nil {
		return invalid(err.Error())
	}
	if len(text) > uiMaxTextFieldBytes {
		return invalid(fmt.Sprintf("text 超过 %d 字节上限", uiMaxTextFieldBytes))
	}
	timeout, err := argDuration(req.Args, uiDefaultTimeout)
	if err != nil {
		return invalid(err.Error())
	}

	matches, findErr := m.findUINodes(ctx, sel, timeout, 1)
	if findErr != nil {
		return fail("UI_ELEMENT_NOT_FOUND", findErr.Error(), findErr, "uiautomator_type")
	}
	target := matches[0]

	// 先聚焦，再输入。
	focus := m.runAndroidRoot(ctx, commandVariant{
		Name:     "input",
		Args:     []string{"tap", strconv.Itoa(target.CenterX), strconv.Itoa(target.CenterY)},
		Strategy: "input_tap_focus",
	})
	if !focus.Success {
		return withData(focus, map[string]any{"selector": sel.describe(), "element": target})
	}

	// Android 的 input text 用 %s 表示空格，且不接受裸换行。
	escaped := strings.ReplaceAll(text, " ", "%s")
	escaped = strings.ReplaceAll(escaped, "\n", "")
	typed := m.runAndroidRoot(ctx, commandVariant{
		Name: "input", Args: []string{"text", escaped}, Strategy: "input_text_element",
	})
	if !typed.Success {
		return withData(typed, map[string]any{"selector": sel.describe(), "element": target})
	}
	return ok("已向目标元素输入文本", map[string]any{
		"selector":  sel.describe(),
		"element":   target,
		"textBytes": len(text),
	}, "input_text_element")
}

func (m *Manager) uiScrollTo(ctx context.Context, req Request) Result {
	sel, err := parseUISelector(req.Args)
	if err != nil {
		return invalid(err.Error())
	}
	maxSwipes, err := argInt64(req.Args, uiDefaultSwipes, "maxSwipes")
	if err != nil {
		return invalid(err.Error())
	}
	if maxSwipes < 1 || maxSwipes > uiMaxSwipes {
		return invalid(fmt.Sprintf("maxSwipes 必须在 1 到 %d 之间", uiMaxSwipes))
	}
	timeout, err := argDuration(req.Args, uiDefaultTimeout)
	if err != nil {
		return invalid(err.Error())
	}
	direction, err := argOptionalString(req.Args, "up", "direction")
	if err != nil {
		return invalid(err.Error())
	}
	if direction != "up" && direction != "down" {
		return invalid("direction 必须是 up 或 down")
	}
	// 滑动距离占屏幕高度的比例，用一个保守值覆盖大多数列表。
	swipePortion, err := argInt64(req.Args, 40, "swipePercent")
	if err != nil {
		return invalid(err.Error())
	}
	if swipePortion < 10 || swipePortion > 80 {
		return invalid("swipePercent 必须在 10 到 80 之间")
	}

	// 屏幕尺寸用于把百分比换算成像素。
	screen := m.runAndroid(ctx, commandVariant{
		Name: "wm", Args: []string{"size"}, Strategy: "wm_size",
	})
	width, height := parseWMSize(screen.Stdout)
	if width <= 0 || height <= 0 {
		return fail("UI_SCREEN_SIZE_UNKNOWN", "无法读取屏幕尺寸，scroll_to 需要 wm size 支持",
			fmt.Errorf("wm size unavailable"), "wm_size")
	}
	span := height * int(swipePortion) / 100
	centerX := width / 2
	var y1, y2 int
	if direction == "down" {
		// 内容向上滚：手指从下往上滑。
		y1, y2 = height*3/4, height*3/4-span
	} else {
		y1, y2 = height/4, height/4+span
	}
	if y2 < 0 {
		y2 = 0
	}

	swipes := 0
	for swipes <= int(maxSwipes) {
		matches, findErr := m.findUINodes(ctx, sel, 0, 1)
		if findErr == nil && len(matches) > 0 {
			return ok("已滚动到目标元素", map[string]any{
				"selector": sel.describe(),
				"swipes":   swipes,
				"element":  matches[0],
			}, "uiautomator_scroll_to")
		}
		if swipes == int(maxSwipes) {
			break
		}
		if ctx.Err() != nil {
			return fail("UI_SCROLL_CANCELLED", "滚动被取消", ctx.Err(), "uiautomator_scroll_to")
		}
		swipe := m.runAndroidRoot(ctx, commandVariant{
			Name: "input",
			Args: []string{"swipe",
				strconv.Itoa(centerX), strconv.Itoa(y1),
				strconv.Itoa(centerX), strconv.Itoa(y2),
				"300"},
			Strategy: "input_swipe_scroll",
		})
		if !swipe.Success {
			return withData(swipe, map[string]any{"swipes": swipes, "selector": sel.describe()})
		}
		swipes++
		select {
		case <-ctx.Done():
			return fail("UI_SCROLL_CANCELLED", "滚动被取消", ctx.Err(), "uiautomator_scroll_to")
		case <-time.After(time.Duration(timeout) / time.Duration(maxSwipes+1)):
		}
	}
	return fail("UI_ELEMENT_NOT_FOUND",
		fmt.Sprintf("滚动 %d 次仍未找到元素：%s", swipes, sel.describe()),
		fmt.Errorf("element not found after scrolling"), "uiautomator_scroll_to")
}

// parseWMSize 解析 `wm size` 输出，例如 "Physical size: 1080x2400"。
func parseWMSize(output string) (width, height int) {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		idx := strings.Index(line, ":")
		if idx < 0 {
			continue
		}
		value := strings.TrimSpace(line[idx+1:])
		parts := strings.SplitN(value, "x", 2)
		if len(parts) != 2 {
			continue
		}
		w, errW := strconv.Atoi(strings.TrimSpace(parts[0]))
		h, errH := strconv.Atoi(strings.TrimSpace(parts[1]))
		if errW == nil && errH == nil && w > 0 && h > 0 {
			return w, h
		}
	}
	return 0, 0
}
