package frontend

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// 2026-07-28 协议（SEP-2575）的 _meta 兼容层。
//
// 背景
// ----
// 官方 Go SDK 按 SEP-2575 实现：当请求声明协议版本 >= 2026-07-28 时，
// 要求请求体的 `_meta` 中必须带有与 `Mcp-Protocol-Version` 头一致的
// protocolVersion，并且必须提供 clientCapabilities，否则返回
//   400 missing or invalid _meta field "io.modelcontextprotocol/clientCapabilities"
//
// 但多数 MCP 客户端只发 HTTP 头、不在请求体里重复这些字段，于是
// 2026 协议在能力探测阶段就失败，看起来像"服务不支持新版协议"。
//
// 做法
// ----
// 在请求进入 SDK handler 之前做一次规范化：仅在请求头声明的版本
// >= 2026-07-28 时，把缺失的 `_meta` 字段补齐再放行。
//
// 边界
// ----
//   - 不改写客户端**已提供**的值：若客户端给的 _meta.protocolVersion 与头
//     不一致，保留原样交给 SDK 报错——那是真实的协议不一致，不该被掩盖。
//   - 只在版本 >= 2026-07-28 时生效，其它版本原样透传。
//   - 请求体超过限制时跳过补全，交给下游返回 413。
//   - 支持批量请求（JSON 数组）逐条补全。
//   - 补全后完整恢复 req.Body，下游按正常流程读取。
//   - 不修改 vendor/ 下的 SDK，SDK 日后自行放宽时可直接移除此文件。

const (
	// 触发补全的最低协议版本。
	protocolVersion20260728 = "2026-07-28"

	// SEP-2575 定义的 _meta 键名。
	metaKeyProtocolVersion    = "io.modelcontextprotocol/protocolVersion"
	metaKeyClientCapabilities = "io.modelcontextprotocol/clientCapabilities"

	// 2026-07-28 起要求的 MCP 标准头。
	// SDK 在 streamable_headers.go 中校验：Mcp-Method 必须存在且与请求体
	// 的 method 一致；tools/call 等还必须有 Mcp-Name。
	headerMcpMethod = "Mcp-Method"
	headerMcpName   = "Mcp-Name"

	// 单次补全允许读取的最大体积，避免把大请求整份载入内存。
	metaCompatMaxBytes = 4 << 20
)

// normalizeNewProtocolMeta 是包装 MCP handler 的中间件。
func normalizeNewProtocolMeta(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		version := strings.TrimSpace(r.Header.Get("Mcp-Protocol-Version"))
		if version == "" || version < protocolVersion20260728 {
			// 旧协议路径不需要 _meta，原样透传。
			next(w, r)
			return
		}
		if r.Method != http.MethodPost {
			next(w, r)
			return
		}
		if contentType := r.Header.Get("Content-Type"); contentType != "" &&
			!strings.HasPrefix(strings.ToLower(contentType), "application/json") {
			next(w, r)
			return
		}
		if r.ContentLength > metaCompatMaxBytes {
			// 超大请求不整份读取，交给下游按既有逻辑处理（通常是 413）。
			next(w, r)
			return
		}

		body, err := io.ReadAll(io.LimitReader(r.Body, metaCompatMaxBytes+1))
		if err != nil {
			// 读不出来就保持原状，让下游自己报错。
			next(w, r)
			return
		}
		_ = r.Body.Close()
		if len(body) > metaCompatMaxBytes {
			r.Body = io.NopCloser(bytes.NewReader(body))
			next(w, r)
			return
		}

		// 先补齐 MCP 标准头（不改请求体，安全）。
		// SDK 要求 Mcp-Method 与请求体 method 一致，缺失即 400；
		// 多数客户端只发 MCP-Protocol-Version，因此在服务端补齐。
		applyStandardHeaders(r.Header, body)

		// 补齐 SDK 校验所必需的 _meta 字段（见 patchNewProtocolMeta）。
		normalized, changed := patchNewProtocolMeta(body, version)
		if changed {
			r.Body = io.NopCloser(bytes.NewReader(normalized))
			r.ContentLength = int64(len(normalized))
		} else {
			r.Body = io.NopCloser(bytes.NewReader(body))
		}
		next(w, r)
	}
}

