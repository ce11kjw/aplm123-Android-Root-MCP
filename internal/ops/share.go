package ops

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// aplm_share —— 把文件/链接交给系统处理：打开、分享、安装。
//
// 解决的问题：AI 在本机生成的文件此前只能躺在工作目录里，
// 用户还得自己去文件管理器里翻。这个工具补上「最后一公里」。
//
// 全部通过 am start 触发系统 Intent，不需要额外权限。
func (m *Manager) shareOperation(ctx context.Context, req Request, action string) Result {
	if result := requireAndroid(); result != nil {
		return *result
	}
	switch action {
	case "view":
		return m.shareView(ctx, req)
	case "share":
		return m.shareSend(ctx, req)
	case "open_url":
		return m.shareOpenURL(ctx, req)
	case "install":
		return m.shareInstall(ctx, req)
	case "resolve":
		return m.shareResolve(ctx, req)
	}
	return invalidAction("aplm_share", action)
}

// mimeForPath 按扩展名给出 MIME，避免系统弹「无法打开」。
func mimeForPath(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".mp4":
		return "video/mp4"
	case ".mkv":
		return "video/x-matroska"
	case ".mp3":
		return "audio/mpeg"
	case ".m4a":
		return "audio/mp4"
	case ".wav":
		return "audio/wav"
	case ".pdf":
		return "application/pdf"
	case ".apk":
		return "application/vnd.android.package-archive"
	case ".zip":
		return "application/zip"
	case ".txt", ".md", ".log":
		return "text/plain"
	case ".json":
		return "application/json"
	case ".html", ".htm":
		return "text/html"
	default:
		return "*/*"
	}
}

// requireLocalFile 校验路径存在且是普通文件，返回绝对路径。
func requireLocalFile(raw, workDir, label string) (string, error) {
	resolved, err := resolveSSHPath(raw, workDir)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("%s 不可用：%v", label, err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("%s 是目录；请先压缩为单个文件", label)
	}
	return resolved, nil
}

func (m *Manager) shareView(ctx context.Context, req Request) Result {
	rawPath, err := argString(req.Args, "path", "file", "source")
	if err != nil {
		return invalid("缺少参数 path")
	}
	resolved, err := requireLocalFile(rawPath, m.cfg.WorkDir, "文件")
	if err != nil {
		return fail("SHARE_FILE_UNAVAILABLE", err.Error(), err, "share_view")
	}
	mime, err := argOptionalString(req.Args, mimeForPath(resolved), "mime", "type")
	if err != nil {
		return invalid(err.Error())
	}
	user, err := m.resolveAndroidUser(ctx, req.Args, false)
	if err != nil {
		return invalid(err.Error())
	}

	start := m.runAndroid(ctx, commandVariant{
		Name: "am",
		Args: []string{"start", "--user", user, "-a", "android.intent.action.VIEW",
			"-d", "file://" + resolved, "-t", mime,
			"--grant-read-uri-permission"},
		Strategy: "am_start_view_file",
	})
	if !start.Success {
		return withData(
			fail("SHARE_VIEW_FAILED", "系统未能打开该文件（可能没有关联的应用）",
				fmt.Errorf("am start VIEW failed"), "am_start_view_file"),
			map[string]any{"path": resolved, "mime": mime, "stdout": start.Stdout},
		)
	}
	return ok("已交给系统打开文件", map[string]any{
		"path": resolved, "mime": mime, "user": user,
	}, "am_start_view_file")
}

func (m *Manager) shareSend(ctx context.Context, req Request) Result {
	rawPath, err := argString(req.Args, "path", "file", "source")
	if err != nil {
		return invalid("缺少参数 path")
	}
	resolved, err := requireLocalFile(rawPath, m.cfg.WorkDir, "文件")
	if err != nil {
		return fail("SHARE_FILE_UNAVAILABLE", err.Error(), err, "share_send")
	}
	user, err := m.resolveAndroidUser(ctx, req.Args, false)
	if err != nil {
		return invalid(err.Error())
	}
	target, err := argOptionalString(req.Args, "", "package", "to", "packageName")
	if err != nil {
		return invalid(err.Error())
	}
	mime, err := argOptionalString(req.Args, mimeForPath(resolved), "mime", "type")
	if err != nil {
		return invalid(err.Error())
	}
	extraText, err := argOptionalString(req.Args, "", "text", "extraText")
	if err != nil {
		return invalid(err.Error())
	}

	args := []string{"start", "--user", user, "-a", "android.intent.action.SEND",
		"-t", mime, "--grant-read-uri-permission",
		"--es", "android.intent.extra.STREAM", "file://" + resolved}
	if extraText != "" {
		args = append(args, "--es", "android.intent.extra.TEXT", extraText)
	}
	if target != "" {
		// 指定目标包，跳过选择器。
		args = append(args, "-p", target)
	} else {
		args = append(args, "-f", "0x10000000") // FLAG_ACTIVITY_NEW_TASK，让选择器可用
	}

	start := m.runAndroid(ctx, commandVariant{Name: "am", Args: args, Strategy: "am_start_send_file"})
	if !start.Success {
		return withData(
			fail("SHARE_SEND_FAILED", "系统未能分享该文件",
				fmt.Errorf("am start SEND failed"), "am_start_send_file"),
			map[string]any{"path": resolved, "package": target, "mime": mime, "stdout": start.Stdout},
		)
	}
	return ok("已调起分享", map[string]any{
		"path": resolved, "mime": mime, "package": target, "user": user,
	}, "am_start_send_file")
}

