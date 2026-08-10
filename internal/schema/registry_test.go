package schema

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/zcr521/android-ai-mcp/internal/buildinfo"
)

func TestRegistryIsCompleteAndUnique(t *testing.T) {
	registry := All("2026-07-28")
	if got, want := len(registry.Tools), 49; got != want {
		t.Fatalf("tool count %d, want %d", got, want)
	}
	seen := map[string]bool{}
	canonicalName := regexp.MustCompile(`^zcr521_[a-z0-9]+(?:_[a-z0-9]+)*$`)
	for _, tool := range registry.Tools {
		if seen[tool.Name] {
			t.Fatalf("duplicate tool %s", tool.Name)
		}
		seen[tool.Name] = true
		if len(tool.Name) > 64 || !canonicalName.MatchString(tool.Name) {
			t.Fatalf("non-canonical tool name %q", tool.Name)
		}
		if tool.Title == "" || tool.Title == tool.Name {
			t.Fatalf("%s lacks a human-readable title", tool.Name)
		}
		if tool.InputSchema["$schema"] != Draft202012 {
			t.Fatalf("%s lacks JSON Schema 2020-12 marker", tool.Name)
		}
		if _, ok := tool.InputSchema["oneOf"]; !ok {
			t.Fatalf("%s lacks action oneOf", tool.Name)
		}
		if _, err := json.Marshal(tool); err != nil {
			t.Fatalf("%s is not serializable: %v", tool.Name, err)
		}
	}
}

func TestRuntimeSchemaIsProgressiveAndSmaller(t *testing.T) {
	full := All("test")
	runtime := RuntimeCompact("test")
	fullRaw, _ := json.Marshal(full)
	runtimeRaw, _ := json.Marshal(runtime)
	if len(runtimeRaw) >= len(fullRaw)*3/4 {
		t.Fatalf("runtime registry was not materially compacted: full=%d runtime=%d", len(fullRaw), len(runtimeRaw))
	}
	for _, tool := range runtime.Tools {
		if tool.OutputSchema != nil || tool.InputSchema["$schema"] != nil {
			t.Fatalf("runtime tool still contains discovery duplication: %s", tool.Name)
		}
		preservesActionContract := tool.Name == "zcr521_network" || tool.Name == "zcr521_skill" ||
			tool.Name == "zcr521_app_info" || tool.Name == "zcr521_app_manage"
		if !preservesActionContract && tool.InputSchema["oneOf"] != nil {
			t.Fatalf("legacy runtime tool still contains discovery-only oneOf: %s", tool.Name)
		}
		if preservesActionContract && tool.InputSchema["oneOf"] == nil {
			t.Fatalf("runtime tool lost its action-specific contract: %s", tool.Name)
		}
	}
	if full.Tools[0].OutputSchema == nil || full.Tools[0].InputSchema["oneOf"] == nil {
		t.Fatal("full offline schema was mutated")
	}
}

