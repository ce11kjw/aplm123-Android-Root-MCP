package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zcr521/android-ai-mcp/internal/config"
	"github.com/zcr521/android-ai-mcp/internal/knowledge"
	"github.com/zcr521/android-ai-mcp/internal/model"
	"github.com/zcr521/android-ai-mcp/internal/tasks"
)

func TestCompletedCallsLearnButAcceptanceAndControlCallsDoNot(t *testing.T) {
	root := t.TempDir()
	cfg := config.Default()
	cfg.Paths.StateDir = filepath.Join(root, "state")
	cfg.Paths.WorkDir = filepath.Join(root, "work")
	cfg.Paths.DownloadsDir = filepath.Join(cfg.Paths.WorkDir, "downloads")
	cfg.Paths.UploadsDir = filepath.Join(cfg.Paths.WorkDir, "uploads")
	cfg.Paths.ArtifactsDir = filepath.Join(cfg.Paths.WorkDir, "output")
	cfg.Paths.TempDir = filepath.Join(cfg.Paths.StateDir, "tmp")
	if err := config.EnsureDirectories(cfg); err != nil {
		t.Fatal(err)
	}
	service, err := New(filepath.Join(cfg.Paths.StateDir, "config.json"), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	ctx := context.Background()
	written := service.Call(ctx, "zcr521_fs_write", map[string]any{"action": "create", "path": "learn.txt", "content": "learn me", "createParents": true})
	if !written.Success {
		t.Fatalf("write failed: %+v", written)
	}
	accepted := service.Call(ctx, "zcr521_archive", map[string]any{"action": "create", "destination": "learn.zip", "sources": []string{"learn.txt"}, "format": "zip", "background": true})
	if !accepted.Success || accepted.TaskID == "" {
		t.Fatalf("background not accepted: %+v", accepted)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		state := service.Call(ctx, "zcr521_task", map[string]any{"action": "get", "taskId": accepted.TaskID})
		raw, _ := json.Marshal(state.Data)
		if strings.Contains(string(raw), `"arguments"`) {
			t.Fatalf("task get exposed durable arguments: %s", raw)
		}
		var task struct {
			Status string `json:"status"`
		}
		_ = json.Unmarshal(raw, &task)
		if task.Status == "succeeded" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("background task did not finish: %s", raw)
		}
		time.Sleep(20 * time.Millisecond)
	}
	listed := service.Call(ctx, "zcr521_task", map[string]any{"action": "list", "limit": float64(100)})
	listedRaw, _ := json.Marshal(listed.Data)
	if strings.Contains(string(listedRaw), `"arguments"`) {
		t.Fatalf("task list exposed durable arguments: %s", listedRaw)
	}
	snapshot, err := os.ReadFile(filepath.Join(cfg.Paths.StateDir, "tasks", accepted.TaskID+".json"))
	if err != nil || strings.Contains(string(snapshot), `"arguments"`) {
		t.Fatalf("terminal task snapshot retained arguments: %s err=%v", snapshot, err)
	}
	stats := service.Call(ctx, "zcr521_skill", map[string]any{"action": "stats"})
	if !stats.Success || stats.Data.(map[string]any)["receipts"] != 2 {
		t.Fatalf("unexpected learning stats: %+v", stats)
	}
	web := service.Call(ctx, "zcr521_network", map[string]any{"action": "interfaces"})
	hints, ok := web.Meta["skillHints"].([]knowledge.Match)
	if !ok || len(hints) == 0 {
		t.Fatalf("web call received no automatic skill hints: %+v", web.Meta)
	}
}

