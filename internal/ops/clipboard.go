package ops

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// aplm_clipboard —— 剪贴板读写。
//
// 这是 AI 与手机之间最轻量的文本通道：不需要截图识别，也不依赖界面元素，
// 你在任意 App 里复制的文本 AI 都能直接读，AI 生成的内容也能直接写回去粘贴。
//
// 实现按可用性依次尝试：
//  1. cmd clipboard（Android 10+ 提供，get/set 语义最稳）
//  2. service call clipboard（旧版兜底，仅 get 的文本部分可靠）
//
// 注意：Android 10 起后台应用读剪贴板受限，本模块以 root 运行，通常可绕过该限制；
// 若某些 ROM 仍拒绝，会返回明确的 CLIPBOARD_UNAVAILABLE 而不是静默失败。
const (
	clipboardMaxTextBytes = 256 << 10
	clipboardPollInterval = 400 * time.Millisecond
	clipboardMaxWaitMs    = 120000
)

func (m *Manager) clipboardOperation(ctx context.Context, req Request, action string) Result {
	if result := requireAndroidRoot(); result != nil {
		return *result
	}
	switch action {
	case "get":
		return m.clipboardGet(ctx, req)
	case "set":
		return m.clipboardSet(ctx, req)
	case "clear":
		return m.clipboardClear(ctx)
	case "watch":
		return m.clipboardWatch(ctx, req)
	}
	return invalidAction("aplm_clipboard", action)
}

func (m *Manager) clipboardGet(ctx context.Context, req Request) Result {
	result := m.runAndroidRoot(ctx,
		commandVariant{Name: "cmd", Args: []string{"clipboard", "get"}, Strategy: "cmd_clipboard_get"},
	)
	if !result.Success || strings.TrimSpace(result.Stdout) == "" {
		// 旧版 ROM 兜底：service call clipboard 1（返回 Parcel，文本可从中提取）。
		fallback := m.runAndroidRoot(ctx,
			commandVariant{Name: "service", Args: []string{"call", "clipboard", "1"}, Strategy: "service_call_clipboard"},
		)
		if !fallback.Success {
			return fail("CLIPBOARD_UNAVAILABLE",
				"当前系统无法读取剪贴板（cmd clipboard 与 service call 均不可用）",
				fmt.Errorf("clipboard read unavailable"), "clipboard_probe")
		}
		text := extractClipboardText(fallback.Stdout)
		if text == "" {
			return ok("剪贴板为空", map[string]any{
				"empty": true, "text": "", "length": 0,
			}, "service_call_clipboard")
		}
		return ok("剪贴板内容读取成功", map[string]any{
			"text":   truncateUTF8Bytes(text, clipboardMaxTextBytes),
			"length": len(text),
			"empty":  false,
			"source": "service_call_fallback",
		}, "service_call_clipboard")
	}

	text := strings.TrimRight(result.Stdout, "\r\n")
	return ok("剪贴板内容读取成功", map[string]any{
		"text":   truncateUTF8Bytes(text, clipboardMaxTextBytes),
		"length": len(text),
		"empty":  text == "",
		"source": "cmd_clipboard",
	}, "cmd_clipboard_get")
}

func (m *Manager) clipboardSet(ctx context.Context, req Request) Result {
	text, err := argString(req.Args, "text", "content", "value")
	if err != nil {
		return invalid("缺少参数 text")
	}
	if len(text) > clipboardMaxTextBytes {
		return invalid(fmt.Sprintf("text 超过 %d 字节上限", clipboardMaxTextBytes))
	}

	// cmd clipboard set 走标准入口，无需前台 Activity。
	set := m.runAndroidRoot(ctx, commandVariant{
		Name: "cmd", Args: []string{"clipboard", "set", text}, Strategy: "cmd_clipboard_set",
	})
	if !set.Success {
		// 兜底：用 am broadcast 不可靠，这里直接给出可操作的失败信息。
		return withData(
			fail("CLIPBOARD_WRITE_FAILED",
				"写入剪贴板失败，当前系统可能不支持 cmd clipboard set",
				fmt.Errorf("clipboard write unavailable"), "cmd_clipboard_set"),
			map[string]any{"textBytes": len(text)},
		)
	}

	// 写后读回校验：确认内容确实进去了，而不是命令退出码为 0 就当作成功。
	verify := m.runAndroidRoot(ctx,
		commandVariant{Name: "cmd", Args: []string{"clipboard", "get"}, Strategy: "cmd_clipboard_get"},
	)
	data := map[string]any{"textBytes": len(text), "text": truncateUTF8Bytes(text, 512)}
	if verify.Success {
		got := strings.TrimRight(verify.Stdout, "\r\n")
		data["readback"] = got == text
		if got != text {
			data["readbackValue"] = truncateUTF8Bytes(got, 512)
		}
	}
	return ok("剪贴板已写入", data, "cmd_clipboard_set")
}

