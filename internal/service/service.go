// Package service composes configuration, durable tasks and privileged
// operations behind the broker.Handler interface.
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/aplm123/android-ai-mcp/internal/buildinfo"
	"github.com/aplm123/android-ai-mcp/internal/config"
	"github.com/aplm123/android-ai-mcp/internal/knowledge"
	"github.com/aplm123/android-ai-mcp/internal/model"
	"github.com/aplm123/android-ai-mcp/internal/ops"
	"github.com/aplm123/android-ai-mcp/internal/schema"
	"github.com/aplm123/android-ai-mcp/internal/tasks"
)

type Service struct {
	startedAt  time.Time
	configPath string
	mu         sync.RWMutex
	config     config.Config
	operations *ops.Manager
	tasks      *tasks.Manager
	knowledge  *knowledge.Store
	probeOnce  sync.Once
	probe      map[string]any
}

func New(configPath string, cfg config.Config) (*Service, error) {
	operations := ops.New(ops.Config{
		WorkDir:            cfg.Paths.WorkDir,
		StateDir:           cfg.Paths.StateDir,
		ShellTimeout:       time.Duration(cfg.Limits.ShellTimeoutSeconds) * time.Second,
		ResultPreviewBytes: cfg.Limits.ResultPreviewBytes,
	})
	service := &Service{
		startedAt:  time.Now().UTC(),
		configPath: configPath,
		config:     cfg,
		operations: operations,
		knowledge:  knowledge.New(cfg.Paths.StateDir, cfg.Paths.WorkDir),
	}
	manager, err := tasks.New(
		filepath.Join(cfg.Paths.StateDir, "tasks"),
		cfg.Limits.TotalTasks,
		cfg.Limits.HeavyTasks,
		service.runTask,
	)
	if err != nil {
		return nil, err
	}
	service.tasks = manager
	manager.SetCompletionHook(service.recordCompletedTask)
	service.retainCompletionTasks()
	manager.ReconcileCompletions()
	manager.PurgeTerminalArguments()
	service.settleInterruptedTasks()
	manager.ResumeInterrupted()
	return service, nil
}

func (s *Service) settleInterruptedTasks() {
	for _, task := range s.tasks.List() {
		if task.Status != tasks.StatusInterrupted || !task.Durable {
			continue
		}
		if result, completed := s.knowledge.CompletionResult(task.ID); completed {
			_ = s.tasks.SettleInterrupted(task.ID, result)
			continue
		}
		definition, known := schema.Find(buildinfo.ProtocolCurrent, task.Tool)
		if known && definition.Annotations.ReadOnly && definition.Annotations.Idempotent {
			continue
		}
		result := model.Failure("RESTART_REVIEW_REQUIRED", "上次进程退出时任务没有独立完成回执，且该操作不能证明可安全重放；未自动重试", "Interrupted", "请先读回目标状态，再显式提交新任务")
		result.TaskID = task.ID
		result.Strategy = "non_idempotent_restart_guard"
		_ = s.tasks.SettleInterrupted(task.ID, result)
	}
}

func (s *Service) Close() {
	if s.tasks != nil {
		s.tasks.Close()
	}
	if s.operations != nil {
		s.operations.Close()
	}
}

