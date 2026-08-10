package knowledge

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zcr521/android-ai-mcp/internal/model"
)

func TestProgressiveBuiltinsAndRedactedLearning(t *testing.T) {
	root := t.TempDir()
	store := New(filepath.Join(root, "state"), filepath.Join(root, "work"))
	if got := len(store.List("builtin")); got != 2 {
		t.Fatalf("built-in skill count=%d, want 2", got)
	}
	matches := store.Match("Android 网页截图", 3)
	if len(matches) == 0 {
		t.Fatal("Android web query matched no built-in skill")
	}
	if _, body, err := store.Get(matches[0].ID); err != nil || !strings.Contains(body, "# ") {
		t.Fatalf("progressive get failed: %v", err)
	}
	args := map[string]any{
		"action":        "http",
		"Authorization": "Bearer super-secret-token",
		"headers":       map[string]any{"Cookie": "sid=super-secret-cookie"},
		"body":          "super-secret-body",
		"value":         "super-secret-cookie-value",
		"message":       "super-secret-websocket-message",
		"text":          "super-secret-text",
		"command":       "echo super-secret-command",
		"cmd":           "echo super-secret-cmd",
		"script":        "echo super-secret-script",
		"query":         "super-secret-query",
		"reason":        "super-secret-reason",
		"pattern":       "super-secret-pattern",
		"custom":        map[string]any{"privateKey": "super-secret-private-key", "bearer": "super-secret-bearer"},
		"url":           "https://user:super-secret@example.test/private-token?access_token=super-secret",
		"path":          "/private/user/project/super-secret/report.apk",
		"host":          "private-super-secret.internal:8443",
	}
	result := model.Success("OK", "request completed", map[string]any{"ignored": "large output is not learned"})
	result.Strategy = "test_http"
	result.Artifacts = []model.Artifact{{Path: "/private/output/archive.super-secret-extension"}}
	for range 2 {
		if err := store.Record("zcr521_network", args, result); err != nil {
			t.Fatal(err)
		}
	}
	stats := store.Stats()
	if stats["receipts"] != 2 {
		t.Fatalf("receipt count=%v, want 2", stats["receipts"])
	}
	learned := store.List("learned")
	if len(learned) != 1 || learned[0].Attempts != 2 || learned[0].Successes != 2 {
		t.Fatalf("learned index=%+v", learned)
	}
	entry, skillBody, err := store.Get(learned[0].ID)
	if err != nil || entry.Source != "learned" {
		t.Fatalf("learned get: entry=%+v err=%v", entry, err)
	}
	if strings.Contains(skillBody, "super-secret") || len(entry.LastArtifacts) != 1 || entry.LastArtifacts[0] != "file" {
		t.Fatal("learned SKILL.md contains a secret")
	}
	receipts, err := filepath.Glob(filepath.Join(root, "state", "MCP", "invocations", "*.json"))
	if err != nil || len(receipts) != 2 {
		t.Fatalf("receipts=%v err=%v", receipts, err)
	}
	for _, path := range receipts {
		raw, readErr := os.ReadFile(path)
		if readErr != nil || strings.Contains(string(raw), "super-secret") || !strings.Contains(string(raw), `"redacted": true`) {
			t.Fatalf("unsafe receipt %s: err=%v body=%s", path, readErr, raw)
		}
	}
	mirror := filepath.Join(root, "work", "MCP", "skills", filepath.FromSlash(learned[0].ID), "SKILL.md")
	if _, err := os.Stat(mirror); err != nil {
		t.Fatalf("MCP skill mirror missing: %v", err)
	}
}

func TestBuiltinTriggerMatrixUsesExactToolMappings(t *testing.T) {
	store := New(filepath.Join(t.TempDir(), "state"), filepath.Join(t.TempDir(), "work"))
	tests := map[string]string{
		"zcr521_network web_extract":     "builtin/android-web-automation",
		"zcr521_network browser_capture": "builtin/android-web-automation",
		"zcr521_network feed_parse":      "builtin/android-web-automation",
		"zcr521_network cookie_list":     "builtin/android-web-automation",
		"zcr521_network ws_connect":      "builtin/android-web-automation",
		"zcr521_status get":              "builtin/mcp-efficient-workflow",
		"zcr521_task get":                "builtin/mcp-efficient-workflow",
		"zcr521_transfer_upload create":  "builtin/mcp-efficient-workflow",
		"zcr521_backup restore":          "builtin/mcp-efficient-workflow",
		"zcr521_shell run":               "builtin/mcp-efficient-workflow",
	}
	for query, expected := range tests {
		matches := store.Match(query, 1)
		if len(matches) != 1 || matches[0].ID != expected {
			t.Errorf("%q => %+v, want %s", query, matches, expected)
		}
	}
}

