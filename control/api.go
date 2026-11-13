package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

var errNotFound = errors.New("对象不存在")

// API 聚合了所有依赖。
type API struct {
	store     *Store
	srs       *SRSClient
	runner    *Runner
	rec       *Recorder
	mediaDir  string
	token     string
	startedAt time.Time
	access    *accessLog
}

func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func readJSON(r *http.Request, v interface{}) error {
	defer r.Body.Close()
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	return dec.Decode(v)
}

// publicBase 计算对外可访问的 HTTP 地址（用于播放 FLV/HLS）。
func (a *API) publicBase(r *http.Request) string {
	// 配置了对外地址就用它；否则返回空，由前端用「当前访问地址 + 反代路径」拼播放链接，
	// 这样无论 SRS 的 8080 映射到宿主机哪个端口都能播放。
	if s := a.store.Settings().PublicBase; s != "" {
		return strings.TrimRight(s, "/")
	}
	return ""
}

func (a *API) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.token != "" {
			t := r.Header.Get("X-Auth-Token")
			if t == "" {
				t = r.URL.Query().Get("token")
			}
			if t != a.token {
				writeErr(w, http.StatusUnauthorized, errors.New("token 校验失败"))
				return
			}
		}
		next(w, r)
	}
}

// ---------------- 概览 / 流 ----------------

func (a *API) handleOverview(w http.ResponseWriter, r *http.Request) {
	version, verr := a.srs.Version()
	streams, serr := a.srs.ListStreams(a.publicBase(r))
	active := 0
	for _, s := range streams {
		if s.Active {
			active++
		}
	}
	jobs := a.store.Jobs()
	running := 0
	for _, j := range jobs {
		if a.runner.Runtime(j.ID).Status == StatusRunning {
			running++
		}
	}
	out := map[string]interface{}{
		"ok":                true,
		"time":              nowStr(),
		"uptime_seconds":    int(time.Since(a.startedAt).Seconds()),
		"srs_version":       version,
		"srs_error":         errStr(verr),
		"streams_total":     len(streams),
		"streams_active":    active,
		"jobs_total":        len(jobs),
		"jobs_running":      running,
		"recordings_total":  len(a.rec.List()),
		"recordings_active": len(a.rec.Active()),
		"targets_total":     len(a.store.Targets()),
		"settings":          a.store.Settings(),
		"streams_error":     errStr(serr),
	}
	writeJSON(w, 200, out)
}

func errStr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func (a *API) handleStreams(w http.ResponseWriter, r *http.Request) {
	list, err := a.srs.ListStreams(a.publicBase(r))
	if err != nil {
		writeErr(w, 502, err)
		return
	}
	// 合并"是否正在录制"，让前端能显示状态并把按钮切成"停止录制"
	if recs := a.rec.ActiveByStream(); len(recs) > 0 {
		for i := range list {
			if rb, ok := recs[list[i].Key]; ok {
				b := rb
				list[i].Recording = &b
			}
		}
	}
	writeJSON(w, 200, map[string]interface{}{
		"streams":     list,
		"auto_record": a.store.Settings().AutoRecord,
	})
}

// handleDiscover 供局域网内的客户端（如捷兔行迹 App）自动发现本服务。
// 返回的地址用请求方看到的 Host 拼装，因此手机/PC 在局域网内都能直接拿到可用的推流地址。
func (a *API) handleDiscover(w http.ResponseWriter, r *http.Request) {
	st := a.store.Settings()
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	// RTMP 对外端口：优先从「对外推流地址」里取，其次容器内地址，最后默认 1935
	rtmpPort := "1935"
	for _, cand := range []string{st.RTMPPublic, st.RTMPBase} {
		if u, err := url.Parse(cand); err == nil && u.Port() != "" {
			rtmpPort = u.Port()
			break
		}
	}
	writeJSON(w, 200, map[string]interface{}{
		"service":     "jetstream",
		"name":        "捷兔流媒体服务",
		"version":     "1.0.0",
		"srs_version": func() string { v, _ := a.srs.Version(); return v }(),
		"rtmp_base":   fmt.Sprintf("rtmp://%s:%s/live/", host, rtmpPort),
		"rtmp_port":   rtmpPort,
		"ui":          fmt.Sprintf("http://%s", r.Host),
		"auto_record": st.AutoRecord,
	})
}