func TestSensitiveBackgroundArgumentsAreRejectedAndNeverPersisted(t *testing.T) {
	root := t.TempDir()
	cfg := config.Default()
	cfg.Paths.StateDir = filepath.Join(root, "state")
	cfg.Paths.WorkDir = filepath.Join(root, "work")
	cfg.Paths.DownloadsDir = ""
	cfg.Paths.UploadsDir = ""
	cfg.Paths.ArtifactsDir = ""
	cfg.Paths.TempDir = ""
	cfg = config.Normalize(cfg)
	if err := config.EnsureDirectories(cfg); err != nil {
		t.Fatal(err)
	}
	service, err := New(filepath.Join(cfg.Paths.StateDir, "config.json"), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	secret := "never-persist-this-secret"
	result := service.Call(context.Background(), "zcr521_shell", map[string]any{"action": "exec", "command": "printf " + secret, "background": true})
	if result.Success || result.Code != "BACKGROUND_SENSITIVE_ARGUMENTS_UNSUPPORTED" || result.TaskID != "" {
		t.Fatalf("sensitive background task was accepted: %+v", result)
	}
	script := service.Call(context.Background(), "zcr521_script", map[string]any{"action": "run", "script": "printf " + secret, "background": true})
	if script.Success || script.Code != "BACKGROUND_SENSITIVE_ARGUMENTS_UNSUPPORTED" {
		t.Fatalf("script-bearing background task was accepted: %+v", script)
	}
	pathToken := strings.Repeat("a", 64)
	network := service.Call(context.Background(), "zcr521_download", map[string]any{"action": "start", "url": "https://example.test/" + pathToken, "path": "blocked.bin", "background": true})
	if network.Success || network.Code != "BACKGROUND_SENSITIVE_ARGUMENTS_UNSUPPORTED" {
		t.Fatalf("opaque URL credential path was accepted: %+v", network)
	}
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() {
			return walkErr
		}
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if strings.Contains(string(raw), secret) || strings.Contains(string(raw), pathToken) {
			t.Fatalf("secret persisted in %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestRestartDoesNotReplayUncertainNetworkPOST(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	root := t.TempDir()
	cfg := config.Default()
	cfg.Paths.StateDir = filepath.Join(root, "state")
	cfg.Paths.WorkDir = filepath.Join(root, "work")
	cfg.Paths.DownloadsDir = ""
	cfg.Paths.UploadsDir = ""
	cfg.Paths.ArtifactsDir = ""
	cfg.Paths.TempDir = ""
	cfg = config.Normalize(cfg)
	if err := config.EnsureDirectories(cfg); err != nil {
		t.Fatal(err)
	}
	id := "00000000-0000-4000-8000-000000000088"
	rawArgs, _ := json.Marshal(map[string]any{"action": "http", "url": server.URL, "method": "POST", "body": "one-shot"})
	now := time.Now().UTC()
	stale := tasks.Task{ID: id, Tool: "zcr521_network", Arguments: rawArgs, Status: tasks.StatusRunning, Durable: true, CreatedAt: now, StartedAt: now, UpdatedAt: now}
	rawTask, _ := json.Marshal(stale)
	taskDir := filepath.Join(cfg.Paths.StateDir, "tasks")
	if err := os.MkdirAll(taskDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(taskDir, id+".json"), rawTask, 0o600); err != nil {
		t.Fatal(err)
	}
	service, err := New(filepath.Join(cfg.Paths.StateDir, "config.json"), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	settled, ok := service.tasks.Get(id)
	if !ok || settled.Result == nil || settled.Result.Code != "RESTART_REVIEW_REQUIRED" || calls.Load() != 0 {
		t.Fatalf("uncertain POST was replayed or not settled: task=%+v calls=%d", settled, calls.Load())
	}
}

func TestRestartDoesNotReexecuteCompletedOrUncertainDestructiveTask(t *testing.T) {
	for _, journaled := range []bool{true, false} {
		t.Run(map[bool]string{true: "journaled", false: "uncertain"}[journaled], func(t *testing.T) {
			root := t.TempDir()
			cfg := config.Default()
			cfg.Paths.StateDir = filepath.Join(root, "state")
			cfg.Paths.WorkDir = filepath.Join(root, "work")
			cfg.Paths.DownloadsDir = filepath.Join(cfg.Paths.WorkDir, "downloads")
			cfg.Paths.UploadsDir = filepath.Join(cfg.Paths.WorkDir, "uploads")
			cfg.Paths.ArtifactsDir = filepath.Join(cfg.Paths.WorkDir, "output")
			cfg.Paths.TempDir = filepath.Join(cfg.Paths.StateDir, "tmp")
			if err := config.EnsureDirectories(cfg); err != nil {
				t.Fatal(err)
			}
			id := "00000000-0000-4000-8000-000000000099"
			args := map[string]any{"action": "create", "path": "must-not-run.txt", "content": "unsafe"}
			rawArgs, _ := json.Marshal(args)
			now := time.Now().UTC()
			stale := tasks.Task{ID: id, Tool: "zcr521_fs_write", Arguments: rawArgs, Status: tasks.StatusRunning, Durable: true, CreatedAt: now, StartedAt: now, UpdatedAt: now}
			rawTask, _ := json.Marshal(stale)
			taskDir := filepath.Join(cfg.Paths.StateDir, "tasks")
			if err := os.MkdirAll(taskDir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(taskDir, id+".json"), rawTask, 0o600); err != nil {
				t.Fatal(err)
			}
			if journaled {
				completed := model.Success("OK", "already done", nil)
				completed.TaskID = id
				if err := knowledge.New(cfg.Paths.StateDir, cfg.Paths.WorkDir).Record(stale.Tool, args, completed); err != nil {
					t.Fatal(err)
				}
			}
			service, err := New(filepath.Join(cfg.Paths.StateDir, "config.json"), cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer service.Close()
			settled, ok := service.tasks.Get(id)
			if !ok || !settled.Terminal() || settled.Result == nil {
				t.Fatalf("interrupted destructive task was not settled: %+v", settled)
			}
			if journaled && settled.Status != tasks.StatusSucceeded {
				t.Fatalf("journaled terminal status=%s", settled.Status)
			}
			if !journaled && settled.Result.Code != "RESTART_REVIEW_REQUIRED" {
				t.Fatalf("uncertain destructive result=%+v", settled.Result)
			}
			if _, err := os.Stat(filepath.Join(cfg.Paths.WorkDir, "must-not-run.txt")); !os.IsNotExist(err) {
				t.Fatalf("destructive task was re-executed: %v", err)
			}
		})
	}
}
