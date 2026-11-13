package main

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ----------------------------------------------------------------------------
// 环形日志
// ----------------------------------------------------------------------------

type ringLog struct {
	mu      sync.Mutex
	max     int
	lines   []string
	partial string
	file    *os.File
}

func newRingLog(max int) *ringLog { return &ringLog{max: max} }

func (l *ringLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file != nil {
		_, _ = l.file.Write(p)
	}
	l.partial += string(p)
	for {
		i := strings.IndexByte(l.partial, '\n')
		if i < 0 {
			break
		}
		line := strings.TrimRight(l.partial[:i], "\r")
		l.partial = l.partial[i+1:]
		if len(l.lines) >= l.max {
			l.lines = l.lines[len(l.lines)-l.max+1:]
		}
		l.lines = append(l.lines, line)
	}
	return len(p), nil
}

func (l *ringLog) Printf(format string, args ...interface{}) {
	_, _ = l.Write([]byte(fmt.Sprintf(format, args...) + "\n"))
}

func (l *ringLog) Lines(n int) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, len(l.lines))
	copy(out, l.lines)
	if n > 0 && len(out) > n {
		out = out[len(out)-n:]
	}
	return out
}

func (l *ringLog) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file != nil {
		_ = l.file.Close()
		l.file = nil
	}
}

// prefixWriter 给子进程输出加前缀，便于在同一个日志里区分编码器与各目标。
type prefixWriter struct {
	w      io.Writer
	prefix string
	buf    []byte
	tail   []string // 最后几行，用于把真实错误回显到界面
	mu     sync.Mutex
}

// LastLine 返回最后一条非空输出（通常是 ffmpeg 的报错）。
func (p *prefixWriter) LastLine() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := len(p.tail) - 1; i >= 0; i-- {
		if p.tail[i] != "" {
			return p.tail[i]
		}
	}
	return ""
}

func (p *prefixWriter) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.buf = append(p.buf, b...)
	for {
		i := strings.IndexByte(string(p.buf), '\n')
		if i < 0 {
			break
		}
		line := strings.TrimRight(string(p.buf[:i]), "\r")
		p.buf = p.buf[i+1:]
		if line != "" {
			_, _ = p.w.Write([]byte(p.prefix + line + "\n"))
			p.tail = append(p.tail, line)
			if len(p.tail) > 5 {
				p.tail = p.tail[len(p.tail)-5:]
			}
		}
	}
	return len(b), nil
}

// ----------------------------------------------------------------------------
// 任务状态
// ----------------------------------------------------------------------------

const (
	StatusStopped  = "stopped"
	StatusStarting = "starting"
	StatusRunning  = "running"
	StatusWaiting  = "waiting"
	StatusRetrying = "retrying"
	StatusError    = "error"
)

// TargetRuntime 描述单个推流目标的转发状态。
type TargetRuntime struct {
	URL      string `json:"url"`
	Status   string `json:"status"`
	PID      int    `json:"pid"`
	Restarts int    `json:"restarts"`
	Detail   string `json:"detail,omitempty"`
}

type relayState struct {
	url      string
	status   string
	pid      int
	restarts int
	detail   string
}

type jobRT struct {
	mu        sync.Mutex
	status    string
	detail    string
	pid       int // 混流编码器 pid
	startedAt time.Time
	active    bool
	stopped   bool
	sources   []string
	encoder   string // 混流输出地址
	relays    map[string]*relayState
	log       *ringLog
	cancel    context.CancelFunc
}

func (rt *jobRT) set(status, detail string) {
	rt.mu.Lock()
	rt.status, rt.detail = status, detail
	rt.mu.Unlock()
}

// JobRuntime 是暴露给 API 的运行时状态。
type JobRuntime struct {
	JobID     string          `json:"job_id"`
	Status    string          `json:"status"`
	Detail    string          `json:"detail,omitempty"`
	PID       int             `json:"pid"`
	StartedAt string          `json:"started_at,omitempty"`
	Sources   []string        `json:"sources"`
	Encoder   string          `json:"encoder_url,omitempty"`
	Targets   []TargetRuntime `json:"targets"`
}

func (rt *jobRT) snapshot(jobID string) JobRuntime {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	r := JobRuntime{
		JobID:   jobID,
		Status:  rt.status,
		Detail:  rt.detail,
		PID:     rt.pid,
		Sources: append([]string{}, rt.sources...),
		Encoder: rt.encoder,
	}
	if !rt.startedAt.IsZero() {
		r.StartedAt = rt.startedAt.Format(time.RFC3339)
	}
	for _, rs := range rt.relays {
		r.Targets = append(r.Targets, TargetRuntime{
			URL: rs.url, Status: rs.status, PID: rs.pid,
			Restarts: rs.restarts, Detail: rs.detail,
		})
	}
	sortTargets(r.Targets)
	return r
}

