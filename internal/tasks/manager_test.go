package tasks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zcr521/android-ai-mcp/internal/model"
)

func TestCompletionHookFailureKeepsOnlyBoundedRetryEnvelope(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tasks")
	manager, err := New(dir, 1, 1, func(context.Context, Task, Reporter) model.Result {
		return model.Success("OK", "done", nil)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	manager.SetCompletionHook(func(Task) (json.RawMessage, error) {
		return json.RawMessage(`{"action":"calculate","path":{"redacted":true,"type":"path"}}`), errors.New("journal unavailable")
	})
	task, err := manager.Submit("zcr521_fs_hash", map[string]any{"action": "calculate", "path": "never-persist-raw"}, false, true)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done, err := manager.Wait(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !done.CompletionPending || len(done.Arguments) != 0 || string(done.CompletionArguments) == "" {
		t.Fatalf("completion retry state=%+v", done)
	}
	raw, err := os.ReadFile(filepath.Join(dir, task.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) == "" || json.Valid(raw) == false || bytes.Contains(raw, []byte("never-persist-raw")) {
		t.Fatalf("terminal snapshot retained raw arguments: %s", raw)
	}
	manager.SetCompletionHook(func(Task) (json.RawMessage, error) { return nil, nil })
	_ = manager.ReconcileCompletions()
	deadline := time.Now().Add(3 * time.Second)
	for {
		reconciled, _ := manager.Get(task.ID)
		if !reconciled.CompletionPending && len(reconciled.CompletionArguments) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("completion retry envelope was not cleared: %+v", reconciled)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestCompletionIsNotVisibleUntilHookAndSnapshotCommit(t *testing.T) {
	manager, err := New(filepath.Join(t.TempDir(), "tasks"), 1, 1, func(context.Context, Task, Reporter) model.Result {
		return model.Success("OK", "done", nil)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	started := make(chan struct{})
	release := make(chan struct{})
	manager.SetCompletionHook(func(Task) (json.RawMessage, error) {
		close(started)
		<-release
		return nil, nil
	})
	task, err := manager.Submit("zcr521_fs_hash", map[string]any{"action": "calculate", "path": "x"}, false, true)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("completion hook was not reached")
	}
	visible, ok := manager.Get(task.ID)
	if !ok || visible.Terminal() || visible.Result != nil {
		t.Fatalf("terminal task became visible before commit: %+v", visible)
	}
	for _, listed := range manager.List() {
		if listed.ID == task.ID && (listed.Terminal() || listed.Result != nil) {
			t.Fatalf("list exposed uncommitted terminal task: %+v", listed)
		}
	}
	waited := make(chan Task, 1)
	waitErr := make(chan error, 1)
	go func() {
		result, err := manager.Wait(context.Background(), task.ID)
		if err != nil {
			waitErr <- err
			return
		}
		waited <- result
	}()
	select {
	case result := <-waited:
		t.Fatalf("wait returned before completion commit: %+v", result)
	case err := <-waitErr:
		t.Fatalf("wait failed before completion commit: %v", err)
	case <-time.After(75 * time.Millisecond):
	}
	close(release)
	select {
	case result := <-waited:
		if !result.Terminal() || result.Result == nil {
			t.Fatalf("committed completion not visible: %+v", result)
		}
	case err := <-waitErr:
		t.Fatal(err)
	case <-time.After(3 * time.Second):
		t.Fatal("wait was not released after completion commit")
	}
}

func TestPendingCompletionRetriesAutomatically(t *testing.T) {
	manager, err := New(filepath.Join(t.TempDir(), "tasks"), 1, 1, func(context.Context, Task, Reporter) model.Result {
		return model.Success("OK", "done", nil)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	var attempts atomic.Int32
	manager.SetCompletionHook(func(Task) (json.RawMessage, error) {
		if attempts.Add(1) == 1 {
			return json.RawMessage(`{"action":"calculate","path":{"redacted":true,"type":"path"}}`), errors.New("temporary journal failure")
		}
		return nil, nil
	})
	task, err := manager.Submit("zcr521_fs_hash", map[string]any{"action": "calculate", "path": "x"}, false, true)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	first, err := manager.Wait(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !first.CompletionPending || manager.LastPersistenceError() == "" {
		t.Fatalf("pending failure was not published safely: %+v", first)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		got, _ := manager.Get(task.ID)
		if !got.CompletionPending && attempts.Load() >= 2 {
			if manager.LastPersistenceError() != "" {
				t.Fatalf("successful retry left a stale error: %s", manager.LastPersistenceError())
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("pending completion was not retried automatically: attempts=%d task=%+v", attempts.Load(), got)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestPendingCompletionErrorSurvivesUnrelatedSuccess(t *testing.T) {
	manager, err := New(filepath.Join(t.TempDir(), "tasks"), 1, 1, func(context.Context, Task, Reporter) model.Result {
		return model.Success("OK", "done", nil)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	manager.SetCompletionHook(func(task Task) (json.RawMessage, error) {
		if task.Tool == "always_fail" {
			return json.RawMessage(`{"action":"run"}`), errors.New("journal remains unavailable")
		}
		return nil, nil
	})
	failed, err := manager.Submit("always_fail", map[string]any{"action": "run"}, false, true)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := manager.Wait(ctx, failed.ID); err != nil {
		t.Fatal(err)
	}
	succeeded, err := manager.Submit("succeeds", map[string]any{"action": "run"}, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Wait(ctx, succeeded.ID); err != nil {
		t.Fatal(err)
	}
	if manager.LastPersistenceError() == "" {
		t.Fatal("an unrelated successful completion hid the pending failure")
	}
}

func TestRestartRecoveryIsBounded(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tasks")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	const total = 20
	for index := 0; index < total; index++ {
		id := fmt.Sprintf("00000000-0000-4000-8000-%012d", index)
		task := Task{
			ID: id, Tool: "zcr521_fs_hash", Arguments: json.RawMessage(`{"action":"calculate","path":"x"}`),
			Status: StatusRunning, Durable: true, CreatedAt: now.Add(time.Duration(index) * time.Millisecond), UpdatedAt: now,
		}
		raw, err := json.Marshal(task)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, id+".json"), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	release := make(chan struct{})
	manager, err := New(dir, 1, 1, func(ctx context.Context, _ Task, _ Reporter) model.Result {
		select {
		case <-release:
			return model.Success("OK", "done", nil)
		case <-ctx.Done():
			return model.Failure("CANCELLED", "cancelled", "Cancelled", "cancelled")
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	released := false
	defer func() {
		if !released {
			close(release)
		}
		manager.Close()
	}()
	if resumed := manager.ResumeInterrupted(); resumed != manager.outstandingLimit {
		t.Fatalf("resumed=%d, limit=%d", resumed, manager.outstandingLimit)
	}
	overflow := 0
	for _, task := range manager.List() {
		if task.Result != nil && task.Result.Code == "TASK_RECOVERY_QUEUE_LIMIT" {
			overflow++
		}
	}
	if want := total - manager.outstandingLimit; overflow != want {
		t.Fatalf("overflow failures=%d, want %d", overflow, want)
	}
	close(release)
	released = true
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, task := range manager.List() {
		if _, err := manager.Wait(ctx, task.ID); err != nil {
			t.Fatalf("wait for recovered task %s: %v", task.ID, err)
		}
	}
}

func TestOversizedSnapshotIsQuarantinedWithoutReadingIt(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tasks")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "oversized.json")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(maxTaskSnapshotBytes + 1); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	manager, err := New(dir, 1, 1, func(context.Context, Task, Reporter) model.Result {
		return model.Success("OK", "done", nil)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	if len(manager.List()) != 0 {
		t.Fatal("oversized task snapshot was loaded")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("oversized task snapshot was not quarantined: %v", err)
	}
	quarantined, err := filepath.Glob(path + ".corrupt-*")
	if err != nil || len(quarantined) != 1 {
		t.Fatalf("quarantine files=%v err=%v", quarantined, err)
	}
}

func TestOutstandingTaskQueueIsBounded(t *testing.T) {
	manager, err := New(filepath.Join(t.TempDir(), "tasks"), 1, 1, func(ctx context.Context, _ Task, _ Reporter) model.Result {
		<-ctx.Done()
		return model.Failure("CANCELLED", "cancelled", "Cancelled", "cancelled")
	})
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, manager.outstandingLimit)
	for index := 0; index < manager.outstandingLimit; index++ {
		task, err := manager.Submit("zcr521_fs_hash", map[string]any{"action": "calculate", "path": "x"}, false, true)
		if err != nil {
			t.Fatalf("submit %d: %v", index, err)
		}
		ids = append(ids, task.ID)
	}
	if _, err := manager.Submit("zcr521_fs_hash", map[string]any{"action": "calculate", "path": "x"}, false, true); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("unbounded queue error=%v", err)
	}
	manager.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, id := range ids {
		if _, err := manager.Wait(ctx, id); err != nil {
			t.Fatalf("wait for %s: %v", id, err)
		}
	}
}

func TestCanceledWaitDoesNotLeakWaiters(t *testing.T) {
	manager, err := New(filepath.Join(t.TempDir(), "tasks"), 1, 1, func(ctx context.Context, _ Task, _ Reporter) model.Result {
		<-ctx.Done()
		return model.Failure("CANCELLED", "cancelled", "Cancelled", "cancelled")
	})
	if err != nil {
		t.Fatal(err)
	}
	task, err := manager.Submit("zcr521_fs_hash", map[string]any{"action": "calculate", "path": "x"}, false, true)
	if err != nil {
		t.Fatal(err)
	}
	for range 32 {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := manager.Wait(ctx, task.ID); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled wait error=%v", err)
		}
	}
	manager.mu.RLock()
	waiters := len(manager.waiters[task.ID])
	manager.mu.RUnlock()
	if waiters != 0 {
		t.Fatalf("cancelled waits leaked %d waiter(s)", waiters)
	}
	manager.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := manager.Wait(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
}

func TestSubmitPersistsAndCompletes(t *testing.T) {
	manager, err := New(filepath.Join(t.TempDir(), "tasks"), 2, 1, func(ctx context.Context, task Task, reporter Reporter) model.Result {
		_ = reporter.Progress(0.5, "一半")
		_ = reporter.Log("stdout", "ok")
		return model.Success("OK", "完成", map[string]any{"tool": task.Tool})
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	task, err := manager.Submit("zcr521_fs_hash", map[string]any{"path": "x"}, true, true)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done, err := manager.Wait(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != StatusSucceeded || done.Result == nil || !done.Result.Success {
		t.Fatalf("unexpected result: %#v", done)
	}
	data, err := json.Marshal(done)
	if err != nil || len(data) == 0 {
		t.Fatal("task is not serializable")
	}
}

func TestDurableInterruptedTaskResumes(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tasks")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	task := Task{
		ID:        "00000000-0000-4000-8000-000000000001",
		Tool:      "zcr521_download",
		Arguments: json.RawMessage(`{"url":"x"}`),
		Status:    StatusRunning,
		Heavy:     true,
		Durable:   true,
		CreatedAt: now,
		StartedAt: now,
		UpdatedAt: now,
	}
	raw, err := json.Marshal(task)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, task.ID+".json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}

	restored, err := New(dir, 1, 1, func(context.Context, Task, Reporter) model.Result {
		return model.Success("OK", "resumed", nil)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if count := restored.ResumeInterrupted(); count != 1 {
		t.Fatalf("expected one resumed task, got %d", count)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done, err := restored.Wait(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != StatusSucceeded {
		t.Fatalf("unexpected status: %s", done.Status)
	}
}

func TestTimeoutResultHasDistinctTerminalStatus(t *testing.T) {
	manager, err := New(filepath.Join(t.TempDir(), "tasks"), 1, 1, func(context.Context, Task, Reporter) model.Result {
		return model.Failure("TIMEOUT", "命令执行超时", "Timeout", "deadline exceeded")
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	task, err := manager.Submit("zcr521_shell", map[string]any{"command": "sleep 10"}, false, true)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done, err := manager.Wait(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != StatusTimedOut || !done.Terminal() {
		t.Fatalf("timeout status = %q, terminal=%v", done.Status, done.Terminal())
	}
}