// handleDiag 汇总排障信息：地址是否正确、客户端到底请求了什么、录像文件是否可播。
func (a *API) handleDiag(w http.ResponseWriter, r *http.Request) {
	st := a.store.Settings()
	out := map[string]interface{}{
		"time":        nowStr(),
		"rtmp_in":     st.RTMPBase,
		"rtmp_public": st.RTMPPublic,
		"srs_api":     st.SRSAPI,
		"srs_http":    st.SRSHTTP,
		"public_base": st.PublicBase,
		"request": map[string]string{
			"host":  r.Host,
			"proto": "http",
		},
	}
	if v, err := a.srs.Version(); err == nil {
		out["srs_version"] = v
	} else {
		out["srs_error"] = err.Error()
	}
	if streams, err := a.srs.ListStreams(a.publicBase(r)); err == nil {
		out["streams"] = streams
	}
	recs := a.rec.List()
	health := make([]map[string]interface{}, 0, len(recs))
	for _, m := range recs {
		h := map[string]interface{}{
			"file": m.File, "stream": m.Stream,
			"size": m.Size, "duration": m.Duration,
			"active": m.Active, "needs_repair": m.NeedsRepair,
		}
		health = append(health, h)
	}
	out["recordings"] = health
	if a.access != nil {
		out["recent_requests"] = a.access.Recent(60)
	}
	writeJSON(w, 200, out)
}

func (a *API) handleLayouts(w http.ResponseWriter, r *http.Request) {
	type layout struct {
		Key  string `json:"key"`
		Name string `json:"name"`
		Desc string `json:"desc"`
		Min  int    `json:"min_sources"`
		Max  int    `json:"max_sources"`
	}
	out := []layout{
		{LayoutSingle, "单画面", "单路铺满整幅画面（多路时只用第一路）", 1, 1},
		{LayoutVStack, "上下分屏", "多路竖向等分排列", 2, 6},
		{LayoutHStack, "左右分屏", "多路横向等分排列", 2, 6},
		{LayoutGrid, "宫格", "自动按方形宫格排布（2~9 路）", 2, 9},
		{LayoutPip, "画中画", "第一路铺满，其余为方窗小画面叠在角落", 2, 5},
		{LayoutCircle, "圆窗口", "第一路铺满，其余为圆形窗口叠在角落", 2, 5},
	}
	writeJSON(w, 200, map[string]interface{}{"layouts": out})
}

// ---------------- 推流目标 ----------------

func (a *API) handleTargets(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, 200, map[string]interface{}{"targets": a.store.Targets()})
	case http.MethodPost:
		var in Target
		if err := readJSON(r, &in); err != nil {
			writeErr(w, 400, err)
			return
		}
		if err := validatePushURL(in.URL); err != nil {
			writeErr(w, 400, err)
			return
		}
		t, err := a.store.AddTarget(in)
		if err != nil {
			writeErr(w, 400, err)
			return
		}
		writeJSON(w, 201, t)
	default:
		writeErr(w, 405, errors.New("方法不支持"))
	}
}

func (a *API) handleTargetByID(w http.ResponseWriter, r *http.Request, id string) {
	switch r.Method {
	case http.MethodGet:
		t, ok := a.store.TargetByID(id)
		if !ok {
			writeErr(w, 404, errNotFound)
			return
		}
		writeJSON(w, 200, t)
	case http.MethodPut, http.MethodPatch:
		var in Target
		if err := readJSON(r, &in); err != nil {
			writeErr(w, 400, err)
			return
		}
		if err := validatePushURL(in.URL); err != nil {
			writeErr(w, 400, err)
			return
		}
		t, err := a.store.UpdateTarget(id, in)
		if err != nil {
			writeErr(w, 404, err)
			return
		}
		writeJSON(w, 200, t)
	case http.MethodDelete:
		if err := a.store.DeleteTarget(id); err != nil {
			writeErr(w, 404, err)
			return
		}
		writeJSON(w, 200, map[string]interface{}{"deleted": id})
	default:
		writeErr(w, 405, errors.New("方法不支持"))
	}
}

func validatePushURL(u string) error {
	u = strings.TrimSpace(u)
	if u == "" {
		return errors.New("推流地址不能为空")
	}
	if !strings.HasPrefix(u, "rtmp://") && !strings.HasPrefix(u, "rtmps://") {
		return errors.New("推流地址需以 rtmp:// 或 rtmps:// 开头")
	}
	rest := strings.TrimPrefix(strings.TrimPrefix(u, "rtmps://"), "rtmp://")
	if !strings.Contains(rest, "/") {
		return errors.New("推流地址需包含 app 与流名，例如 rtmp://live.twitch.tv/app/xxx")
	}
	return nil
}

