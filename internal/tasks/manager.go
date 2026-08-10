// Package tasks provides durable, cancellable background work with independent
// total and heavyweight concurrency limits.
package tasks

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/zcr521/android-ai-mcp/internal/atomicfile"
	"github.com/zcr521/android-ai-mcp/internal/model"
)

type Status string

const (
	StatusQueued      Status = "queued"
	StatusRunning     Status = "running"
	StatusSucceeded   Status = "succeeded"
	StatusFailed      Status = "failed"
	StatusTimedOut    Status = "timed_out"
	StatusCancelling  Status = "cancelling"
	StatusCancelled   Status = "cancelled"
	StatusInterrupted Status = "interrupted"

	maxTerminalTasks         = 2000
	terminalTaskLowWater     = 1500
	maxTerminalSnapshotBytes = int64(64 << 20)
	terminalBytesLowWater    = int64(48 << 20)
	maxTaskLogBytes          = int64(4 << 20)
	maxTaskArgumentsBytes    = 64 << 10
	maxCompletionRetryBytes  = 64 << 10
	maxTaskSnapshotBytes     = 1 << 20
	maxTaskStorageBytes      = int64(128 << 20)
	maxTaskMessageBytes      = 1024
	completionRetryInitial   = 250 * time.Millisecond
	completionRetryMaximum   = 30 * time.Second
)

var (
	ErrQueueFull     = errors.New("task queue limit reached")
	ErrStorageFull   = errors.New("task storage limit reached")
	errSnapshotLimit = errors.New("task snapshot exceeds safe read limit")
)

type Task struct {
	ID                  string          `json:"id"`
	Tool                string          `json:"tool"`
	Arguments           json.RawMessage `json:"arguments,omitempty"`
	CompletionArguments json.RawMessage `json:"completionArguments,omitempty"`
	CompletionPending   bool            `json:"completionPending"`
	Status              Status          `json:"status"`
	Progress            float64         `json:"progress"`
	Message             string          `json:"message"`
	Heavy               bool            `json:"heavy"`
	Durable             bool            `json:"durable"`
	CreatedAt           time.Time       `json:"createdAt"`
	StartedAt           time.Time       `json:"startedAt"`
	UpdatedAt           time.Time       `json:"updatedAt"`
	CompletedAt         time.Time       `json:"completedAt"`
	Result              *model.Result   `json:"result"`
}

func (t Task) Terminal() bool {
	return t.Status == StatusSucceeded || t.Status == StatusFailed ||
		t.Status == StatusTimedOut || t.Status == StatusCancelled
}

type Runner func(context.Context, Task, Reporter) model.Result

type Reporter interface {
	Progress(value float64, message string) error
	Log(stream, text string) error
}

type Manager struct {
	dir                  string
	runner               Runner
	totalSem             chan struct{}
	heavySem             chan struct{}
	mu                   sync.RWMutex
	tasks                map[string]*Task
	cancel               map[string]context.CancelFunc
	waiters              map[string][]chan struct{}
	completing           map[string]bool
	completionActive     map[string]bool
	completionErrors     map[string]string
	onComplete           func(Task) (json.RawMessage, error)
	lastPersistenceError string
	outstandingLimit     int
	retryScheduled       bool
	retryDelay           time.Duration
	closeOnce            sync.Once
	closed               chan struct{}
}

func New(dir string, totalLimit, heavyLimit int, runner Runner) (*Manager, error) {
	if totalLimit < 1 || heavyLimit < 1 || heavyLimit > totalLimit {
		return nil, errors.New("invalid task concurrency limits")
	}
	if runner == nil {
		return nil, errors.New("task runner is required")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create task directory: %w", err)
	}
	m := &Manager{
		dir:              dir,
		runner:           runner,
		totalSem:         make(chan struct{}, totalLimit),
		heavySem:         make(chan struct{}, heavyLimit),
		tasks:            map[string]*Task{},
		cancel:           map[string]context.CancelFunc{},
		waiters:          map[string][]chan struct{}{},
		completing:       map[string]bool{},
		completionActive: map[string]bool{},
		completionErrors: map[string]string{},
		closed:           make(chan struct{}),
	}
	m.outstandingLimit = totalLimit * 8
	if m.outstandingLimit < 16 {
		m.outstandingLimit = 16
	}
	if m.outstandingLimit > 1024 {
		m.outstandingLimit = 1024
	}
	if err := m.load(); err != nil {
		return nil, err
	}
	return m, nil
}

