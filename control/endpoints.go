package main

// 端点契约（.jet「端口/地址契约」的服务端实现）
// =====================================================
//
// 背景：.jet 过去只在 jdconfig 里写一个 endpointPort，包里所有「对外地址」都是写死的
// （compose 里 PUBLIC_RTMP=rtmp://127.0.0.1:1935/）。当捷兔桌面因为宿主端口冲突把
// SRS 的宿主端口从 1935 自动改成 1936 时，容器完全不知情：桌面/社区上报 1936，
// 控制面 /api/discover 仍返回 1935，手机于是推到占用 1935 的别的进程上。
//
// 本文件建立的规则：
//
//  1. 包里声明的是**容器内端口**（jdconfig.endpoints[].containerPort），永远不变；
//  2. **宿主端口由桌面分配**，容器不猜、不写死，只接受外部告知（通道见下，实现在 hostPortFor）；
//  3. 对外地址一律用「请求方看到的 Host + 解析出的宿主端口」拼，不写死 127.0.0.1；
//  4. 宿主端口未知时退回容器端口，并在 /api/diag 里标出来源，便于真机排查。
//
// 桌面告知宿主端口的通道（优先级从高到低，前三个是推荐通道）：
//
//	A. POST /jetdesk/config  {"endpoints":[{"name":"rtmp","hostPort":1936}]}  ← 实时、权威
//	B. 环境变量 JETDESK_PORT_<containerPort>   例：JETDESK_PORT_1935=1936
//	C. 环境变量 JETDESK_ENDPOINT_<NAME>_PORT   例：JETDESK_ENDPOINT_RTMP_PORT=1936
//	D. 环境变量 JETDESK_ENDPOINTS='[{"name":"rtmp","containerPort":1935,"hostPort":1936}]'
//	E. 旧变量 SRS_RTMP_PORT / UI_PORT（只有显式设置时才采信，见 hostPortFor 与 envPort）
//	F. 请求 Host 里的端口（仅主入口 http 端点，桌面打开界面时天然带上）
//	G. 都没有 → 容器内端口（并在 diag 标记 source=container-port）

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
)

// Endpoint 是一个对外端点。前 7 个字段与 jdconfig 的 endpoints[] 一一对应；
// HostPort / Host 是运行时由桌面告知的信息，不回写 jdconfig。
type Endpoint struct {
	Name          string `json:"name"`                  // 稳定标识，用于环境变量与上报
	Service       string `json:"service,omitempty"`     // compose 服务名
	Protocol      string `json:"protocol"`              // http/https/rtmp/rtmps/tcp/udp
	ContainerPort int    `json:"containerPort"`         // 容器内端口，必填
	Primary       bool   `json:"primary,omitempty"`     // 首页主入口，最多一个
	Path          string `json:"path,omitempty"`        // URL 路径前缀
	PublishPath   string `json:"publishPath,omitempty"` // 推流等完整路径
	// KeyQuery 是这个端点携带访问口令的 query 参数名（本服务实现是 key）。
	// 桌面按它把口令拼进入口 URL，例如 http://host:1986/?key=xxx；
	// 不配的话桌面不会拼口令，受保护的入口打不开 —— 这就是「必须配置 keyQuery」的原因。
	KeyQuery   string `json:"keyQuery,omitempty"`   // 口令参数名，默认 key
	HostPort   int    `json:"hostPort,omitempty"`   // 宿主端口（桌面分配）
	Host       string `json:"host,omitempty"`       // 宿主对外主机名/IP（可选）
	PortSource string `json:"portSource,omitempty"` // 该宿主端口是谁给的（排障用）
}

// ResolvedEndpoint 是「容器内端口 + 宿主端口 + 对外主机」合成后的最终结果。
type ResolvedEndpoint struct {
	Endpoint
	URL        string `json:"url"`                  // 基址，如 rtmp://192.168.1.10:1936/live/
	PublishURL string `json:"publishUrl,omitempty"` // 推流完整地址（不含口令）
}