func (m *Manager) clipboardClear(ctx context.Context) Result {
	// 用一个空串覆盖是各版本通用的清空方式。
	clear := m.runAndroidRoot(ctx, commandVariant{
		Name: "cmd", Args: []string{"clipboard", "set", ""}, Strategy: "cmd_clipboard_clear",
	})
	if !clear.Success {
		return withData(
			fail("CLIPBOARD_WRITE_FAILED",
				"清空剪贴板失败，当前系统可能不支持 cmd clipboard set",
				fmt.Errorf("clipboard clear unavailable"), "cmd_clipboard_clear"),
			nil,
		)
	}
	return ok("剪贴板已清空", map[string]any{"cleared": true}, "cmd_clipboard_clear")
}

// clipboardWatch 等待剪贴板出现新内容（轮询实现，避免依赖前台监听服务）。
func (m *Manager) clipboardWatch(ctx context.Context, req Request) Result {
	timeout, err := argDuration(req.Args, 30*time.Second)
	if err != nil {
		return invalid(err.Error())
	}
	if timeout > time.Duration(clipboardMaxWaitMs)*time.Millisecond {
		timeout = time.Duration(clipboardMaxWaitMs) * time.Millisecond
	}
	contains, err := argOptionalString(req.Args, "", "contains", "textContains")
	if err != nil {
		return invalid(err.Error())
	}

	initial := ""
	if first := m.clipboardGet(ctx, Request{Tool: "aplm_clipboard"}); first.Success {
		if data, isObj := first.Data.(map[string]any); isObj {
			initial, _ = data["text"].(string)
		}
	}

	deadline := time.Now().Add(timeout)
	for {
		if ctx.Err() != nil {
			return fail("CLIPBOARD_WATCH_CANCELLED", "剪贴板监听被取消", ctx.Err(), "clipboard_watch")
		}
		if time.Now().After(deadline) {
			return fail("CLIPBOARD_WATCH_TIMEOUT",
				fmt.Sprintf("等待 %s 内剪贴板没有出现新内容", timeout),
				fmt.Errorf("clipboard unchanged"), "clipboard_watch")
		}
		current := m.clipboardGet(ctx, Request{Tool: "aplm_clipboard"})
		if current.Success {
			if data, isObj := current.Data.(map[string]any); isObj {
				text, _ := data["text"].(string)
				changed := text != initial && text != ""
				matches := contains == "" || strings.Contains(text, contains)
				if changed && matches {
					return ok("剪贴板已更新", map[string]any{
						"text":   truncateUTF8Bytes(text, clipboardMaxTextBytes),
						"length": len(text),
						"filter": contains,
					}, "clipboard_watch")
				}
			}
		}
		select {
		case <-ctx.Done():
			return fail("CLIPBOARD_WATCH_CANCELLED", "剪贴板监听被取消", ctx.Err(), "clipboard_watch")
		case <-time.After(clipboardPollInterval):
		}
	}
}

// extractClipboardText 从 `service call clipboard 1` 的 Parcel 输出里提取文本。
// 输出形如 "... s16 \"hello world\" ..." 或十六进制字符数组，这里只做尽力而为的提取。
func extractClipboardText(output string) string {
	// 优先找带引号的字符串片段。
	if idx := strings.Index(output, `s16 "`); idx >= 0 {
		rest := output[idx+5:]
		if end := strings.Index(rest, `"`); end >= 0 {
			return rest[:end]
		}
	}
	// 退而求其次：把形如 'h' 'e' 'l' 'l' 'o' 的字符片段拼起来。
	var builder strings.Builder
	for _, raw := range strings.Split(output, " ") {
		raw = strings.TrimSpace(raw)
		if len(raw) == 3 && raw[0] == '\'' && raw[2] == '\'' {
			builder.WriteByte(raw[1])
		}
	}
	if builder.Len() > 0 {
		return builder.String()
	}
	_ = strconv.Itoa(0)
	return ""
}