func TestForgetOnlyRemovesLearnedSkill(t *testing.T) {
	root := t.TempDir()
	store := New(filepath.Join(root, "state"), filepath.Join(root, "work"))
	if err := store.Forget("builtin/mcp-efficient-workflow"); err == nil {
		t.Fatal("built-in skill deletion was allowed")
	}
	if err := store.Record("zcr521_status", map[string]any{"action": "get"}, model.Success("OK", "ok", nil)); err != nil {
		t.Fatal(err)
	}
	id := store.List("learned")[0].ID
	if err := store.Forget(id); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Get(id); !os.IsNotExist(err) {
		t.Fatalf("forgotten skill remains: %v", err)
	}
	if store.Stats()["receipts"] != 1 {
		t.Fatal("forget unexpectedly deleted execution receipts")
	}
}

func TestBackgroundTaskReceiptIsIdempotentAcrossRestart(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, "state")
	work := filepath.Join(root, "work")
	result := model.Success("OK", "done", nil)
	result.TaskID = "task-123"
	first := New(state, work)
	if err := first.Record("zcr521_fs_hash", map[string]any{"action": "calculate", "path": "a"}, result); err != nil {
		t.Fatal(err)
	}
	second := New(state, work)
	if err := second.Record("zcr521_fs_hash", map[string]any{"action": "calculate", "path": "a"}, result); err != nil {
		t.Fatal(err)
	}
	if got := second.Stats()["receipts"]; got != 1 {
		t.Fatalf("duplicate background receipt count=%v", got)
	}
}

func TestReceiptRecoversMissingIndexSkillAndMirror(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, "state")
	work := filepath.Join(root, "work")
	result := model.Failure("IO_ERROR", "failed", "IOError", "private path")
	first := New(state, work)
	if err := first.Record("zcr521_fs_hash", map[string]any{"action": "calculate", "path": "a"}, result); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(state, "MCP", "skills")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(work, "MCP", "skills")); err != nil {
		t.Fatal(err)
	}
	recovered := New(state, work)
	learned := recovered.List("learned")
	if len(learned) != 1 || learned[0].Attempts != 1 || learned[0].FailureClasses["IOError"] != 1 {
		t.Fatalf("receipt recovery failed: %+v", learned)
	}
	if _, _, err := recovered.Get(learned[0].ID); err != nil {
		t.Fatalf("internal skill not rebuilt: %v", err)
	}
	if _, err := os.Stat(filepath.Join(work, "MCP", "skills", filepath.FromSlash(learned[0].ID), "SKILL.md")); err != nil {
		t.Fatalf("mirror not rebuilt: %v", err)
	}
}

func TestForgetTombstoneSurvivesRestart(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, "state")
	work := filepath.Join(root, "work")
	store := New(state, work)
	if err := store.Record("zcr521_status", map[string]any{"action": "get"}, model.Success("OK", "ok", nil)); err != nil {
		t.Fatal(err)
	}
	id := store.List("learned")[0].ID
	if err := store.Forget(id); err != nil {
		t.Fatal(err)
	}
	restarted := New(state, work)
	if got := restarted.List("learned"); len(got) != 0 {
		t.Fatalf("forgotten skill was resurrected from receipts: %+v", got)
	}
	if err := restarted.Record("zcr521_status", map[string]any{"action": "get"}, model.Success("OK", "new generation", nil)); err != nil {
		t.Fatal(err)
	}
	again := New(state, work)
	if got := again.List("learned"); len(got) != 1 || got[0].Attempts != 1 {
		t.Fatalf("pre-forget generation leaked into relearned skill: %+v", got)
	}
}

