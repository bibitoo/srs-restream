package main

import (
	"fmt"
	"strconv"
	"strings"
)

// 支持的混流布局。
const (
	LayoutSingle = "single" // 单画面（整幅铺满）
	LayoutVStack = "vstack" // 上下排列
	LayoutHStack = "hstack" // 左右排列
	LayoutGrid   = "grid"   // 宫格
	LayoutPip    = "pip"    // 画中画（方窗）
	LayoutCircle = "circle" // 圆窗口画中画
)

var ValidLayouts = []string{LayoutSingle, LayoutVStack, LayoutHStack, LayoutGrid, LayoutPip, LayoutCircle}

// cell 描述画布上的一个窗口。
type cell struct {
	idx    int  // 对应的输入序号
	x, y   int  // 左上角
	w, h   int  // 尺寸
	circle bool // 是否圆形遮罩
}

// even 向上取偶，保证编码器要求。
func even(v int) int {
	if v < 2 {
		return 2
	}
	return v &^ 1
}

// layoutCells 计算每个源在画布上的位置与尺寸。
func layoutCells(job Job, n int) []cell {
	W, H := even(job.Width), even(job.Height)
	gap := job.Gap
	if gap < 0 {
		gap = 0
	}
	if n <= 0 {
		return nil
	}
	if n == 1 {
		return []cell{{idx: 0, x: 0, y: 0, w: W, h: H}}
	}

	switch job.Layout {
	case LayoutHStack:
		cw := even((W - gap*(n-1)) / n)
		out := make([]cell, 0, n)
		for i := 0; i < n; i++ {
			out = append(out, cell{idx: i, x: i * (cw + gap), y: 0, w: cw, h: H})
		}
		return out

	case LayoutGrid:
		cols := 1
		for cols*cols < n {
			cols++
		}
		rows := (n + cols - 1) / cols
		cw := even((W - gap*(cols-1)) / cols)
		ch := even((H - gap*(rows-1)) / rows)
		out := make([]cell, 0, n)
		for i := 0; i < n; i++ {
			r, c := i/cols, i%cols
			out = append(out, cell{idx: i, x: c * (cw + gap), y: r * (ch + gap), w: cw, h: ch})
		}
		return out

	case LayoutPip, LayoutCircle:
		// 第 1 路铺满，其余作为小窗叠在角落。
		out := []cell{{idx: 0, x: 0, y: 0, w: W, h: H}}
		size := even(W * job.PipSize / 100)
		if size < 64 {
			size = 64
		}
		if size > even(H/2) {
			size = even(H / 2)
		}
		m := job.Gap
		if m < 16 {
			m = 16
		}
		for i := 1; i < n; i++ {
			k := i - 1
			var x, y int
			switch strings.ToLower(job.PipPos) {
			case "topleft":
				x, y = m, m+k*(size+m)
			case "bottomleft":
				x, y = m, H-size-m-k*(size+m)
			case "bottomright":
				x, y = W-size-m, H-size-m-k*(size+m)
			default: // topright
				x, y = W-size-m, m+k*(size+m)
			}
			if y < 0 {
				y = 0
			}
			out = append(out, cell{idx: i, x: x, y: y, w: size, h: size, circle: job.Layout == LayoutCircle})
		}
		return out

	default: // LayoutVStack
		ch := even((H - gap*(n-1)) / n)
		out := make([]cell, 0, n)
		for i := 0; i < n; i++ {
			out = append(out, cell{idx: i, x: 0, y: i * (ch + gap), w: W, h: ch})
		}
		return out
	}
}

// ffEnv 是构建 ffmpeg 命令所需的运行时参数。
type ffEnv struct {
	FFmpeg     string
	MaskPath   string // 圆形遮罩 PNG
	RTMPBase   string // rtmp://127.0.0.1:1935/
	LocalMux   bool
	StreamInfo map[string]StreamInfo
}

// streamURL 把 app/stream 转成 RTMP 拉流地址。
func (e ffEnv) streamURL(key string) string {
	base := e.RTMPBase
	if !strings.HasSuffix(base, "/") {
		base += "/"
	}
	return base + strings.TrimPrefix(key, "/")
}