func sortTargets(ts []TargetRuntime) {
	for i := 1; i < len(ts); i++ {
		for j := i; j > 0 && ts[j].URL < ts[j-1].URL; j-- {
			ts[j], ts[j-1] = ts[j-1], ts[j]
		}
	}
}

// ----------------------------------------------------------------------------
// Runner
// ----------------------------------------------------------------------------

type Runner struct {
	store *Store
	srs   *SRSClient
	env   ffEnv
	logs  string

	mu  sync.Mutex
	rts map[string]*jobRT
}

func NewRunner(store *Store, srs *SRSClient, env ffEnv, logsDir string) *Runner {
	return &Runner{store: store, srs: srs, env: env, logs: logsDir, rts: map[string]*jobRT{}}
}

func (r *Runner) rt(jobID string) *jobRT {
	r.mu.Lock()
	defer r.mu.Unlock()
	rt, ok := r.rts[jobID]
	if !ok {
		rt = &jobRT{status: StatusStopped, relays: map[string]*relayState{}, log: newRingLog(400)}
		r.rts[jobID] = rt
	}
	return rt
}

func (r *Runner) getRT(jobID string) (*jobRT, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rt, ok := r.rts[jobID]
	return rt, ok
}

func (r *Runner) isStopped(rt *jobRT) bool {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.stopped
}

// Start 启动任务（幂等）。
func (r *Runner) Start(jobID string) (bool, error) {
	job, ok := r.store.JobByID(jobID)
	if !ok {
		return false, errNotFound
	}
	rt := r.rt(jobID)
	rt.mu.Lock()
	if rt.active {
		rt.mu.Unlock()
		return false, nil
	}
	rt.active = true
	rt.stopped = false
	rt.status = StatusStarting
	rt.detail = ""
	rt.relays = map[string]*relayState{}
	rt.mu.Unlock()

	logPath := fmt.Sprintf("%s/job-%s.log", r.logs, jobID)
	if f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644); err == nil {
		rt.log.mu.Lock()
		if rt.log.file != nil {
			_ = rt.log.file.Close()
		}
		rt.log.file = f
		rt.log.mu.Unlock()
	}
	rt.log.Printf("=== %s 启动任务「%s」(%s) 模式=%s ===",
		time.Now().Format("2006-01-02 15:04:05"), job.Name, job.ID, job.Mode)

	go r.loop(job)
	return true, nil
}

func (r *Runner) sleepOrStop(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return true
	case <-time.After(d):
		return false
	}
}

