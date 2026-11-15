package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// ----------------------------------------------------------------------------
// AI 工具集（agent）
//
// 同一份工具定义供两处使用：
//   1. jdconfig 的 agent.tools[] —— 捷兔桌面据此把 `<slug>__<name>` 暴露给本机 AI 客户端；
//   2. 控制面内置的 /mcp 端点 —— Cursor / Claude Desktop 等可以直连，不经过桌面。
//
// 约定：每个工具都有固定的 HTTP 端点（/api/agent/...），参数走 query（GET）或 JSON body（POST），
// 因此既能被桌面按固定 path 调用，也能被 MCP 的 tools/call 用 arguments 调用。
// ----------------------------------------------------------------------------

type agentTool struct {
	Name           string                                                         `json:"name"`
	Title          string                                                         `json:"title"`
	Description    string                                                         `json:"description"`
	Method         string                                                         `json:"method"`
	Path           string                                                         `json:"path"`
	InputSchema    map[string]interface{}                                         `json:"inputSchema"`
	TimeoutSeconds int                                                            `json:"timeoutSeconds,omitempty"`
	Mutating       bool                                                           `json:"mutating,omitempty"`
	Run            func(a *API, args map[string]interface{}) (interface{}, error) `json:"-"`
}

func obj(props map[string]interface{}, required ...string) map[string]interface{} {
	if props == nil {
		props = map[string]interface{}{} // 必须输出 {}，不能是 null（桌面校验会当成"无参数表"）
	}
	m := map[string]interface{}{"type": "object", "properties": props}
	if len(required) > 0 {
		m["required"] = required
	}
	return m
}

func str(desc string) map[string]interface{} {
	return map[string]interface{}{"type": "string", "description": desc}
}