// SetCompletionHook installs a callback invoked outside the manager lock after
// terminal state is reached. The callback receives an isolated snapshot with
// resumable arguments, allowing the independent MCP completion journal to be
// committed before those arguments are removed from long-lived task state.
// The callback must be idempotent because startup reconciliation may replay it.
func (m *Manager) SetCompletionHook(hook func(Task) (json.RawMessage, error)) {
	m.mu.Lock()
	m.onComplete = hook
	m.mu.Unlock()
}

// LastPersistenceError reports the most recent task snapshot write failure.
// An empty string means the latest persistence attempt succeeded.
func (m *Manager) LastPersistenceError() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.lastPersistenceError != "" {
		return m.lastPersistenceError
	}
	keys := make([]string, 0, len(m.completionErrors))
	for id := range m.completionErrors {
		keys = append(keys, id)
	}
	slices.Sort(keys)
	if len(keys) > 0 {
		return m.completionErrors[keys[0]]
	}
	return ""
}

func (m *Manager) Submit(tool string, arguments any, heavy, durable bool) (Task, error) {
	if tool == "" {
		return Task{}, errors.New("tool is required")
	}
	raw, err := json.Marshal(arguments)
	if err != nil {
		return Task{}, fmt.Errorf("encode task arguments: %w", err)
	}
	if len(raw) > maxTaskArgumentsBytes {
		return Task{}, fmt.Errorf("%w: arguments exceed %d bytes", ErrStorageFull, maxTaskArgumentsBytes)
	}
	id, err := newID()
	if err != nil {
		return Task{}, err
	}
	now := time.Now().UTC()
	task := &Task{
		ID:        id,
		Tool:      tool,
		Arguments: raw,
		Status:    StatusQueued,
		Message:   "等待执行",
		Heavy:     heavy,
		Durable:   durable,
		CreatedAt: now,
		UpdatedAt: now,
	}
	m.mu.Lock()
	select {
	case <-m.closed:
		m.mu.Unlock()
		return Task{}, errors.New("task manager is closed")
	default:
	}
	if err := m.pruneTerminalLocked(""); err != nil {
		m.mu.Unlock()
		return Task{}, err
	}
	outstanding := 0
	for _, existing := range m.tasks {
		if !existing.Terminal() {
			outstanding++
		}
	}
	if outstanding >= m.outstandingLimit {
		m.mu.Unlock()
		return Task{}, fmt.Errorf("%w: %d outstanding tasks", ErrQueueFull, outstanding)
	}
	m.tasks[id] = task
	if err := m.persistLocked(task); err != nil {
		delete(m.tasks, id)
		m.mu.Unlock()
		return Task{}, err
	}
	copy := cloneTask(task)
	m.mu.Unlock()
	go m.run(id)
	return copy, nil
}