// loop 是任务主循环：等源流 → 混流编码（如需）→ 逐个目标中继 → 异常重试。
func (r *Runner) loop(job Job) {
	rt := r.rt(job.ID)
	ctx, cancel := context.WithCancel(context.Background())
	rt.mu.Lock()
	rt.cancel = cancel
	rt.mu.Unlock()
	defer func() {
		cancel()
		rt.mu.Lock()
		rt.active = false
		rt.pid = 0
		rt.status = StatusStopped
		for _, rs := range rt.relays {
			rs.status = StatusStopped
			rs.pid = 0
		}
		rt.mu.Unlock()
	}()

	for {
		if r.isStopped(rt) {
			return
		}
		job, ok := r.store.JobByID(job.ID)
		if !ok || !job.Enabled {
			rt.set(StatusStopped, "任务未启用")
			return
		}

		// ---- 1. 源流检查 ----
		srcs, missing, err := r.resolveSources(job)
		if err != nil {
			rt.set(StatusError, "无法访问 SRS API: "+err.Error())
			if r.sleepOrStop(ctx, 5*time.Second) {
				return
			}
			continue
		}
		if len(srcs) == 0 {
			rt.set(StatusWaiting, "等待源流上线: "+strings.Join(missing, ", "))
			if r.sleepOrStop(ctx, 2*time.Second) {
				return
			}
			continue
		}

		// ---- 2. 目标地址 ----
		urls := r.store.ResolveURLs(job)
		if len(urls) == 0 {
			rt.set(StatusError, "未配置任何推流目标")
			if r.sleepOrStop(ctx, 5*time.Second) {
				return
			}
			continue
		}
		rt.mu.Lock()
		rt.sources = keysOf(srcs)
		rt.relays = map[string]*relayState{}
		for _, u := range urls {
			rt.relays[u] = &relayState{url: u, status: StatusStarting}
		}
		rt.mu.Unlock()

		// ---- 3. 混流：先编码一次并写回本地 SRS（mix/<id>）----
		var relaySrc string
		var encCmd *exec.Cmd
		if job.Mode == "mix" || len(srcs) > 1 {
			localURL := r.localMixURL(job.ID)
			args, err := BuildMixArgs(r.env, job, srcs, []string{localURL})
			if err != nil {
				rt.set(StatusError, err.Error())
				if r.sleepOrStop(ctx, 5*time.Second) {
					return
				}
				continue
			}
			rt.log.Printf("[%s] 混流编码: %s", time.Now().Format("15:04:05"), DescribeArgs(r.env.FFmpeg, args))
			cmd := exec.Command(r.env.FFmpeg, args...)
			cmd.Stdout = &prefixWriter{w: rt.log, prefix: "[enc] "}
			cmd.Stderr = &prefixWriter{w: rt.log, prefix: "[enc] "}
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			if err := cmd.Start(); err != nil {
				rt.set(StatusError, "启动混流编码失败: "+err.Error())
				if r.sleepOrStop(ctx, 5*time.Second) {
					return
				}
				continue
			}
			encCmd = cmd
			rt.mu.Lock()
			rt.pid = cmd.Process.Pid
			rt.encoder = localURL
			rt.startedAt = time.Now()
			rt.status = StatusRunning
			rt.detail = "混流编码中"
			if len(missing) > 0 {
				rt.detail = "部分源流缺失: " + strings.Join(missing, ", ")
			}
			rt.mu.Unlock()

			// 等本地混流上线后再开始转发
			if !r.waitStream(localURLKey(job.ID), 15*time.Second, ctx) {
				rt.log.Printf("[%s] 混流输出未按时上线，仍然继续", time.Now().Format("15:04:05"))
			}
			relaySrc = localURL
		} else {
			relaySrc = r.env.streamURL(srcs[0].Key)
			rt.mu.Lock()
			rt.encoder = ""
			rt.startedAt = time.Now()
			rt.status = StatusRunning
			rt.detail = "直转中"
			if len(missing) > 0 {
				rt.detail = "部分源流缺失: " + strings.Join(missing, ", ")
			}
			rt.mu.Unlock()
		}

		// ---- 4. 每个目标一个中继进程（-c copy，不重复转码）----
		var wg sync.WaitGroup
		relayCtx, relayCancel := context.WithCancel(ctx)
		for _, u := range urls {
			wg.Add(1)
			go func(u string) {
				defer wg.Done()
				r.relay(relayCtx, rt, job, relaySrc, u)
			}(u)
		}

		if encCmd != nil {
			_ = encCmd.Wait()
			relayCancel()
			wg.Wait()
			if r.isStopped(rt) {
				return
			}
			rt.log.Printf("[%s] 混流编码退出，3 秒后重建", time.Now().Format("15:04:05"))
			rt.mu.Lock()
			rt.status = StatusRetrying
			rt.detail = "混流编码退出，准备重连"
			rt.pid = 0
			rt.mu.Unlock()
			if r.sleepOrStop(ctx, 3*time.Second) {
				return
			}
			continue
		}

		// 直转模式：中继各自重试，这里等源流掉线或任务停止。
		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
	waitLoop:
		for {
			select {
			case <-ctx.Done():
				relayCancel()
				<-done
				return
			case <-done:
				relayCancel()
				if r.isStopped(rt) {
					return
				}
				rt.mu.Lock()
				rt.status = StatusRetrying
				rt.detail = "所有目标已断开，准备重连"
				rt.mu.Unlock()
				break waitLoop
			case <-time.After(3 * time.Second):
				st, err := r.srs.StreamMap(r.store.Settings().PublicBase)
				if err == nil {
					if _, ok := st[srcs[0].Key]; !ok {
						relayCancel()
						<-done
						rt.log.Printf("[%s] 源流 %s 已离线，任务转入等待", time.Now().Format("15:04:05"), srcs[0].Key)
						break waitLoop
					}
				}
			}
		}
		if r.sleepOrStop(ctx, 2*time.Second) {
			return
		}
	}
}