func (s *Service) Call(ctx context.Context, tool string, args map[string]any) model.Result {
	started := time.Now()
	if err := schema.ValidateInvocation(tool, args); err != nil {
		result := model.Failure("INVALID_ARGUMENT", "工具参数未通过 Schema 校验", "ValidationError", err.Error())
		result.DurationMS = time.Since(started).Milliseconds()
		return result
	}
	var result model.Result
	matches := s.knowledge.Match(tool+" "+stringArgument(args, "action"), 3)
	switch tool {
	case "aplm_status":
		status, err := s.Status(ctx)
		if err != nil {
			result = model.Failure("STATUS_FAILED", "读取状态失败", "StatusError", err.Error())
		} else {
			result = model.Success("OK", "状态读取成功", status)
			result.Strategy = "broker"
		}
	case "aplm_config":
		result = s.configCall(args)
	case "aplm_task":
		result = s.taskCall(ctx, args)
	case "aplm_skill":
		result = s.skillCall(args)
	default:
		background, _ := args["background"].(bool)
		if background {
			cleanArgs := cloneMap(args)
			delete(cleanArgs, "background")
			if reason := sensitiveBackgroundReason(cleanArgs); reason != "" {
				result = model.Failure("BACKGROUND_SENSITIVE_ARGUMENTS_UNSUPPORTED", "含敏感或不可安全持久化参数的调用不能转为持久后台任务", "SafetyError", reason+"；请同步调用，或把输入放入权限受控文件后仅传文件路径")
				break
			}
			task, err := s.tasks.Submit(tool, cleanArgs, isHeavy(tool), true)
			if err != nil {
				code := "TASK_SUBMIT_FAILED"
				if errors.Is(err, tasks.ErrQueueFull) || errors.Is(err, tasks.ErrStorageFull) {
					code = "TASK_QUEUE_FULL"
				}
				result = model.Failure(code, "后台任务提交失败", "TaskError", err.Error())
			} else {
				result = model.Success("TASK_ACCEPTED", "任务已进入后台队列", map[string]any{
					"taskId": task.ID,
					"status": task.Status,
				})
				result.TaskID = task.ID
				result.Strategy = "durable_task"
			}
		} else {
			result = convertOps(s.operations.Execute(ctx, ops.Request{Tool: tool, Args: args}))
		}
	}
	result.DurationMS = time.Since(started).Milliseconds()
	// Background acceptance is not a completion; its terminal hook records the
	// real result. Skill administration is excluded to avoid self-referential
	// memories and forget/recreate loops.
	if shouldLearnCompletion(tool, args, result) {
		if err := s.knowledge.Record(tool, args, result); err != nil {
			if result.Meta == nil {
				result.Meta = map[string]any{}
			}
			result.Meta["knowledgeWarning"] = "完成结果已返回，但长期记忆回执未保存；请检查 aplm_skill.stats.lastPersistenceError"
		}
	}
	if len(matches) > 0 && tool != "aplm_skill" {
		if result.Meta == nil {
			result.Meta = map[string]any{}
		}
		result.Meta["skillHints"] = matches
		result.Meta["skillLoading"] = "摘要已自动匹配；仅在需要时调用 aplm_skill.get 加载一个完整技能。"
	}
	return result
}

func shouldLearnCompletion(tool string, args map[string]any, result model.Result) bool {
	if result.TaskID != "" || tool == "aplm_skill" || tool == "aplm_task" || tool == "aplm_status" {
		return false
	}
	action, _ := args["action"].(string)
	// High-frequency health/config reads add no reusable execution knowledge.
	// Mutations, probes and all substantive operations are still journaled.
	return !(tool == "aplm_capabilities" && action == "get") && !(tool == "aplm_config" && action == "get")
}

func (s *Service) Status(ctx context.Context) (map[string]any, error) {
	s.mu.RLock()
	cfg := s.config
	s.mu.RUnlock()
	taskList := s.tasks.List()
	active := 0
	for _, task := range taskList {
		if !task.Terminal() {
			active++
		}
	}
	s.probeOnce.Do(func() {
		probeContext, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		s.probe = s.collectProbe(probeContext)
	})
	probe := cloneMap(s.probe)
	return map[string]any{
		"name":              buildinfo.Name,
		"version":           buildinfo.Version,
		"commit":            buildinfo.Commit,
		"buildTime":         buildinfo.BuildTime,
		"protocolCurrent":   buildinfo.ProtocolCurrent,
		"protocolPrevious":  buildinfo.ProtocolPrevious,
		"protocolLegacySse": buildinfo.ProtocolLegacySSE,
		"moduleId":          buildinfo.ModuleID,
		"uptimeSeconds":     int64(time.Since(s.startedAt).Seconds()),
		"effectiveUid":      effectiveUID(),
		"root":              effectiveUID() == 0,
		"security": map[string]any{
			"anonymous":      cfg.Security.Anonymous,
			"lanEnabled":     cfg.Network.ListenLAN,
			"onLinkOnly":     cfg.Security.OnLinkOnly,
			"warning":        "同网段设备可获得完整 Root；切勿将端口映射到公网。",
			"frontendTarget": cfg.Security.DropFrontendUID,
		},
		"addresses": map[string]any{
			"port":      cfg.Network.Port,
			"loopback":  cfg.Network.ListenLoopback,
			"lan":       cfg.Network.ListenLAN,
			"mcp":       "/mcp",
			"legacySse": cfg.Network.LegacySSE,
		},
		"paths": map[string]any{
			"stateDir": cfg.Paths.StateDir,
			"workDir":  cfg.Paths.WorkDir,
		},
		"tasks": map[string]any{
			"active":               active,
			"total":                len(taskList),
			"limit":                cfg.Limits.TotalTasks,
			"heavy":                cfg.Limits.HeavyTasks,
			"lastPersistenceError": s.tasks.LastPersistenceError(),
		},
		"androidVersion": probe["androidVersion"],
		"apiLevel":       probe["apiLevel"],
		"abiList":        probe["abiList"],
		"rootFramework":  probe["rootFramework"],
		"capabilities":   probe["capabilities"],
	}, nil
}

