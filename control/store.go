package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// ----------------------------------------------------------------------------
// 数据模型
// ----------------------------------------------------------------------------

// Target 是一个第三方推流目标（RTMP/RTMPS 地址）。
type Target struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	URL       string `json:"url"`
	Enabled   bool   `json:"enabled"`
	Remark    string `json:"remark,omitempty"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// Job 是一条转推任务：把本地 SRS 的 1 个或多个流，推到 1 个或多个第三方目标。
//
//	Mode=direct：单流直接转推（不转码，-c copy）
//	Mode=mix   ：多流按 Layout 混流后转推（转码）
type Job struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Mode    string   `json:"mode"`
	Sources []string `json:"sources"` // 形如 live/camera01

	// 混流参数（Mode=mix 时生效）
	Layout       string `json:"layout"`        // vstack|hstack|grid|pip|circle
	Width        int    `json:"width"`         // 输出宽
	Height       int    `json:"height"`        // 输出高
	FPS          int    `json:"fps"`           // 输出帧率
	VideoBitrate int    `json:"video_bitrate"` // kbps
	AudioBitrate int    `json:"audio_bitrate"` // kbps
	AudioMode    string `json:"audio_mode"`    // mix|first|none
	PipSize      int    `json:"pip_size"`      // 圆窗口/画中画直径，占画面宽度百分比
	PipPos       string `json:"pip_pos"`       // topright|topleft|bottomright|bottomleft
	Gap          int    `json:"gap"`           // 宫格间距（像素）
	Background   string `json:"background"`    // 填充底色，如 0x101010

	// 输出
	TargetIDs []string `json:"target_ids"` // 引用 /api/targets
	ExtraURLs []string `json:"extra_urls"` // 临时地址，直接填写

	AutoStart bool `json:"auto_start"` // 源流一上线就自动开始
	Enabled   bool `json:"enabled"`    // 是否允许运行

	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// Settings 是全局设置。
type Settings struct {
	AutoRecord           bool   `json:"auto_record"`            // 有流推上来就自动录制
	RecordSegmentMinutes int    `json:"record_segment_minutes"` // 单个录像分段时长，0 表示不分段
	SRSAPI               string `json:"srs_api"`
	SRSHTTP              string `json:"srs_http"`    // SRS 的 HTTP 服务地址（HTTP-FLV/HLS/播放器），控制面对其做反代
	RTMPBase             string `json:"rtmp_base"`   // 容器内推流地址（SRS 监听）
	RTMPPublic           string `json:"rtmp_public"` // 对外推流地址（端口映射后的真实地址，前端展示用）
	PublicBase           string `json:"public_base"` // 对外可访问地址，用于生成播放/推流地址
}

type state struct {
	Targets  []Target `json:"targets"`
	Jobs     []Job    `json:"jobs"`
	Settings Settings `json:"settings"`
}

// Store 是持久化状态（/data/config.json）。
type Store struct {
	mu   sync.RWMutex
	path string
	st   state
}

func NewStore(path string) (*Store, error) {
	s := &Store{path: path}
	s.st.Settings = defaultSettings()
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func defaultSettings() Settings {
	return Settings{
		AutoRecord:           true,
		RecordSegmentMinutes: 30,
		SRSAPI:               envOr("SRS_API", "http://127.0.0.1:1985"),
		SRSHTTP:              envOr("SRS_HTTP_BASE", "http://127.0.0.1:8080"),
		RTMPBase:             envOr("RTMP_BASE", "rtmp://127.0.0.1:1935/"),
		RTMPPublic:           envOr("PUBLIC_RTMP", ""),
		PublicBase:           envOr("PUBLIC_BASE", ""),
	}
}

func defaultJob(in Job) Job {
	if in.Mode == "" {
		in.Mode = "direct"
	}
	if in.Layout == "" {
		in.Layout = "vstack"
	}
	if in.Width <= 0 {
		in.Width = 1280
	}
	if in.Height <= 0 {
		in.Height = 720
	}
	if in.FPS <= 0 {
		in.FPS = 25
	}
	if in.VideoBitrate <= 0 {
		in.VideoBitrate = 2500
	}
	if in.AudioBitrate <= 0 {
		in.AudioBitrate = 128
	}
	if in.AudioMode == "" {
		in.AudioMode = "mix"
	}
	if in.PipSize <= 0 {
		in.PipSize = 28
	}
	if in.PipPos == "" {
		in.PipPos = "topright"
	}
	if in.Gap < 0 {
		in.Gap = 0
	}
	if in.Background == "" {
		in.Background = "0x101014"
	}
	return in
}

func (s *Store) load() error {
	b, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var st state
	if err := json.Unmarshal(b, &st); err != nil {
		return fmt.Errorf("解析 %s 失败: %w", s.path, err)
	}
	// 环境变量优先于配置文件里的连接参数。
	def := defaultSettings()
	if v := os.Getenv("SRS_API"); v != "" {
		st.Settings.SRSAPI = v
	} else if st.Settings.SRSAPI == "" {
		st.Settings.SRSAPI = def.SRSAPI
	}
	if v := os.Getenv("RTMP_BASE"); v != "" {
		st.Settings.RTMPBase = v
	} else if st.Settings.RTMPBase == "" {
		st.Settings.RTMPBase = def.RTMPBase
	}
	if v := os.Getenv("PUBLIC_RTMP"); v != "" {
		st.Settings.RTMPPublic = v
	} else if st.Settings.RTMPPublic == "" {
		st.Settings.RTMPPublic = def.RTMPPublic
	}
	if v := os.Getenv("SRS_HTTP_BASE"); v != "" {
		st.Settings.SRSHTTP = v
	} else if st.Settings.SRSHTTP == "" {
		st.Settings.SRSHTTP = def.SRSHTTP
	}
	if v := os.Getenv("PUBLIC_BASE"); v != "" {
		st.Settings.PublicBase = v
	} else if st.Settings.PublicBase == "" {
		st.Settings.PublicBase = def.PublicBase
	}
	for i := range st.Jobs {
		st.Jobs[i] = defaultJob(st.Jobs[i])
	}
	s.st = st
	return nil
}

func (s *Store) saveLocked() error {
	b, err := json.MarshalIndent(s.st, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func (s *Store) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveLocked()
}

// ---- Settings ----

func (s *Store) Settings() Settings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.st.Settings
}

func (s *Store) SetSettings(in Settings) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if in.SRSAPI == "" {
		in.SRSAPI = s.st.Settings.SRSAPI
	}
	if in.RTMPBase == "" {
		in.RTMPBase = s.st.Settings.RTMPBase
	}
	if in.SRSHTTP == "" {
		in.SRSHTTP = s.st.Settings.SRSHTTP
	}
	if in.RTMPPublic == "" {
		in.RTMPPublic = s.st.Settings.RTMPPublic
	}
	s.st.Settings = in
	return s.saveLocked()
}

// ---- Targets ----

func (s *Store) Targets() []Target {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Target, len(s.st.Targets))
	copy(out, s.st.Targets)
	return out
}

func (s *Store) TargetByID(id string) (Target, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, t := range s.st.Targets {
		if t.ID == id {
			return t, true
		}
	}
	return Target{}, false
}

func (s *Store) AddTarget(t Target) (Target, error) {
	if strings.TrimSpace(t.URL) == "" {
		return t, fmt.Errorf("推流地址不能为空")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if t.ID == "" {
		t.ID = newID("tg")
	}
	now := nowStr()
	t.CreatedAt, t.UpdatedAt = now, now
	s.st.Targets = append(s.st.Targets, t)
	return t, s.saveLocked()
}

func (s *Store) UpdateTarget(id string, in Target) (Target, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.st.Targets {
		if s.st.Targets[i].ID != id {
			continue
		}
		old := s.st.Targets[i]
		if strings.TrimSpace(in.URL) == "" {
			return old, fmt.Errorf("推流地址不能为空")
		}
		old.Name = in.Name
		old.URL = in.URL
		old.Enabled = in.Enabled
		old.Remark = in.Remark
		old.UpdatedAt = nowStr()
		s.st.Targets[i] = old
		return old, s.saveLocked()
	}
	return Target{}, errNotFound
}

func (s *Store) DeleteTarget(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.st.Targets {
		if s.st.Targets[i].ID == id {
			s.st.Targets = append(s.st.Targets[:i], s.st.Targets[i+1:]...)
			return s.saveLocked()
		}
	}
	return errNotFound
}

// ---- Jobs ----

func (s *Store) Jobs() []Job {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Job, len(s.st.Jobs))
	copy(out, s.st.Jobs)
	return out
}

func (s *Store) JobByID(id string) (Job, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, j := range s.st.Jobs {
		if j.ID == id {
			return j, true
		}
	}
	return Job{}, false
}

func (s *Store) AddJob(in Job) (Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	in = defaultJob(in)
	if in.ID == "" {
		in.ID = newID("job")
	}
	now := nowStr()
	in.CreatedAt, in.UpdatedAt = now, now
	s.st.Jobs = append(s.st.Jobs, in)
	return in, s.saveLocked()
}

func (s *Store) UpdateJob(id string, in Job) (Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.st.Jobs {
		if s.st.Jobs[i].ID != id {
			continue
		}
		in = defaultJob(in)
		in.ID = id
		in.CreatedAt = s.st.Jobs[i].CreatedAt
		in.UpdatedAt = nowStr()
		s.st.Jobs[i] = in
		return in, s.saveLocked()
	}
	return Job{}, errNotFound
}

func (s *Store) DeleteJob(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.st.Jobs {
		if s.st.Jobs[i].ID == id {
			s.st.Jobs = append(s.st.Jobs[:i], s.st.Jobs[i+1:]...)
			return s.saveLocked()
		}
	}
	return errNotFound
}

// ResolveURLs 返回任务最终要推的地址列表（去重）。
func (s *Store) ResolveURLs(j Job) []string {
	var out []string
	seen := map[string]bool{}
	for _, id := range j.TargetIDs {
		if t, ok := s.TargetByID(id); ok && t.Enabled {
			if !seen[t.URL] {
				seen[t.URL] = true
				out = append(out, t.URL)
			}
		}
	}
	for _, u := range j.ExtraURLs {
		u = strings.TrimSpace(u)
		if u != "" && !seen[u] {
			seen[u] = true
			out = append(out, u)
		}
	}
	sort.Strings(out)
	return out
}

// ---- helpers ----

func newID(prefix string) string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return prefix + "_" + hex.EncodeToString(b)
}

func nowStr() string {
	return time.Now().Format("2006-01-02T15:04:05Z07:00")
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func ensureDir(p string) error { return os.MkdirAll(p, 0755) }

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

func absPath(p string) string {
	a, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	return a
}
