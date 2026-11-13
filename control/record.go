package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// RecordMeta 描述一个录像文件。
type RecordMeta struct {
	ID        string  `json:"id"`
	Stream    string  `json:"stream"`
	File      string  `json:"file"`
	Size      int64   `json:"size"`
	StartedAt string  `json:"started_at"`
	EndedAt   string  `json:"ended_at,omitempty"`
	Duration  float64 `json:"duration"`
	Active    bool    `json:"active"`
	// NeedsRepair 为 true 表示这是未收尾的分片 MP4（容器被强杀、录制进程异常退出等），
	// 浏览器只能播到已写入的位置，需要"修复"（重新封装）后才能正常拖动。
	NeedsRepair bool   `json:"needs_repair,omitempty"`
	URL         string `json:"url"`
}

type recItem struct {
	mu         sync.Mutex
	meta       RecordMeta
	cmd        *exec.Cmd
	stopByUser bool
	rotating   bool
	done       chan struct{}
}

// Recorder 负责把 SRS 上的流录制成本地 MP4 文件。
type Recorder struct {
	mu       sync.Mutex
	dir      string
	bin      string
	probe    string
	rtmpBase string
	items    map[string]*recItem // id -> item
	segMin   int
}

func NewRecorder(dir, bin, probe, rtmpBase string) *Recorder {
	_ = ensureDir(dir)
	return &Recorder{dir: dir, bin: bin, probe: probe, rtmpBase: rtmpBase, items: map[string]*recItem{}}
}

func (r *Recorder) SetSegmentMinutes(m int) {
	r.mu.Lock()
	r.segMin = m
	r.mu.Unlock()
}

func safeName(s string) string {
	s = strings.ReplaceAll(s, "/", "_")
	var b strings.Builder
	for _, c := range s {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.' {
			b.WriteRune(c)
		} else {
			b.WriteRune('_')
		}
	}
	if b.Len() == 0 {
		return "stream"
	}
	return b.String()
}

func (r *Recorder) streamURL(key string) string {
	base := r.rtmpBase
	if !strings.HasSuffix(base, "/") {
		base += "/"
	}
	return base + strings.TrimPrefix(key, "/")
}

// Start 开始录制某一路流。
func (r *Recorder) Start(stream string) (RecordMeta, error) {
	stream = strings.TrimPrefix(strings.TrimSpace(stream), "/")
	if stream == "" {
		return RecordMeta{}, fmt.Errorf("流名不能为空")
	}
	r.mu.Lock()
	if rec := r.activeByStreamLocked(stream); rec != nil {
		m := rec.meta
		r.mu.Unlock()
		return m, nil
	}
	now := time.Now()
	base := fmt.Sprintf("%s_%s", safeName(stream), now.Format("20060102-150405"))
	file := base + ".mp4"
	id := base
	item := &recItem{
		done: make(chan struct{}),
		meta: RecordMeta{
			ID:        id,
			Stream:    stream,
			File:      file,
			StartedAt: now.Format(time.RFC3339),
			Active:    true,
		},
	}
	r.items[id] = item
	seg := r.segMin
	r.mu.Unlock()

	path := filepath.Join(r.dir, file)
	args := []string{
		"-y", "-hide_banner", "-loglevel", "warning", "-nostdin",
		"-rw_timeout", "5000000",
		"-i", r.streamURL(stream),
		// 源如果是手机/相机推流，时间基常是 1/16000 之类，统一成 90kHz 更利于播放器解析
		"-c", "copy", "-fflags", "+genpts", "-avoid_negative_ts", "make_zero",
		"-video_track_timescale", "90000",
		"-f", "mp4", "-movflags", "+frag_keyframe+empty_moov+default_base_moof",
		"-max_muxing_queue_size", "2048",
		path,
	}
	cmd := exec.Command(r.bin, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		r.mu.Lock()
		delete(r.items, id)
		r.mu.Unlock()
		return item.meta, fmt.Errorf("启动录制失败: %w", err)
	}
	item.mu.Lock()
	item.cmd = cmd
	item.mu.Unlock()
	r.writeMeta(item.meta)

	go r.monitor(item, seg)
	return item.meta, nil
}