func (s *Service) collectProbe(ctx context.Context) map[string]any {
	capability := s.operations.Execute(ctx, ops.Request{
		Tool: "aplm_capabilities",
		Args: map[string]any{"action": "get"},
	})
	device := s.operations.Execute(ctx, ops.Request{
		Tool: "aplm_device_info",
		Args: map[string]any{"action": "get"},
	})
	result := map[string]any{
		"androidVersion": "",
		"apiLevel":       "",
		"abiList":        "",
		"rootFramework":  map[string]any{"name": "unknown"},
		"capabilities": map[string]any{
			"success": capability.Success,
			"data":    capability.Data,
			"error":   capability.Error,
		},
	}
	if data, ok := capability.Data.(map[string]any); ok {
		if framework, exists := data["rootFramework"]; exists {
			result["rootFramework"] = framework
		}
	}
	if data, ok := device.Data.(map[string]any); ok {
		if properties, ok := data["properties"].(map[string]string); ok {
			result["androidVersion"] = properties["androidVersion"]
			result["apiLevel"] = properties["apiLevel"]
			result["abiList"] = properties["abiList"]
		} else if properties, ok := data["properties"].(map[string]any); ok {
			result["androidVersion"] = properties["androidVersion"]
			result["apiLevel"] = properties["apiLevel"]
			result["abiList"] = properties["abiList"]
		}
		if framework, exists := data["rootFramework"]; exists {
			result["rootFramework"] = framework
		}
	}
	return result
}

func (s *Service) TaskGet(_ context.Context, id string) (any, error) {
	if id == "" {
		return nil, errors.New("taskId is required")
	}
	task, ok := s.tasks.Get(id)
	if !ok {
		return nil, os.ErrNotExist
	}
	return publicTask(task, true), nil
}

func (s *Service) TaskList(context.Context) (any, error) {
	return s.taskListPage(0, 100), nil
}

func (s *Service) TaskUpdate(_ context.Context, id string, progress float64, message string) (any, error) {
	task, err := s.tasks.Update(id, progress, message)
	if err != nil {
		return nil, err
	}
	return publicTask(task, task.Terminal()), nil
}

func (s *Service) TaskCancel(_ context.Context, id string) (any, error) {
	task, err := s.tasks.Cancel(id)
	if err != nil {
		return nil, err
	}
	return publicTask(task, task.Terminal()), nil
}

func (s *Service) runTask(ctx context.Context, task tasks.Task, reporter tasks.Reporter) model.Result {
	var args map[string]any
	if err := json.Unmarshal(task.Arguments, &args); err != nil {
		return model.Failure("INVALID_ARGUMENT", "无法恢复任务参数", "DecodeError", err.Error())
	}
	_ = reporter.Progress(0.01, "任务开始")
	result := convertOps(s.operations.Execute(ctx, ops.Request{Tool: task.Tool, Args: args}))
	if result.Stdout != "" {
		_ = reporter.Log("stdout", result.Stdout)
	}
	if result.Stderr != "" {
		_ = reporter.Log("stderr", result.Stderr)
	}
	if result.Success {
		_ = reporter.Progress(1, "任务完成")
	}
	result.TaskID = task.ID
	return result
}

