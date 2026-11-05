// Package mcpapi binds the official MCP Go SDK to the privileged broker.
package mcpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"mime"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/aplm123/android-ai-mcp/internal/buildinfo"
	"github.com/aplm123/android-ai-mcp/internal/model"
	"github.com/aplm123/android-ai-mcp/internal/schema"
)

type Caller interface {
	Call(context.Context, string, map[string]any) model.Result
}

type Publisher func(path, name, sha256 string, ttl time.Duration) (model.Artifact, error)

// resultTextMode 保存运行期的结果文本形态：
//
//	"auto"（默认）跟随协议：只有协商到最新协议的客户端才收到短文本
//	"on"          所有客户端都只收到「OK: 消息」
//	"off"         所有客户端都收到完整 JSON 文本
//
// structuredContent 始终是完整对象，形态只影响 content 里的文本。
var resultTextMode atomic.Value

func init() {
	resultTextMode.Store("auto")
}

// SetResultTextMode 更新结果文本形态；空值或未知值回落到 "auto"。
func SetResultTextMode(mode string) {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "on":
		resultTextMode.Store("on")
	case "off":
		resultTextMode.Store("off")
	default:
		resultTextMode.Store("auto")
	}
}

// ResultTextMode 返回当前形态，供自检与测试读取。
func ResultTextMode() string {
	if value, ok := resultTextMode.Load().(string); ok {
		return value
	}
	return "auto"
}

// compactText 判断本次调用是否使用「OK: 消息」短文本。
func compactText(request *mcp.CallToolRequest) bool {
	switch ResultTextMode() {
	case "on":
		return true
	case "off":
		return false
	default:
		return currentProtocol(request)
	}
}

type API struct {
	server    *mcp.Server
	caller    Caller
	publishMu sync.RWMutex
	publisher Publisher
}

func New(caller Caller) (*API, error) {
	if caller == nil {
		return nil, fmt.Errorf("MCP caller is required")
	}
	// The SDK exposes prompts/list even when the prompt store is empty. Declare
	// that empty, stable collection so current-protocol discovery matches the
	// methods the server actually honors. The 49-tool catalog is static for a
	// running process, therefore listChanged must remain false.
	capabilities := &mcp.ServerCapabilities{
		Prompts: &mcp.PromptCapabilities{ListChanged: false},
		Tools:   &mcp.ToolCapabilities{ListChanged: false},
	}
	// SEP-2663 currently defines no extension-specific settings. An empty
	// object is the complete capability declaration.
	capabilities.AddExtension("io.modelcontextprotocol/tasks", nil)
	server := mcp.NewServer(&mcp.Implementation{
		Name:        "aplm123-android-mcp",
		Title:       buildinfo.Name,
		Description: "Android Root 文件、Shell、应用、系统、网络、诊断与自动化工具。",
		Version:     buildinfo.Version,
	}, &mcp.ServerOptions{
		Instructions: "这是完整 Root 能力服务。非平凡任务先用 aplm_skill.match 匹配至多 3 条技能摘要，仅在摘要不足时用 get 加载最相关的 1 条正文，禁止一次加载全部技能；普通工具也会自动返回匹配提示。破坏性操作必须先确认目标；大文件使用 ResourceLink、分页或分块传输。",
		Capabilities: capabilities,
	})
	api := &API{server: server, caller: caller}
	for _, definition := range schema.RuntimeCompact(buildinfo.ProtocolCurrent).Tools {
		api.register(definition)
	}
	return api, nil
}

func (a *API) Server() *mcp.Server {
	return a.server
}

func (a *API) SetPublisher(publisher Publisher) {
	a.publishMu.Lock()
	a.publisher = publisher
	a.publishMu.Unlock()
}

func (a *API) register(definition schema.Tool) {
	name := definition.Name
	description := definition.Description
	readOnly := definition.Annotations.ReadOnly
	idempotent := definition.Annotations.Idempotent
	destructive := definition.Annotations.Destructive
	openWorld := name == "aplm_download" || name == "aplm_network"
	a.server.AddTool(&mcp.Tool{
		Name:         name,
		Title:        definition.Title,
		Description:  description,
		InputSchema:  definition.InputSchema,
		OutputSchema: definition.OutputSchema,
		Annotations: &mcp.ToolAnnotations{
			Title:           definition.Title,
			ReadOnlyHint:    readOnly,
			IdempotentHint:  idempotent,
			DestructiveHint: boolPointer(destructive),
			OpenWorldHint:   boolPointer(openWorld),
		},
	}, func(ctx context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := map[string]any{}
		if request != nil && request.Params != nil && len(request.Params.Arguments) > 0 {
			decoderErr := json.Unmarshal(request.Params.Arguments, &args)
			if decoderErr != nil {
				result := model.Failure("INVALID_ARGUMENT", "工具参数必须是 JSON 对象", "DecodeError", decoderErr.Error())
				return toolResult(result, compactText(request)), nil
			}
		}
		result := a.caller.Call(ctx, name, args)
		a.publishArtifacts(&result)
		return toolResult(result, compactText(request)), nil
	})
}

func (a *API) publishArtifacts(result *model.Result) {
	a.publishMu.RLock()
	publisher := a.publisher
	a.publishMu.RUnlock()
	if publisher == nil {
		return
	}
	for index := range result.Artifacts {
		artifact := &result.Artifacts[index]
		if artifact.URI != "" || artifact.Path == "" {
			continue
		}
		published, err := publisher(artifact.Path, artifact.Name, artifact.SHA256, time.Hour)
		if err != nil {
			continue
		}
		artifact.URI = published.URI
		if artifact.Size == 0 {
			artifact.Size = published.Size
		}
		if artifact.ExpiresAt.IsZero() {
			artifact.ExpiresAt = published.ExpiresAt
		}
	}
}

func toolResult(result model.Result, compact bool) *mcp.CallToolResult {
	structured := result
	structured.Meta = nil
	raw, err := json.Marshal(structured)
	if err != nil {
		fallback := model.Failure("RESULT_ENCODING_FAILED", "工具结果编码失败", "EncodingError", err.Error())
		raw, _ = json.Marshal(fallback)
		result = fallback
		structured = fallback
	}
	text := string(raw)
	if compact {
		text = result.Code + ": " + result.Message
		if result.TaskID != "" {
			text += " (taskId=" + result.TaskID + ")"
		}
	}
	content := []mcp.Content{&mcp.TextContent{Text: text}}
	for _, artifact := range result.Artifacts {
		if artifact.URI == "" {
			continue
		}
		name := artifact.Name
		if name == "" {
			name = filepath.Base(artifact.Path)
		}
		size := artifact.Size
		mediaType := artifact.MediaType
		if mediaType == "" {
			mediaType = mime.TypeByExtension(filepath.Ext(name))
		}
		content = append(content, &mcp.ResourceLink{
			URI:         artifact.URI,
			Name:        name,
			Title:       name,
			Description: "aplm123 临时流式下载；过期后需重新导出。",
			MIMEType:    mediaType,
			Size:        &size,
		})
	}
	return &mcp.CallToolResult{
		Meta:              mcp.Meta(result.Meta),
		Content:           content,
		StructuredContent: structured,
		IsError:           !result.Success,
	}
}

func currentProtocol(request *mcp.CallToolRequest) bool {
	return request != nil && request.ProtocolVersion() == buildinfo.ProtocolCurrent
}

func boolPointer(value bool) *bool {
	return &value
}