func TestForgetGenerationIgnoresFutureDatedOldReceipt(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, "state")
	work := filepath.Join(root, "work")
	store := New(state, work)
	if err := store.Record("zcr521_status", map[string]any{"action": "get"}, model.Success("OK", "old", nil)); err != nil {
		t.Fatal(err)
	}
	id := store.List("learned")[0].ID
	if err := store.Forget(id); err != nil {
		t.Fatal(err)
	}
	oldGeneration := receipt{
		Version: learnedEntryVersion, ID: "fedcba9876543210fedcba9876543210", CompletedAt: time.Now().UTC().Add(365 * 24 * time.Hour),
		Tool: "zcr521_status", Action: "get", Fingerprint: strings.Repeat("b", 64), Generation: 0,
		Arguments: map[string]any{"action": "get"}, Outcome: outcome{Success: true, Code: "OK"},
	}
	raw, _ := json.Marshal(oldGeneration)
	if err := os.WriteFile(filepath.Join(state, "MCP", "invocations", "99999999-future-old-generation.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := New(state, work).List("learned"); len(got) != 0 {
		t.Fatalf("future-dated old generation resurrected forgotten skill: %+v", got)
	}
	relearned := New(state, work)
	if err := relearned.Record("zcr521_status", map[string]any{"action": "get"}, model.Success("OK", "new", nil)); err != nil {
		t.Fatal(err)
	}
	got := New(state, work).List("learned")
	if len(got) != 1 || got[0].Generation != 1 || got[0].Attempts != 1 {
		t.Fatalf("unexpected relearned generation: %+v", got)
	}
}

func TestReceiptIDRecoveryIgnoresWallClockRollback(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, "state")
	work := filepath.Join(root, "work")
	store := New(state, work)
	if err := store.Record("zcr521_fs_hash", map[string]any{"action": "calculate", "path": "a"}, model.Success("OK", "first", nil)); err != nil {
		t.Fatal(err)
	}
	entry := store.List("learned")[0]
	rolledBack := receipt{
		Version: 3, ID: "0123456789abcdef0123456789abcdef", CompletedAt: entry.UpdatedAt.Add(-24 * time.Hour),
		Tool: "zcr521_fs_hash", Action: "calculate", Fingerprint: strings.Repeat("a", 64),
		Arguments: map[string]any{"action": "calculate", "path": "b"}, Outcome: outcome{Success: false, Code: "IO_ERROR", ErrorClass: "IOError"},
	}
	raw, _ := json.Marshal(rolledBack)
	path := filepath.Join(state, "MCP", "invocations", "00000000-rollback.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	recovered := New(state, work)
	got := recovered.List("learned")
	if len(got) != 1 || got[0].Attempts != 2 || got[0].FailureClasses["IOError"] != 1 || got[0].LastCode != "OK" {
		t.Fatalf("receipt-id recovery failed across clock rollback: %+v", got)
	}
}