// defaultEndpoints 是 .jet 未写 endpoints[] 时的等价默认值，与历史行为完全一致：
// 控制面 1986 是首页入口，SRS 的 1935 是推流入口。
func defaultEndpoints() []Endpoint {
	return []Endpoint{
		{Name: "http", Service: "control", Protocol: "http", ContainerPort: 1986, Primary: true, Path: "/", KeyQuery: "key"},
		{Name: "rtmp", Service: "srs", Protocol: "rtmp", ContainerPort: 1935, Path: "/live/", PublishPath: "/live/stream", KeyQuery: "key"},
	}
}

// validProtocol 判断协议是否可拼成 URL。
func validProtocol(p string) bool {
	switch strings.ToLower(strings.TrimSpace(p)) {
	case "http", "https", "rtmp", "rtmps":
		return true
	}
	return false
}

// defaultPortOf 返回协议的缺省端口。
func defaultPortOf(protocol string) int {
	switch strings.ToLower(protocol) {
	case "http":
		return 80
	case "https":
		return 443
	case "rtmp":
		return 1935
	case "rtmps":
		return 443
	}
	return 0
}

// normEndpoints 校验并规范声明表：丢掉非法项、补默认值、保证最多一个 primary。
// 返回 (规范表, 警告列表)。
func normEndpoints(in []Endpoint) ([]Endpoint, []string) {
	var warns []string
	out := make([]Endpoint, 0, len(in))
	seen := map[string]bool{}
	primary := false
	for _, ep := range in {
		ep.Name = strings.ToLower(strings.TrimSpace(ep.Name))
		ep.Protocol = strings.ToLower(strings.TrimSpace(ep.Protocol))
		ep.Service = strings.TrimSpace(ep.Service)
		ep.Host = strings.TrimSpace(ep.Host)
		if ep.Name == "" {
			warns = append(warns, "endpoints 里有一项没有 name，已忽略")
			continue
		}
		if ep.ContainerPort <= 0 || ep.ContainerPort > 65535 {
			warns = append(warns, fmt.Sprintf("endpoint %s 的 containerPort 无效（%d），已忽略", ep.Name, ep.ContainerPort))
			continue
		}
		if !validProtocol(ep.Protocol) {
			warns = append(warns, fmt.Sprintf("endpoint %s 的 protocol %q 不支持拼 URL，按 http 处理", ep.Name, ep.Protocol))
			ep.Protocol = "http"
		}
		if seen[ep.Name] {
			warns = append(warns, fmt.Sprintf("endpoint %s 重复声明，保留第一个", ep.Name))
			continue
		}
		seen[ep.Name] = true
		if ep.Primary {
			if primary {
				warns = append(warns, fmt.Sprintf("endpoint %s 也标了 primary：primary 只能有一个，已降级为普通端点", ep.Name))
				ep.Primary = false
			} else {
				primary = true
			}
		}
		ep.Path = normPath(ep.Path)
		ep.PublishPath = normPublishPath(ep.PublishPath)
		// 口令参数名：本服务所有受保护入口都用 ?key=，声明里没写就按 key 处理，
		// 免得桌面拿不到 keyQuery、不拼口令，前端被 401 挡住。
		ep.KeyQuery = normKeyQuery(ep.KeyQuery)
		ep.HostPort = 0
		ep.PortSource = ""
		out = append(out, ep)
	}
	if len(out) == 0 {
		out = defaultEndpoints()
	}
	if !primary {
		// 没有任何 primary 时把 http 端点（或第一个）当首页入口，避免界面没有入口。
		for i := range out {
			if out[i].Protocol == "http" {
				out[i].Primary = true
				primary = true
				break
			}
		}
		if !primary {
			out[0].Primary = true
		}
	}
	return out, warns
}

// normPath 把 path 规范成 "/" 或 "/xxx/" 形式。
func normPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return p
}

// normPublishPath 规范推流路径：必须带前导 "/"。
func normPublishPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return p
}