// agentTools 是全部工具。顺序即展示顺序。
func agentTools() []*agentTool {
	return []*agentTool{
		{
			Name: "streams_list", Title: "列出直播流",
			Description: "列出当前 SRS 上的所有流（含混流产生的 mix/*）：名称、是否在推流、分辨率、码率、是否正在录制。",
			Method:      "GET", Path: "/api/agent/streams", InputSchema: obj(nil),
			Run: func(a *API, _ map[string]interface{}) (interface{}, error) {
				list, err := a.srs.ListStreams(a.store.Settings().PublicBase)
				if err != nil {
					return nil, err
				}
				recs := a.rec.ActiveByStream()
				out := make([]map[string]interface{}, 0, len(list))
				for _, s := range list {
					item := map[string]interface{}{
						"key": s.Key, "active": s.Active, "video": s.Video,
						"has_audio": s.HasAudio, "kbps": s.KbpsRecv,
					}
					if r, ok := recs[s.Key]; ok {
						item["recording"] = map[string]interface{}{"id": r.ID, "duration": int(r.Duration), "size": r.Size}
					}
					out = append(out, item)
				}
				return map[string]interface{}{"count": len(out), "streams": out}, nil
			},
		},
		{
			Name: "jobs_list", Title: "列出转推任务",
			Description: "列出所有转推任务及其运行状态、每个第三方目标的推送状态与重连次数。",
			Method:      "GET", Path: "/api/agent/jobs", InputSchema: obj(nil),
			Run: func(a *API, _ map[string]interface{}) (interface{}, error) {
				jobs := a.store.Jobs()
				out := make([]map[string]interface{}, 0, len(jobs))
				for _, j := range jobs {
					rt := a.runner.Runtime(j.ID)
					out = append(out, map[string]interface{}{
						"id": j.ID, "name": j.Name, "mode": j.Mode, "sources": j.Sources,
						"layout": j.Layout, "status": rt.Status, "detail": rt.Detail,
						"targets": rt.Targets,
					})
				}
				return map[string]interface{}{"count": len(out), "jobs": out}, nil
			},
		},
		{
			Name: "job_create", Title: "创建转推任务",
			Description: "创建一条转推任务。mode=direct 为单流转推（不转码）；mode=mix 为多流混流后转推，" +
				"layout 可选 vstack(上下)/hstack(左右)/grid(宫格)/pip(画中画)/circle(圆窗口)。" +
				"targets 填第三方 RTMP 推流地址数组。创建后默认不自动启动，需再调 job_start。",
			Method: "POST", Path: "/api/agent/jobs", Mutating: true, TimeoutSeconds: 30,
			InputSchema: obj(map[string]interface{}{
				"name":     str("任务名"),
				"mode":     str("direct 或 mix"),
				"sources":  map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}, "description": "源流列表，如 [\"live/camera01\"]"},
				"layout":   str("混流布局：vstack/hstack/grid/pip/circle，mix 模式有效"),
				"targets":  map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}, "description": "第三方 RTMP 地址数组"},
				"width":    map[string]interface{}{"type": "integer", "description": "输出宽，默认 1280"},
				"height":   map[string]interface{}{"type": "integer", "description": "输出高，默认 720"},
				"fps":      map[string]interface{}{"type": "integer", "description": "帧率，默认 25"},
				"pip_size": map[string]interface{}{"type": "integer", "description": "画中画/圆窗直径占宽百分比，默认 28"},
			}, "name", "mode", "sources"),
			Run: func(a *API, args map[string]interface{}) (interface{}, error) {
				var in Job
				b, _ := json.Marshal(args)
				_ = json.Unmarshal(b, &in)
				if v, ok := args["targets"].([]interface{}); ok {
					for _, x := range v {
						if s, ok := x.(string); ok && strings.TrimSpace(s) != "" {
							in.ExtraURLs = append(in.ExtraURLs, strings.TrimSpace(s))
						}
					}
				}
				if len(in.ExtraURLs) == 0 {
					return nil, errors.New("targets 不能为空，需给出至少一个第三方 RTMP 推流地址")
				}
				if err := a.validateJob(&in); err != nil {
					return nil, err
				}
				if err := a.checkSelfLoop(&in); err != nil {
					return nil, err
				}
				j, err := a.store.AddJob(in)
				if err != nil {
					return nil, err
				}
				return map[string]interface{}{"id": j.ID, "name": j.Name, "created": true}, nil
			},
		},
		{
			Name: "job_start", Title: "启动转推任务",
			Description: "启动指定任务；源流不在线时会进入「等待源流」状态，源流上线后自动开始推。",
			Method:      "POST", Path: "/api/agent/job/start", Mutating: true,
			InputSchema: obj(map[string]interface{}{"id": str("任务 ID")}, "id"),
			Run: func(a *API, args map[string]interface{}) (interface{}, error) {
				id, _ := args["id"].(string)
				if _, ok := a.store.JobByID(id); !ok {
					return nil, fmt.Errorf("任务不存在：%s", id)
				}
				if _, err := a.runner.Start(id); err != nil {
					return nil, err
				}
				rt := a.runner.Runtime(id)
				return map[string]interface{}{"id": id, "status": rt.Status, "detail": rt.Detail}, nil
			},
		},
		{
			Name: "job_stop", Title: "停止转推任务",
			Description: "停止指定任务（会同时停掉它对所有第三方目标的中继）。",
			Method:      "POST", Path: "/api/agent/job/stop", Mutating: true,
			InputSchema: obj(map[string]interface{}{"id": str("任务 ID")}, "id"),
			Run: func(a *API, args map[string]interface{}) (interface{}, error) {
				id, _ := args["id"].(string)
				a.runner.Stop(id)
				return map[string]interface{}{"id": id, "status": StatusStopped}, nil
			},
		},
		{
			Name: "stream_overview", Title: "全局概览",
			Description: "一次拿到当前全局状态：有哪些直播流（是否在推、是否在录）、有哪些转推任务及其推送状态、录像总览。" +
				"适合作为「现在什么情况」的第一句提问，避免多次调用。",
			Method: "GET", Path: "/api/agent/overview", InputSchema: obj(nil),
			Run: func(a *API, _ map[string]interface{}) (interface{}, error) {
				// 流
				streams := []map[string]interface{}{}
				if list, err := a.srs.ListStreams(a.store.Settings().PublicBase); err == nil {
					recs := a.rec.ActiveByStream()
					for _, s := range list {
						item := map[string]interface{}{
							"key": s.Key, "active": s.Active, "video": s.Video, "kbps": s.KbpsRecv,
						}
						if r, ok := recs[s.Key]; ok {
							item["recording_seconds"] = int(r.Duration)
						}
						streams = append(streams, item)
					}
				}
				// 任务
				jobs := []map[string]interface{}{}
				running := 0
				for _, j := range a.store.Jobs() {
					rt := a.runner.Runtime(j.ID)
					if rt.Status == StatusRunning {
						running++
					}
					okTargets := 0
					for _, t := range rt.Targets {
						if t.Status == StatusRunning {
							okTargets++
						}
					}
					jobs = append(jobs, map[string]interface{}{
						"id": j.ID, "name": j.Name, "mode": j.Mode, "layout": j.Layout,
						"sources": j.Sources, "status": rt.Status, "detail": rt.Detail,
						"targets_total": len(rt.Targets), "targets_running": okTargets,
					})
				}
				// 录像
				all := a.rec.List()
				activeRec := 0
				var totalSize int64
				for _, r := range all {
					totalSize += r.Size
					if r.Active {
						activeRec++
					}
				}
				recent := []map[string]interface{}{}
				for i, r := range all {
					if i >= 5 {
						break
					}
					recent = append(recent, map[string]interface{}{
						"stream": r.Stream, "file": r.File,
						"duration": int(r.Duration), "size": r.Size,
						"active": r.Active, "needs_repair": r.NeedsRepair,
					})
				}
				return map[string]interface{}{
					"streams": map[string]interface{}{"count": len(streams), "items": streams},
					"jobs": map[string]interface{}{
						"count": len(jobs), "running": running, "items": jobs,
					},
					"recordings": map[string]interface{}{
						"count": len(all), "active": activeRec,
						"total_size_bytes": totalSize, "recent": recent,
					},
				}, nil
			},
		},
		{
			Name: "mix_and_push", Title: "多路混流并推送",
			Description: "一步完成「多路合流 + 推到第三方平台」：创建混流任务并立即启动。" +
				"例如「把 camera01 和 camera02 做成上下分屏推到 B 站」。layout 不填默认圆窗口(circle)。" +
				"创建后可用 jobs_list 看状态，用 job_stop 停止。",
			Method: "POST", Path: "/api/agent/mix_and_push", Mutating: true, TimeoutSeconds: 60,
			InputSchema: obj(map[string]interface{}{
				"name":    str("任务名，不填自动生成"),
				"sources": map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}, "description": "要合成的源流，例如 live/camera01 与 live/camera02"},
				"targets": map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}, "description": "第三方 RTMP 推流地址数组"},
				"layout":  str("布局：vstack(上下)/hstack(左右)/grid(宫格)/pip(画中画)/circle(圆窗口，默认)"),
				"width":   map[string]interface{}{"type": "integer", "description": "输出宽，默认 1280"},
				"height":  map[string]interface{}{"type": "integer", "description": "输出高，默认 720"},
				"fps":     map[string]interface{}{"type": "integer", "description": "帧率，默认 25"},
			}, "sources", "targets"),
			Run: func(a *API, args map[string]interface{}) (interface{}, error) {
				var srcs, tgs []string
				if v, ok := args["sources"].([]interface{}); ok {
					for _, x := range v {
						if s, ok := x.(string); ok && strings.TrimSpace(s) != "" {
							srcs = append(srcs, strings.TrimSpace(s))
						}
					}
				}
				if v, ok := args["targets"].([]interface{}); ok {
					for _, x := range v {
						if s, ok := x.(string); ok && strings.TrimSpace(s) != "" {
							tgs = append(tgs, strings.TrimSpace(s))
						}
					}
				}
				if len(srcs) < 2 {
					return nil, errors.New("sources 至少需要 2 路流；只有 1 路请用 job_create(mode=direct)")
				}
				if len(tgs) == 0 {
					return nil, errors.New("targets 不能为空，需给出至少一个第三方 RTMP 推流地址")
				}
				in := Job{
					Name:      orEmpty(strings.TrimSpace(strOf(args["name"])), "混流任务"),
					Mode:      "mix",
					Sources:   srcs,
					Layout:    orEmpty(strings.TrimSpace(strOf(args["layout"])), "circle"),
					ExtraURLs: tgs,
					Width:     intOf(args["width"], 1280),
					Height:    intOf(args["height"], 720),
					FPS:       intOf(args["fps"], 25),
					Enabled:   true,
					AudioMode: "mix",
				}
				if err := a.validateJob(&in); err != nil {
					return nil, err
				}
				if err := a.checkSelfLoop(&in); err != nil {
					return nil, err
				}
				j, err := a.store.AddJob(in)
				if err != nil {
					return nil, err
				}
				_, _ = a.runner.Start(j.ID)
				rt := a.runner.Runtime(j.ID)
				return map[string]interface{}{
					"id": j.ID, "name": j.Name, "layout": j.Layout,
					"status": rt.Status, "detail": rt.Detail,
					"sources": srcs, "targets": tgs,
					"hint": "任务已创建并启动；源流不在线时会自动等源流上线。用 jobs_list 查看状态。",
				}, nil
			},
		},
		{
			Name: "recordings_list", Title: "列出录像",
			Description: "列出录像文件：流名、时长、大小、是否正在录制、是否需要修复。",
			Method:      "GET", Path: "/api/agent/recordings", InputSchema: obj(nil),
			Run: func(a *API, _ map[string]interface{}) (interface{}, error) {
				recs := a.rec.List()
				out := make([]map[string]interface{}, 0, len(recs))
				for _, r := range recs {
					out = append(out, map[string]interface{}{
						"id": r.ID, "stream": r.Stream, "file": r.File,
						"duration": int(r.Duration), "size": r.Size,
						"active": r.Active, "needs_repair": r.NeedsRepair,
						"started_at": r.StartedAt,
					})
				}
				return map[string]interface{}{"count": len(out), "recordings": out}, nil
			},
		},
		{
			Name: "recording_start", Title: "开始录制",
			Description: "对指定流开始录制；即使该流当前离线也会挂着，等流上线后自动写入文件。",
			Method:      "POST", Path: "/api/agent/recording/start", Mutating: true,
			InputSchema: obj(map[string]interface{}{"stream": str("流名，如 live/camera01")}, "stream"),
			Run: func(a *API, args map[string]interface{}) (interface{}, error) {
				s, _ := args["stream"].(string)
				if strings.TrimSpace(s) == "" {
					return nil, errors.New("stream 不能为空")
				}
				m, err := a.rec.Start(s)
				if err != nil {
					return nil, err
				}
				return map[string]interface{}{"id": m.ID, "stream": m.Stream, "file": m.File}, nil
			},
		},
		{
			Name: "recording_stop", Title: "停止录制",
			Description: "停止指定录像并自动封装成可播放的 MP4。",
			Method:      "POST", Path: "/api/agent/recording/stop", Mutating: true, TimeoutSeconds: 60,
			InputSchema: obj(map[string]interface{}{"id": str("录像 ID")}, "id"),
			Run: func(a *API, args map[string]interface{}) (interface{}, error) {
				id, _ := args["id"].(string)
				if err := a.rec.Stop(id); err != nil {
					return nil, err
				}
				return map[string]interface{}{"id": id, "stopped": true}, nil
			},
		},
	}
}