// monitor 等待 ffmpeg 结束并收尾（转封装为可快速起播的 MP4）。
func (r *Recorder) monitor(item *recItem, segMin int) {
	var timer <-chan time.Time
	if segMin > 0 {
		t := time.NewTimer(time.Duration(segMin) * time.Minute)
		defer t.Stop()
		timer = t.C
	}
	waitCh := make(chan struct{})
	go func() {
		_ = item.cmd.Wait()
		close(waitCh)
	}()
	for {
		select {
		case <-waitCh:
			goto done
		case <-timer:
			item.mu.Lock()
			item.rotating = true
			cmd := item.cmd
			item.mu.Unlock()
			if cmd != nil && cmd.Process != nil {
				_ = cmd.Process.Signal(syscall.SIGINT)
			}
			<-waitCh
			goto done
		}
	}
done:
	r.finalize(item)
	close(item.done)
	item.mu.Lock()
	rotate := item.rotating && !item.stopByUser
	stream := item.meta.Stream
	item.mu.Unlock()
	if rotate {
		if _, err := r.Start(stream); err != nil {
			fmt.Printf("分段录制重启失败: %v\n", err)
		}
	}
}

func (r *Recorder) finalize(item *recItem) {
	item.mu.Lock()
	meta := item.meta
	item.mu.Unlock()
	path := filepath.Join(r.dir, meta.File)

	item.mu.Lock()
	meta.Active = false
	meta.EndedAt = time.Now().Format(time.RFC3339)
	item.mu.Unlock()

	// 录制过程中写入的是「分片 MP4」（边写边可用，但没有完整索引、时长未知），
	// 停止后转封装成带完整索引的标准 MP4，浏览器才能正常显示时长、拖动进度条。
	_ = r.remuxFaststart(path)
	meta.Size = fileSize(path)
	meta.Duration = r.probeDuration(path)
	r.writeMeta(meta)

	item.mu.Lock()
	item.meta = meta
	item.mu.Unlock()

	r.mu.Lock()
	delete(r.items, meta.ID)
	r.mu.Unlock()
}

// remuxFaststart 把分片 MP4 转封装成带完整索引的标准 MP4（原地替换）。
// 只做 -c copy，不重新编码，速度取决于磁盘 IO。
func (r *Recorder) remuxFaststart(path string) error {
	if st, err := os.Stat(path); err != nil || st.Size() == 0 {
		return fmt.Errorf("文件为空或不存在")
	}
	tmp := path + ".remux.mp4"
	args := []string{"-y", "-hide_banner", "-loglevel", "error", "-i", path,
		"-c", "copy", "-fflags", "+genpts", "-avoid_negative_ts", "make_zero",
		"-video_track_timescale", "90000",
		"-movflags", "+faststart", tmp}
	if out, err := exec.Command(r.bin, args...).CombinedOutput(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("转封装失败: %v: %s", err, strings.TrimSpace(string(out)))
	}
	if fi, err := os.Stat(tmp); err != nil || fi.Size() == 0 {
		_ = os.Remove(tmp)
		return fmt.Errorf("转封装产出为空")
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// RecordBrief 是"某路流正在录制"的简要状态。
type RecordBrief struct {
	ID        string  `json:"id"`
	StartedAt string  `json:"started_at"`
	Duration  float64 `json:"duration"`
	Size      int64   `json:"size"`
}

// ActiveByStream 返回 流名 → 正在录制的简报。
func (r *Recorder) ActiveByStream() map[string]RecordBrief {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]RecordBrief, len(r.items))
	for _, it := range r.items {
		m := it.meta
		var dur float64
		if t, err := time.Parse(time.RFC3339, m.StartedAt); err == nil {
			dur = time.Since(t).Seconds()
		}
		out[m.Stream] = RecordBrief{ID: m.ID, StartedAt: m.StartedAt, Duration: dur, Size: fileSize(filepath.Join(r.dir, m.File))}
	}
	return out
}