// normKeyQuery 规范口令参数名：只保留 URL query 安全字符；空值回落 "key"。
func normKeyQuery(q string) string {
	q = strings.TrimSpace(q)
	if q == "" {
		return "key"
	}
	var b strings.Builder
	for _, r := range q {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '_' || r == '-' || r == '.' {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "key"
	}
	return b.String()
}

// envToken 把端点名转成环境变量片段：rtmp → RTMP，web-ui → WEB_UI。
func envToken(name string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(name) {
		switch {
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// envStr 读非空环境变量（去空白）。
func envStr(key string) string {
	return strings.TrimSpace(os.Getenv(key))
}

// endpointEnv 拼端点相关的环境变量名：JETDESK_ENDPOINT_<NAME>_<SUFFIX>。
func endpointEnv(name, suffix string) string {
	return "JETDESK_ENDPOINT_" + envToken(name) + "_" + suffix
}

// endpointBaseEnv 取桌面注入的端点基址（_BASE 优先，其次 _URL），并去掉 query：
// 里面可能带 ?key=<access_key>（规范允许 HTTP 端点这么带），但本服务以自己持久化的
// 口令为准，外来口串不参与鉴权，只当作基址用。
func endpointBaseEnv(name string) string {
	for _, suffix := range []string{"BASE", "URL"} {
		v := envStr(endpointEnv(name, suffix))
		if v == "" {
			continue
		}
		if u, err := url.Parse(v); err == nil && u.Host != "" {
			u.RawQuery = ""
			u.Fragment = ""
			return strings.TrimRight(u.String(), "/")
		}
	}
	return ""
}

// loopbackHost 判断是否回环/空主机（这种地址给局域网客户端没有意义）。
func loopbackHost(h string) bool {
	if h == "" {
		return true
	}
	if h == "::1" || h == "localhost" {
		return true
	}
	ip := net.ParseIP(strings.Trim(h, "[]"))
	return ip != nil && (ip.IsLoopback() || ip.IsUnspecified())
}

// envPort 读正整数环境变量（0 表示没设或非法）。
func envPort(key string) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 || n > 65535 {
		return 0
	}
	return n
}

// EndpointRegistry 保存「包内声明」与「桌面告知」两部分信息。
type EndpointRegistry struct {
	mu       sync.RWMutex
	declared []Endpoint        // 容器内声明（不变）
	assigned map[string]int    // name → 宿主端口（桌面推送）
	hosts    map[string]string // name → 对外主机（桌面推送，可选）
	pushedAt string            // 最近一次推送时间（排障用）
	warns    []string
	path     string // 宿主端口绑定的落盘路径（空表示不持久化）
}

// endpointBinding 是持久化的「桌面告知」部分。
// 为什么要落盘：容器重建后到桌面首次推送之间有几秒空窗，这段时间里
// /api/discover 必须仍然给出正确端口，否则手机一上来就推到错端口。
type endpointBinding struct {
	Assigned  map[string]int    `json:"assigned"`
	Hosts     map[string]string `json:"hosts"`
	UpdatedAt string            `json:"updatedAt,omitempty"`
}

// NewEndpointRegistry 建立注册表：默认表 ← JETDESK_ENDPOINTS 环境变量（可只覆盖部分字段）。
// persistPath 非空时，桌面推送过的宿主端口会落盘，重启后立即生效（随后被新推送覆盖）。
func NewEndpointRegistry(persistPath string) *EndpointRegistry {
	r := &EndpointRegistry{
		declared: defaultEndpoints(),
		assigned: map[string]int{},
		hosts:    map[string]string{},
		path:     persistPath,
	}
	r.loadBinding()
	if raw := strings.TrimSpace(os.Getenv("JETDESK_ENDPOINTS")); raw != "" {
		var in []Endpoint
		if err := json.Unmarshal([]byte(raw), &in); err != nil {
			r.warns = append(r.warns, "JETDESK_ENDPOINTS 不是合法 JSON，已忽略："+err.Error())
		} else {
			r.declared = mergeEndpoints(defaultEndpoints(), in)
			// D 通道：环境变量里直接带了宿主端口
			for _, ep := range in {
				if ep.HostPort > 0 {
					r.assigned[strings.ToLower(ep.Name)] = ep.HostPort
				}
			}
		}
	}
	return r
}

// mergeEndpoints 用 over 覆盖 base：同名端点覆盖声明字段，新端点追加。
func mergeEndpoints(base, over []Endpoint) []Endpoint {
	out := make([]Endpoint, len(base))
	copy(out, base)
	for _, o := range over {
		name := strings.ToLower(strings.TrimSpace(o.Name))
		if name == "" {
			continue
		}
		idx := -1
		for i := range out {
			if out[i].Name == name {
				idx = i
				break
			}
		}
		if idx < 0 {
			out = append(out, o)
			continue
		}
		m := out[idx]
		if o.Protocol != "" {
			m.Protocol = o.Protocol
		}
		if o.Service != "" {
			m.Service = o.Service
		}
		if o.ContainerPort > 0 {
			m.ContainerPort = o.ContainerPort
		}
		if o.Path != "" {
			m.Path = o.Path
		}
		if o.PublishPath != "" {
			m.PublishPath = o.PublishPath
		}
		if o.KeyQuery != "" {
			m.KeyQuery = o.KeyQuery
		}
		if o.Primary {
			m.Primary = true
		}
		out[idx] = m
	}
	return out
}

// Declared 返回当前声明表（规范后）。
func (r *EndpointRegistry) Declared() []Endpoint {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out, _ := normEndpoints(r.declared)
	return out
}

// Warnings 返回启动期发现的问题。
func (r *EndpointRegistry) Warnings() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]string(nil), r.warns...)
}