// ResumeInterrupted restarts durable tasks that were interrupted by a process
// restart. Non-durable interrupted tasks remain queryable and failed.
func (m *Manager) ResumeInterrupted() int {
	m.mu.Lock()
	var interrupted []*Task
	active := 0
	for _, task := range m.tasks {
		if task.Status == StatusInterrupted && task.Durable {
			interrupted = append(interrupted, task)
		} else if !task.Terminal() {
			active++
		}
	}
	slices.SortFunc(interrupted, func(a, b *Task) int {
		if a.CreatedAt.Before(b.CreatedAt) {
			return -1
		}
		if a.CreatedAt.After(b.CreatedAt) {
			return 1
		}
		return strings.Compare(a.ID, b.ID)
	})
	capacity := m.outstandingLimit - active
	if capacity < 0 {
		capacity = 0
	}
	var ids []string
	var overflow []Task
	for index, task := range interrupted {
		if index >= capacity {
			now := time.Now().UTC()
			failure := model.Failure("TASK_RECOVERY_QUEUE_LIMIT", "重启恢复队列已达到安全上限，任务未自动重放", "RecoveryLimit", "请核对目标当前状态后再显式提交")
			failure.TaskID = task.ID
			task.Status = StatusFailed
			task.Message = failure.Message
			task.Result = &failure
			task.UpdatedAt = now
			task.CompletedAt = now
			m.completing[task.ID] = true
			overflow = append(overflow, cloneTask(task))
			continue
		}
		task.Status = StatusQueued
		task.Message = "服务重启后恢复执行"
		task.UpdatedAt = time.Now().UTC()
		if err := m.persistLocked(task); err != nil {
			task.Status = StatusInterrupted
			task.Message = "恢复前无法持久化状态，未执行"
			continue
		}
		ids = append(ids, task.ID)
	}
	m.mu.Unlock()
	for _, task := range overflow {
		_ = m.commitCompletion(task)
	}
	for _, id := range ids {
		go m.run(id)
	}
	return len(ids)
}

// SettleInterrupted writes a terminal result recovered from an independent
// completion journal (or a safety decision) before ResumeInterrupted runs.
func (m *Manager) SettleInterrupted(id string, result model.Result) error {
	m.mu.Lock()
	task := m.tasks[id]
	if task == nil || task.Status != StatusInterrupted {
		m.mu.Unlock()
		return errors.New("task is not interrupted")
	}
	now := time.Now().UTC()
	result.TaskID = id
	task.Result = &result
	task.UpdatedAt = now
	task.CompletedAt = now
	task.Message = result.Message
	if result.Success {
		task.Status = StatusSucceeded
		task.Progress = 1
	} else if result.Code == "TIMEOUT" {
		task.Status = StatusTimedOut
	} else {
		task.Status = StatusFailed
	}
	completed := cloneTask(task)
	m.completing[id] = true
	m.mu.Unlock()
	return m.commitCompletion(completed)
}

func (m *Manager) commitCompletion(completed Task) error {
	m.mu.Lock()
	task := m.tasks[completed.ID]
	if task == nil || !task.Terminal() {
		m.mu.Unlock()
		return errors.New("task is not terminal")
	}
	if m.completionActive[completed.ID] {
		m.mu.Unlock()
		return errors.New("completion commit is already active")
	}
	m.completing[completed.ID] = true
	m.completionActive[completed.ID] = true
	hook := m.onComplete
	m.mu.Unlock()
	var retryArguments json.RawMessage
	var hookErr error
	if hook != nil {
		retryArguments, hookErr = hook(completed)
	}
	if hookErr != nil && (len(retryArguments) == 0 || len(retryArguments) > maxCompletionRetryBytes || !json.Valid(retryArguments)) {
		hookErr = fmt.Errorf("completion journal failed and retry envelope is invalid: %w", hookErr)
		retryArguments = nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.completionActive, completed.ID)
	task = m.tasks[completed.ID]
	if task == nil || !task.Terminal() {
		delete(m.completing, completed.ID)
		m.notifyLocked(completed.ID)
		return errors.New("terminal task disappeared before completion commit")
	}
	if hookErr != nil {
		task.Arguments = nil
		task.CompletionPending = true
		task.CompletionArguments = append(json.RawMessage(nil), retryArguments...)
	} else {
		task.CompletionPending = false
		task.CompletionArguments = nil
	}
	persistErr := m.persistLocked(task)
	var pruneErr error
	if persistErr == nil {
		task.Arguments = nil
		delete(m.completing, task.ID)
		m.notifyLocked(task.ID)
		pruneErr = m.pruneTerminalLocked(task.ID)
	}
	completionErr := errors.Join(hookErr, persistErr)
	if completionErr != nil {
		m.completionErrors[task.ID] = completionErr.Error()
		m.scheduleCompletionRetryLocked()
	} else {
		delete(m.completionErrors, task.ID)
		if len(m.completionErrors) == 0 {
			m.retryDelay = 0
		}
	}
	if completionErr != nil && pruneErr != nil {
		return errors.Join(completionErr, pruneErr)
	}
	if completionErr != nil {
		return completionErr
	}
	return pruneErr
}