// patchNewProtocolMeta 为 2026-07-28 请求补齐 SDK 必需的 _meta 字段。
//
// SDK 在 streamable.go 中的校验（当协议版本 >= 2026-07-28 时）要求：
//   - params._meta.protocolVersion 必须存在，且与 Mcp-Protocol-Version 头一致
//   - params._meta.clientCapabilities 必须存在（validateRequestMeta 校验）
//
// 因此这里：
//   - 缺 protocolVersion    → 用头里的版本值补上
//   - 缺 clientCapabilities → 补空对象
//   - 已提供的值            → 一律保留（头与 body 不一致属于真实协议错误，
//     应交由 SDK 报错，不能被静默纠正）
//
// 注意 `_meta` 必须写在 params 内部：SDK 通过 extractRequestMeta(req.Params)
// 读取，放在请求体顶层是读不到的。
func patchNewProtocolMeta(body []byte, version string) ([]byte, bool) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return body, false
	}
	if trimmed[0] == '[' {
		var batch []json.RawMessage
		if err := json.Unmarshal(trimmed, &batch); err != nil {
			return body, false
		}
		changed := false
		out := make([]json.RawMessage, 0, len(batch))
		for _, item := range batch {
			fixed, itemChanged := patchSingleNewProtocolMeta(item, version)
			if itemChanged {
				changed = true
			}
			out = append(out, fixed)
		}
		if !changed {
			return body, false
		}
		encoded, err := json.Marshal(out)
		if err != nil {
			return body, false
		}
		return encoded, true
	}
	return patchSingleNewProtocolMeta(trimmed, version)
}

// patchSingleNewProtocolMeta 处理单个 JSON-RPC 对象。
func patchSingleNewProtocolMeta(raw []byte, version string) ([]byte, bool) {
	var message map[string]json.RawMessage
	if err := json.Unmarshal(raw, &message); err != nil {
		return raw, false
	}
	if _, ok := message["jsonrpc"]; !ok {
		return raw, false
	}

	// params 缺失或不是对象时无法承载 _meta，原样返回。
	params := map[string]json.RawMessage{}
	if existing, ok := message["params"]; ok {
		if err := json.Unmarshal(existing, &params); err != nil {
			return raw, false
		}
	}

	meta := map[string]json.RawMessage{}
	if existing, ok := params["_meta"]; ok {
		if err := json.Unmarshal(existing, &meta); err != nil {
			// _meta 类型异常：无法安全合并，保持原样交给 SDK 报错。
			return raw, false
		}
	}

	changed := false
	if _, present := meta[metaKeyProtocolVersion]; !present {
		encoded, err := json.Marshal(version)
		if err != nil {
			return raw, false
		}
		meta[metaKeyProtocolVersion] = encoded
		changed = true
	}
	if _, present := meta[metaKeyClientCapabilities]; !present {
		meta[metaKeyClientCapabilities] = json.RawMessage(`{}`)
		changed = true
	}
	if !changed {
		return raw, false
	}

	encodedMeta, err := json.Marshal(meta)
	if err != nil {
		return raw, false
	}
	params["_meta"] = encodedMeta
	encodedParams, err := json.Marshal(params)
	if err != nil {
		return raw, false
	}
	message["params"] = encodedParams
	out, err := json.Marshal(message)
	if err != nil {
		return raw, false
	}
	return out, true
}

// stripInjectedMeta 供测试使用：判断 body 是否被注入过 _meta。
func stripInjectedMeta(body []byte) bool {
	return bytes.Contains(body, []byte(metaKeyProtocolVersion))
}

// requestDeclaresNewProtocol 供测试与诊断使用：判断请求头是否要求新协议。
func requestDeclaresNewProtocol(headerValue string) bool {
	value := strings.TrimSpace(headerValue)
	if value == "" {
		return false
	}
	return value >= protocolVersion20260728
}

// applyStandardHeaders 依据请求体补齐缺失的 MCP 标准头。
//
// 只补空缺，绝不覆盖客户端已提供的值——若客户端给的头与请求体不一致，
// 那属于真实的协议错误，应由 SDK 报错而不是被静默纠正。
func applyStandardHeaders(header http.Header, body []byte) {
	method, name := extractMethodAndName(body)
	if method == "" {
		return
	}
	if header.Get(headerMcpMethod) == "" {
		header.Set(headerMcpMethod, method)
	}
	// Mcp-Name 仅对这几类方法有要求。
	if name != "" &&
		(method == "tools/call" || method == "resources/read" || method == "prompts/get") &&
		header.Get(headerMcpName) == "" {
		header.Set(headerMcpName, name)
	}
}

// extractMethodAndName 从（可能是批量的）请求体中取出 method 与元素名。
// 批量请求只取第一条——SDK 对批量不做标准头校验。
func extractMethodAndName(body []byte) (method, name string) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return "", ""
	}
	if trimmed[0] == '[' {
		var batch []json.RawMessage
		if err := json.Unmarshal(trimmed, &batch); err != nil || len(batch) == 0 {
			return "", ""
		}
		return extractMethodAndName(batch[0])
	}
	var message struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(trimmed, &message); err != nil {
		return "", ""
	}
	if message.Method == "" {
		return "", ""
	}
	if len(message.Params) == 0 {
		return message.Method, ""
	}
	var params struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(message.Params, &params); err != nil {
		return message.Method, ""
	}
	return message.Method, params.Name
}

var _ = strconv.Itoa
