package ops

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"
)

var publicTools = []string{
	"aplm_status",
	"aplm_capabilities",
	"aplm_config",
	"aplm_fs_info",
	"aplm_fs_read",
	"aplm_fs_write",
	"aplm_fs_manage",
	"aplm_fs_search",
	"aplm_fs_hash",
	"aplm_archive",
	"aplm_download",
	"aplm_transfer_upload",
	"aplm_transfer_export",
	"aplm_shell",
	"aplm_script",
	"aplm_task",
	"aplm_skill",
	"aplm_app_list",
	"aplm_app_info",
	"aplm_app_install",
	"aplm_app_manage",
	"aplm_app_permission",
	"aplm_app_export",
	"aplm_root_info",
	"aplm_root_module",
	"aplm_systemless",
	"aplm_process",
	"aplm_service",
	"aplm_property",
	"aplm_setting",
	"aplm_display",
	"aplm_audio",
	"aplm_connectivity",
	"aplm_locale_time",
	"aplm_input_method",
	"aplm_app_policy",
	"aplm_default_app",
	"aplm_notification",
	"aplm_accessibility",
	"aplm_developer",
	"aplm_device_info",
	"aplm_power",
	"aplm_screen",
	"aplm_input",
	"aplm_network",
	"aplm_log",
	"aplm_diagnostics",
	"aplm_schedule",
	"aplm_backup",
	"aplm_ssh",
	"aplm_clipboard",
	"aplm_ui",
	"aplm_share",
}

// SupportedTools returns a copy of the stable public tool-name list.
func SupportedTools() []string {
	return append([]string(nil), publicTools...)
}

func (m *Manager) executeSync(ctx context.Context, req Request) Result {
	switch req.Tool {
	case "aplm_status":
		return m.statusOperation(req)
	case "aplm_capabilities":
		return m.capabilityOperation(req)
	case "aplm_config":
		return m.configOperation(req)
	case "aplm_fs_info":
		return m.fsInfoOperation(req)
	case "aplm_fs_read":
		return m.fsReadOperation(req)
	case "aplm_fs_write":
		return m.fsWriteOperation(req)
	case "aplm_fs_manage":
		return m.fsManageOperation(req)
	case "aplm_fs_search":
		return m.fsSearchOperation(req)
	case "aplm_fs_hash":
		return m.fsHashOperation(req)
	case "aplm_archive":
		return m.archiveOperation(ctx, req)
	case "aplm_download":
		return m.downloadOperation(ctx, req)
	case "aplm_transfer_upload":
		return m.uploadOperation(req)
	case "aplm_transfer_export":
		return m.exportOperation(req)
	case "aplm_shell":
		return m.publicShellOperation(ctx, req)
	case "aplm_script":
		return m.publicScriptOperation(ctx, req)
	case "aplm_task":
		return m.publicTaskOperation(req)
	case "aplm_skill":
		return unsupported("技能索引和执行记忆由 service 层提供")
	case "aplm_process":
		return m.processOperation(ctx, req)
	case "aplm_network":
		return m.networkOperation(ctx, req)
	case "aplm_app_list", "aplm_app_info", "aplm_app_install",
		"aplm_app_manage", "aplm_app_permission", "aplm_app_export":
		return m.appOperation(ctx, req)
	case "aplm_root_info", "aplm_root_module", "aplm_systemless":
		return m.rootOperation(ctx, req)
	case "aplm_service":
		return m.serviceOperation(ctx, req)
	case "aplm_property", "aplm_setting", "aplm_display", "aplm_audio",
		"aplm_connectivity", "aplm_locale_time", "aplm_input_method",
		"aplm_app_policy", "aplm_default_app", "aplm_notification",
		"aplm_accessibility", "aplm_developer":
		return m.systemOperation(ctx, req)
	case "aplm_device_info":
		return m.deviceOperation(ctx, req)
	case "aplm_power":
		return m.powerOperation(ctx, req)
	case "aplm_screen":
		return m.screenOperation(ctx, req)
	case "aplm_input":
		return m.inputOperation(ctx, req)
	case "aplm_log":
		return m.logOperation(ctx, req)
	case "aplm_diagnostics":
		return m.diagnosticsOperation(ctx, req)
	case "aplm_schedule":
		return m.scheduleOperation(ctx, req)
	case "aplm_backup":
		return m.backupOperation(ctx, req)
	case "aplm_ssh":
		action, actionErr := actionOf(req, "")
		if actionErr != nil {
			return invalidAction("aplm_ssh", "")
		}
		return m.sshOperation(ctx, req, action)
	case "aplm_clipboard":
		action, actionErr := actionOf(req, "")
		if actionErr != nil {
			return invalidAction("aplm_clipboard", "")
		}
		return m.clipboardOperation(ctx, req, action)
	case "aplm_ui":
		action, actionErr := actionOf(req, "")
		if actionErr != nil {
			return invalidAction("aplm_ui", "")
		}
		return m.uiOperation(ctx, req, action)
	case "aplm_share":
		action, actionErr := actionOf(req, "")
		if actionErr != nil {
			return invalidAction("aplm_share", "")
		}
		return m.shareOperation(ctx, req, action)
	// Internal aliases are useful to unit-test the primitive operation layer.
	case "shell_execute", "shell_exec", "execute_command", "command_run",
		"shell_execute_many", "execute_commands", "shell_script", "script_execute", "script_run":
		return m.shellOperation(ctx, req)
	case "task_get", "task_status", "task_list", "task_cancel", "task_clear", "task_output", "task_artifacts":
		return m.taskOperation(req)
	default:
		return fail("UNKNOWN_TOOL", "未知工具："+req.Tool, fmt.Errorf("unknown tool %q", req.Tool), "dispatcher")
	}
}