// ReconcileCompletions retries terminal journal writes that were interrupted
// or previously failed. Only the bounded redacted completion envelope is kept
// after the first terminal attempt.
func (m *Manager) ReconcileCompletions() int {
	m.mu.RLock()
	pending := make([]Task, 0)
	for _, task := range m.tasks {
		if task.Terminal() && !m.completionActive[task.ID] && (task.CompletionPending || len(task.Arguments) > 0 || m.completing[task.ID]) {
			pending = append(pending, cloneTask(task))
		}
	}
	m.mu.RUnlock()
	for _, task := range pending {
		_ = m.commitCompletion(task)
	}
	return len(pending)
}

func (m *Manager) scheduleCompletionRetryLocked() {
	if m.retryScheduled {
		return
	}
	select {
	case <-m.closed:
		return
	default:
	}
	delay := m.retryDelay
	if delay <= 0 {
		delay = completionRetryInitial
	}
	m.retryDelay = delay * 2
	if m.retryDelay > completionRetryMaximum {
		m.retryDelay = completionRetryMaximum
	}
	m.retryScheduled = true
	go func() {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-m.closed:
			return
		case <-timer.C:
		}
		m.mu.Lock()
		m.retryScheduled = false
		m.mu.Unlock()
		m.ReconcileCompletions()
	}()
}

func (m *Manager) Get(id string) (Task, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	task, ok := m.tasks[id]
	if !ok {
		return Task{}, false
	}
	return m.visibleTaskLocked(task), true
}

func (m *Manager) List() []Task {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make([]Task, 0, len(m.tasks))
	for _, task := range m.tasks {
		result = append(result, m.visibleTaskLocked(task))
	}
	slices.SortFunc(result, func(a, b Task) int {
		if a.CreatedAt.After(b.CreatedAt) {
			return -1
		}
		if a.CreatedAt.Before(b.CreatedAt) {
			return 1
		}
		return 0
	})
	return result
}

// PurgeTerminalArguments removes legacy resumable inputs after startup
// reconciliation has had one opportunity to record a redacted completion.
func (m *Manager) PurgeTerminalArguments() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, task := range m.tasks {
		if task.Terminal() && !m.completing[task.ID] && len(task.Arguments) > 0 {
			task.Arguments = nil
			_ = m.persistLocked(task)
		}
	}
}

func (m *Manager) visibleTaskLocked(task *Task) Task {
	copy := cloneTask(task)
	if !m.completing[task.ID] {
		return copy
	}
	copy.Status = StatusRunning
	copy.Message = "正在提交完成回执"
	if copy.Progress >= 1 {
		copy.Progress = 0.99
	}
	copy.CompletedAt = time.Time{}
	copy.Result = nil
	return copy
}

func (m *Manager) Cancel(id string) (Task, error) {
	m.mu.Lock()
	task, ok := m.tasks[id]
	if !ok {
		m.mu.Unlock()
		return Task{}, os.ErrNotExist
	}
	if task.Terminal() {
		copy := m.visibleTaskLocked(task)
		m.mu.Unlock()
		return copy, nil
	}
	task.Status = StatusCancelling
	task.Message = "正在取消"
	task.UpdatedAt = time.Now().UTC()
	cancel := m.cancel[id]
	if err := m.persistLocked(task); err != nil {
		m.mu.Unlock()
		return Task{}, err
	}
	copy := cloneTask(task)
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return copy, nil
}