func (s *Service) recordCompletedTask(task tasks.Task) (json.RawMessage, error) {
	if !task.Terminal() || task.Result == nil {
		return nil, nil
	}
	rawArguments := task.Arguments
	if len(task.CompletionArguments) > 0 {
		rawArguments = task.CompletionArguments
	}
	var args map[string]any
	if err := json.Unmarshal(rawArguments, &args); err != nil {
		return nil, fmt.Errorf("decode completion retry arguments: %w", err)
	}
	retryArguments, marshalErr := json.Marshal(knowledge.CompletionArguments(task.Tool, args))
	if marshalErr != nil {
		return nil, marshalErr
	}
	result := *task.Result
	result.TaskID = task.ID
	if err := s.knowledge.Record(task.Tool, args, result); err != nil {
		return retryArguments, err
	}
	s.retainCompletionTasks()
	return nil, nil
}

func (s *Service) retainCompletionTasks() {
	list := s.tasks.List()
	ids := make([]string, 0, len(list))
	for _, task := range list {
		ids = append(ids, task.ID)
	}
	_ = s.knowledge.RetainCompletionTasks(ids)
}

func (s *Service) skillCall(args map[string]any) model.Result {
	action := stringArgument(args, "action")
	switch action {
	case "match":
		query := stringArgument(args, "query")
		if strings.TrimSpace(query) == "" {
			return model.Failure("INVALID_ARGUMENT", "query 不能为空", "ValidationError", "query is required")
		}
		limit := 3
		if requested, ok := number(args["limit"]); ok && requested >= 1 && requested <= 3 {
			limit = int(requested)
		}
		result := model.Success("OK", "技能摘要匹配完成", map[string]any{"matches": s.knowledge.Match(query, limit), "progressive": true})
		result.Strategy = "progressive_skill_index"
		return result
	case "get":
		id := stringArgument(args, "id")
		entry, body, err := s.knowledge.Get(id)
		if err != nil {
			return model.Failure("NOT_FOUND", "技能不存在", "SkillError", err.Error())
		}
		result := model.Success("OK", "技能正文按需加载完成", map[string]any{"entry": entry, "content": body})
		result.Strategy = "progressive_skill_body"
		return result
	case "list":
		source := stringArgument(args, "skillSource")
		if source == "" {
			source = "all"
		}
		if source != "all" && source != "builtin" && source != "learned" {
			return model.Failure("INVALID_ARGUMENT", "source 必须是 all、builtin 或 learned", "ValidationError", source)
		}
		limit := 20
		if requested, ok := number(args["limit"]); ok && requested >= 1 && requested <= 100 {
			limit = int(requested)
		}
		offset := 0
		if requested, ok := number(args["offset"]); ok && requested >= 0 {
			offset = int(requested)
		}
		items, total := s.knowledge.ListIndex(source, offset, limit)
		nextOffset := any(nil)
		if offset+len(items) < total {
			nextOffset = offset + len(items)
		}
		result := model.Success("OK", "技能索引读取完成", map[string]any{"skills": items, "total": total, "offset": offset, "limit": limit, "nextOffset": nextOffset, "bodiesIncluded": false})
		result.Strategy = "progressive_skill_index"
		return result
	case "stats":
		result := model.Success("OK", "技能与记忆统计读取完成", s.knowledge.Stats())
		result.Strategy = "local_skill_store"
		return result
	case "forget":
		confirmed, _ := args["confirmDangerous"].(bool)
		if !confirmed {
			return model.Failure("CONFIRMATION_REQUIRED", "删除自动技能需要 confirmDangerous=true", "SafetyError", "confirmation required")
		}
		id := stringArgument(args, "id")
		if err := s.knowledge.Forget(id); err != nil {
			return model.Failure("SKILL_FORGET_FAILED", "自动技能删除失败", "SkillError", err.Error())
		}
		result := model.Success("OK", "自动技能已删除；历史脱敏调用回执保留", map[string]string{"id": id})
		result.Strategy = "bounded_skill_delete"
		return result
	default:
		return model.Failure("INVALID_ARGUMENT", "不支持的技能操作", "ValidationError", action)
	}
}