// relay 把 relaySrc 复制推送到一个第三方目标，失败自动重试。
func (r *Runner) relay(ctx context.Context, rt *jobRT, job Job, relaySrc, url string) {
	prefix := "[relay " + shortURL(url) + "] "
	backoff := 2
	for {
		select {
		case <-ctx.Done():
			setRelay(rt, url, StatusStopped, 0, "")
			return
		default:
		}
		args := BuildRelayArgs(relaySrc, url)
		cmd := exec.Command(r.env.FFmpeg, args...)
		pw := &prefixWriter{w: rt.log, prefix: prefix}
		cmd.Stdout = pw
		cmd.Stderr = pw
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := cmd.Start(); err != nil {
			setRelay(rt, url, StatusError, 0, err.Error())
			rt.log.Printf("%s启动失败: %v", prefix, err)
			if waitCtx(ctx, time.Duration(backoff)*time.Second) {
				return
			}
			backoff = nextBackoff(backoff)
			continue
		}
		setRelay(rt, url, StatusRunning, cmd.Process.Pid, "")
		rt.log.Printf("%s转发开始 pid=%d → %s", prefix, cmd.Process.Pid, maskURL(url))
		werr := cmd.Wait()
		select {
		case <-ctx.Done():
			setRelay(rt, url, StatusStopped, 0, "")
			return
		default:
		}
		lastErr := pw.LastLine()
		if hint := loopbackHint(url, r.localRTMPAddr()); hint != "" {
			lastErr = hint
		}
		rt.mu.Lock()
		rs := rt.relays[url]
		if rs != nil {
			rs.restarts++
			rs.status = StatusRetrying
			rs.pid = 0
			if lastErr != "" {
				rs.detail = fmt.Sprintf("%s（%ds 后重试）", lastErr, backoff)
			} else if werr != nil {
				rs.detail = fmt.Sprintf("断开(%v)，%ds 后重试", werr, backoff)
			} else {
				rs.detail = fmt.Sprintf("已结束，%ds 后重试", backoff)
			}
		}
		rt.mu.Unlock()
		rt.log.Printf("%s断开: %v，%ds 后重试", prefix, werr, backoff)
		if waitCtx(ctx, time.Duration(backoff)*time.Second) {
			return
		}
		backoff = nextBackoff(backoff)
	}
}

// localRTMPAddr 返回容器内 SRS 的 host:port，用于判断目标是"推回自己"。
func (r *Runner) localRTMPAddr() string {
	u, err := url.Parse(r.store.Settings().RTMPBase)
	if err != nil {
		return ""
	}
	host := u.Hostname()
	port := u.Port()
	if port == "" {
		port = "1935"
	}
	return host + ":" + port
}

// loopbackHint 当目标是本机回环地址时，给出人话解释（容器内 127.0.0.1 指向容器自己）。
func loopbackHint(targetURL, localAddr string) string {
	u, err := url.Parse(targetURL)
	if err != nil {
		return ""
	}
	host := u.Hostname()
	if host != "127.0.0.1" && host != "localhost" && host != "::1" && host != "0.0.0.0" {
		return ""
	}
	port := u.Port()
	if port == "" {
		port = "1935"
	}
	msg := fmt.Sprintf("连接失败：%s 在容器内指向容器自己", host)
	if localAddr != "" && (host+":"+port) == localAddr {
		msg += "（就是本容器的 SRS，流名相同会形成自环）"
	}
	msg += "；要推到宿主机上的服务，请改用宿主机 IP 或 host.docker.internal"
	return msg
}

func nextBackoff(b int) int {
	if b >= 16 {
		return 16
	}
	return b * 2
}

func waitCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return true
	case <-time.After(d):
		return false
	}
}

func setRelay(rt *jobRT, url, status string, pid int, detail string) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rs := rt.relays[url]
	if rs == nil {
		rs = &relayState{url: url}
		rt.relays[url] = rs
	}
	rs.status, rs.pid = status, pid
	rs.detail = detail
}

func shortURL(u string) string {
	if i := strings.Index(u, "://"); i >= 0 {
		u = u[i+3:]
	}
	u = strings.TrimPrefix(u, "127.0.0.1")
	u = strings.TrimPrefix(u, "localhost")
	u = strings.TrimPrefix(u, ":")
	if len(u) > 28 {
		u = u[:28] + "…"
	}
	return u
}

func maskURL(u string) string {
	i := strings.LastIndex(u, "/")
	if i <= 0 || i == len(u)-1 {
		return u
	}
	key := u[i+1:]
	if len(key) <= 6 {
		return u
	}
	return u[:i+1] + key[:3] + "****" + key[len(key)-2:]
}

// resolveSources 返回在线源流，以及缺失/不可用的源。
func (r *Runner) resolveSources(job Job) ([]StreamInfo, []string, error) {
	st, err := r.srs.StreamMap(r.store.Settings().PublicBase)
	if err != nil {
		return nil, nil, err
	}
	var srcs []StreamInfo
	var missing []string
	mixing := job.Mode == "mix" || len(job.Sources) > 1
	for _, key := range job.Sources {
		s, ok := st[key]
		if !ok {
			missing = append(missing, key)
			continue
		}
		if mixing && !s.HasVideo {
			missing = append(missing, key+"(无视频)")
			continue
		}
		srcs = append(srcs, s)
	}
	return srcs, missing, nil
}