// Update accepts progress notifications for the MCP task extension. It does
// not permit a client to forge a terminal result.
func (m *Manager) Update(id string, progress float64, message string) (Task, error) {
	if progress < 0 || progress > 1 {
		return Task{}, errors.New("progress must be between 0 and 1")
	}
	if len(message) > maxTaskMessageBytes {
		return Task{}, fmt.Errorf("message exceeds %d bytes", maxTaskMessageBytes)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	task, ok := m.tasks[id]
	if !ok {
		return Task{}, os.ErrNotExist
	}
	if task.Terminal() {
		return m.visibleTaskLocked(task), errors.New("terminal task cannot be updated")
	}
	task.Progress = progress
	task.Message = message
	task.UpdatedAt = time.Now().UTC()
	if err := m.persistLocked(task); err != nil {
		return Task{}, err
	}
	return cloneTask(task), nil
}

func (m *Manager) Wait(ctx context.Context, id string) (Task, error) {
	ch := make(chan struct{})
	m.mu.Lock()
	task, ok := m.tasks[id]
	if !ok {
		m.mu.Unlock()
		return Task{}, os.ErrNotExist
	}
	if task.Terminal() && !m.completing[id] {
		copy := cloneTask(task)
		m.mu.Unlock()
		return copy, nil
	}
	m.waiters[id] = append(m.waiters[id], ch)
	m.mu.Unlock()
	select {
	case <-ctx.Done():
		m.mu.Lock()
		waiters := m.waiters[id]
		for index, waiter := range waiters {
			if waiter == ch {
				m.waiters[id] = append(waiters[:index], waiters[index+1:]...)
				break
			}
		}
		if len(m.waiters[id]) == 0 {
			delete(m.waiters, id)
		}
		m.mu.Unlock()
		return Task{}, ctx.Err()
	case <-ch:
		task, ok := m.Get(id)
		if !ok {
			return Task{}, os.ErrNotExist
		}
		return task, nil
	}
}

func (m *Manager) Close() {
	m.closeOnce.Do(func() {
		close(m.closed)
		m.mu.Lock()
		for _, cancel := range m.cancel {
			cancel()
		}
		m.mu.Unlock()
	})
}

func (m *Manager) run(id string) {
	select {
	case m.totalSem <- struct{}{}:
		defer func() { <-m.totalSem }()
	case <-m.closed:
		m.finishCancelled(id, "服务正在停止")
		return
	}
	m.mu.RLock()
	task, ok := m.tasks[id]
	heavy := ok && task.Heavy
	m.mu.RUnlock()
	if !ok {
		return
	}
	if heavy {
		select {
		case m.heavySem <- struct{}{}:
			defer func() { <-m.heavySem }()
		case <-m.closed:
			m.finishCancelled(id, "服务正在停止")
			return
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	m.mu.Lock()
	task = m.tasks[id]
	if task == nil {
		m.mu.Unlock()
		cancel()
		return
	}
	if task.Status == StatusCancelling {
		m.mu.Unlock()
		cancel()
		m.finishCancelled(id, "任务在启动前取消")
		return
	}
	select {
	case <-m.closed:
		m.mu.Unlock()
		cancel()
		m.finishCancelled(id, "服务正在停止")
		return
	default:
	}
	now := time.Now().UTC()
	task.Status = StatusRunning
	task.Message = "正在执行"
	task.StartedAt = now
	task.UpdatedAt = now
	m.cancel[id] = cancel
	if err := m.persistLocked(task); err != nil {
		delete(m.cancel, id)
		task.Status = StatusFailed
		task.Message = "无法持久化 running 状态，为防止重复执行，本次未启动"
		task.CompletedAt = now
		failure := model.Failure("TASK_STATE_PERSIST_FAILED", task.Message, "PersistenceError", err.Error())
		failure.TaskID = id
		task.Result = &failure
		completed := cloneTask(task)
		m.completing[id] = true
		m.mu.Unlock()
		cancel()
		_ = m.commitCompletion(completed)
		return
	}
	snapshot := cloneTask(task)
	m.mu.Unlock()

	reporter := &taskReporter{manager: m, id: id}
	result := m.runner(ctx, snapshot, reporter)
	wasCancelled := errors.Is(ctx.Err(), context.Canceled)
	cancel()

	m.mu.Lock()
	delete(m.cancel, id)
	task = m.tasks[id]
	if task == nil {
		m.mu.Unlock()
		return
	}
	now = time.Now().UTC()
	task.UpdatedAt = now
	task.CompletedAt = now
	task.Result = &result
	switch {
	case task.Status == StatusCancelling || wasCancelled:
		task.Status = StatusCancelled
		task.Message = "已取消"
	case result.Success:
		task.Status = StatusSucceeded
		task.Progress = 1
		task.Message = result.Message
	case result.Code == "TIMEOUT":
		task.Status = StatusTimedOut
		task.Message = result.Message
	default:
		task.Status = StatusFailed
		task.Message = result.Message
	}
	completed := cloneTask(task)
	m.completing[id] = true
	m.mu.Unlock()
	_ = m.commitCompletion(completed)
}

func (m *Manager) finishCancelled(id, message string) {
	m.mu.Lock()
	task := m.tasks[id]
	if task == nil || task.Terminal() {
		m.mu.Unlock()
		return
	}
	now := time.Now().UTC()
	task.Status = StatusCancelled
	task.Message = message
	task.UpdatedAt = now
	task.CompletedAt = now
	result := model.Failure("CANCELLED", message, "Cancelled", message)
	task.Result = &result
	completed := cloneTask(task)
	m.completing[id] = true
	m.mu.Unlock()
	_ = m.commitCompletion(completed)
}

func readTaskSnapshot(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxTaskSnapshotBytes {
		return nil, fmt.Errorf("%w: %s", errSnapshotLimit, filepath.Base(path))
	}
	raw, err := io.ReadAll(io.LimitReader(file, maxTaskSnapshotBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxTaskSnapshotBytes {
		return nil, fmt.Errorf("%w: %s", errSnapshotLimit, filepath.Base(path))
	}
	return raw, nil
}

func quarantineTaskSnapshot(path string) error {
	bad := path + ".corrupt-" + time.Now().UTC().Format("20060102T150405.000000000Z")
	return os.Rename(path, bad)
}

func (m *Manager) load() error {
	entries, err := os.ReadDir(m.dir)
	if err != nil {
		return fmt.Errorf("read task directory: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		path := filepath.Join(m.dir, entry.Name())
		data, err := readTaskSnapshot(path)
		if err != nil {
			if errors.Is(err, errSnapshotLimit) {
				if renameErr := quarantineTaskSnapshot(path); renameErr != nil {
					return fmt.Errorf("quarantine task snapshot %s: %w", path, renameErr)
				}
				continue
			}
			return fmt.Errorf("read task snapshot %s: %w", path, err)
		}
		var task Task
		if err := json.Unmarshal(data, &task); err != nil {
			if renameErr := quarantineTaskSnapshot(path); renameErr != nil {
				return fmt.Errorf("quarantine task snapshot %s: %w", path, renameErr)
			}
			continue
		}
		if len(task.Arguments) > maxTaskArgumentsBytes || len(task.CompletionArguments) > maxCompletionRetryBytes || len(task.CompletionArguments) > 0 && !json.Valid(task.CompletionArguments) {
			if renameErr := quarantineTaskSnapshot(path); renameErr != nil {
				return fmt.Errorf("quarantine oversized task snapshot %s: %w", path, renameErr)
			}
			continue
		}
		if task.ID == "" || task.Tool == "" {
			continue
		}
		if task.Status == StatusRunning || task.Status == StatusCancelling || task.Status == StatusQueued {
			now := time.Now().UTC()
			task.Status = StatusInterrupted
			task.Message = "上次进程退出时任务尚未完成"
			task.UpdatedAt = now
			if !task.Durable {
				task.Status = StatusFailed
				task.CompletedAt = now
				failed := model.Failure("PROCESS_RESTARTED", task.Message, "Interrupted", task.Message)
				task.Result = &failed
			}
			if err := m.persistLocked(&task); err != nil {
				return err
			}
		}
		copy := task
		m.tasks[task.ID] = &copy
	}
	return m.pruneTerminalLocked("")
}

func (m *Manager) pruneTerminalLocked(protectedID string) error {
	type candidate struct {
		id        string
		completed time.Time
		bytes     int64
	}
	items := make([]candidate, 0)
	var totalBytes int64
	for id, task := range m.tasks {
		if !task.Terminal() {
			continue
		}
		size := int64(0)
		if info, err := os.Stat(filepath.Join(m.dir, id+".json")); err == nil {
			size += info.Size()
		}
		if info, err := os.Stat(filepath.Join(m.dir, id+".jsonl")); err == nil {
			size += info.Size()
		}
		totalBytes += size
		completed := task.CompletedAt
		if completed.IsZero() {
			completed = task.UpdatedAt
		}
		items = append(items, candidate{id: id, completed: completed, bytes: size})
	}
	if len(items) <= maxTerminalTasks && totalBytes <= maxTerminalSnapshotBytes {
		return nil
	}
	slices.SortFunc(items, func(a, b candidate) int {
		if a.completed.Before(b.completed) {
			return -1
		}
		if a.completed.After(b.completed) {
			return 1
		}
		return strings.Compare(a.id, b.id)
	})
	remaining := len(items)
	var first error
	for _, item := range items {
		if remaining <= terminalTaskLowWater && totalBytes <= terminalBytesLowWater {
			break
		}
		if item.id == protectedID || len(m.waiters[item.id]) > 0 {
			continue
		}
		// Remove the optional log first. Never delete the authoritative
		// snapshot if log cleanup failed, so retry remains possible.
		if err := os.Remove(filepath.Join(m.dir, item.id+".jsonl")); err != nil && !os.IsNotExist(err) {
			first = err
			break
		}
		if err := os.Remove(filepath.Join(m.dir, item.id+".json")); err != nil && !os.IsNotExist(err) {
			first = err
			break
		}
		delete(m.tasks, item.id)
		delete(m.cancel, item.id)
		remaining--
		totalBytes -= item.bytes
	}
	return first
}

func (m *Manager) persistLocked(task *Task) error {
	snapshot := cloneTask(task)
	if snapshot.Terminal() {
		// Completion memory is written by the hook before normal terminal
		// persistence. Raw resumable arguments are no longer needed afterward
		// and must not become a long-lived secret-bearing task snapshot.
		snapshot.Arguments = nil
		if snapshot.Result != nil {
			snapshot.Result.Stdout = ""
			snapshot.Result.Stderr = ""
			snapshot.Result.Meta = nil
			if snapshot.Result.Error != nil {
				snapshot.Result.Error.Details = "[redacted after terminal persistence]"
				snapshot.Result.Error.Fields = map[string]any{}
			}
		}
	}
	data, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return fmt.Errorf("encode task %s: %w", task.ID, err)
	}
	data = append(data, '\n')
	if len(data) > maxTaskSnapshotBytes {
		err = fmt.Errorf("%w: task snapshot exceeds %d bytes", ErrStorageFull, maxTaskSnapshotBytes)
	} else {
		path := filepath.Join(m.dir, task.ID+".json")
		total, sizeErr := m.taskStorageBytesLocked()
		if sizeErr != nil {
			err = sizeErr
		} else {
			current := int64(0)
			if info, statErr := os.Stat(path); statErr == nil {
				current = info.Size()
			} else if !os.IsNotExist(statErr) {
				err = statErr
			}
			if err == nil && total-current+int64(len(data)) > maxTaskStorageBytes {
				err = fmt.Errorf("%w: task directory exceeds %d bytes", ErrStorageFull, maxTaskStorageBytes)
			}
			if err == nil {
				err = atomicfile.Write(path, data, 0o600)
			}
		}
	}
	if err != nil {
		m.lastPersistenceError = err.Error()
	} else {
		m.lastPersistenceError = ""
	}
	return err
}

func (m *Manager) taskStorageBytesLocked() (int64, error) {
	entries, err := os.ReadDir(m.dir)
	if err != nil {
		return 0, err
	}
	var total int64
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			return 0, infoErr
		}
		if info.Mode().IsRegular() {
			total += info.Size()
			if total > maxTaskStorageBytes {
				return total, nil
			}
		}
	}
	return total, nil
}

func (m *Manager) notifyLocked(id string) {
	for _, waiter := range m.waiters[id] {
		close(waiter)
	}
	delete(m.waiters, id)
}

type taskReporter struct {
	manager *Manager
	id      string
}

func (r *taskReporter) Progress(value float64, message string) error {
	_, err := r.manager.Update(r.id, value, message)
	return err
}

func (r *taskReporter) Log(stream, text string) error {
	if stream != "stdout" && stream != "stderr" && stream != "event" {
		return errors.New("invalid task log stream")
	}
	line, err := json.Marshal(map[string]any{
		"time":   time.Now().UTC(),
		"stream": stream,
		"text":   text,
	})
	if err != nil {
		return err
	}
	path := filepath.Join(r.manager.dir, r.id+".jsonl")
	r.manager.mu.Lock()
	defer r.manager.mu.Unlock()
	if info, statErr := os.Stat(path); statErr == nil && info.Size()+int64(len(line))+1 > maxTaskLogBytes {
		return errors.New("task log retention limit reached")
	} else if statErr != nil && !os.IsNotExist(statErr) {
		return statErr
	}
	if int64(len(line))+1 > maxTaskLogBytes {
		return errors.New("task log entry exceeds retention limit")
	}
	total, err := r.manager.taskStorageBytesLocked()
	if err != nil {
		return err
	}
	if total+int64(len(line))+1 > maxTaskStorageBytes {
		return ErrStorageFull
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return err
	}
	return f.Sync()
}

func cloneTask(task *Task) Task {
	copy := *task
	if task.Arguments != nil {
		copy.Arguments = append(json.RawMessage(nil), task.Arguments...)
	}
	if task.CompletionArguments != nil {
		copy.CompletionArguments = append(json.RawMessage(nil), task.CompletionArguments...)
	}
	if task.Result != nil {
		resultCopy := *task.Result
		if task.Result.Error != nil {
			errorCopy := *task.Result.Error
			if task.Result.Error.Fields != nil {
				errorCopy.Fields = make(map[string]any, len(task.Result.Error.Fields))
				for key, value := range task.Result.Error.Fields {
					errorCopy.Fields[key] = value
				}
			}
			resultCopy.Error = &errorCopy
		}
		resultCopy.Artifacts = append([]model.Artifact(nil), task.Result.Artifacts...)
		if task.Result.Meta != nil {
			resultCopy.Meta = make(map[string]any, len(task.Result.Meta))
			for key, value := range task.Result.Meta {
				resultCopy.Meta[key] = value
			}
		}
		copy.Result = &resultCopy
	}
	return copy
}

func newID() (string, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", fmt.Errorf("generate task id: %w", err)
	}
	// UUIDv4 shape, generated locally so no database or dependency is needed.
	bytes[6] = (bytes[6] & 0x0f) | 0x40
	bytes[8] = (bytes[8] & 0x3f) | 0x80
	raw := hex.EncodeToString(bytes[:])
	return raw[0:8] + "-" + raw[8:12] + "-" + raw[12:16] + "-" + raw[16:20] + "-" + raw[20:32], nil
}