func (s *Service) taskCall(ctx context.Context, args map[string]any) model.Result {
	action, _ := args["action"].(string)
	id, _ := args["taskId"].(string)
	var (
		data any
		err  error
	)
	switch action {
	case "get":
		data, err = s.TaskGet(ctx, id)
	case "list":
		limit := 20
		if requested, ok := number(args["limit"]); ok && requested >= 1 && requested <= 100 {
			limit = int(requested)
		}
		offset := 0
		if requested, ok := number(args["offset"]); ok && requested >= 0 {
			offset = int(requested)
		}
		data = s.taskListPage(offset, limit)
	case "cancel":
		data, err = s.TaskCancel(ctx, id)
	case "update":
		progress, _ := number(args["progress"])
		message, _ := args["message"].(string)
		data, err = s.TaskUpdate(ctx, id, progress, message)
	case "logs":
		if id == "" {
			err = errors.New("taskId is required")
		} else {
			cfg := s.Config()
			path := filepath.Join(cfg.Paths.StateDir, "tasks", id+".jsonl")
			limit := cfg.Limits.ResultPreviewBytes
			if requested, ok := number(args["limit"]); ok && requested > 0 && int64(requested) < limit {
				limit = int64(requested)
			}
			var (
				raw       []byte
				start     int64
				totalSize int64
			)
			raw, start, totalSize, err = readFileTail(path, limit)
			if err == nil {
				data = map[string]any{
					"taskId":     id,
					"log":        string(raw),
					"offset":     start,
					"nextOffset": totalSize,
					"truncated":  start > 0,
				}
			}
		}
	default:
		err = fmt.Errorf("unsupported task action %q", action)
	}
	if err != nil {
		return model.Failure("TASK_FAILED", "任务操作失败", "TaskError", err.Error())
	}
	result := model.Success("OK", "任务操作成功", data)
	result.Strategy = "durable_task"
	return result
}

func readFileTail(path string, limit int64) ([]byte, int64, int64, error) {
	if limit <= 0 {
		limit = 1 << 20
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, 0, 0, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, 0, 0, err
	}
	start := info.Size() - limit
	if start < 0 {
		start = 0
	}
	if _, err := file.Seek(start, io.SeekStart); err != nil {
		return nil, 0, 0, err
	}
	raw, err := io.ReadAll(io.LimitReader(file, limit))
	return raw, start, info.Size(), err
}

func (s *Service) configCall(args map[string]any) model.Result {
	action, _ := args["action"].(string)
	switch action {
	case "get":
		return model.Success("OK", "配置读取成功", s.Config())
	case "validate":
		candidate, err := decodeConfig(args)
		if err != nil {
			return model.Failure("INVALID_CONFIG", "配置解码失败", "ConfigError", err.Error())
		}
		if err := candidate.Validate(); err != nil {
			return model.Failure("INVALID_CONFIG", "配置校验失败", "ConfigError", err.Error())
		}
		return model.Success("OK", "配置有效", candidate)
	case "update":
		candidate, err := decodeConfig(args)
		if err != nil {
			return model.Failure("INVALID_CONFIG", "配置解码失败", "ConfigError", err.Error())
		}
		current := s.Config()
		if filepath.Clean(candidate.Paths.StateDir) != filepath.Clean(current.Paths.StateDir) {
			return model.Failure("IMMUTABLE_CONFIG", "内部状态目录不能在线迁移", "ConfigError", "paths.stateDir is immutable")
		}
		if err := config.Save(s.configPath, candidate); err != nil {
			return model.Failure("CONFIG_WRITE_FAILED", "配置原子更新失败", "ConfigError", err.Error())
		}
		s.mu.Lock()
		s.config = candidate
		s.mu.Unlock()
		result := model.Success("OK", "配置已更新；网络与并发项在服务重启后完全生效", candidate)
		result.RebootRequired = true
		result.Strategy = "atomic_json"
		return result
	case "reset":
		current := s.Config()
		candidate := config.Default()
		candidate.Paths.StateDir = current.Paths.StateDir
		candidate.Paths.WorkDir = current.Paths.WorkDir
		candidate.Paths.DownloadsDir = ""
		candidate.Paths.UploadsDir = ""
		candidate.Paths.ArtifactsDir = ""
		candidate.Paths.TempDir = ""
		candidate = config.Normalize(candidate)
		if err := config.Save(s.configPath, candidate); err != nil {
			return model.Failure("CONFIG_WRITE_FAILED", "恢复默认配置失败", "ConfigError", err.Error())
		}
		s.mu.Lock()
		s.config = candidate
		s.mu.Unlock()
		result := model.Success("OK", "配置已恢复默认值", candidate)
		result.RebootRequired = true
		result.Strategy = "atomic_json"
		return result
	case "export":
		cfg := s.Config()
		destination, _ := args["destination"].(string)
		if destination == "" {
			destination = filepath.Join(cfg.Paths.WorkDir, "output", "aplm123-config.json")
		} else if !filepath.IsAbs(destination) {
			destination = filepath.Join(cfg.Paths.WorkDir, destination)
		}
		if err := config.Save(destination, cfg); err != nil {
			return model.Failure("CONFIG_EXPORT_FAILED", "配置导出失败", "FilesystemError", err.Error())
		}
		result := model.Success("OK", "配置已导出", map[string]any{"path": destination})
		result.Strategy = "json_export"
		return result
	default:
		return model.Failure("INVALID_ARGUMENT", "不支持的配置操作", "ValidationError", action)
	}
}

