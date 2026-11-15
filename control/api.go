package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
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
	eps       *EndpointRegistry
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

// accessKeyOf 从请求里提取访问密钥。**不保留任何过渡别名**，口径只有下面这几个：
//
//	X-Access-Key: <key>            ← 唯一口令头（桌面 / App / 标准客户端都发这个）
//	X-JetDesk-Agent-Token: <key>   ← MCP 身份头（协议规定的身份载体，可选）
//	Authorization: Bearer <key>    ← 仅 /api/*（标准 API 客户端）；/jetdesk/config 上不是密钥，见 configAccessKeyOf
//	?key=<key>                     ← 无头客户端（<video>/HLS/iframe/播放器页）
//	Cookie: jsk=<key>              ← 浏览器登录一次后自动携带
//
// 已删除（不再识别）：X-API-Key、X-JetDesk-Key、X-Auth-Token —— 有发送方就改发送方，
// 不再靠容器侧双读兜着。
func accessKeyOf(r *http.Request) string {
	for _, h := range []string{"X-Access-Key", "X-JetDesk-Agent-Token"} {
		if v := strings.TrimSpace(r.Header.Get(h)); v != "" {
			return v
		}
	}
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
	}
	if v := r.URL.Query().Get("key"); v != "" {
		return v
	}
	if c, err := r.Cookie("jsk"); err == nil && c.Value != "" {
		return c.Value
	}
	return ""
}

// configAccessKeyOf 只从「访问密钥」头里取，**绝不看 Authorization / query / cookie**。
//
// 为什么单独一个函数：/jetdesk/config 上的 Authorization 是**公开的 configToken**
// （随 .jet 分发，只防误连）。如果这里复用 accessKeyOf，桌面同时带
// `Authorization: Bearer jetool`（configToken）和 `X-Access-Key: <真口令>` 时没问题，
// 但只要顺序写错或少了头，就会拿 configToken 去比用户口令 —— 用户口令被公开常量盖住。
// 只认 X-Access-Key：X-API-Key / X-JetDesk-Key 已删除，没有过渡期。
func configAccessKeyOf(r *http.Request) string {
	return strings.TrimSpace(r.Header.Get("X-Access-Key"))
}

// bearerTokenOf 取 Authorization: Bearer 的值（没有则为空）。
func bearerTokenOf(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
}

// configTokenWant 返回当前生效的 configToken（防误连用，随包公开，不是口令）。
// 与 jdconfig 的 models.services.<service>.configToken 保持一致，可用环境变量覆盖。
func configTokenWant() string {
	return envOr("JETDESK_CONFIG_TOKEN", "jetool")
}

// keyOK 校验访问密钥。密钥为空时视为不启用防护（兼容旧部署）。
func (a *API) keyOK(r *http.Request) bool {
	want := a.store.AccessKey()
	if want == "" {
		return true
	}
	return accessKeyOf(r) == want
}

// auth 是对外的鉴权中间件：未通过一律 401，并告诉调用方该怎么带 key。
// noAuthPaths 是免密钥的白名单：健康检查与局域网发现（只返回服务名/地址，无业务数据）。
var noAuthPaths = map[string]bool{
	"/api/health":   true,
	"/api/discover": true,
}

func (a *API) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if noAuthPaths[r.URL.Path] || a.keyOK(r) {
			next(w, r)
			return
		}
		w.Header().Set("WWW-Authenticate", `Bearer realm="jetstream"`)
		writeErr(w, http.StatusUnauthorized,
			errors.New("访问密钥无效：请带 X-Access-Key / Authorization: Bearer / ?key= / Cookie jsk"))
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
		// 端口契约的解析结果（宿主端口由桌面分配），控制台据此展示推流地址，
		// 不再靠前端猜端口。
		"endpoints": WithPublishKey(a.eps.Resolve(r.Host), a.store.PublishKey()),
		// publish_key 是推流专用口令（与 access_key 分开）：控制台用它拼推流提示，
		// access_key 绝不能出现在 RTMP 地址里。
		"publish_key": a.store.PublishKey(),
	}
	writeJSON(w, 200, out)
}

