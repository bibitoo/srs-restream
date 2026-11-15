package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
)

// ----------------------------------------------------------------------------
// 内置 MCP 端点（Model Context Protocol，Streamable HTTP 传输）
//
// 目的：不经过捷兔桌面，也能被 Cursor / Claude Desktop / Cline 等 MCP 客户端直连。
// 配置示例（Cursor → MCP）：
//   { "mcpServers": { "jetstream": {
//       "url": "http://192.168.1.10:1986/mcp",
//       "headers": { "X-Access-Key": "jetool" } } } }
//
// 实现的是最小必要集合：initialize / notifications/initialized / tools/list / tools/call / ping。
// 工具定义与 /api/agent/... 完全同源（见 agent.go），所以两处行为一致。
// 鉴权沿用访问密钥（header / Bearer / ?key= / Cookie）。
// ----------------------------------------------------------------------------

const mcpProtocolVersion = "2024-11-05"

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int         `json:"code"`
	Message string      `json:"message"`
	Data    interface{} `json:"data,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  interface{}     `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

func writeRPC(w http.ResponseWriter, id json.RawMessage, result interface{}, e *rpcError) {
	resp := rpcResponse{JSONRPC: "2.0", ID: id, Result: result, Error: e}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(200)
	_ = json.NewEncoder(w).Encode(resp)
}

// handleMCP 处理 /mcp 的 JSON-RPC 请求。
func (a *API) handleMCP(w http.ResponseWriter, r *http.Request) {
	// GET：给部分客户端探测用，返回服务信息（不泄露配置）
	if r.Method == http.MethodGet {
		writeJSON(w, 200, map[string]interface{}{
			"service":         "jetstream",
			"protocol":        "mcp",
			"protocolVersion": mcpProtocolVersion,
			"transport":       "streamable-http",
			"tools":           len(agentTools()),
		})
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "只接受 POST（JSON-RPC）", http.StatusMethodNotAllowed)
		return
	}
	// 鉴权：与其它入口一致
	if !a.keyOK(r) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="jetstream"`)
		http.Error(w, "访问密钥无效", http.StatusUnauthorized)
		return
	}

	var req rpcRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeRPC(w, nil, nil, &rpcError{Code: -32700, Message: "解析失败: " + err.Error()})
		return
	}

	switch req.Method {
	case "initialize":
		writeRPC(w, req.ID, map[string]interface{}{
			"protocolVersion": mcpProtocolVersion,
			"capabilities": map[string]interface{}{
				"tools": map[string]interface{}{"listChanged": false},
			},
			"serverInfo": map[string]interface{}{
				"name":    "jetstream",
				"version": "1.1.0",
			},
			"instructions": "捷兔流媒体服务：可查看直播流、创建/启停转推任务、管理录像。所有写操作需自行确认。",
		}, nil)

	case "notifications/initialized", "notifications/cancelled":
		// 通知类不需要响应体，按 JSON-RPC 规范返回空 202
		w.WriteHeader(http.StatusAccepted)

	case "ping":
		writeRPC(w, req.ID, map[string]interface{}{}, nil)

	case "tools/list":
		tools := make([]map[string]interface{}, 0, len(agentTools()))
		for _, t := range agentTools() {
			item := map[string]interface{}{
				"name":        t.Name,
				"description": t.Description,
				"inputSchema": t.InputSchema,
			}
			if t.Title != "" {
				item["title"] = t.Title
			}
			tools = append(tools, item)
		}
		writeRPC(w, req.ID, map[string]interface{}{"tools": tools}, nil)

	case "tools/call":
		var p struct {
			Name      string                 `json:"name"`
			Arguments map[string]interface{} `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			writeRPC(w, req.ID, nil, &rpcError{Code: -32602, Message: "params 无效: " + err.Error()})
			return
		}
		t := agentToolByName(p.Name)
		if t == nil {
			writeRPC(w, req.ID, nil, &rpcError{Code: -32602, Message: "未知工具: " + p.Name})
			return
		}
		if p.Arguments == nil {
			p.Arguments = map[string]interface{}{}
		}
		res, err := t.Run(a, p.Arguments)
		if err != nil {
			// 工具执行失败用 isError 返回，便于模型自行纠正
			writeRPC(w, req.ID, map[string]interface{}{
				"content": []map[string]interface{}{{"type": "text", "text": "执行失败：" + err.Error()}},
				"isError": true,
			}, nil)
			return
		}
		b, _ := json.MarshalIndent(res, "", "  ")
		writeRPC(w, req.ID, map[string]interface{}{
			"content": []map[string]interface{}{{"type": "text", "text": string(b)}},
			"isError": false,
		}, nil)

	default:
		writeRPC(w, req.ID, nil, &rpcError{Code: -32601, Message: "不支持的方法: " + req.Method})
	}
}

// agentJdconfigTools 生成 jdconfig 的 agent.tools[]（字段与协议 §4 对齐）。
func agentJdconfigTools() []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(agentTools()))
	for _, t := range agentTools() {
		item := map[string]interface{}{
			"name":        t.Name,
			"title":       t.Title,
			"description": t.Description,
			"method":      t.Method,
			"path":        t.Path,
			"inputSchema": t.InputSchema,
		}
		if t.TimeoutSeconds > 0 {
			item["timeoutSeconds"] = t.TimeoutSeconds
		}
		if t.Mutating {
			item["mutating"] = true
		}
		out = append(out, item)
	}
	return out
}

// handleAgentManifest 导出 agent 段，方便生成/核对 jdconfig（运维用途）。
func (a *API) handleAgentManifest(w http.ResponseWriter, r *http.Request) {
	tools := agentJdconfigTools()
	for _, t := range tools {
		if p, _ := t["path"].(string); !strings.HasPrefix(p, "/") {
			log.Printf("agent 工具 path 非法：%v", t["name"])
		}
	}
	writeJSON(w, 200, map[string]interface{}{
		"agent": map[string]interface{}{
			"title":   "捷兔流媒体服务",
			"summary": "推流、转推、混流、录像",
			"tools":   tools,
		},
	})
}

var _ = fmt.Sprintf