// ---------------- 转推任务 ----------------

type jobView struct {
	Job
	Runtime JobRuntime `json:"runtime"`
}

func (a *API) jobView(j Job) jobView {
	return jobView{Job: j, Runtime: a.runner.Runtime(j.ID)}
}

func (a *API) handleJobs(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		jobs := a.store.Jobs()
		out := make([]jobView, 0, len(jobs))
		for _, j := range jobs {
			out = append(out, a.jobView(j))
		}
		writeJSON(w, 200, map[string]interface{}{"jobs": out})
	case http.MethodPost:
		var in Job
		if err := readJSON(r, &in); err != nil {
			writeErr(w, 400, err)
			return
		}
		if err := a.validateJob(&in); err != nil {
			writeErr(w, 400, err)
			return
		}
		if err := a.checkSelfLoop(&in); err != nil {
			writeErr(w, 400, err)
			return
		}
		j, err := a.store.AddJob(in)
		if err != nil {
			writeErr(w, 400, err)
			return
		}
		if j.AutoStart && j.Enabled {
			_, _ = a.runner.Start(j.ID)
		}
		writeJSON(w, 201, a.jobView(j))
	default:
		writeErr(w, 405, errors.New("方法不支持"))
	}
}

func (a *API) validateJob(j *Job) error {
	// 先补齐默认值，再校验，避免前端只提交部分字段时报错。
	*j = defaultJob(*j)
	if j.Name == "" {
		j.Name = "未命名任务"
	}
	if j.Mode != "direct" && j.Mode != "mix" {
		return errors.New("mode 只能是 direct 或 mix")
	}
	srcs := make([]string, 0, len(j.Sources))
	for _, s := range j.Sources {
		s = strings.TrimPrefix(strings.TrimSpace(s), "/")
		if s != "" {
			srcs = append(srcs, s)
		}
	}
	j.Sources = srcs
	if len(srcs) == 0 {
		return errors.New("至少需要一个源流，例如 live/camera01")
	}
	if j.Mode == "direct" && len(srcs) > 1 {
		return errors.New("单流转推（direct）只能有一个源流，多路请使用 mix")
	}
	valid := false
	for _, l := range ValidLayouts {
		if j.Layout == l {
			valid = true
		}
	}
	if !valid {
		return fmt.Errorf("layout 只能是 %s", strings.Join(ValidLayouts, "/"))
	}
	if j.Width < 128 || j.Height < 128 || j.Width > 3840 || j.Height > 2160 {
		return errors.New("输出分辨率需在 128~3840 之间")
	}
	if j.FPS < 5 || j.FPS > 60 {
		return errors.New("帧率需在 5~60 之间")
	}
	if j.PipSize < 5 || j.PipSize > 90 {
		return errors.New("画中画尺寸需在 5~90（占画面宽度百分比）")
	}
	for _, u := range j.ExtraURLs {
		if strings.TrimSpace(u) == "" {
			continue
		}
		if err := validatePushURL(u); err != nil {
			return err
		}
	}
	return nil
}

// checkSelfLoop 拦住"把流推回本容器自己的 SRS、且流名与源相同"这种必然死循环的配置。
func (a *API) checkSelfLoop(j *Job) error {
	local := a.runner.localRTMPAddr()
	urls := a.store.ResolveURLs(*j)
	for _, u := range j.ExtraURLs {
		if u = strings.TrimSpace(u); u != "" {
			urls = append(urls, u)
		}
	}
	for _, raw := range urls {
		pu, err := url.Parse(raw)
		if err != nil {
			continue
		}
		host := pu.Hostname()
		if host != "127.0.0.1" && host != "localhost" && host != "::1" && host != "0.0.0.0" {
			continue
		}
		port := pu.Port()
		if port == "" {
			port = "1935"
		}
		if local != "" && (host+":"+port) != local {
			continue // 回环但端口不同，可能真有服务，放行（仅提示）
		}
		path := strings.Trim(strings.TrimSuffix(pu.Path, "/"), " ")
		for _, src := range j.Sources {
			if path == "/"+src {
				gw := detectHostGateway()
				hint := ""
				if gw != "" {
					hint = fmt.Sprintf("例如 rtmp://%s:%s%s", gw, port, path)
				}
				addr := "宿主机 IP"
				if hint != "" {
					addr = "宿主机可达地址（如 " + hint + "）"
				}
				return fmt.Errorf("目标 %s 指向本容器自己的 SRS，且流名与源流相同（%s），会形成自环而不断断开。"+
					"如果要推到宿主机上的其他 SRS，请把 127.0.0.1 换成%s，"+
					"或使用 host.docker.internal（容器需带 --add-host=host.docker.internal:host-gateway）",
					raw, src, addr)
			}
		}
	}
	return nil
}