// parseParamKey 从 SRS 回调的 param 里取 key（param 形如 "?key=abc" 或 "key=abc"）。
func parseParamKey(param string) string {
	p := strings.TrimPrefix(strings.TrimSpace(param), "?")
	if p == "" {
		return ""
	}
	vals, err := url.ParseQuery(p)
	if err != nil {
		return ""
	}
	return vals.Get("key")
}

func orEmpty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
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
//
// 地址一律用「请求方看到的 Host + 端点解析出的宿主端口」拼：
// 请求 Host 给出局域网 IP，端点解析给出桌面实际分配的宿主端口，
// 因此宿主端口冲突被改成 1936 时，这里返回的就是 1936，不是写死的 1935。
func (a *API) handleDiscover(w http.ResponseWriter, r *http.Request) {
	// 免鉴权接口，只放「够用且不敏感」的信息：
	// 客户端用它确认「这是捷兔流媒体服务」，然后带口令去 /api/endpoints 取地址与推流口令。
	//
	// 为什么不能在这里放 rtmp_base / publish_key：
	// 它对局域网完全开放，一旦返回推流口令，等于把「能推流」的权限公开 ——
	// 任何人扫一遍网段就能往这台服务器推垃圾流；返回完整端点地址也会泄露内网拓扑。
	writeJSON(w, 200, map[string]interface{}{
		"service": "jetstream",
		"name":    "捷兔流媒体服务",
		"version": "1.0.0",
		// auth_required：要拿地址/推流口令必须带访问密钥
		"auth_required": true,
		// 下一步该打的接口（相对路径，客户端用同一 host 拼）
		"endpoints_path": "/api/endpoints",
		// 口令头约定，省得客户端猜
		"auth_header": "X-Access-Key",
	})
}

// handleEndpoints 返回完整端点与推流口令（**需要访问密钥**，走 /api/ 鉴权中间件）。
//
// 原 /api/discover 的全部内容搬到这里：宿主端口、对外地址、推流口令、推流完整地址。
func (a *API) handleEndpoints(w http.ResponseWriter, r *http.Request) {
	st := a.store.Settings()
	// publishUrl 拼上推流口令（access_key 绝不进 RTMP 地址）；
	// url 保持干净，供只想要基址的客户端使用。
	eps := WithPublishKey(a.eps.Resolve(r.Host), a.store.PublishKey())

	// 推流基址：用户显式配置过「对外推流地址」就尊重它（老部署/手工指定），
	// 否则用端点解析结果，绝不写死 127.0.0.1 或容器端口。
	rtmpBase := strings.TrimSpace(st.RTMPPublic)
	rtmpPort := ""
	if rtmpBase != "" {
		if u, err := url.Parse(rtmpBase); err == nil && u.Port() != "" {
			rtmpPort = u.Port()
		}
		rtmpBase = ensureTrailingSlash(rtmpBase)
	} else if ep, ok := findRTMPEndpoint(eps); ok {
		rtmpBase = ep.URL
		rtmpPort = strconv.Itoa(ep.HostPort)
	}

	ui, uiPort, uiSource := "", 0, ""
	if ep, ok := primaryEndpoint(eps); ok {
		ui, uiPort, uiSource = ep.URL, ep.HostPort, ep.PortSource
	}

	writeJSON(w, 200, map[string]interface{}{
		"service": "jetstream",
		"name":    "捷兔流媒体服务",
		"version": "1.0.0",
		// 兼容老客户端字段名：rtmp_base / rtmp_port / ui
		"rtmp_base":   rtmpBase,
		"rtmp_port":   rtmpPort,
		"ui":          ui,
		"ui_port":     uiPort,
		"auto_record": st.AutoRecord,
		// publish_key 让客户端拼出能通过 on_publish 鉴权的推流地址。
		// 它是独立的推流口令（不是 access_key），泄露最多被占一条流；
		// 但仍只发给**已通过访问密钥校验**的调用方。
		"publish_key": a.store.PublishKey(),
		// endpoints 是端口契约的完整形态：宿主端口、来源、推流完整地址都在这里
		"endpoints": eps,
		// 便于排障：主入口端口是从哪来的（desktop-push / request-host / container-port …）
		"ui_port_source": uiSource,
		"srs_version":    func() string { v, _ := a.srs.Version(); return v }(),
	})
}