func (s *Service) Config() config.Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.config
}

func decodeConfig(args map[string]any) (config.Config, error) {
	value, ok := args["config"]
	if !ok {
		return config.Config{}, errors.New("config object is required")
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return config.Config{}, err
	}
	var candidate config.Config
	if err := json.Unmarshal(raw, &candidate); err != nil {
		return config.Config{}, err
	}
	candidate = config.Normalize(candidate)
	return candidate, candidate.Validate()
}

func convertOps(source ops.Result) model.Result {
	result := model.Result{
		Success:        source.Success,
		Code:           source.Code,
		Message:        source.Message,
		Data:           source.Data,
		Stdout:         source.Stdout,
		Stderr:         source.Stderr,
		ExitCode:       source.ExitCode,
		DurationMS:     source.DurationMs,
		TaskID:         source.TaskID,
		RebootRequired: source.RebootRequired,
		Artifacts:      []model.Artifact{},
		Strategy:       source.Strategy,
	}
	if source.Error != "" {
		result.Error = &model.Error{
			Type:    source.Code,
			Details: source.Error,
			Fields:  map[string]any{},
		}
	}
	for _, path := range source.Artifacts {
		result.Artifacts = append(result.Artifacts, model.Artifact{Name: filepath.Base(path), Path: path})
	}
	return result
}

func cloneMap(source map[string]any) map[string]any {
	result := make(map[string]any, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func number(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case json.Number:
		number, err := typed.Float64()
		return number, err == nil
	default:
		return 0, false
	}
}

func stringArgument(args map[string]any, key string) string {
	value, _ := args[key].(string)
	return value
}

func isHeavy(tool string) bool {
	definition, ok := schema.Find(buildinfo.ProtocolCurrent, tool)
	return ok && definition.Annotations.Heavy
}

func publicTask(task tasks.Task, includeResult bool) tasks.Task {
	task.Arguments = nil
	task.CompletionArguments = nil
	if !includeResult {
		task.Result = nil
	}
	return task
}

func (s *Service) taskListPage(offset, limit int) map[string]any {
	items := s.tasks.List()
	if offset < 0 {
		offset = 0
	}
	if limit < 1 || limit > 100 {
		limit = 20
	}
	if offset > len(items) {
		offset = len(items)
	}
	end := offset + limit
	if end > len(items) {
		end = len(items)
	}
	page := make([]tasks.Task, 0, end-offset)
	for _, task := range items[offset:end] {
		page = append(page, publicTask(task, false))
	}
	var nextOffset any
	if end < len(items) {
		nextOffset = end
	}
	return map[string]any{
		"tasks":      page,
		"total":      len(items),
		"offset":     offset,
		"limit":      limit,
		"nextOffset": nextOffset,
	}
}

// sensitiveBackgroundReason rejects inputs that cannot safely be written to a
// durable resumable snapshot. Paths and public identifiers remain supported;
// secret-bearing payloads must run synchronously or be referenced indirectly.
func sensitiveBackgroundReason(value any) string {
	arguments, ok := value.(map[string]any)
	if !ok {
		return "arguments must be an object"
	}
	for key := range arguments {
		normalized := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(key, "-", ""), "_", ""))
		if _, safe := durableSafeArgumentKeys[normalized]; !safe {
			return "arguments." + key + " is not on the durable-safe field whitelist"
		}
	}
	return sensitiveValueReason(value, "arguments", 0)
}