// SetAssigned 接收桌面推送的宿主端口（通道 A）。空 map 表示桌面没给。
// 只记录变化，避免每 2 秒重推时反复刷日志。
func (r *EndpointRegistry) SetAssigned(ports map[string]int, hosts map[string]string, at string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	changed := false
	for k, v := range ports {
		k = strings.ToLower(strings.TrimSpace(k))
		if k == "" || v <= 0 || v > 65535 {
			continue
		}
		if r.assigned[k] != v {
			r.assigned[k] = v
			changed = true
		}
	}
	for k, v := range hosts {
		k = strings.ToLower(strings.TrimSpace(k))
		v = strings.TrimSpace(v)
		if k == "" || v == "" {
			continue
		}
		if r.hosts[k] != v {
			r.hosts[k] = v
			changed = true
		}
	}
	if changed {
		r.pushedAt = at
		r.saveBindingLocked()
	}
	return changed
}

// loadBinding 读取上次落盘的宿主端口绑定（不存在或损坏都当作没有）。
func (r *EndpointRegistry) loadBinding() {
	if r.path == "" {
		return
	}
	b, err := os.ReadFile(r.path)
	if err != nil {
		return
	}
	var in endpointBinding
	if err := json.Unmarshal(b, &in); err != nil {
		r.warns = append(r.warns, "端点绑定文件解析失败，已忽略："+err.Error())
		return
	}
	for k, v := range in.Assigned {
		if v > 0 && v <= 65535 {
			r.assigned[strings.ToLower(k)] = v
		}
	}
	for k, v := range in.Hosts {
		if strings.TrimSpace(v) != "" {
			r.hosts[strings.ToLower(k)] = strings.TrimSpace(v)
		}
	}
	r.pushedAt = in.UpdatedAt
	if len(r.assigned) > 0 {
		r.warns = append(r.warns, "沿用上次落盘的宿主端口（等待桌面重新推送以确认）")
	}
}

// saveBindingLocked 落盘（调用方需持锁）。写失败只影响「重建后立刻可用」，不阻断业务。
func (r *EndpointRegistry) saveBindingLocked() {
	if r.path == "" {
		return
	}
	b, err := json.MarshalIndent(endpointBinding{Assigned: r.assigned, Hosts: r.hosts, UpdatedAt: r.pushedAt}, "", "  ")
	if err != nil {
		return
	}
	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0644); err != nil {
		return
	}
	_ = os.Rename(tmp, r.path)
}