// findEndpoint 按名字取端点。
func findEndpoint(eps []ResolvedEndpoint, name string) (ResolvedEndpoint, bool) {
	for _, ep := range eps {
		if ep.Name == name {
			return ep, true
		}
	}
	return ResolvedEndpoint{}, false
}

// findRTMPEndpoint 优先取名为 rtmp 的端点，否则取第一个 rtmp/rtmps 协议的端点。
// 端点是可自定义的，不能只认名字，否则换个名字就退回容器端口。
func findRTMPEndpoint(eps []ResolvedEndpoint) (ResolvedEndpoint, bool) {
	if ep, ok := findEndpoint(eps, "rtmp"); ok {
		return ep, true
	}
	for _, ep := range eps {
		if ep.Protocol == "rtmp" || ep.Protocol == "rtmps" {
			return ep, true
		}
	}
	return ResolvedEndpoint{}, false
}

// primaryEndpoint 取首页主入口端点。
func primaryEndpoint(eps []ResolvedEndpoint) (ResolvedEndpoint, bool) {
	for _, ep := range eps {
		if ep.Primary {
			return ep, true
		}
	}
	return ResolvedEndpoint{}, false
}

// ensureTrailingSlash 给基址补尾部斜杠（rtmp://host:port 后要接 app 名）。
func ensureTrailingSlash(s string) string {
	if s == "" || strings.HasSuffix(s, "/") {
		return s
	}
	return s + "/"
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
		// 端口契约：每个端点的容器内端口 / 宿主端口 / 端口来源 / 对外地址。
		// 真机上「手机推到 1935、SRS 看不到流」这类问题，看这里的 hostPort 与 portSource 就能定位：
		// portSource=container-port 说明桌面没把分配的宿主端口告诉我们。
		"endpoints":         a.eps.Resolve(r.Host),
		"endpoint_warnings": a.eps.Warnings(),
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

// ---------------- 捷兔桌面：配置接收端（协议 B 面） ----------------

// configPushAllowed 判定配置推送是否放行。只有两个放行口，没有任何过渡后门：
//
//  1. 服务本身没配口令（effective == ""）—— 无凭据可校；
//  2. X-Access-Key == 当前生效口令 —— 唯一正规通道。
//
// 另有 JETDESK_CONFIG_STRICT=0，那是**排障开关**（默认关，运维显式打开），不是兼容通道。
//
// 已删除（不再存在）：
//   - 「生效口令仍是出厂 jetool 就放行」—— 出厂弱口令等于公开常量，不该持有改配置的权限；
//   - 「报文自证：body 里出现生效口令」—— 鉴权只看头，不看报文内容；
//   - 「Authorization 等值于口令也放行」—— 那个 Bearer 是公开 configToken，不是口令。
//
// Authorization: Bearer <configToken> 是另一码事（只防误连），在
// handleJetDeskConfig 里单独校验，且**不能**替代这里的口令门。
func (a *API) configPushAllowed(r *http.Request) (bool, string) {
	effective := a.store.AccessKey()

	// 放行口 1：没配口令
	if effective == "" {
		return true, "未配置口令"
	}
	// 排障开关（默认关）：仅运维显式打开时放宽，用完必须关回
	if v := strings.TrimSpace(os.Getenv("JETDESK_CONFIG_STRICT")); v == "0" || strings.EqualFold(v, "false") {
		return true, "JETDESK_CONFIG_STRICT=0 放宽"
	}
	// 放行口 2：唯一正规通道 —— X-Access-Key 等于当前口令
	// 这里**不能**用 accessKeyOf：它会识别 Authorization，而那是公开的 configToken。
	if supplied := configAccessKeyOf(r); supplied != "" && supplied == effective {
		return true, "X-Access-Key 匹配"
	}
	return false, ""
}

// handleJetDeskConfig 处理桌面推送的配置。
//
// 鉴权见 configPushAllowed（规范 §5.1 两道门）。通过后：
//   - 若 body 带 accessKey（或 key）→ **用新值覆盖**当前密钥（此后业务访问都用新 key）
//   - 其余字段（models / communityBaseUrl / instanceId / self）当前版本只记录，不改变业务
//
// 必须幂等：桌面会每 2 秒重试一次，同一份内容重复 POST 结果一致。
func (a *API) handleJetDeskConfig(w http.ResponseWriter, r *http.Request) {
	// GET 作为健康探针：规范里 healthPath 取自 configPath，App 会用 GET 打这里。
	// 只回 ok，不泄露任何配置，因此免鉴权；POST 仍要密钥。
	if r.Method == http.MethodGet {
		writeJSON(w, 200, map[string]interface{}{"ok": true, "service": "jetstream"})
		return
	}
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, errors.New("只接受 POST"))
		return
	}
	var in struct {
		Self             string                 `json:"self"`
		AccessKey        string                 `json:"accessKey"`
		AccessKeySnake   string                 `json:"access_key"`
		Key              string                 `json:"key"`
		Token            string                 `json:"token"`
		Models           map[string]interface{} `json:"models"`
		CommunityBaseURL string                 `json:"communityBaseUrl"`
		InstanceID       string                 `json:"instanceId"`
		// 端口事实：endpoints 只放 name/service/protocol/containerPort/hostPort，
		// 不放 url/path（容器用自己 jdconfig 的 path/publishPath/keyQuery 拼地址）。
		// 宿主端口冲突时桌面会把 1935 改成 1936，必须第一时间告诉容器，
		// 否则容器只能按容器内端口对外报地址，客户端就会推到被占用的端口上。
		// self 是 compose 服务名 / 实例 key（不是 instanceId）；instanceId 单独给。
		Endpoints []struct {
			Name          string `json:"name"`
			ContainerPort int    `json:"containerPort"`
			HostPort      int    `json:"hostPort"`
			Port          int    `json:"port"`
			Host          string `json:"host"`
		} `json:"endpoints"`
		Ports map[string]json.Number `json:"ports"`
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	// 第一道门：Authorization: Bearer <configToken>（公开常量，只防「打错地址」）。
	// 带了就必须等于 configToken —— 不认「等值于访问密钥」这种过渡写法。
	// 注意：它**不是**访问密钥，不能替代下面第二道门。
	if tok := bearerTokenOf(r); tok != "" && tok != configTokenWant() {
		log.Printf("拒绝 /jetdesk/config：configToken 不匹配（来自 %s）", r.RemoteAddr)
		writeErr(w, http.StatusUnauthorized, errors.New("访问密钥无效：配置令牌不匹配"))
		return
	}

	// 第二道门：访问密钥 —— 只看 X-Access-Key，报文内容不参与鉴权。
	bodyKeys := []string{
		strings.TrimSpace(in.AccessKey),
		strings.TrimSpace(in.AccessKeySnake),
		strings.TrimSpace(in.Key),
		strings.TrimSpace(in.Token),
	}
	if ok, why := a.configPushAllowed(r); !ok {
		log.Printf("拒绝 /jetdesk/config：访问密钥无效（来自 %s）", r.RemoteAddr)
		writeErr(w, http.StatusUnauthorized, errors.New("访问密钥无效"))
		return
	} else if why != "X-Access-Key 匹配" {
		log.Printf("放行 /jetdesk/config：%s（来自 %s）", why, r.RemoteAddr)
	}

	oldKey := a.store.AccessKey()
	newKey := ""
	for _, v := range bodyKeys {
		if v != "" {
			newKey = v
			break
		}
	}
	keyUpdated := false
	if newKey != "" && newKey != oldKey {
		if err := a.store.SetAccessKey(newKey); err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		keyUpdated = true
		log.Printf("访问密钥已由桌面覆盖（self=%s instanceId=%s）", in.Self, in.InstanceID)
	}
	if in.CommunityBaseURL != "" || in.InstanceID != "" {
		log.Printf("桌面配置：self=%s instanceId=%s community=%s models=%d 项",
			in.Self, in.InstanceID, in.CommunityBaseURL, len(in.Models))
	}

	// 端口契约：把桌面分配的宿主端口记下来（每个端点优先用 endpoints[]，其次 ports 映射）。
	ports, hosts := a.bindingFromPush(in.Endpoints, in.Ports)
	if a.eps.SetAssigned(ports, hosts, nowStr()) {
		log.Printf("宿主端口已由桌面告知：%v（此后对外地址按这些端口拼）", ports)
	}

	// 回报当前生效的端点：
	//   endpoints —— 只含端口事实（name/service/protocol/containerPort/hostPort/portSource），
	//                形状与推上来的请求一致，桌面可直接逐项比对（不一致＝契约没走通）；
	//   resolved  —— 容器自己按 jdconfig 的 path/publishPath/keyQuery + 上面的 hostPort 拼出的地址，
	//                仅供排查，桌面不必依赖它。
	writeJSON(w, 200, map[string]interface{}{
		"ok":         true,
		"service":    "jetstream",
		"keyUpdated": keyUpdated,
		"self":       in.Self,
		"instanceId": in.InstanceID,
		"endpoints":  a.eps.ResolveFacts(r.Host),
		"resolved":   a.eps.Resolve(r.Host),
	})
}