// HLS 为录像生成 HLS 切片（-c copy，不转码），用于手机/微信内置浏览器等
// 对整段大 MP4 支持不好的场景。生成结果放在 /data/hls/<id>/index.m3u8。
func (r *Recorder) HLS(id string) (string, string, error) {
	m, ok := r.Get(id)
	if !ok {
		return "", "", errNotFound
	}
	if m.Active {
		return "", "", fmt.Errorf("正在录制中，停止后才能生成 HLS")
	}
	src := filepath.Join(r.dir, m.File)
	if !fileExists(src) {
		return "", "", errNotFound
	}
	base := filepath.Join(filepath.Dir(r.dir), "hls")
	dir := filepath.Join(base, m.ID)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", "", err
	}
	idx := filepath.Join(dir, "index.m3u8")
	// 已生成且比源文件新，直接复用
	if st, err := os.Stat(idx); err == nil {
		if si, err2 := os.Stat(src); err2 == nil && st.ModTime().After(si.ModTime()) {
			return m.ID, "/hls/" + m.ID + "/index.m3u8", nil
		}
	}
	args := []string{"-y", "-hide_banner", "-loglevel", "error", "-i", src,
		"-c", "copy", "-f", "hls", "-hls_time", "6", "-hls_playlist_type", "vod",
		"-hls_segment_filename", filepath.Join(dir, "seg_%05d.ts"), idx}
	if out, err := exec.Command(r.bin, args...).CombinedOutput(); err != nil {
		return "", "", fmt.Errorf("生成 HLS 失败: %v: %s", err, strings.TrimSpace(string(out)))
	}
	if !fileExists(idx) {
		return "", "", fmt.Errorf("生成 HLS 失败：没有产出播放列表")
	}
	return m.ID, "/hls/" + m.ID + "/index.m3u8", nil
}

// Repair 对已存在的录像重新转封装（修复索引/时长），用于早期录制或异常中断的文件。
func (r *Recorder) Repair(id string) (RecordMeta, error) {
	r.mu.Lock()
	for _, it := range r.items {
		if it.meta.ID == id || it.meta.File == id {
			r.mu.Unlock()
			return RecordMeta{}, fmt.Errorf("该录像正在录制中，请先停止再修复")
		}
	}
	r.mu.Unlock()

	m, ok := r.Get(id)
	if !ok {
		return RecordMeta{}, errNotFound
	}
	path := filepath.Join(r.dir, m.File)
	if err := r.remuxFaststart(path); err != nil {
		return m, err
	}
	m.Size = fileSize(path)
	m.Duration = r.probeDuration(path)
	if m.StartedAt == "" {
		if _, t := parseName(m.File); !t.IsZero() {
			m.StartedAt = t.Format(time.RFC3339)
		}
	}
	r.writeMeta(m)
	return m, nil
}

// isFragmentedMP4 通过顶层 box 判断文件是不是「分片 MP4」。
// 普通 MP4 是 ftyp/moov/mdat；分片 MP4 在 mdat 之前会有 moof。
func isFragmentedMP4(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	hdr := make([]byte, 8)
	off := int64(0)
	for i := 0; i < 64; i++ {
		if _, err := f.ReadAt(hdr, off); err != nil {
			return false
		}
		size := int64(binary.BigEndian.Uint32(hdr[0:4]))
		typ := string(hdr[4:8])
		switch typ {
		case "moof":
			return true
		case "mdat":
			return false // 走到数据区前都没有 moof，说明是普通 MP4
		}
		if size < 8 {
			return false
		}
		off += size
	}
	return false
}