func TestReceiptOrderDefinesLatestAcrossWallClockRollback(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, "state")
	work := filepath.Join(root, "work")
	store := New(state, work)
	if err := store.Record("zcr521_fs_hash", map[string]any{"action": "calculate", "path": "a"}, model.Success("OK", "first", nil)); err != nil {
		t.Fatal(err)
	}
	entry := store.List("learned")[0]
	newerEvent := receipt{
		Version: learnedEntryVersion, ID: "11111111111111111111111111111111", CompletedAt: entry.UpdatedAt.Add(-48 * time.Hour), Order: entry.LastOrder + 1,
		Tool: "zcr521_fs_hash", Action: "calculate", Fingerprint: strings.Repeat("c", 64),
		Arguments: map[string]any{"action": "calculate", "path": "b"}, Outcome: outcome{Success: false, Code: "IO_ERROR", ErrorClass: "IOError"},
	}
	raw, _ := json.Marshal(newerEvent)
	if err := os.WriteFile(filepath.Join(state, "MCP", "invocations", "00000000-new-order-old-clock.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	got := New(state, work).List("learned")
	if len(got) != 1 || got[0].LastCode != "IO_ERROR" || got[0].LastOrder != newerEvent.Order {
		t.Fatalf("receipt order did not win over wall clock: %+v", got)
	}
	_, body, err := New(state, work).Get(got[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "最近一次：失败") {
		t.Fatalf("rendered status ignored receipt order: %s", body)
	}
}

func TestReceiptRecoverySortsByEventOrderBeforeFilename(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, "state")
	invocations := filepath.Join(state, "MCP", "invocations")
	if err := os.MkdirAll(invocations, 0o700); err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC()
	failure := receipt{Version: learnedEntryVersion, ID: strings.Repeat("1", 32), CompletedAt: base, Order: 1, Tool: "zcr521_fs_hash", Action: "calculate", Fingerprint: strings.Repeat("a", 64), Arguments: map[string]any{"action": "calculate", "path": "old"}, Outcome: outcome{Success: false, Code: "IO_ERROR", ErrorClass: "IOError"}}
	success := receipt{Version: learnedEntryVersion, ID: strings.Repeat("2", 32), CompletedAt: base.Add(-24 * time.Hour), Order: 2, Tool: failure.Tool, Action: failure.Action, Fingerprint: strings.Repeat("b", 64), Arguments: map[string]any{"action": "calculate", "path": "new"}, Outcome: outcome{Success: true, Code: "OK"}}
	for name, value := range map[string]receipt{"z-failure.json": failure, "a-success-old-clock.json": success} {
		raw, _ := json.Marshal(value)
		if err := os.WriteFile(filepath.Join(invocations, name), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	store := New(state, filepath.Join(root, "work"))
	got := store.List("learned")
	if len(got) != 1 || got[0].Recoveries != 1 || got[0].LastCode != "OK" || got[0].LastOrder != 2 {
		t.Fatalf("event-order recovery failed: %+v", got)
	}
}

func TestCorruptForgetStateFailsClosed(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, "state")
	work := filepath.Join(root, "work")
	store := New(state, work)
	if err := store.Record("zcr521_status", map[string]any{"action": "get"}, model.Success("OK", "ok", nil)); err != nil {
		t.Fatal(err)
	}
	indexPath := filepath.Join(state, "MCP", "skills", "index.json")
	indexBefore, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		filepath.Join(state, "MCP", "skills", "forgotten.json"),
		filepath.Join(state, "MCP", "skills", "forgotten.json.bak"),
	} {
		if err := os.WriteFile(path, []byte("{corrupt"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	restarted := New(state, work)
	if got := restarted.List("learned"); len(got) != 0 {
		t.Fatalf("untrusted forget state exposed learned skills: %+v", got)
	}
	if restarted.Stats()["forgetStateTrusted"] != false {
		t.Fatalf("forget corruption not exposed: %+v", restarted.Stats())
	}
	if err := restarted.Record("zcr521_status", map[string]any{"action": "get"}, model.Success("OK", "new", nil)); err == nil {
		t.Fatal("untrusted forget state was overwritten")
	}
	indexAfter, err := os.ReadFile(indexPath)
	if err != nil || !bytes.Equal(indexBefore, indexAfter) {
		t.Fatalf("forget corruption rewrote learned index: err=%v", err)
	}
}

func TestKnowledgeStateRejectsLegacyDowngradeAfterMigration(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, "state")
	work := filepath.Join(root, "work")
	store := New(state, work)
	if err := store.Record("zcr521_status", map[string]any{"action": "get"}, model.Success("OK", "old", nil)); err != nil {
		t.Fatal(err)
	}
	id := store.List("learned")[0].ID
	if err := store.Forget(id); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		filepath.Join(state, "MCP", "skills", "forgotten.json"),
		filepath.Join(state, "MCP", "skills", "forgotten.json.bak"),
	} {
		if err := os.WriteFile(path, []byte(`{}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	downgradedForget := New(state, work)
	if downgradedForget.Stats()["forgetStateTrusted"] != false || len(downgradedForget.List("learned")) != 0 {
		t.Fatalf("legacy forget downgrade was accepted: %+v", downgradedForget.Stats())
	}

	cleanRoot := t.TempDir()
	cleanState := filepath.Join(cleanRoot, "state")
	cleanWork := filepath.Join(cleanRoot, "work")
	clean := New(cleanState, cleanWork)
	if err := clean.Record("zcr521_status", map[string]any{"action": "get"}, model.Success("OK", "kept", nil)); err != nil {
		t.Fatal(err)
	}
	receipts, _ := filepath.Glob(filepath.Join(cleanState, "MCP", "invocations", "*.json"))
	for _, path := range receipts {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	index := filepath.Join(cleanState, "MCP", "skills", "index.json")
	for _, path := range []string{index, index + ".bak"} {
		if err := os.WriteFile(path, []byte(`[]`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	downgradedIndex := New(cleanState, cleanWork)
	if downgradedIndex.Stats()["skillIndexTrusted"] != false {
		t.Fatalf("legacy index downgrade was accepted: %+v", downgradedIndex.Stats())
	}
	if err := downgradedIndex.Record("zcr521_status", map[string]any{"action": "get"}, model.Success("OK", "new", nil)); err == nil {
		t.Fatal("downgraded index was overwritten")
	}
}

func TestForgetBackupGenerationWinsOverStalePrimary(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, "state")
	work := filepath.Join(root, "work")
	store := New(state, work)
	if err := store.Record("zcr521_status", map[string]any{"action": "get"}, model.Success("OK", "old", nil)); err != nil {
		t.Fatal(err)
	}
	forgotten := filepath.Join(state, "MCP", "skills", "forgotten.json")
	stalePrimary, err := os.ReadFile(forgotten)
	if err != nil {
		t.Fatal(err)
	}
	id := store.List("learned")[0].ID
	if err := store.Forget(id); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(forgotten, stalePrimary, 0o600); err != nil {
		t.Fatal(err)
	}
	restarted := New(state, work)
	if learned := restarted.List("learned"); len(learned) != 0 {
		t.Fatalf("stale primary resurrected a forgotten skill: %+v", learned)
	}
	if restarted.forgotten[id].Generation == 0 {
		t.Fatalf("newer backup tombstone was ignored: %+v", restarted.forgotten[id])
	}
}

func TestOversizedReceiptIsNeverReadAndIsCountedForRetention(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, "state")
	work := filepath.Join(root, "work")
	_ = New(state, work)
	if err := os.MkdirAll(filepath.Join(state, "MCP", "invocations"), 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(state, "MCP", "invocations", "oversized.json")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(maxReceiptBytes + 1); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	store := New(state, work)
	if len(store.List("learned")) != 0 {
		t.Fatal("oversized receipt was parsed")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("oversized invalid receipt evaded retention: %v", err)
	}
}

func TestMatchHardCapsSummariesAtThree(t *testing.T) {
	store := New(filepath.Join(t.TempDir(), "state"), filepath.Join(t.TempDir(), "work"))
	matches := store.Match("Android Web MCP zcr521_network zcr521_status token", 5)
	if len(matches) > 3 {
		t.Fatalf("match returned %d summaries", len(matches))
	}
}

func TestLegacyReceiptIsRescrubbedAndSkillBodyIsBounded(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, "state")
	work := filepath.Join(root, "work")
	invocations := filepath.Join(state, "MCP", "invocations")
	if err := os.MkdirAll(invocations, 0o700); err != nil {
		t.Fatal(err)
	}
	legacy := receipt{Version: 3, ID: strings.Repeat("3", 32), CompletedAt: time.Now().UTC(), Tool: "zcr521_network", Action: "http", Fingerprint: strings.Repeat("c", 64), Arguments: map[string]any{"action": "http", "url": "https://example.test/private/secret?token=leak", "headers": map[string]any{"Authorization": "Bearer legacy-secret"}}, Outcome: outcome{Success: true, Code: "OK", Artifacts: []string{"private-secret-name.txt"}}}
	raw, _ := json.Marshal(legacy)
	if err := os.WriteFile(filepath.Join(invocations, "legacy.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	store := New(state, work)
	entry := store.List("learned")[0]
	_, body, err := store.Get(entry.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"legacy-secret", "token=leak", "private-secret-name"} {
		if strings.Contains(body, secret) {
			t.Fatalf("legacy secret leaked into skill body: %q", secret)
		}
	}
	path := filepath.Join(state, "MCP", "skills", filepath.FromSlash(entry.ID), "SKILL.md")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", maxSkillBodyBytes+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Get(entry.ID); err == nil {
		t.Fatal("oversized skill body was returned")
	}
}

func TestCompactedTaskLedgerIsExactChecksummedAndFailsClosed(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, "state")
	work := filepath.Join(root, "work")
	store := New(state, work)
	if err := os.MkdirAll(filepath.Join(state, "MCP"), 0o700); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	store.compactedIDs[taskDigest("completed-task")] = struct{}{}
	err := store.writeCompactedTaskIDsLocked()
	store.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}

	restarted := New(state, work)
	result, completed := restarted.CompletionResult("completed-task")
	if !completed || result.Code != "COMPLETION_COMPACTED" {
		t.Fatalf("exact compacted task was not recovered: completed=%v result=%+v", completed, result)
	}
	if _, completed := restarted.CompletionResult("different-task"); completed {
		t.Fatal("different task produced a false positive")
	}

	ledger := filepath.Join(state, "MCP", "compacted-task-ids.bin")
	raw, err := os.ReadFile(ledger)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)-1] ^= 0xff
	if err := os.WriteFile(ledger, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	corrupt := New(state, work)
	result, completed = corrupt.CompletionResult("different-task")
	if !completed || result.Code != "COMPLETION_LEDGER_UNTRUSTED" || corrupt.Stats()["completionLedgerTrusted"] != false {
		t.Fatalf("corrupt ledger did not fail closed: completed=%v result=%+v stats=%+v", completed, result, corrupt.Stats())
	}
	corrupt.mu.Lock()
	err = corrupt.writeCompactedTaskIDsLocked()
	corrupt.mu.Unlock()
	if err == nil {
		t.Fatal("corrupt ledger was overwritten")
	}
	if err := os.Remove(ledger); err != nil {
		t.Fatal(err)
	}
	missing := New(state, work)
	result, completed = missing.CompletionResult("different-task")
	if !completed || result.Code != "COMPLETION_LEDGER_UNTRUSTED" {
		t.Fatalf("missing expected ledger did not fail closed: completed=%v result=%+v", completed, result)
	}
}

func TestChecksummedSkillIndexRecoversBackupAndFailsClosedIfBothCorrupt(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, "state")
	work := filepath.Join(root, "work")
	store := New(state, work)
	result := model.Success("OK", "done", nil)
	if err := store.Record("zcr521_network", map[string]any{"action": "http", "url": "https://example.test/"}, result); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	if err := store.writeCompactedTaskIDsLocked(); err != nil {
		store.mu.Unlock()
		t.Fatal(err)
	}
	store.mu.Unlock()
	receipts, err := filepath.Glob(filepath.Join(state, "MCP", "invocations", "*.json"))
	if err != nil || len(receipts) != 1 {
		t.Fatalf("receipts=%v err=%v", receipts, err)
	}
	if err := os.Remove(receipts[0]); err != nil {
		t.Fatal(err)
	}
	index := filepath.Join(state, "MCP", "skills", "index.json")
	backup := index + ".bak"
	if err := os.WriteFile(index, []byte(`{"version":1,"entries":[],"checksum":"broken"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	recovered := New(state, work)
	entry, _, err := recovered.Get("learned/zcr521-network-http")
	if err != nil || entry.Attempts != 1 || recovered.Stats()["skillIndexTrusted"] != true {
		t.Fatalf("backup recovery failed: entry=%+v err=%v stats=%+v", entry, err, recovered.Stats())
	}

	for _, path := range []string{index, backup} {
		if err := os.WriteFile(path, []byte(`{"version":1,"entries":[],"checksum":"broken"}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	corrupt := New(state, work)
	if corrupt.Stats()["skillIndexTrusted"] != false {
		t.Fatalf("double-corrupt index was trusted: %+v", corrupt.Stats())
	}
	if err := corrupt.Record("zcr521_network", map[string]any{"action": "http"}, result); err == nil {
		t.Fatal("untrusted compacted index was overwritten")
	}
	raw, err := os.ReadFile(index)
	if err != nil || !bytes.Contains(raw, []byte(`"checksum":"broken"`)) {
		t.Fatalf("corrupt primary was unexpectedly replaced: %s err=%v", raw, err)
	}
}
