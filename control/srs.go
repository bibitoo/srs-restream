package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// SRSClient 访问 SRS 内置 HTTP API（默认 1985）。
type SRSClient struct {
	Base string
	HTTP *http.Client
}

func NewSRSClient(base string) *SRSClient {
	return &SRSClient{
		Base: strings.TrimRight(base, "/"),
		HTTP: &http.Client{Timeout: 5 * time.Second},
	}
}

type srsVideo struct {
	Codec   string `json:"codec"`
	Profile string `json:"profile"`
	Level   string `json:"level"`
	Width   int    `json:"width"`
	Height  int    `json:"height"`
}

type srsAudio struct {
	Codec      string `json:"codec"`
	SampleRate int    `json:"sample_rate"`
	Channel    int    `json:"channel"`
	Profile    string `json:"profile"`
}

type srsKbps struct {
	Recv30s int `json:"recv_30s"`
	Send30s int `json:"send_30s"`
}

type srsRawStream struct {
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	Vhost   string    `json:"vhost"`
	App     string    `json:"app"`
	Clients int       `json:"clients"`
	Kbps    srsKbps   `json:"kbps"`
	Video   *srsVideo `json:"video"`
	Audio   *srsAudio `json:"audio"`
	Publish *struct {
		Active bool   `json:"active"`
		CID    string `json:"cid"`
	} `json:"publish"`
	VideoFrames int   `json:"video_frames"`
	BytesRecv   int64 `json:"bytes"`
}

// StreamInfo 是给前端使用的流信息。
type StreamInfo struct {
	Key      string `json:"key"` // app/stream
	App      string `json:"app"`
	Stream   string `json:"stream"`
	Vhost    string `json:"vhost"`
	Active   bool   `json:"active"`
	Clients  int    `json:"clients"`
	KbpsRecv int    `json:"kbps_recv"`
	Video    string `json:"video"` // H264 1280x720
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	HasVideo bool   `json:"has_video"`
	HasAudio bool   `json:"has_audio"`
	Audio    string `json:"audio"`
	PlayFLV  string `json:"play_flv"`
	PlayHLS  string `json:"play_hls"`
	// Recording 非空表示该流正在录制（用于直播流列表显示状态、切换按钮）
	Recording *RecordBrief `json:"recording,omitempty"`
}

func (c *SRSClient) getJSON(path string, out interface{}) error {
	resp, err := c.HTTP.Get(c.Base + path)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("SRS API %s 返回 %d: %s", path, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return json.Unmarshal(body, out)
}

func (c *SRSClient) Version() (string, error) {
	var v struct {
		Code int `json:"code"`
		Data struct {
			Version string `json:"version"`
		} `json:"data"`
	}
	if err := c.getJSON("/api/v1/versions", &v); err != nil {
		return "", err
	}
	return v.Data.Version, nil
}

func (c *SRSClient) ListStreams(publicBase string) ([]StreamInfo, error) {
	var res struct {
		Code    int            `json:"code"`
		Streams []srsRawStream `json:"streams"`
	}
	if err := c.getJSON("/api/v1/streams?start=0&count=200", &res); err != nil {
		return nil, err
	}
	out := make([]StreamInfo, 0, len(res.Streams))
	pb := strings.TrimRight(publicBase, "/")
	for _, s := range res.Streams {
		si := StreamInfo{
			Key:      s.App + "/" + s.Name,
			App:      s.App,
			Stream:   s.Name,
			Vhost:    s.Vhost,
			Clients:  s.Clients,
			KbpsRecv: s.Kbps.Recv30s,
		}
		if s.Publish != nil {
			si.Active = s.Publish.Active
		}
		if s.Video != nil && s.Video.Width > 0 {
			si.HasVideo = true
			si.Width, si.Height = s.Video.Width, s.Video.Height
			si.Video = fmt.Sprintf("%s %dx%d", s.Video.Codec, s.Video.Width, s.Video.Height)
		}
		if s.Audio != nil && s.Audio.Codec != "" {
			si.HasAudio = true
			si.Audio = fmt.Sprintf("%s %dHz/%dch", s.Audio.Codec, s.Audio.SampleRate, s.Audio.Channel)
		}
		// 播放地址：与 SRS 的 http_server / http_remux / hls 配置保持一致。
		streamPath := s.App + "/" + s.Name
		if pb != "" {
			si.PlayFLV = fmt.Sprintf("%s/%s.flv", pb, streamPath)
			si.PlayHLS = fmt.Sprintf("%s/%s.m3u8", pb, streamPath)
		} else {
			si.PlayFLV = fmt.Sprintf("/%s.flv", streamPath)
			si.PlayHLS = fmt.Sprintf("/%s.m3u8", streamPath)
		}
		out = append(out, si)
	}
	return out, nil
}

// StreamMap 以 app/stream 为键返回在线且正在推流的流。
func (c *SRSClient) StreamMap(publicBase string) (map[string]StreamInfo, error) {
	list, err := c.ListStreams(publicBase)
	if err != nil {
		return nil, err
	}
	m := make(map[string]StreamInfo, len(list))
	for _, s := range list {
		if s.Active {
			m[s.Key] = s
		}
	}
	return m, nil
}