// RecoverOrphans 修复"没收尾"的录像：容器被强杀、录制进程异常退出时，
// 文件只剩分片 MP4，meta 里还停在 active=true。启动时扫一遍并自动重新封装。
func (r *Recorder) RecoverOrphans() {
	entries, err := os.ReadDir(r.dir)
	if err != nil {
		return
	}
	// 正在录制的文件不动
	r.mu.Lock()
	inUse := map[string]bool{}
	for _, it := range r.items {
		inUse[it.meta.File] = true
	}
	r.mu.Unlock()

	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(name), ".mp4") {
			continue
		}
		if inUse[name] {
			continue
		}
		path := filepath.Join(r.dir, name)
		size := fileSize(path)
		if size <= 0 {
			continue
		}

		var meta RecordMeta
		metaPath := filepath.Join(r.dir, strings.TrimSuffix(name, ".mp4")+".json")
		hasMeta := false
		if b, err := os.ReadFile(metaPath); err == nil {
			if json.Unmarshal(b, &meta) == nil && meta.File != "" {
				hasMeta = true
			}
		}

		orphan := hasMeta && meta.Active
		fragmented := isFragmentedMP4(path)
		if !orphan && !fragmented {
			continue
		}
		// 超大文件启动时不自动转封装，避免占满磁盘；留给界面手动「修复」
		if size > 2<<30 {
			fmt.Printf("录像 %s 未收尾（%d MB），体积较大，跳过自动修复\n", name, size>>20)
			continue
		}

		fmt.Printf("发现未收尾的录像 %s，正在重新封装…\n", name)
		if err := r.remuxFaststart(path); err != nil {
			fmt.Printf("录像 %s 修复失败: %v\n", name, err)
			continue
		}
		if !hasMeta {
			stream, t := parseName(name)
			meta = RecordMeta{ID: strings.TrimSuffix(name, ".mp4"), Stream: stream, File: name}
			if !t.IsZero() {
				meta.StartedAt = t.Format(time.RFC3339)
			}
		}
		meta.Active = false
		meta.NeedsRepair = false
		meta.Size = fileSize(path)
		meta.Duration = r.probeDuration(path)
		if meta.EndedAt == "" {
			meta.EndedAt = time.Now().Format(time.RFC3339)
		}
		r.writeMeta(meta)
		fmt.Printf("录像 %s 已修复: %.1f 秒, %d MB\n", name, meta.Duration, meta.Size>>20)
	}
}

func fileSize(p string) int64 {
	if st, err := os.Stat(p); err == nil {
		return st.Size()
	}
	return 0
}

// probeDuration 用 ffprobe 读取时长（秒）。
func (r *Recorder) probeDuration(p string) float64 {
	out, err := exec.Command(r.probe, "-v", "error",
		"-show_entries", "format=duration", "-of", "default=nw=1:nk=1", p).Output()
	if err != nil {
		return 0
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil {
		return 0
	}
	return v
}

func (r *Recorder) writeMeta(m RecordMeta) {
	p := filepath.Join(r.dir, strings.TrimSuffix(m.File, ".mp4")+".json")
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(p, b, 0644)
}

func (r *Recorder) activeByStreamLocked(stream string) *recItem {
	for _, it := range r.items {
		if it.meta.Stream == stream {
			return it
		}
	}
	return nil
}

// Stop 停止某条录像。
func (r *Recorder) Stop(id string) error {
	r.mu.Lock()
	item, ok := r.items[id]
	r.mu.Unlock()
	if !ok {
		return errNotFound
	}
	item.mu.Lock()
	item.stopByUser = true
	cmd := item.cmd
	item.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	_ = cmd.Process.Signal(syscall.SIGINT)
	select {
	case <-item.done:
	case <-time.After(20 * time.Second):
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	return nil
}

// StopAsync 后台停止录制，立刻返回，避免阻塞 SRS 的 http_hooks（否则 hook 会超时）。
func (r *Recorder) StopAsync(id string) {
	go func() {
		if err := r.Stop(id); err != nil {
			fmt.Printf("停止录制 %s 失败: %v\n", id, err)
		}
	}()
}

// StopByStream 按流名异步停止录制（供 on_unpublish 回调使用）。
func (r *Recorder) StopByStream(stream string) {
	r.mu.Lock()
	var ids []string
	for _, it := range r.items {
		if it.meta.Stream == stream {
			ids = append(ids, it.meta.ID)
		}
	}
	r.mu.Unlock()
	for _, id := range ids {
		r.StopAsync(id)
	}
}

// Reconcile 兜底巡检：某个流已经从 SRS 下线（hook 丢失、SRS 重启等），
// 但录制还挂着时，主动收尾，避免文件长期停在"未收尾"状态。
func (r *Recorder) Reconcile(online map[string]bool) []string {
	r.mu.Lock()
	var stale []string
	for _, it := range r.items {
		if !online[it.meta.Stream] {
			stale = append(stale, it.meta.ID)
		}
	}
	r.mu.Unlock()
	for _, id := range stale {
		fmt.Printf("巡检：流已下线但仍在录制，停止 %s\n", id)
		r.StopAsync(id)
	}
	return stale
}

// Active 返回正在录制的列表。
func (r *Recorder) Active() []RecordMeta {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]RecordMeta, 0, len(r.items))
	for _, it := range r.items {
		out = append(out, it.meta)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt > out[j].StartedAt })
	return out
}

