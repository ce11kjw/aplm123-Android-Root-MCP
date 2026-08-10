package mcpapi

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/zcr521/android-ai-mcp/internal/model"
)

func TestToolResultAvoidsCurrentProtocolDuplication(t *testing.T) {
	result := model.Success("OK", "done", map[string]any{"payload": "structured-only"})
	result.Meta = map[string]any{"skillHints": []string{"one"}}
	current := toolResult(result, true)
	text := current.Content[0].(*mcp.TextContent).Text
	if text != "OK: done" || strings.Contains(text, "structured-only") {
		t.Fatalf("current text duplicates structured result: %q", text)
	}
	structured := current.StructuredContent.(model.Result)
	if structured.Meta != nil {
		t.Fatal("private metadata leaked into StructuredContent")
	}
	if current.Meta["skillHints"] == nil {
		t.Fatal("skill hints missing from MCP _meta")
	}
	legacy := toolResult(result, false)
	legacyText := legacy.Content[0].(*mcp.TextContent).Text
	var decoded model.Result
	if json.Unmarshal([]byte(legacyText), &decoded) != nil || decoded.Data == nil {
		t.Fatalf("legacy JSON text compatibility lost: %q", legacyText)
	}
}
