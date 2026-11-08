package ops

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type taskState struct {
	ID          string             `json:"id"`
	Tool        string             `json:"tool"`
	Status      string             `json:"status"`
	CreatedAt   time.Time          `json:"createdAt"`
	StartedAt   time.Time          `json:"startedAt,omitempty"`
	CompletedAt time.Time          `json:"completedAt,omitempty"`
	Progress    float64            `json:"progress"`
	Result      Result             `json:"result"`
	Cancel      context.CancelFunc `json:"-"`
}

type taskView struct {
	ID          string    `json:"id"`
	Tool        string    `json:"tool"`
	Status      string    `json:"status"`
	CreatedAt   time.Time `json:"createdAt"`
	StartedAt   time.Time `json:"startedAt,omitempty"`
	CompletedAt time.Time `json:"completedAt,omitempty"`
	Progress    float64   `json:"progress"`
	Result      Result    `json:"result,omitempty"`
}

func isTaskControl(tool string) bool {
	switch tool {
	case "task_get", "task_status", "task_list", "task_cancel", "task_clear", "task_output", "task_artifacts":
		return true
	default:
		return false
	}
}

func randomTaskID() string {
	var data [12]byte
	if _, err := rand.Read(data[:]); err == nil {
		return hex.EncodeToString(data[:])
	}
	return time.Now().UTC().Format("20060102T150405.000000000")
}

func (m *Manager) startBackground(req Request) Result {
	if err := m.ensureRuntimeDirs(); err != nil {
		return fail("IO_ERROR", "无法初始化后台任务目录", err, "task_manager")
	}
	m.tasksMu.Lock()
	active := 0
	for _, task := range m.tasks {
		if task.Status == "waiting" || task.Status == "running" {
			active++
		}
	}
	if active >= m.taskLimit {
		m.tasksMu.Unlock()
		return fail("RESOURCE_LIMIT", "后台任务数量已达到上限", errors.New("too many active tasks"), "task_manager")
	}
	id := randomTaskID()
	detached, cancel := context.WithCancel(context.Background())
	state := &taskState{
		ID:        id,
		Tool:      req.Tool,
		Status:    "waiting",
		CreatedAt: time.Now().UTC(),
		Progress:  0,
		Cancel:    cancel,
		Result: Result{
			Success:  false,
			Code:     "PENDING",
			Message:  "任务等待执行",
			ExitCode: -1,
		},
	}
	m.tasks[id] = state
	m.tasksMu.Unlock()
	m.persistTask(state)

	req.Args = copyArgs(req.Args)
	delete(req.Args, "background")
	delete(req.Args, "autoBackgroundSeconds")
	go func() {
		m.tasksMu.Lock()
		state.Status = "running"
		state.StartedAt = time.Now().UTC()
		state.Progress = 0.01
		m.tasksMu.Unlock()
		m.persistTask(state)

		result := m.Execute(detached, req)
		m.tasksMu.Lock()
		state.Result = result
		state.CompletedAt = time.Now().UTC()
		state.Progress = 1
		switch {
		case errors.Is(detached.Err(), context.Canceled):
			state.Status = "cancelled"
			state.Result.Success = false
			state.Result.Code = "CANCELLED"
			state.Result.Message = "任务已取消"
		case result.Code == "TIMEOUT":
			state.Status = "timeout"
		case result.Success:
			state.Status = "completed"
		default:
			state.Status = "failed"
		}
		m.tasksMu.Unlock()
		m.persistTask(state)
	}()

	return Result{
		Success:  true,
		Code:     "ACCEPTED",
		Message:  "后台任务已创建",
		Data:     map[string]any{"status": "waiting"},
		ExitCode: 0,
		TaskID:   id,
		Strategy: "detached_task",
	}
}

// autoBackgroundSeconds 读取自动后台化的等待窗口（秒）。0 或非法值视为关闭。
func autoBackgroundSeconds(args map[string]any) (int, bool) {
	if args == nil {
		return 0, false
	}
	raw, exists := args["autoBackgroundSeconds"]
	if !exists {
		return 0, false
	}
	switch value := raw.(type) {
	case float64:
		if value >= 1 {
			return int(value), true
		}
	case int:
		if value >= 1 {
			return value, true
		}
	case int64:
		if value >= 1 {
			return int(value), true
		}
	}
	return 0, false
}

// startBackgroundWait 先同步等最多 seconds 秒：窗口内完成直接返回结果，
// 超窗转进程内后台任务并返回 taskId（用 aplm_task get 兜底查询）。
func (m *Manager) startBackgroundWait(req Request, seconds int) Result {
	accepted := m.startBackground(req)
	if !accepted.Success || accepted.TaskID == "" {
		return accepted
	}
	deadline := time.Now().Add(time.Duration(seconds) * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
		m.tasksMu.RLock()
		state, exists := m.tasks[accepted.TaskID]
		var status string
		var result Result
		if exists {
			status = state.Status
			result = state.Result
		}
		m.tasksMu.RUnlock()
		if !exists {
			break
		}
		switch status {
		case "completed", "failed", "timeout", "cancelled":
			result.TaskID = accepted.TaskID
			return result
		}
	}
	accepted.Message = "超过等待窗口仍未完成，已转进程内后台任务继续执行；用 aplm_task get 取结果"
	accepted.Data = map[string]any{"status": "running", "waitedSeconds": seconds}
	accepted.Strategy = "detached_after_wait"
	return accepted
}