// detectHostGateway 读取容器默认网关（通常就是宿主机在 docker 网络里的地址）。
func detectHostGateway() string {
	b, err := os.ReadFile("/proc/net/route")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n")[1:] {
		f := strings.Fields(line)
		if len(f) < 3 || f[1] != "00000000" {
			continue
		}
		hex := f[2] // 小端序
		if len(hex) != 8 {
			continue
		}
		var ip [4]uint64
		for i := 0; i < 4; i++ {
			v, err := strconv.ParseUint(hex[6-i*2:8-i*2], 16, 8)
			if err != nil {
				return ""
			}
			ip[i] = v
		}
		return fmt.Sprintf("%d.%d.%d.%d", ip[0], ip[1], ip[2], ip[3])
	}
	return ""
}

func (a *API) handleJobByID(w http.ResponseWriter, r *http.Request, rest string) {
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	id := parts[0]
	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}
	if id == "" {
		writeErr(w, 404, errNotFound)
		return
	}

	switch action {
	case "":
		switch r.Method {
		case http.MethodGet:
			j, ok := a.store.JobByID(id)
			if !ok {
				writeErr(w, 404, errNotFound)
				return
			}
			writeJSON(w, 200, a.jobView(j))
		case http.MethodPut, http.MethodPatch:
			var in Job
			if err := readJSON(r, &in); err != nil {
				writeErr(w, 400, err)
				return
			}
			if err := a.validateJob(&in); err != nil {
				writeErr(w, 400, err)
				return
			}
			if err := a.checkSelfLoop(&in); err != nil {
				writeErr(w, 400, err)
				return
			}
			j, err := a.store.UpdateJob(id, in)
			if err != nil {
				writeErr(w, 404, err)
				return
			}
			// 若在运行中改配置，则按新配置重启。
			if a.runner.IsActive(id) {
				_, _ = a.runner.Restart(id)
			}
			writeJSON(w, 200, a.jobView(j))
		case http.MethodDelete:
			a.runner.Stop(id)
			if err := a.store.DeleteJob(id); err != nil {
				writeErr(w, 404, err)
				return
			}
			writeJSON(w, 200, map[string]interface{}{"deleted": id})
		default:
			writeErr(w, 405, errors.New("方法不支持"))
		}
	case "start":
		if _, ok := a.store.JobByID(id); !ok {
			writeErr(w, 404, errNotFound)
			return
		}
		if _, err := a.runner.Start(id); err != nil {
			writeErr(w, 400, err)
			return
		}
		time.Sleep(300 * time.Millisecond)
		writeJSON(w, 200, a.runner.Runtime(id))
	case "stop":
		a.runner.Stop(id)
		writeJSON(w, 200, a.runner.Runtime(id))
	case "restart":
		if _, err := a.runner.Restart(id); err != nil {
			writeErr(w, 400, err)
			return
		}
		time.Sleep(300 * time.Millisecond)
		writeJSON(w, 200, a.runner.Runtime(id))
	case "log":
		n := 200
		if v := r.URL.Query().Get("lines"); v != "" {
			if x, err := strconv.Atoi(v); err == nil && x > 0 {
				n = x
			}
		}
		writeJSON(w, 200, map[string]interface{}{
			"lines":   a.runner.Logs(id, n),
			"runtime": a.runner.Runtime(id),
		})
	default:
		writeErr(w, 404, errNotFound)
	}
}

// ---------------- 录像 ----------------

func (a *API) handleRecordings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		list := a.rec.List()
		for i := range list {
			list[i].URL = "/media/" + list[i].File
		}
		writeJSON(w, 200, map[string]interface{}{
			"recordings": list,
			"active":     a.rec.Active(),
			"dir":        a.mediaDir,
		})
	case http.MethodPost:
		var in struct {
			Stream string `json:"stream"`
		}
		if err := readJSON(r, &in); err != nil {
			writeErr(w, 400, err)
			return
		}
		m, err := a.rec.Start(in.Stream)
		if err != nil {
			writeErr(w, 400, err)
			return
		}
		writeJSON(w, 201, m)
	default:
		writeErr(w, 405, errors.New("方法不支持"))
	}
}