// bindingFromPush 把推送内容翻译成「端点名 → 宿主端口」。
// 兼容三种写法：
//
//	endpoints:[{name:"rtmp",hostPort:1936}]
//	endpoints:[{containerPort:1935,hostPort:1936}]   （按容器内端口匹配声明表）
//	ports:{"rtmp":1936} / ports:{"1935":1936}        （更省事的简写）
func (a *API) bindingFromPush(items []struct {
	Name          string `json:"name"`
	ContainerPort int    `json:"containerPort"`
	HostPort      int    `json:"hostPort"`
	Port          int    `json:"port"`
	Host          string `json:"host"`
}, ports map[string]json.Number) (map[string]int, map[string]string) {
	declared := a.eps.Declared()
	nameOfContainerPort := func(p int) string {
		for _, ep := range declared {
			if ep.ContainerPort == p {
				return ep.Name
			}
		}
		return ""
	}

	out := map[string]int{}
	hosts := map[string]string{}
	// 先处理 ports 简写，再用 endpoints[] 覆盖：完整写法优先于简写
	for k, v := range ports {
		name := strings.ToLower(strings.TrimSpace(k))
		if name == "" {
			continue
		}
		if n, err := strconv.Atoi(name); err == nil {
			name = nameOfContainerPort(n)
		}
		if name == "" {
			continue
		}
		p, err := strconv.Atoi(v.String())
		if err != nil || p <= 0 || p > 65535 {
			continue
		}
		out[name] = p
	}
	for _, it := range items {
		name := strings.ToLower(strings.TrimSpace(it.Name))
		if name == "" && it.ContainerPort > 0 {
			name = nameOfContainerPort(it.ContainerPort)
		}
		if name == "" {
			continue
		}
		hp := it.HostPort
		if hp <= 0 {
			hp = it.Port
		}
		if hp > 0 && hp <= 65535 {
			out[name] = hp
		}
		if h := strings.TrimSpace(it.Host); h != "" {
			hosts[name] = h
		}
	}
	return out, hosts
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
	// 推流鉴权：SRS 把推流 URL 的 query 放在 param 里（如 "?key=jetool"）。
	// 密钥不匹配直接返回非 0，SRS 会拒绝这次推流。
	if action == "on_publish" {
		// 推流口令优先用独立的 PublishKey（/api/discover 会公开它），没配时回退 AccessKey。
		want := a.store.PublishKey()
		if want != "" {
			got := parseParamKey(in.Param)
			if got != want {
				log.Printf("拒绝推流 %s：密钥%s", key, orEmpty(got, "缺失"))
				writeJSON(w, 200, map[string]int{"code": 1})
				return
			}
		}
	}

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