func (r *Runner) localMixURL(jobID string) string {
	base := strings.TrimRight(r.store.Settings().RTMPBase, "/")
	u := base + "/mix/" + jobID
	// 推流鉴权：SRS 的 on_publish 会校验 ?key=，控制面自己推也要带
	if k := r.store.AccessKey(); k != "" {
		u += "?key=" + url.QueryEscape(k)
	}
	return u
}

func localURLKey(jobID string) string { return "mix/" + jobID }

// waitStream 等待本地流上线。
func (r *Runner) waitStream(key string, timeout time.Duration, ctx context.Context) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(400 * time.Millisecond):
		}
		st, err := r.srs.StreamMap(r.store.Settings().PublicBase)
		if err != nil {
			continue
		}
		if s, ok := st[key]; ok && s.HasVideo {
			return true
		}
	}
	return false
}

// Stop 停止任务。
func (r *Runner) Stop(jobID string) bool {
	rt, ok := r.getRT(jobID)
	if !ok {
		return false
	}
	rt.mu.Lock()
	rt.stopped = true
	active := rt.active
	cancel := rt.cancel
	pid := rt.pid
	rt.status = StatusStopped
	rt.detail = ""
	relayPids := make([]int, 0, len(rt.relays))
	for _, rs := range rt.relays {
		if rs.pid > 0 {
			relayPids = append(relayPids, rs.pid)
		}
	}
	rt.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	sigAll := func(p int, sig syscall.Signal) {
		if p > 0 {
			_ = syscall.Kill(-p, sig)
		}
	}
	sigAll(pid, syscall.SIGINT)
	for _, p := range relayPids {
		sigAll(p, syscall.SIGINT)
	}
	go func() {
		time.Sleep(5 * time.Second)
		rt.mu.Lock()
		still := rt.pid
		var rest []int
		for _, rs := range rt.relays {
			if rs.pid > 0 {
				rest = append(rest, rs.pid)
			}
		}
		rt.mu.Unlock()
		sigAll(still, syscall.SIGKILL)
		for _, p := range rest {
			sigAll(p, syscall.SIGKILL)
		}
	}()
	return active
}

// Restart 重启任务。
func (r *Runner) Restart(jobID string) (bool, error) {
	r.Stop(jobID)
	for i := 0; i < 100; i++ {
		if !r.IsActive(jobID) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	return r.Start(jobID)
}

func (r *Runner) Runtime(jobID string) JobRuntime {
	rt, ok := r.getRT(jobID)
	if !ok {
		return JobRuntime{JobID: jobID, Status: StatusStopped}
	}
	return rt.snapshot(jobID)
}

func (r *Runner) Logs(jobID string, n int) []string {
	rt, ok := r.getRT(jobID)
	if !ok {
		return nil
	}
	return rt.log.Lines(n)
}

func (r *Runner) IsActive(jobID string) bool {
	rt, ok := r.getRT(jobID)
	if !ok {
		return false
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.active
}

// AutoStartAll 拉起所有开启了自动启动的任务。
func (r *Runner) AutoStartAll() {
	for _, j := range r.store.Jobs() {
		if j.AutoStart && j.Enabled {
			_, _ = r.Start(j.ID)
		}
	}
}

// OnStreamOnline 源流上线时自动开始相关任务。
func (r *Runner) OnStreamOnline(key string) {
	if strings.HasPrefix(key, "mix/") {
		return
	}
	for _, j := range r.store.Jobs() {
		if !j.AutoStart || !j.Enabled {
			continue
		}
		for _, s := range j.Sources {
			if s == key {
				if !r.IsActive(j.ID) {
					_, _ = r.Start(j.ID)
				}
				break
			}
		}
	}
}

// StopAll 用于容器优雅退出。
func (r *Runner) StopAll() {
	r.mu.Lock()
	ids := make([]string, 0, len(r.rts))
	for id := range r.rts {
		ids = append(ids, id)
	}
	r.mu.Unlock()
	for _, id := range ids {
		r.Stop(id)
	}
	time.Sleep(2 * time.Second)
	for _, id := range ids {
		if rt, ok := r.getRT(id); ok {
			rt.log.Close()
		}
	}
}

func keysOf(srcs []StreamInfo) []string {
	out := make([]string, 0, len(srcs))
	for _, s := range srcs {
		out = append(out, s.Key)
	}
	return out
}
