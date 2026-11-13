package main

import (
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
	sp.proxy.ServeHTTP(w, r)
}