func (m *Manager) shareOpenURL(ctx context.Context, req Request) Result {
	rawURL, err := argString(req.Args, "url", "link")
	if err != nil {
		return invalid("缺少参数 url")
	}
	if _, err := parseHTTPURL(rawURL); err != nil {
		return invalid(err.Error())
	}
	user, err := m.resolveAndroidUser(ctx, req.Args, false)
	if err != nil {
		return invalid(err.Error())
	}
	target, err := argOptionalString(req.Args, "", "package", "to", "packageName")
	if err != nil {
		return invalid(err.Error())
	}

	args := []string{"start", "--user", user, "-a", "android.intent.action.VIEW", "-d", rawURL}
	if target != "" {
		args = append(args, "-p", target)
	}
	start := m.runAndroid(ctx, commandVariant{Name: "am", Args: args, Strategy: "am_start_view_url"})
	if !start.Success {
		return withData(
			fail("SHARE_OPEN_URL_FAILED", "系统未能打开该链接",
				fmt.Errorf("am start VIEW url failed"), "am_start_view_url"),
			map[string]any{"url": displayHTTPURL(rawURL), "package": target},
		)
	}
	return ok("已交给系统打开链接", map[string]any{
		"url": displayHTTPURL(rawURL), "package": target, "user": user,
	}, "am_start_view_url")
}

func (m *Manager) shareInstall(ctx context.Context, req Request) Result {
	rawPath, err := argString(req.Args, "path", "file", "apk")
	if err != nil {
		return invalid("缺少参数 path")
	}
	resolved, err := requireLocalFile(rawPath, m.cfg.WorkDir, "APK")
	if err != nil {
		return fail("SHARE_FILE_UNAVAILABLE", err.Error(), err, "share_install")
	}
	if !strings.HasSuffix(strings.ToLower(resolved), ".apk") {
		return invalid("install 需要 .apk 文件；批量/分片安装请使用 aplm_app_install")
	}
	user, err := m.resolveAndroidUser(ctx, req.Args, false)
	if err != nil {
		return invalid(err.Error())
	}

	start := m.runAndroid(ctx, commandVariant{
		Name: "am",
		Args: []string{"start", "--user", user,
			"-a", "android.intent.action.VIEW",
			"-t", "application/vnd.android.package-archive",
			"-d", "file://" + resolved,
			"--grant-read-uri-permission"},
		Strategy: "am_start_install_apk",
	})
	if !start.Success {
		return withData(
			fail("SHARE_INSTALL_FAILED", "系统未能拉起安装界面",
				fmt.Errorf("am start install failed"), "am_start_install_apk"),
			map[string]any{"path": resolved, "stdout": start.Stdout},
		)
	}
	return ok("已拉起系统安装界面（需在屏幕上确认）", map[string]any{
		"path": resolved, "user": user,
		"note": "此动作只调起安装器，真正的安装确认在屏幕上完成；静默安装请用 aplm_app_install",
	}, "am_start_install_apk")
}

// shareResolve 查询某个文件/链接会由哪个组件处理，用于排障。
func (m *Manager) shareResolve(ctx context.Context, req Request) Result {
	rawPath, pathErr := argOptionalString(req.Args, "", "path", "file")
	rawURL, urlErr := argOptionalString(req.Args, "", "url", "link")
	if pathErr != nil {
		return invalid(pathErr.Error())
	}
	if urlErr != nil {
		return invalid(urlErr.Error())
	}
	if (rawPath == "") == (rawURL == "") {
		return invalid("必须且只能提供 path 或 url 之一")
	}
	mime, err := argOptionalString(req.Args, "*/*", "mime", "type")
	if err != nil {
		return invalid(err.Error())
	}
	user, err := m.resolveAndroidUser(ctx, req.Args, false)
	if err != nil {
		return invalid(err.Error())
	}

	args := []string{"start", "--user", user, "-a", "android.intent.action.VIEW", "-t", mime}
	subject := ""
	if rawPath != "" {
		resolved, resolveErr := requireLocalFile(rawPath, m.cfg.WorkDir, "文件")
		if resolveErr != nil {
			return fail("SHARE_FILE_UNAVAILABLE", resolveErr.Error(), resolveErr, "share_resolve")
		}
		args = append(args, "-d", "file://"+resolved)
		subject = resolved
	} else {
		if _, parseErr := parseHTTPURL(rawURL); parseErr != nil {
			return invalid(parseErr.Error())
		}
		args = append(args, "-d", rawURL)
		subject = displayHTTPURL(rawURL)
	}
	// -D 让 am 打印 Intent 解析结果但不真正启动。
	args = append(args, "-D")

	resolve := m.runAndroid(ctx, commandVariant{Name: "am", Args: args, Strategy: "am_resolve_view"})
	data := map[string]any{"subject": subject, "mime": mime, "user": user}
	if resolve.Success {
		data["resolution"] = truncateUTF8Bytes(resolve.Stdout, 2048)
	}
	return ok("Intent 解析完成", data, "am_resolve_view")
}