// List 返回录像列表（磁盘为准，合并侧车元数据）。
func (r *Recorder) List() []RecordMeta {
	r.mu.Lock()
	active := map[string]bool{}
	for _, it := range r.items {
		active[it.meta.File] = true
	}
	r.mu.Unlock()

	entries, err := os.ReadDir(r.dir)
	if err != nil {
		return nil
	}
	out := []RecordMeta{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".mp4") {
			continue
		}
		path := filepath.Join(r.dir, e.Name())
		metaPath := filepath.Join(r.dir, strings.TrimSuffix(e.Name(), ".mp4")+".json")
		var m RecordMeta
		if b, err := os.ReadFile(metaPath); err == nil {
			if json.Unmarshal(b, &m) == nil && m.File != "" {
				m.Size = fileSize(path)
				m.Active = active[e.Name()]
				m.NeedsRepair = !m.Active && isFragmentedMP4(path)
				if m.Active {
					// 录制中：时长按开始时间推算，前端用来显示"已录 N 分钟"
					if t, err := time.Parse(time.RFC3339, m.StartedAt); err == nil {
						m.Duration = time.Since(t).Seconds()
					}
				}
				out = append(out, m)
				continue
			}
		}
		st, err := e.Info()
		if err != nil {
			continue
		}
		stream, _ := parseName(e.Name())
		m = RecordMeta{
			ID:        strings.TrimSuffix(e.Name(), ".mp4"),
			Stream:    stream,
			File:      e.Name(),
			Size:      st.Size(),
			StartedAt: st.ModTime().Format(time.RFC3339),
			Active:    active[e.Name()],
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt > out[j].StartedAt })
	return out
}

// parseName 从文件名反解流名与开始时间（xxx_20240101-120000.mp4）。
func parseName(file string) (stream string, t time.Time) {
	base := strings.TrimSuffix(file, ".mp4")
	i := strings.LastIndex(base, "_")
	if i < 0 {
		return base, time.Time{}
	}
	stream = base[:i]
	if ts, err := time.Parse("20060102-150405", base[i+1:]); err == nil {
		t = ts
	}
	return stream, t
}

// Get 返回单个录像元数据。
func (r *Recorder) Get(id string) (RecordMeta, bool) {
	for _, m := range r.List() {
		if m.ID == id || m.File == id {
			return m, true
		}
	}
	return RecordMeta{}, false
}

// Delete 删除录像文件。
func (r *Recorder) Delete(id string) error {
	r.mu.Lock()
	for _, it := range r.items {
		if it.meta.ID == id || it.meta.File == id {
			r.mu.Unlock()
			return fmt.Errorf("该录像正在录制中，请先停止")
		}
	}
	r.mu.Unlock()

	m, ok := r.Get(id)
	if !ok {
		return errNotFound
	}
	path := filepath.Join(r.dir, m.File)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	_ = os.Remove(filepath.Join(r.dir, strings.TrimSuffix(m.File, ".mp4")+".json"))
	_ = os.RemoveAll(filepath.Join(filepath.Dir(r.dir), "hls", m.ID))
	return nil
}

// StopAll 停止所有录制（退出时调用）。
func (r *Recorder) StopAll() {
	for _, m := range r.Active() {
		_ = r.Stop(m.ID)
	}
}