func strOf(v interface{}) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func intOf(v interface{}, def int) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case string:
		var x int
		if _, err := fmt.Sscanf(n, "%d", &x); err == nil {
			return x
		}
	}
	return def
}

func agentToolByName(name string) *agentTool {
	for _, t := range agentTools() {
		if t.Name == name {
			return t
		}
	}
	return nil
}

// agentToolByPath 按 HTTP 路径找工具（/api/agent/streams → streams_list）。
func agentToolByPath(path string) *agentTool {
	p := "/" + strings.Trim(path, "/")
	for _, t := range agentTools() {
		if t.Path == p {
			return t
		}
	}
	return nil
}

// argsFromRequest 把 query 与 JSON body 合成 MCP 风格的 arguments。
func argsFromRequest(r *http.Request) map[string]interface{} {
	args := map[string]interface{}{}
	for k, v := range r.URL.Query() {
		if k == "key" || len(v) == 0 {
			continue
		}
		args[k] = v[0]
	}
	if r.Method != http.MethodGet && r.Body != nil {
		var body map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&body); err == nil {
			for k, v := range body {
				args[k] = v
			}
		}
	}
	return args
}

// handleAgentTool 是 /api/agent/... 的 HTTP 入口（供桌面 agent.tools 按固定 path 调用）。
func (a *API) handleAgentTool(path string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		t := agentToolByPath(path)
		if t == nil {
			writeErr(w, 404, fmt.Errorf("未知工具端点：%s", path))
			return
		}
		res, err := t.Run(a, argsFromRequest(r))
		if err != nil {
			writeErr(w, 400, err)
			return
		}
		writeJSON(w, 200, map[string]interface{}{"ok": true, "result": res})
	}
}