func (a *API) handleRecordingByID(w http.ResponseWriter, r *http.Request, rest string) {
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	id := parts[0]
	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}
	switch action {
	case "stop":
		if err := a.rec.Stop(id); err != nil {
			writeErr(w, 404, err)
			return
		}
		writeJSON(w, 200, map[string]interface{}{"stopped": id})
		return
	case "hls":
		// 生成 HLS 切片，供手机/微信等对整段 MP4 支持不佳的客户端播放
		id2, url, err := a.rec.HLS(id)
		if err != nil {
			writeErr(w, 400, err)
			return
		}
		writeJSON(w, 200, map[string]string{"id": id2, "url": url})
		return
	case "repair":
		// 重新转封装：修复索引/时长，让浏览器能正常播放与拖动
		m, err := a.rec.Repair(id)
		if err != nil {
			writeErr(w, 400, err)
			return
		}
		m.URL = "/media/" + m.File
		writeJSON(w, 200, m)
		return
	case "":
	default:
		writeErr(w, 404, errNotFound)
		return
	}
	switch r.Method {
	case http.MethodGet:
		m, ok := a.rec.Get(id)
		if !ok {
			writeErr(w, 404, errNotFound)
			return
		}
		m.URL = "/media/" + m.File
		writeJSON(w, 200, m)
	case http.MethodDelete:
		if err := a.rec.Delete(id); err != nil {
			writeErr(w, 404, err)
			return
		}
		writeJSON(w, 200, map[string]interface{}{"deleted": id})
	default:
		writeErr(w, 405, errors.New("方法不支持"))
	}
}

// serveMedia 提供录像播放（支持 Range，可拖动进度条）。
func (a *API) serveMedia(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/media/")
	if name == "" || strings.Contains(name, "..") || strings.Contains(name, "/") {
		http.NotFound(w, r)
		return
	}
	path := filepath.Join(a.mediaDir, name)
	if !fileExists(path) {
		http.NotFound(w, r)
		return
	}
	if r.URL.Query().Get("download") == "1" {
		w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	} else {
		w.Header().Set("Content-Disposition", `inline; filename="`+name+`"`)
	}
	http.ServeFile(w, r, path)
}

// ---------------- 设置 ----------------

func (a *API) handleSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, 200, a.store.Settings())
	case http.MethodPut:
		var in Settings
		if err := readJSON(r, &in); err != nil {
			writeErr(w, 400, err)
			return
		}
		if err := a.store.SetSettings(in); err != nil {
			writeErr(w, 400, err)
			return
		}
		a.rec.SetSegmentMinutes(in.RecordSegmentMinutes)
		writeJSON(w, 200, a.store.Settings())
	default:
		writeErr(w, 405, errors.New("方法不支持"))
	}
}

// ---------------- SRS 回调 ----------------

type hookBody struct {
	Action   string `json:"action"`
	ClientID string `json:"client_id"`
	IP       string `json:"ip"`
	Vhost    string `json:"vhost"`
	App      string `json:"app"`
	Stream   string `json:"stream"`
	Param    string `json:"param"`
	ServerID string `json:"server_id"`
}

func (a *API) handleHook(w http.ResponseWriter, r *http.Request, action string) {
	var in hookBody
	_ = json.NewDecoder(r.Body).Decode(&in)
	key := strings.TrimPrefix(in.App+"/"+in.Stream, "/")
	// 注意：这些操作（启动/停止 ffmpeg、转封装）可能耗时数秒，
	// 必须在后台执行，否则 SRS 的 http_hooks 会超时（实测会卡 10 秒以上）。
	switch action {
	case "on_publish":
		autoRecord := a.store.Settings().AutoRecord
		go func() {
			if autoRecord && in.App != "mix" {
				if _, err := a.rec.Start(key); err != nil {
					fmt.Printf("自动录制失败 %s: %v\n", key, err)
				}
			}
			a.runner.OnStreamOnline(key)
		}()
	case "on_unpublish":
		go a.rec.StopByStream(key)
	}
	// SRS 期望返回 code=0 才放行。
	writeJSON(w, 200, map[string]int{"code": 0})
}
