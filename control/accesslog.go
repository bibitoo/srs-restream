package main

import (
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// accessEntry 是一条访问记录，用于排查"播放只能播一点""接口 404"这类客户端问题。
type accessEntry struct {
	Time         string `json:"time"`
	Method       string `json:"method"`
	Path         string `json:"path"`
	Status       int    `json:"status"`
	Sent         int64  `json:"sent"`  // 实际写给客户端的字节数
	Range        string `json:"range"` // 请求的 Range
	ContentRange string `json:"content_range,omitempty"`
	UA           string `json:"ua"` // 客户端标识（能区分微信/手机/桌面浏览器）
	Referer      string `json:"referer,omitempty"`
	DurMs        int64  `json:"dur_ms"`  // 处理耗时
	Aborted      bool   `json:"aborted"` // 是否中途断开（客户端主动取消/网络断）
}

// accessLog 是内存环形访问日志 + 文件落盘。
type accessLog struct {
	mu    sync.Mutex
	max   int
	items []accessEntry
	file  *ringLog
}

func newAccessLog(max int, file *ringLog) *accessLog {
	return &accessLog{max: max, items: make([]accessEntry, 0, max), file: file}
}

func (a *accessLog) add(e accessEntry) {
	a.mu.Lock()
	if len(a.items) >= a.max {
		a.items = a.items[len(a.items)-a.max+1:]
	}
	a.items = append(a.items, e)
	a.mu.Unlock()
	if a.file != nil {
		a.file.Printf("%s %s %s %d sent=%d range=%q ua=%q dur=%dms aborted=%v",
			e.Time, e.Method, e.Path, e.Status, e.Sent, e.Range, e.UA, e.DurMs, e.Aborted)
	}
}

// Recent 返回最近 n 条（新的在前）。
func (a *accessLog) Recent(n int) []accessEntry {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]accessEntry, 0, n)
	for i := len(a.items) - 1; i >= 0 && len(out) < n; i-- {
		out = append(out, a.items[i])
	}
	return out
}

// countWriter 统计状态码与写出字节，并判断是否中途断开。
type countWriter struct {
	http.ResponseWriter
	status  int
	sent    int64
	aborted bool
}

func (w *countWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *countWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.sent += int64(n)
	if err != nil {
		w.aborted = true
	}
	return n, err
}

// Flush 保证直播流反代不受影响。
func (w *countWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// ReadFrom 让 ServeFile 仍然可以走 sendfile 优化，同时统计实际发送字节。
func (w *countWriter) ReadFrom(r io.Reader) (int64, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if rf, ok := w.ResponseWriter.(io.ReaderFrom); ok {
		n, err := rf.ReadFrom(r)
		w.sent += n
		if err != nil {
			w.aborted = true
		}
		return n, err
	}
	n, err := io.Copy(struct{ io.Writer }{w}, r)
	return n, err
}

// wrapWithAccessLog 只记录关心的路径，避免噪音。
func (a *API) wrapWithAccessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		interesting := strings.HasPrefix(p, "/media/") ||
			strings.HasSuffix(p, ".flv") || strings.HasSuffix(p, ".m3u8") ||
			strings.HasPrefix(p, "/players/") ||
			strings.HasPrefix(p, "/api/recordings") || strings.HasPrefix(p, "/api/streams")
		if !interesting {
			next.ServeHTTP(w, r)
			return
		}
		cw := &countWriter{ResponseWriter: w}
		start := time.Now()
		next.ServeHTTP(cw, r)
		ua := r.Header.Get("User-Agent")
		if len(ua) > 120 {
			ua = ua[:120]
		}
		a.access.add(accessEntry{
			Time:         time.Now().Format("2006-01-02 15:04:05"),
			Method:       r.Method,
			Path:         p,
			Status:       cw.status,
			Sent:         cw.sent,
			Range:        r.Header.Get("Range"),
			ContentRange: cw.Header().Get("Content-Range"),
			UA:           ua,
			Referer:      r.Header.Get("Referer"),
			DurMs:        time.Since(start).Milliseconds(),
			Aborted:      cw.aborted,
		})
	})
}