var durableSafeArgumentKeys = func() map[string]struct{} {
	keys := strings.Fields(`action algorithm allowed apk apks archive artifactindex at auto backup bitrate bytes code component connectionid confirmdangerous confirmsensitive context count cookiepath createparents cron cwd delayms dest destination domain downgrade duration durationms enabled encoding everyseconds expected expectedsize expression ext extension follow format gid grantruntimepermissions hash host hostname httponly id includevalues identity index inputs intervalseconds jar key keepdata kind lazy length level limit line linecount lines locale lower maxage maxbytes maxchars maxfiles maxitems maxmessagebytes maxresponsebytes maxresults maxsize method milliseconds minsize mode modifiedafter modifiedbefore moduleid muted name namespace offset operation output overwrite package packagename parents path paths permission pid port priority progress recursive renderwaitms replace reset resume retries retrycount retrydelayseconds role root scale scheduleid seconds secure service sessionid sha256 signal size skillsource source sources splitname src startline stream subprotocols system table target targettool taskid time timeout timeoutms timezone tool transaction type uid upper url user weekday work workdir x x1 x2 y y1 y2 zip`)
	result := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		result[key] = struct{}{}
	}
	return result
}()

func sensitiveValueReason(value any, path string, depth int) string {
	if depth > 12 {
		return path + " exceeds the durable argument nesting limit"
	}
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			lower := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(key, "-", ""), "_", ""))
			if sensitiveArgumentKey(lower) {
				return path + "." + key + " may contain credentials or raw payload data"
			}
			if reason := sensitiveValueReason(child, path+"."+key, depth+1); reason != "" {
				return reason
			}
		}
	case map[string]string:
		converted := make(map[string]any, len(typed))
		for key, child := range typed {
			converted[key] = child
		}
		return sensitiveValueReason(converted, path, depth+1)
	case []any:
		for index, child := range typed {
			if reason := sensitiveValueReason(child, fmt.Sprintf("%s[%d]", path, index), depth+1); reason != "" {
				return reason
			}
		}
	case string:
		candidate := strings.TrimSpace(typed)
		if parsed, err := url.Parse(candidate); err == nil && parsed.IsAbs() {
			if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || sensitiveURLPath(parsed) {
				return path + " contains URL credentials, an opaque path token, a query string, or a fragment"
			}
		}
	}
	return ""
}

func sensitiveArgumentKey(normalized string) bool {
	for _, marker := range []string{"authorization", "proxyauthorization", "cookie", "token", "password", "passwd", "secret", "privatekey", "apikey"} {
		if strings.Contains(normalized, marker) {
			return true
		}
	}
	switch normalized {
	case "headers", "env", "stdin", "body", "content", "data", "message", "command", "cmd", "script", "binarybase64", "value", "query", "text", "pattern", "reason", "targetargs", "items", "config", "args", "arguments":
		return true
	default:
		return false
	}
}

func sensitiveURLPath(value *url.URL) bool {
	for _, segment := range strings.Split(value.EscapedPath(), "/") {
		decoded, err := url.PathUnescape(segment)
		if err != nil {
			return true
		}
		lower := strings.ToLower(decoded)
		for _, marker := range []string{"token", "secret", "password", "passwd", "bearer", "credential", "signature", "signed", "session", "apikey", "api-key", "auth"} {
			if strings.Contains(lower, marker) {
				return true
			}
		}
		if len(decoded) >= 48 {
			alphaNumeric := 0
			for _, character := range decoded {
				if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '-' || character == '_' {
					alphaNumeric++
				}
			}
			if alphaNumeric*10 >= len(decoded)*9 {
				return true
			}
		}
	}
	return false
}
