package main

import (
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
)

// srsProxy 把 SRS 的 HTTP 服务（HTTP-FLV / HLS / 内置播放器页）反向代理到控制面端口，
// 这样浏览器只需要访问控制台所在的 1986 端口即可预览直播与混流，不必依赖 8080 的对外映射。
type srsProxy struct {
	api    *API
	proxy  *httputil.ReverseProxy
	target *url.URL
}

// streamSuffixes 是需要代理给 SRS 的播放资源后缀。
var streamSuffixes = []string{".flv", ".m3u8", ".ts", ".aac", ".mp4", ".m4s"}

// shouldProxyToSRS 判断某个请求是否该转发给 SRS。
func shouldProxyToSRS(path string) bool {
	for _, p := range []string{"/players/", "/console/", "/demos/"} {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	lower := strings.ToLower(path)
	for _, suf := range streamSuffixes {
		if strings.HasSuffix(lower, suf) {
			return true
		}
	}
	// SRS 的 HLS/FLV 也可能挂在 app 目录下（不带后缀的 .m3u8 请求少见，忽略）
	return false
}

func newSRSProxy(a *API) *srsProxy {
	sp := &srsProxy{api: a}
	tr := &http.Transport{
		Proxy: nil,
		DialContext: (&net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       60 * time.Second,
		DisableCompression:    true, // 直播流不能压缩
		ExpectContinueTimeout: 1 * time.Second,
	}
	sp.proxy = &httputil.ReverseProxy{
		// -1 表示每次写入立即 flush，保证 FLV/TS 低延迟
		FlushInterval: -1,
		Transport:     tr,
		Director: func(r *http.Request) {
			base := a.store.Settings().SRSHTTP
			if u, err := url.Parse(base); err == nil && u.Host != "" {
				r.URL.Scheme = u.Scheme
				r.URL.Host = u.Host
				r.Host = u.Host
			} else {
				r.URL.Scheme = "http"
				r.URL.Host = "127.0.0.1:8080"
				r.Host = "127.0.0.1:8080"
			}
			// 透传原始访问来源，便于排查
			r.Header.Set("X-Forwarded-For", r.RemoteAddr)
			r.Header.Set("X-Forwarded-Proto", "http")
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			http.Error(w, "无法连接 SRS 播放服务（HTTP-FLV/HLS）: "+err.Error()+
				"\n请检查容器内 SRS 是否正常，或设置项 srs_http 是否正确。", http.StatusBadGateway)
		},
	}
	return sp
}

func (sp *srsProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// 播放链路（播放器页、FLV、HLS 分片）同样受访问密钥保护。
	// 浏览器里 <video> / iframe 无法加自定义头，所以用 Cookie jsk 或 ?key= 携带。
	if !sp.api.keyOK(r) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="jetstream"`)
		http.Error(w, "访问密钥无效：请先登录控制台，或用 ?key= 访问", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Access-Control-Allow-Origin", "*")

	// HLS 播放列表要重写：SRS 生成的分片 URI 不带 key，直接透传会被鉴权拦掉。
	if strings.HasSuffix(strings.ToLower(r.URL.Path), ".m3u8") {
		sp.serveM3U8(w, r)
		return
	}
	sp.proxy.ServeHTTP(w, r)
}

// m3u8Rewriter 把 HLS 播放列表里的 URI 逐行加上访问密钥。
type m3u8Rewriter struct {
	w   http.ResponseWriter
	key string
	buf []byte
}

func (m *m3u8Rewriter) Header() http.Header { return m.w.Header() }

func (m *m3u8Rewriter) WriteHeader(code int) { m.w.WriteHeader(code) }

func (m *m3u8Rewriter) Flush() {
	if f, ok := m.w.(http.Flusher); ok {
		f.Flush()
	}
}

func (m *m3u8Rewriter) Write(b []byte) (int, error) {
	m.buf = append(m.buf, b...)
	if _, err := m.w.Write(m.rewrite()); err != nil {
		return 0, err
	}
	return len(b), nil
}

// rewrite 给每个非注释行（以及 URI="..." 属性）追加 &key=xxx。
func (m *m3u8Rewriter) rewrite() []byte {
	if m.key == "" {
		return m.buf
	}
	lines := strings.Split(string(m.buf), "\n")
	for i, ln := range lines {
		trimmed := strings.TrimRight(ln, "\r")
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "#") {
			// #EXT-X-KEY:METHOD=NONE 之类没有 URI；带 URI="..." 的（如加密 key）也补上
			if idx := strings.Index(trimmed, `URI="`); idx >= 0 {
				rest := trimmed[idx+5:]
				if end := strings.Index(rest, `"`); end > 0 {
					uri := rest[:end]
					lines[i] = trimmed[:idx+5] + withKeyParam(uri, m.key) + rest[end:]
				}
			}
			continue
		}
		lines[i] = withKeyParam(trimmed, m.key)
	}
	return []byte(strings.Join(lines, "\n"))
}

// withKeyParam 在 URI 上追加 key 参数（已存在则不重复加）。
func withKeyParam(uri, key string) string {
	if uri == "" || strings.Contains(uri, "key=") {
		return uri
	}
	sep := "?"
	if strings.Contains(uri, "?") {
		sep = "&"
	}
	return uri + sep + "key=" + url.QueryEscape(key)
}

// serveM3U8 反向取播放列表并重写后再返回给客户端。
func (sp *srsProxy) serveM3U8(w http.ResponseWriter, r *http.Request) {
	base := sp.api.store.Settings().SRSHTTP
	target := strings.TrimRight(base, "/") + r.URL.RequestURI()
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, target, nil)
	if err != nil {
		http.Error(w, "构造请求失败: "+err.Error(), http.StatusBadGateway)
		return
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		http.Error(w, "无法连接 SRS 播放服务: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		http.Error(w, "读取播放列表失败: "+err.Error(), http.StatusBadGateway)
		return
	}
	key := accessKeyOf(r)
	if key == "" {
		key = sp.api.store.AccessKey()
	}
	out := (&m3u8Rewriter{w: w, key: key})
	out.buf = body

	for k, vv := range resp.Header {
		if strings.EqualFold(k, "Content-Length") || strings.EqualFold(k, "Content-Encoding") {
			continue
		}
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(out.rewrite())
}