// hostPortFor 按 A→G 的顺序解析某个端点的宿主端口，同时返回来源说明。
// reqHostPort 只在「主入口 http 端点」时作为参考（请求方实际访问的端口）。
func (r *EndpointRegistry) hostPortFor(ep Endpoint, reqHostPort int) (int, string) {
	r.mu.RLock()
	pushed := r.assigned[ep.Name]
	r.mu.RUnlock()

	if pushed > 0 {
		return pushed, "desktop-push"
	}
	// B：JETDESK_PORT_<containerPort>
	if p := envPort(fmt.Sprintf("JETDESK_PORT_%d", ep.ContainerPort)); p > 0 {
		return p, fmt.Sprintf("env JETDESK_PORT_%d", ep.ContainerPort)
	}
	// C：JETDESK_ENDPOINT_<NAME>_PORT
	if p := envPort("JETDESK_ENDPOINT_" + envToken(ep.Name) + "_PORT"); p > 0 {
		return p, "env JETDESK_ENDPOINT_" + envToken(ep.Name) + "_PORT"
	}
	// E：历史变量（只在显式设置时采信；compose 里的 ":-1935" 缺省值不会被当成事实）
	if ep.Protocol == "rtmp" {
		if p := envPort("SRS_RTMP_PORT"); p > 0 {
			return p, "env SRS_RTMP_PORT"
		}
	}
	if ep.Primary && ep.Protocol == "http" {
		if p := envPort("UI_PORT"); p > 0 {
			return p, "env UI_PORT"
		}
	}
	// F：请求方实际访问的端口（桌面打开界面时一定带上）
	if ep.Primary && ep.Protocol == "http" && reqHostPort > 0 {
		return reqHostPort, "request-host"
	}
	// G：容器内端口
	return ep.ContainerPort, "container-port"
}

// EndpointFact 是「只含端口事实」的端点形状，与 /jetdesk/config 推上来的请求一致。
// 桌面拿它逐项比对，不要 url/path（那是容器按自己 jdconfig 拼出来的，桌面不必依赖）。
type EndpointFact struct {
	Name          string `json:"name"`
	Service       string `json:"service,omitempty"`
	Protocol      string `json:"protocol"`
	ContainerPort int    `json:"containerPort"`
	HostPort      int    `json:"hostPort"`
	PortSource    string `json:"portSource,omitempty"`
}

// ResolveFacts 只返回端口事实。
func (r *EndpointRegistry) ResolveFacts(reqHost string) []EndpointFact {
	resolved := r.Resolve(reqHost)
	out := make([]EndpointFact, 0, len(resolved))
	for _, ep := range resolved {
		out = append(out, EndpointFact{
			Name:          ep.Name,
			Service:       ep.Service,
			Protocol:      ep.Protocol,
			ContainerPort: ep.ContainerPort,
			HostPort:      ep.HostPort,
			PortSource:    ep.PortSource,
		})
	}
	return out
}

// WithPublishKey 给 publishUrl 拼上推流口令。
//
// 为什么只给 publishUrl 拼：`/api/discover` 本来就公开 publish_key，
// 客户端拿 publishUrl 可以直接推流；而 access_key **绝不能**出现在 RTMP 地址里
// （推流地址会被贴到 OBS/手机里，泄露面比控制台大得多）。
func WithPublishKey(eps []ResolvedEndpoint, publishKey string) []ResolvedEndpoint {
	if publishKey == "" {
		return eps
	}
	out := make([]ResolvedEndpoint, len(eps))
	copy(out, eps)
	for i := range out {
		if out[i].PublishURL == "" {
			continue
		}
		name := out[i].KeyQuery
		if name == "" {
			name = "key"
		}
		out[i].PublishURL = appendQueryParam(out[i].PublishURL, name, publishKey)
	}
	return out
}

// appendQueryParam 在 URL 上追加一个 query 参数（已存在则不重复加）。
func appendQueryParam(u, name, value string) string {
	if u == "" || value == "" || name == "" {
		return u
	}
	if strings.Contains(u, name+"=") {
		return u
	}
	sep := "?"
	if strings.Contains(u, "?") {
		sep = "&"
	}
	return u + sep + name + "=" + url.QueryEscape(value)
}