// TaskView 返回进程内后台任务的快照。持久任务查不到时由 service 兜底调用。
func (m *Manager) TaskView(id string) (any, bool) {
	m.tasksMu.RLock()
	defer m.tasksMu.RUnlock()
	state, exists := m.tasks[id]
	if !exists {
		return nil, false
	}
	return snapshotTask(state), true
}

func copyArgs(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for key, value := range in {
		out[key] = jsonClone(value)
	}
	return out
}

func (m *Manager) taskOperation(req Request) Result {
	switch req.Tool {
	case "task_list":
		m.tasksMu.RLock()
		views := make([]taskView, 0, len(m.tasks))
		for _, task := range m.tasks {
			views = append(views, snapshotTask(task))
		}
		m.tasksMu.RUnlock()
		sort.Slice(views, func(i, j int) bool { return views[i].CreatedAt.After(views[j].CreatedAt) })
		return ok("任务列表读取成功", views, "task_manager")
	case "task_clear":
		m.tasksMu.Lock()
		removed := 0
		for id, task := range m.tasks {
			if task.Status != "waiting" && task.Status != "running" {
				delete(m.tasks, id)
				removed++
				_ = os.Remove(m.taskPath(id))
			}
		}
		m.tasksMu.Unlock()
		return ok("已清理完成的任务记录", map[string]int{"removed": removed}, "task_manager")
	}

	id, err := argString(req.Args, "taskId", "id")
	if err != nil {
		return invalid(err.Error())
	}
	m.tasksMu.RLock()
	task, exists := m.tasks[id]
	if !exists {
		m.tasksMu.RUnlock()
		if loaded, loadErr := m.loadTask(id); loadErr == nil {
			task = loaded
			exists = true
		}
	} else {
		defer m.tasksMu.RUnlock()
	}
	if !exists {
		return fail("NOT_FOUND", "任务不存在", os.ErrNotExist, "task_manager")
	}
	view := snapshotTask(task)
	if exists && task.Cancel != nil && (req.Tool == "task_cancel") {
		task.Cancel()
		return ok("取消请求已提交", map[string]string{"taskId": id}, "task_manager")
	}
	switch req.Tool {
	case "task_get", "task_status":
		return ok("任务状态读取成功", view, "task_manager")
	case "task_output":
		return ok("任务输出读取成功", map[string]any{
			"stdout": view.Result.Stdout,
			"stderr": view.Result.Stderr,
			"code":   view.Result.Code,
		}, "task_manager")
	case "task_artifacts":
		return ok("任务产物读取成功", view.Result.Artifacts, "task_manager")
	case "task_cancel":
		return fail("CONFLICT", "任务已结束，无法取消", errors.New(view.Status), "task_manager")
	default:
		return unsupported("未知任务操作")
	}
}

func snapshotTask(task *taskState) taskView {
	return taskView{
		ID:          task.ID,
		Tool:        task.Tool,
		Status:      task.Status,
		CreatedAt:   task.CreatedAt,
		StartedAt:   task.StartedAt,
		CompletedAt: task.CompletedAt,
		Progress:    task.Progress,
		Result:      task.Result,
	}
}

func (m *Manager) taskPath(id string) string {
	id = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r >= 'A' && r <= 'Z' || r == '-' {
			return r
		}
		return -1
	}, id)
	return filepath.Join(m.cfg.StateDir, "tasks", id+".json")
}

func (m *Manager) persistTask(task *taskState) {
	path := m.taskPath(task.ID)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return
	}
	raw, err := json.MarshalIndent(snapshotTask(task), "", "  ")
	if err != nil {
		return
	}
	temp := path + ".tmp"
	if os.WriteFile(temp, raw, 0o600) == nil {
		_ = os.Rename(temp, path)
	}
}

func (m *Manager) loadTask(id string) (*taskState, error) {
	raw, err := os.ReadFile(m.taskPath(id))
	if err != nil {
		return nil, err
	}
	var view taskView
	if err := json.Unmarshal(raw, &view); err != nil {
		return nil, err
	}
	return &taskState{
		ID:          view.ID,
		Tool:        view.Tool,
		Status:      view.Status,
		CreatedAt:   view.CreatedAt,
		StartedAt:   view.StartedAt,
		CompletedAt: view.CompletedAt,
		Progress:    view.Progress,
		Result:      view.Result,
	}, nil
}