func actionOf(req Request, fallback string) (string, error) {
	action, err := argOptionalString(req.Args, fallback, "action")
	if err != nil {
		return "", err
	}
	return normalizeTool(action), nil
}

func invalidAction(tool, action string, allowed ...string) Result {
	sort.Strings(allowed)
	message := fmt.Sprintf("%s 不支持 action=%q；允许值：%s", tool, action, strings.Join(allowed, ", "))
	return fail("INVALID_ARGUMENT", message, fmt.Errorf("unknown action %q", action), "action_validation")
}

func (m *Manager) statusOperation(req Request) Result {
	action, err := actionOf(req, "get")
	if err != nil {
		return invalid(err.Error())
	}
	if action != "get" && action != "status" {
		return invalidAction(req.Tool, action, "get", "status")
	}
	m.tasksMu.RLock()
	active := 0
	for _, task := range m.tasks {
		if task.Status == "waiting" || task.Status == "running" {
			active++
		}
	}
	m.tasksMu.RUnlock()
	return ok("运行状态读取成功", map[string]any{
		"workDir":      m.cfg.WorkDir,
		"stateDir":     m.cfg.StateDir,
		"root":         platformEUID() == 0,
		"effectiveUid": platformEUID(),
		"goos":         runtime.GOOS,
		"goarch":       runtime.GOARCH,
		"activeTasks":  active,
	}, "native_runtime")
}

func (m *Manager) configOperation(req Request) Result {
	action, err := actionOf(req, "get")
	if err != nil {
		return invalid(err.Error())
	}
	if action != "get" {
		switch action {
		case "validate", "update", "export", "reset":
			return unsupported("配置持久化由 service/config 层负责，ops 不伪造配置写入结果")
		default:
			return invalidAction(req.Tool, action, "export", "get", "reset", "update", "validate")
		}
	}
	return ok("有效配置读取成功", map[string]any{
		"workDir":        m.cfg.WorkDir,
		"stateDir":       m.cfg.StateDir,
		"shellTimeoutMs": m.cfg.ShellTimeout.Milliseconds(),
	}, "in_memory_config")
}

func (m *Manager) capabilityOperation(req Request) Result {
	action, err := actionOf(req, "probe")
	if err != nil {
		return invalid(err.Error())
	}
	if action != "probe" && action != "get" {
		return invalidAction(req.Tool, action, "get", "probe")
	}
	commands := []string{
		"sh", "toybox", "busybox", "cmd", "pm", "am", "settings", "svc",
		"dumpsys", "input", "screencap", "screenrecord", "logcat", "dmesg",
		"tar", "gzip", "xz", "7z", "7za", "ping", "ip", "ss", "getprop",
		"setprop", "getenforce", "chcon", "restorecon", "magisk", "ksud", "apd",
	}
	available := make(map[string]string)
	missing := make([]string, 0)
	for _, command := range commands {
		if path, lookErr := exec.LookPath(command); lookErr == nil {
			available[command] = path
		} else {
			missing = append(missing, command)
		}
	}
	api := readFirstFile("/system/build.prop", "")
	if raw, readErr := os.ReadFile("/proc/version"); readErr == nil {
		api = strings.TrimSpace(string(raw))
	}
	return ok("能力探测完成", map[string]any{
		"tools":         SupportedTools(),
		"commands":      available,
		"missing":       missing,
		"root":          platformEUID() == 0,
		"effectiveUid":  platformEUID(),
		"runtime":       runtime.GOOS + "/" + runtime.GOARCH,
		"kernelVersion": api,
		"selinux":       detectSELinux(),
		"rootFramework": detectRootFramework(),
	}, "runtime_probe")
}

func readFirstFile(paths ...string) string {
	for _, path := range paths {
		if path == "" {
			continue
		}
		if raw, err := os.ReadFile(path); err == nil {
			return strings.TrimSpace(string(raw))
		}
	}
	return ""
}

func detectSELinux() map[string]any {
	enforce := readFirstFile("/sys/fs/selinux/enforce")
	state := "unavailable"
	switch enforce {
	case "1":
		state = "enforcing"
	case "0":
		state = "permissive"
	}
	return map[string]any{
		"state":      state,
		"filesystem": enforce != "",
	}
}

func detectRootFramework() map[string]any {
	type candidate struct {
		Name  string
		Paths []string
		Cmd   string
	}
	candidates := []candidate{
		{Name: "KernelSU", Paths: []string{"/data/adb/ksu", "/data/adb/ksu/bin/ksud"}, Cmd: "ksud"},
		{Name: "APatch", Paths: []string{"/data/adb/ap", "/data/adb/ap/bin/apd"}, Cmd: "apd"},
		{Name: "Magisk", Paths: []string{"/data/adb/magisk", "/sbin/.magisk"}, Cmd: "magisk"},
	}
	for _, item := range candidates {
		for _, path := range item.Paths {
			if _, err := os.Stat(path); err == nil {
				return map[string]any{"name": item.Name, "detectedBy": path}
			}
		}
		if path, err := exec.LookPath(item.Cmd); err == nil {
			return map[string]any{"name": item.Name, "detectedBy": path}
		}
	}
	return map[string]any{"name": "unknown", "detectedBy": ""}
}