// relayArgs 生成单目标中继参数（拉本地流 + copy 转推）。
func BuildRelayArgs(relaySrc, url string) []string {
	return []string{
		"-hide_banner", "-loglevel", "warning", "-nostdin",
		"-rw_timeout", "5000000",
		"-i", relaySrc,
		"-c", "copy",
		"-map", "0:v?", "-map", "0:a?",
		"-flvflags", "no_duration_filesize",
		"-max_muxing_queue_size", "2048",
		"-f", "flv", url,
	}
}

// BuildMixArgs 多流混流（或单流重编码）后转推。
func BuildMixArgs(env ffEnv, job Job, srcs []StreamInfo, urls []string) ([]string, error) {
	if len(urls) != 1 {
		return nil, fmt.Errorf("混流编码只接受一个输出地址（本地 mix 流）")
	}
	W, H := even(job.Width), even(job.Height)
	fps := job.FPS
	if fps <= 0 {
		fps = 25
	}
	bg := job.Background
	if bg == "" {
		bg = "0x101014"
	}
	cells := layoutCells(job, len(srcs))
	if len(cells) == 0 {
		return nil, fmt.Errorf("没有可混流的输入")
	}

	hasCircle := false
	for _, c := range cells {
		if c.circle {
			hasCircle = true
		}
	}

	args := []string{"-hide_banner", "-loglevel", "warning", "-nostdin"}
	for _, s := range srcs {
		args = append(args, "-thread_queue_size", "1024", "-rw_timeout", "5000000", "-i", env.streamURL(s.Key))
	}
	maskIdx := -1
	if hasCircle {
		if !fileExists(env.MaskPath) {
			return nil, fmt.Errorf("圆形遮罩文件不存在: %s", env.MaskPath)
		}
		maskIdx = len(srcs)
		args = append(args, "-loop", "1", "-framerate", strconv.Itoa(fps), "-i", env.MaskPath)
	}

	var g []string
	// 每一路先归一到自己的窗口尺寸。
	for i, c := range cells {
		if c.circle {
			// 圆形窗口：放大填满后裁剪成正方形，再套圆形 alpha 遮罩。
			g = append(g, fmt.Sprintf(
				"[%d:v]scale=%d:%d:force_original_aspect_ratio=increase:flags=bicubic,crop=%d:%d,setsar=1,fps=%d,format=rgba[c%d]",
				c.idx, c.w, c.h, c.w, c.h, fps, i))
		} else {
			g = append(g, fmt.Sprintf(
				"[%d:v]scale=%d:%d:force_original_aspect_ratio=decrease:flags=bicubic,pad=%d:%d:(ow-iw)/2:(oh-ih)/2:color=%s,setsar=1,fps=%d[c%d]",
				c.idx, c.w, c.h, c.w, c.h, bg, fps, i))
		}
	}
	if hasCircle {
		// 同一张遮罩要供多个圆窗使用，必须先 split 成多路。
		var circleIdx []int
		for i, c := range cells {
			if c.circle {
				circleIdx = append(circleIdx, i)
			}
		}
		if len(circleIdx) == 1 {
			i := circleIdx[0]
			g = append(g, fmt.Sprintf("[%d:v]scale=%d:%d,format=gray[mk%d]", maskIdx, cells[i].w, cells[i].h, i))
		} else {
			var outs []string
			for _, i := range circleIdx {
				outs = append(outs, fmt.Sprintf("[mraw%d]", i))
			}
			g = append(g, fmt.Sprintf("[%d:v]split=%d%s", maskIdx, len(circleIdx), strings.Join(outs, "")))
			for _, i := range circleIdx {
				g = append(g, fmt.Sprintf("[mraw%d]scale=%d:%d,format=gray[mk%d]", i, cells[i].w, cells[i].h, i))
			}
		}
		for _, i := range circleIdx {
			g = append(g, fmt.Sprintf("[c%d][mk%d]alphamerge[cx%d]", i, i, i))
		}
	}

	// 画布 + 逐个叠加。
	g = append(g, fmt.Sprintf("color=c=%s:s=%dx%d:r=%d,format=yuv420p[base]", bg, W, H, fps))
	prev := "[base]"
	for i, c := range cells {
		src := fmt.Sprintf("[c%d]", i)
		if c.circle {
			src = fmt.Sprintf("[cx%d]", i)
		}
		out := fmt.Sprintf("[o%d]", i)
		g = append(g, fmt.Sprintf("%s%soverlay=x=%d:y=%d:eof_action=pass:shortest=0%s", prev, src, c.x, c.y, out))
		prev = out
	}
	g = append(g, fmt.Sprintf("%sformat=yuv420p,setsar=1[vout]", prev))

	// 音频。
	audioIdx := audioInputs(job, srcs)
	var aout string
	switch {
	case len(audioIdx) == 0:
		aout = ""
	case len(audioIdx) == 1 || job.AudioMode == "first":
		g = append(g, fmt.Sprintf("[%d:a]aresample=async=1:first_pts=0,aformat=sample_fmts=fltp:channel_layouts=stereo[aout]", audioIdx[0]))
		aout = "[aout]"
	default:
		var parts []string
		for k, idx := range audioIdx {
			n := fmt.Sprintf("[a%d]", k)
			g = append(g, fmt.Sprintf("[%d:a]aresample=async=1:first_pts=0,aformat=sample_fmts=fltp:channel_layouts=stereo%s", idx, n))
			parts = append(parts, n)
		}
		g = append(g, fmt.Sprintf("%samix=inputs=%d:duration=longest:dropout_transition=0:normalize=0,alimiter=limit=0.9[aout]",
			strings.Join(parts, ""), len(parts)))
		aout = "[aout]"
	}

	vb := job.VideoBitrate
	if vb <= 0 {
		vb = 2500
	}
	ab := job.AudioBitrate
	if ab <= 0 {
		ab = 128
	}
	gop := fps * 2

	args = append(args, "-filter_complex", strings.Join(g, ";"))
	args = append(args, "-map", "[vout]")
	if aout != "" {
		args = append(args, "-map", aout)
	}
	args = append(args,
		"-c:v", "libx264", "-preset", "veryfast", "-tune", "zerolatency",
		"-profile:v", "main", "-pix_fmt", "yuv420p",
		"-b:v", fmt.Sprintf("%dk", vb),
		"-maxrate", fmt.Sprintf("%dk", vb),
		"-bufsize", fmt.Sprintf("%dk", vb*2),
		"-g", strconv.Itoa(gop), "-keyint_min", strconv.Itoa(gop), "-sc_threshold", "0",
		"-r", strconv.Itoa(fps),
	)
	if aout != "" {
		args = append(args, "-c:a", "aac", "-b:a", fmt.Sprintf("%dk", ab), "-ar", "44100", "-ac", "2")
	} else {
		args = append(args, "-an")
	}
	args = append(args, "-max_muxing_queue_size", "2048", "-f", "flv", urls[0])
	return args, nil
}

// audioInputs 决定音频参与混音的输入序号。
func audioInputs(job Job, srcs []StreamInfo) []int {
	if job.AudioMode == "none" {
		return nil
	}
	var idx []int
	for i, s := range srcs {
		if s.HasAudio {
			idx = append(idx, i)
		}
	}
	if len(idx) == 0 {
		return nil
	}
	if job.AudioMode == "first" || len(idx) == 1 {
		return idx[:1]
	}
	return idx
}

// DescribeArgs 返回便于阅读/排查的命令行（日志用）。
func DescribeArgs(bin string, args []string) string {
	var b strings.Builder
	b.WriteString(bin)
	for _, a := range args {
		if strings.ContainsAny(a, " \t\"'|;") {
			b.WriteString(" '" + strings.ReplaceAll(a, "'", `'\''`) + "'")
		} else {
			b.WriteString(" " + a)
		}
	}
	return b.String()
}

func mustAbs(p string) string { return absPath(p) }