func TestCheckedInSchemaMatchesRegistry(t *testing.T) {
	expected, err := Marshal(buildinfo.ProtocolCurrent)
	if err != nil {
		t.Fatal(err)
	}
	expected = append(expected, '\n')
	actual, err := os.ReadFile(filepath.Join("..", "..", "schemas", "tools.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(actual, expected) {
		t.Fatal("schemas/tools.json is stale; regenerate with zcr521d schema --output schemas/tools.json")
	}
}

func TestValidateInvocation(t *testing.T) {
	if err := ValidateInvocation("zcr521_fs_hash", map[string]any{"action": "calculate"}); err != nil {
		t.Fatal(err)
	}
	if err := ValidateInvocation("zcr521_fs_hash", map[string]any{"action": "delete"}); err == nil {
		t.Fatal("expected invalid action")
	}
	if err := ValidateInvocation("zcr521_fs_hash", map[string]any{"action": "calculate", "unknown": true}); err != nil {
		t.Fatalf("legacy tool compatibility changed: %v", err)
	}
	if err := ValidateInvocation("zcr521_network", map[string]any{"action": "http", "url": []any{"https://example.test"}}); err == nil {
		t.Fatal("expected URL type rejection")
	}
	if err := ValidateInvocation("zcr521_config", map[string]any{"action": "update", "config": map[string]any{"network": map[string]any{"port": "5322"}}}); err != nil {
		t.Fatalf("legacy nested validation compatibility changed: %v", err)
	}
	if err := ValidateInvocation("zcr521_network", map[string]any{"action": "ws_connect", "url": "wss://example.test/socket"}); err != nil {
		t.Fatalf("valid WebSocket URL rejected: %v", err)
	}
	if err := ValidateInvocation("zcr521_skill", map[string]any{"action": "match", "query": "android", "limit": float64(3)}); err != nil {
		t.Fatalf("valid skill match limit rejected: %v", err)
	}
	if err := ValidateInvocation("zcr521_skill", map[string]any{"action": "match", "query": "android", "limit": float64(4)}); err == nil {
		t.Fatal("skill match accepted more than three summaries")
	}
	for _, args := range []map[string]any{{"action": "match"}, {"action": "get"}, {"action": "forget"}} {
		if err := ValidateInvocation("zcr521_skill", args); err == nil {
			t.Errorf("skill schema accepted missing required selector: %+v", args)
		}
	}
	for name, args := range map[string]map[string]any{
		"missing HTTP URL":     {"action": "http"},
		"HTTP with WS URL":     {"action": "http", "url": "wss://example.test/socket"},
		"WS with HTTP URL":     {"action": "ws_connect", "url": "https://example.test/socket"},
		"empty feed page":      {"action": "feed_parse", "url": "https://example.test/feed", "maxItems": float64(0)},
		"oversized HTML read":  {"action": "web_extract", "url": "https://example.test/", "maxResponseBytes": float64(2097153)},
		"oversized HTML text":  {"action": "web_extract", "url": "https://example.test/", "maxChars": float64(262145)},
		"oversized WS message": {"action": "ws_connect", "url": "wss://example.test/socket", "maxMessageBytes": float64(2 << 20)},
		"WS send no payload":   {"action": "ws_send", "connectionId": "id"},
		"WS send two payloads": {"action": "ws_send", "connectionId": "id", "message": "x", "binaryBase64": "eA=="},
	} {
		if err := ValidateInvocation("zcr521_network", args); err == nil {
			t.Errorf("%s passed schema validation", name)
		}
	}
	for name, args := range map[string]map[string]any{
		"list by display name": {"action": "list", "query": "设置", "limit": float64(20)},
		"info by display name": {"action": "get", "appName": "设置"},
		"launch by name":       {"action": "launch", "name": "设置"},
		"launch by component":  {"action": "launch", "component": "com.android.settings/.Settings"},
		"launch by package":    {"action": "launch", "package": "com.android.settings"},
		"launch with aliases":  {"action": "launch", "package": "com.android.settings", "query": "设置"},
	} {
		tool := "zcr521_app_manage"
		if strings.HasPrefix(name, "list") {
			tool = "zcr521_app_list"
		} else if strings.HasPrefix(name, "info") {
			tool = "zcr521_app_info"
		}
		if err := ValidateInvocation(tool, args); err != nil {
			t.Errorf("%s failed schema validation: %v", name, err)
		}
	}
	for name, invocation := range map[string]struct {
		tool string
		args map[string]any
	}{
		"info without target":        {"zcr521_app_info", map[string]any{"action": "get"}},
		"launch without target":      {"zcr521_app_manage", map[string]any{"action": "launch"}},
		"stop by ambiguous name":     {"zcr521_app_manage", map[string]any{"action": "stop", "query": "设置"}},
		"oversized application name": {"zcr521_app_manage", map[string]any{"action": "launch", "appName": strings.Repeat("x", 257)}},
		"invalid list limit":         {"zcr521_app_list", map[string]any{"action": "list", "query": "设置", "limit": float64(-1)}},
	} {
		if err := ValidateInvocation(invocation.tool, invocation.args); err == nil {
			t.Errorf("%s passed schema validation", name)
		}
	}
}

func TestRuntimeCompactPreservesNewActionContracts(t *testing.T) {
	registry := RuntimeCompact("2026-07-28")
	for _, tool := range registry.Tools {
		switch tool.Name {
		case "zcr521_network":
			for _, args := range []map[string]any{{"action": "http"}, {"action": "ws_connect"}} {
				if err := validateSchemaValue("arguments", args, tool.InputSchema); err == nil {
					t.Fatalf("runtime network schema accepted missing URL: %+v", args)
				}
			}
		case "zcr521_app_info":
			if err := validateSchemaValue("arguments", map[string]any{"action": "get"}, tool.InputSchema); err == nil {
				t.Fatal("runtime app info schema accepted a missing application target")
			}
		case "zcr521_app_manage":
			for _, args := range []map[string]any{{"action": "launch"}, {"action": "stop", "query": "设置"}} {
				if err := validateSchemaValue("arguments", args, tool.InputSchema); err == nil {
					t.Fatalf("runtime app management schema accepted an unsafe target: %+v", args)
				}
			}
		case "zcr521_fs_hash":
			if _, exists := tool.InputSchema["oneOf"]; exists {
				t.Fatal("legacy runtime schema retained discovery-only oneOf duplication")
			}
		}
	}
}
