package main

import (
	"context"
	"embed"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

//go:embed web
var webFS embed.FS

func main() {
	dataDir := envOr("DATA_DIR", "/data")
	mediaDir := filepath.Join(dataDir, "recordings")
	logsDir := filepath.Join(dataDir, "logs")
	assetsDir := filepath.Join(dataDir, "assets")
	for _, d := range []string{dataDir, mediaDir, logsDir, assetsDir} {
		if err := ensureDir(d); err != nil {
			log.Fatalf("创建目录 %s 失败: %v", d, err)
		}
	}

	ffmpegBin := envOr("FFMPEG_BIN", "ffmpeg")
	ffprobeBin := envOr("FFPROBE_BIN", "ffprobe")
	maskPath := filepath.Join(assetsDir, "circle_mask.png")
	if err := ensureCircleMask(maskPath); err != nil {
		log.Printf("生成圆形遮罩失败: %v", err)
	}

	store, err := NewStore(filepath.Join(dataDir, "config.json"))
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}
	st := store.Settings()
	srsClient := NewSRSClient(st.SRSAPI)
	env := ffEnv{
		FFmpeg:   ffmpegBin,
		MaskPath: maskPath,
		RTMPBase: st.RTMPBase,
	}
	runner := NewRunner(store, srsClient, env, logsDir)
	rec := NewRecorder(mediaDir, ffmpegBin, ffprobeBin, st.RTMPBase)
	rec.SetSegmentMinutes(st.RecordSegmentMinutes)

	var accessFile *ringLog
	if f, err := os.OpenFile(filepath.Join(logsDir, "access.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644); err == nil {
		accessFile = newRingLog(1)
		accessFile.file = f
	}
	api := &API{
		store:     store,
		srs:       srsClient,
		runner:    runner,
		rec:       rec,
		mediaDir:  mediaDir,
		token:     os.Getenv("AUTH_TOKEN"),
		startedAt: time.Now(),
		access:    newAccessLog(300, accessFile),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/", api.auth(api.router))
	mux.HandleFunc("/media/", api.serveMedia)
	mux.HandleFunc("/hooks/", api.hookRouter)
	// 捷兔桌面推配置（协议 B 面）：接口内部用访问密钥校验，可覆盖密钥
	mux.HandleFunc("/jetdesk/config", api.handleJetDeskConfig)
	// 录像的 HLS 切片（手机/微信兼容播放）
	hlsDir := filepath.Join(dataDir, "hls")
	_ = ensureDir(hlsDir)
	mux.Handle("/hls/", noCache(http.StripPrefix("/hls/", http.FileServer(http.Dir(hlsDir)))))

	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		log.Fatalf("内置界面资源异常: %v", err)
	}
	// 播放相关路径（播放器页、HTTP-FLV、HLS 分片）反代给 SRS，
	// 其余路径是控制台自身的静态资源。
	proxy := newSRSProxy(api)
	staticFS := http.FileServer(http.FS(sub))
	mux.Handle("/", noCache(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if shouldProxyToSRS(r.URL.Path) {
			proxy.ServeHTTP(w, r)
			return
		}
		staticFS.ServeHTTP(w, r)
	})))

	addr := envOr("LISTEN", ":1986")
	srv := &http.Server{
		Addr:              addr,
		Handler:           api.wrapWithAccessLog(logRequests(mux)),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Printf("SRS 转推控制台已启动: http://0.0.0.0%s  (SRS API: %s, 播放服务: %s)",
			addr, st.SRSAPI, st.SRSHTTP)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTP 服务异常: %v", err)
		}
	}()

	// 修复上次异常退出留下的"未收尾"录像（分片 MP4 转封装成可拖动播放的标准 MP4）。
	go func() {
		time.Sleep(3 * time.Second)
		rec.RecoverOrphans()
	}()

	// 自动拉起开启了自动启动的任务（源流不在线时会等待）。
	go func() {
		time.Sleep(2 * time.Second)
		runner.AutoStartAll()
	}()

	// 兜底巡检：hook 丢失（SRS 重启、网络抖动）时也能让录制正常收尾
	go func() {
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		for range t.C {
			st, err := srsClient.StreamMap(store.Settings().PublicBase)
			if err != nil {
				continue
			}
			online := make(map[string]bool, len(st))
			for k := range st {
				online[k] = true
			}
			rec.Reconcile(online)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Printf("收到退出信号，正在停止任务与录制…")
	runner.StopAll()
	rec.StopAll()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
	log.Printf("已退出")
}

func noCache(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/hooks/") {
			start := time.Now()
			next.ServeHTTP(w, r)
			if d := time.Since(start); d > 2*time.Second {
				log.Printf("慢请求 %s %s %s", r.Method, r.URL.Path, d)
			}
			return
		}
		next.ServeHTTP(w, r)
	})
}

// router 处理 /api/ 下的所有请求。
func (a *API) router(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/")
	segs := strings.SplitN(path, "/", 2)
	head := segs[0]
	rest := ""
	if len(segs) > 1 {
		rest = segs[1]
	}
	switch head {
	case "health":
		writeJSON(w, 200, map[string]interface{}{"ok": true, "time": nowStr()})
	case "overview":
		a.handleOverview(w, r)
	case "streams":
		a.handleStreams(w, r)
	case "layouts":
		a.handleLayouts(w, r)
	case "diag":
		a.handleDiag(w, r)
	case "discover":
		a.handleDiscover(w, r)
	case "targets":
		if rest == "" {
			a.handleTargets(w, r)
		} else {
			a.handleTargetByID(w, r, rest)
		}
	case "jobs":
		if rest == "" {
			a.handleJobs(w, r)
		} else {
			a.handleJobByID(w, r, rest)
		}
	case "recordings":
		if rest == "" {
			a.handleRecordings(w, r)
		} else {
			a.handleRecordingByID(w, r, rest)
		}
	case "settings":
		a.handleSettings(w, r)
	default:
		writeErr(w, 404, errNotFound)
	}
}

func (a *API) hookRouter(w http.ResponseWriter, r *http.Request) {
	action := strings.TrimPrefix(r.URL.Path, "/hooks/")
	switch action {
	case "on_publish", "on_unpublish":
		a.handleHook(w, r, action)
	default:
		writeJSON(w, 200, map[string]int{"code": 0})
	}
}

// ensureCircleMask 生成圆形遮罩（白圆黑底，供 alphamerge 使用）。
func ensureCircleMask(path string) error {
	if fileExists(path) {
		return nil
	}
	const size = 1024
	img := image.NewGray(image.Rect(0, 0, size, size))
	cx, cy := float64(size)/2, float64(size)/2
	r := float64(size)/2 - 2
	const ss = 4 // 4x4 超采样，边缘抗锯齿
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			var inside int
			for sy := 0; sy < ss; sy++ {
				for sx := 0; sx < ss; sx++ {
					px := float64(x) + (float64(sx)+0.5)/ss
					py := float64(y) + (float64(sy)+0.5)/ss
					dx, dy := px-cx, py-cy
					if dx*dx+dy*dy <= r*r {
						inside++
					}
				}
			}
			v := uint8(inside * 255 / (ss * ss))
			img.SetGray(x, y, color.Gray{Y: v})
		}
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		return fmt.Errorf("写遮罩失败: %w", err)
	}
	return nil
}