// Resolve 合成对外端点表。reqHost 是请求方看到的 Host（可含端口），
// 用它作为对外主机名——局域网里的手机因此能拿到电脑的 IP，而不是容器的 127.0.0.1。
func (r *EndpointRegistry) Resolve(reqHost string) []ResolvedEndpoint {
	reqIP, reqPort := splitHostPortLoose(reqHost)
	declared := r.Declared()

	r.mu.RLock()
	hosts := make(map[string]string, len(r.hosts))
	for k, v := range r.hosts {
		hosts[k] = v
	}
	r.mu.RUnlock()

	out := make([]ResolvedEndpoint, 0, len(declared))
	lanIP := envStr("JETDESK_LAN_IP")

	for _, ep := range declared {
		hostPort, src := r.hostPortFor(ep, reqPort)
		// 对外主机优先级：桌面推送的 host → JETDESK_ENDPOINT_<NAME>_HOST → 请求方 Host → JETDESK_LAN_IP
		host := hosts[ep.Name]
		if host == "" {
			host = envStr(endpointEnv(ep.Name, "HOST"))
		}
		if host == "" {
			host = reqIP
		}
		// 回环地址给局域网客户端没用：桌面若注入了局域网 IP，就用它顶替
		if loopbackHost(host) && lanIP != "" {
			host = lanIP
		}
		ep.HostPort = hostPort
		ep.PortSource = src
		ep.Host = host

		re := ResolvedEndpoint{Endpoint: ep}
		re.URL = buildURL(ep, host, ep.Path)
		if ep.PublishPath != "" {
			re.PublishURL = buildURL(ep, host, ep.PublishPath)
		}
		// 桌面显式注入了 _BASE/_URL 就以它为准（端口/协议/域名全听桌面的）
		if base := endpointBaseEnv(ep.Name); base != "" {
			p := ep.Path
			if p == "" {
				p = "/"
			}
			re.URL = base + p
			if ep.PublishPath != "" {
				re.PublishURL = base + ep.PublishPath
			}
		}
		out = append(out, re)
	}
	return out
}

// ByName 取某个端点的解析结果。
func (r *EndpointRegistry) ByName(name, reqHost string) (ResolvedEndpoint, bool) {
	name = strings.ToLower(strings.TrimSpace(name))
	for _, ep := range r.Resolve(reqHost) {
		if ep.Name == name {
			return ep, true
		}
	}
	return ResolvedEndpoint{}, false
}

// Primary 返回主入口端点。
func (r *EndpointRegistry) Primary(reqHost string) (ResolvedEndpoint, bool) {
	for _, ep := range r.Resolve(reqHost) {
		if ep.Primary {
			return ep, true
		}
	}
	return ResolvedEndpoint{}, false
}

// buildURL 拼对外地址；协议缺省端口不写进 URL，保持地址干净。
// host 为空时用 localhost（仅本地无请求上下文时出现，绝不当成对外地址写死）。
func buildURL(ep Endpoint, host, path string) string {
	scheme := strings.ToLower(ep.Protocol)
	if scheme == "" {
		scheme = "http"
	}
	if host == "" {
		host = "localhost"
	}
	hostOnly := host
	// 端口是权威值，统一由 hostPort 决定，避免 host 里带端口造成重复
	if h, _, err := net.SplitHostPort(host); err == nil {
		hostOnly = h
	}
	authority := hostOnly
	// 推流协议一律显式带端口（rtmp 的默认 1935 常被宿主映射成别的号，省掉端口会误导用户）
	alwaysPort := scheme == "rtmp" || scheme == "rtmps"
	if ep.HostPort > 0 && (alwaysPort || ep.HostPort != defaultPortOf(scheme)) {
		authority = net.JoinHostPort(hostOnly, strconv.Itoa(ep.HostPort))
	} else if strings.Contains(hostOnly, ":") && !strings.HasPrefix(hostOnly, "[") {
		// IPv6 字面量需要加方括号
		authority = "[" + hostOnly + "]"
	}
	if path == "" {
		path = "/"
	}
	return scheme + "://" + authority + path
}

// splitHostPortLoose 容错解析 Host 头：可能是 "192.168.1.10:1936"、"192.168.1.10"、"host"。
func splitHostPortLoose(host string) (string, int) {
	host = strings.TrimSpace(host)
	if host == "" {
		return "", 0
	}
	if h, p, err := net.SplitHostPort(host); err == nil {
		n, _ := strconv.Atoi(p)
		return strings.Trim(h, "[]"), n
	}
	// 没有端口（或 IPv6 字面量）
	return strings.Trim(host, "[]"), 0
}
