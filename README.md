# SRS 流媒体容器（含第三方转推 / 混流 / 录像）

以 [SRS](https://github.com/ossrs/srs) 为底座的单容器方案：保留 SRS 的 RTMP / HTTP-FLV / HLS / 统计 API 能力，
额外增加了一套 **推流设置** 能力——把本地流按需转推到第三方平台，单流直接转推、多流先混流再转推，
同时支持自动录制与在线回放。

```
                     ┌──────────────────────── 容器 srs-restream ────────────────────────┐
   OBS / 手机 / SDK  │                                                                  │
   rtmp://ip:1935 ───┼──►  SRS  ──┬──► HTTP-FLV / HLS 播放（8080）                        │
                     │  (1935)   │                                                       │
                     │           └──► HTTP API（1985）──┐                                   │
                     │                               │ 查询流状态 / 推流上下线回调         │
   Web 控制台  ───────┼──► srs-control（1986）◄────────┘                                   │
   /api 接口          │      │                                                            │
                     │      ├── ffmpeg 混流编码（多路 → 1 路，写回本地 mix/<任务ID>）        │
                     │      ├── ffmpeg 中继（-c copy，每个第三方平台一个进程）              │
                     │      └── ffmpeg 录制（流 → /data/recordings/*.mp4）                  │
                     └──────────────────────────────────────────────────────────────────┘
                                          │                     │
                             rtmp://平台A/...          rtmp://平台B/...
```

## 功能

| 能力 | 说明 |
| --- | --- |
| 推流接收 | SRS 原生：`rtmp://<ip>:1935/<app>/<stream>`，支持 OBS、ffmpeg、浏览器 SDK 等 |
| 播放 | SRS 原生：HTTP-FLV（低延迟）、HLS（`/live/xxx.m3u8`）、内置播放器页 `/players/` |
| 推流地址管理 | REST API + 控制台增删改查第三方 RTMP/RTMPS 推流地址，密钥在界面上打码显示 |
| 单流转推 | 源为单路时 `-c copy` 直接转推，**不转码、几乎不占 CPU**，多个平台各起一个轻量中继 |
| 混流转推 | 源为多路时先按「布局」合成一路（可转码到指定分辨率/帧率/码率），再转推到各平台；**只编码一次**，多个平台共用 |
| 混流布局 | 上下分屏、左右分屏、宫格、画中画（方窗）、圆窗口（圆形遮罩），位置/尺寸/间距可调 |
| 录制 | 推流上线自动录制（可在设置里关闭）或手动按流名录制；分段时长可配 |
| 录像回放 | 列表 + 在线播放（支持拖动进度条）+ 下载 + 删除 |
| 自动启动 | 任务可设置「源流一上线就自动开始推送」，容器重启后配置与自动启动策略保留 |
| 状态与日志 | 每个任务、每个第三方目标都有独立状态、重连次数与 ffmpeg 日志（界面上可实时查看） |

> 单流转推与混流转推的差别只在「要不要合成」：单路走 copy 零转码，多路走 x264 合成。

## 交付形态

| 形态 | 入口 | 适用 |
| --- | --- | --- |
| 单容器（默认） | `Dockerfile` + `docker-compose.yml` | 自有服务器部署，SRS 与控制面同容器 |
| 双容器 | `jet/` 下的 `srs/` + `control/` 与 compose | 想独立升级 SRS，或与其它服务编排 |
| **捷兔桌面软件包** | `scripts/build-jet.sh` → `*.jet` | 终端用户双击安装，自带控制台与说明书 |

`.jet` 包内容见 `jet/`（jdconfig、compose、两个 Dockerfile、包内说明书），
打包后约 7.9MB（含 SRS 源码，首次安装在本机编译，约 3~5 分钟）。

## 快速开始

### 1. 构建镜像

```bash
# 1) 把本地 SRS 源码快照进构建上下文（SRS 仓库内有指向仓库外的软链接，docker 无法直接跟随）
SRS_SRC=/home/sunz/AI/srs ./scripts/prepare-srs-source.sh

# 2) 构建（首次约 2~4 分钟，主要耗时在编译 SRS）
docker build -t srs-restream:1.0 .
```

也可以直接用 Makefile：

```bash
make prepare      # 快照 SRS 源码
make build        # 构建镜像
make run          # 启动容器（标准端口）
make logs         # 查看日志
```

### 2. 运行

```bash
docker run -d --name srs-restream \
  --ulimit nofile=65535:65535 \
  -p 1935:1935 \     # RTMP 推流 / 拉流
  -p 1986:1986 \     # 本项目管理 API + Web 控制台 + 预览（已反代 HTTP-FLV/HLS）
  -p 8080:8080 \     # 可选：直接访问 SRS 的 HTTP-FLV/HLS/播放器
  -p 1985:1985 \     # 可选：SRS 原生 HTTP API
  -v $(pwd)/data:/data \
  --restart unless-stopped \
  srs-restream:1.0
```

或使用 compose：

```bash
docker compose up -d
```

打开控制台：<http://<你的IP>:1986/>

> 预览不需要额外端口：控制台会把 `/players/`、`*.flv`、`*.m3u8`、`*.ts` 反向代理到容器内的 SRS 8080，
> 所以对外只暴露 `1935`（推流）和 `1986`（控制台）即可。
>
> **如果宿主机端口被占用、做了端口映射（例如 `-p 19351:1935`），务必设置 `PUBLIC_RTMP=rtmp://<你的IP>:19351/`**
> （或到设置页填「对外推流地址」）。控制台里的推流地址提示、ffmpeg 复制命令都取自它，否则用户会按 1935 推流而落到别的服务上。

### 3. 推一路流试试

```bash
ffmpeg -re -f lavfi -i testsrc2=size=1280x720:rate=25 -f lavfi -i sine=frequency=440 \
  -c:v libx264 -preset veryfast -g 50 -c:a aac -f flv rtmp://127.0.0.1:1935/live/camera01
```

在控制台「直播流」页即可看到 `live/camera01`，可预览、录制、一键转推。
推流上线后会自动录制（默认开启），录像出现在「录像库」。

## 控制台说明

- **直播流**：列出 SRS 上的所有流（含混流产生的 `mix/*`），显示分辨率/编码/码率/观看数；
  每行可「预览」（**在当前页弹窗内嵌播放器**，直接看到画面，另附「复制 FLV/HLS 地址」）、「转推」（带入源流新建任务）。
  - **「录制」列**显示该流当前是否在录制、已录时长；按钮会随状态切换成「开始录制 / 停止录制」。
  - 顶部有「自动录制：开/关」标识（在设置里可改）。**默认开着自动录制，所以推流一上线就已经在录了**，
    这时点「录制」是幂等的（不会重复建文件），页面现在会明确提示"该流已在录制中"。
- **转推任务**：卡片式展示每个任务的状态、源流、布局、各平台推送状态（运行中/重连中/已停止）、重连次数；
  支持启动/停止/重启、查看实时日志、预览混流结果（同样内嵌弹窗）、编辑、删除。
- **推流目标**：管理第三方平台推流地址（名称、地址、备注、启用状态）。地址形如
  `rtmp://live-push.bilivideo.com/live-bvc/xxxx?key=yyyy`。
- **录像库**：录像卡片直接内嵌 `<video>` 播放/下载，显示流名、开始时间、时长、大小。
- **设置**：自动录制开关、录像分段时长、对外访问地址（生成播放链接用）、SRS 播放服务地址（反代目标）。

### 预览说明

预览走 SRS 自带的 H5 播放器页（mpegts.js 播 HTTP-FLV）。控制台生成的链接形如：

```
http://<你的IP>:1986/players/srs_player.html?autostart=true&schema=http&app=live&stream=camera01.flv
```

- 必须带 `autostart=true`，否则 SRS 播放器页只会显示一个空 URL 输入框（SRS 的默认行为）。
- `stream` 必须带 `.flv` 后缀。
- 页面会自动静音以符合浏览器自动播放策略，点开播放器音量即可听到声音。

### 混流布局

| 布局 | key | 效果 |
| --- | --- | --- |
| 单画面 | `single` | 只用第一路铺满，可顺便把源统一成固定分辨率 |
| 上下分屏 | `vstack` | 多路竖向等分（主持人 + 画面） |
| 左右分屏 | `hstack` | 多路横向等分（双人访谈） |
| 宫格 | `grid` | 按 2×2 / 3×3 自动排布，间距可调 |
| 画中画 | `pip` | 第一路铺满，其余为方窗小画面叠在角落 |
| 圆窗口 | `circle` | 第一路铺满，其余为圆形窗口（类似主播头像圈），位置/直径可调 |

画面统一按「等比缩放 + 黑边补齐」放进各自窗口；圆窗口用超采样遮罩做抗锯齿，边缘不会有毛刺。

## HTTP API

基础地址 `http://<ip>:1986`。若设置了环境变量 `AUTH_TOKEN`，需带 `X-Auth-Token` 头或 `?token=` 参数。

### 概览与流

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/api/health` | 健康检查 |
| GET | `/api/overview` | 版本、在线流数、任务数、录像数 |
| GET | `/api/streams` | 流列表（含播放地址、分辨率、音频信息、**是否正在录制 `recording`**） |
| GET | `/api/layouts` | 支持的混流布局 |

### 推流目标

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/api/targets` | 列表 |
| POST | `/api/targets` | 新增 `{"name","url","enabled","remark"}` |
| GET/PUT/DELETE | `/api/targets/{id}` | 查询/修改/删除 |

### 转推任务

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/api/jobs` | 列表（含运行时状态） |
| POST | `/api/jobs` | 新建任务 |
| GET/PUT/DELETE | `/api/jobs/{id}` | 查询/修改/删除 |
| POST | `/api/jobs/{id}/start` | 启动（源流不在线则进入「等待源流」） |
| POST | `/api/jobs/{id}/stop` | 停止 |
| POST | `/api/jobs/{id}/restart` | 重启 |
| GET | `/api/jobs/{id}/log?lines=200` | 日志与运行时状态 |

新建任务示例：

```bash
# 单流转推：camera01 → B站
curl -X POST http://127.0.0.1:1986/api/jobs -H 'Content-Type: application/json' -d '{
  "name": "主会场推B站",
  "mode": "direct",
  "sources": ["live/camera01"],
  "extra_urls": ["rtmp://live-push.bilivideo.com/live-bvc/xxxx?key=yyyy"],
  "auto_start": true,
  "enabled": true
}'

# 混流转推：camera01 铺满 + camera02 圆形小窗 → 1920x1080 推到两个平台
curl -X POST http://127.0.0.1:1986/api/jobs -H 'Content-Type: application/json' -d '{
  "name": "双机位圆窗混流",
  "mode": "mix",
  "sources": ["live/camera01", "live/camera02"],
  "layout": "circle",
  "width": 1920, "height": 1080, "fps": 25, "video_bitrate": 4000, "audio_bitrate": 128,
  "audio_mode": "mix",
  "pip_size": 28, "pip_pos": "topright", "gap": 12, "background": "0x101014",
  "target_ids": ["tg_xxx"],
  "extra_urls": ["rtmp://a.example.com/live/key1"],
  "auto_start": true,
  "enabled": true
}'
```

任务字段说明：

| 字段 | 说明 |
| --- | --- |
| `mode` | `direct`（单流 copy 转推）/ `mix`（多流混流后转推） |
| `sources` | 源流列表，格式 `app/stream`，如 `live/camera01` |
| `layout` | `single`/`vstack`/`hstack`/`grid`/`pip`/`circle` |
| `width`/`height`/`fps` | 混流输出规格（`direct` 模式忽略） |
| `video_bitrate`/`audio_bitrate` | 码率 kbps |
| `audio_mode` | `mix`（多路混音）/`first`（只用第一路）/`none` |
| `pip_size`/`pip_pos` | 画中画/圆窗尺寸（占宽度百分比）与位置 |
| `gap`/`background` | 宫格间距、填充底色 |
| `target_ids`/`extra_urls` | 引用已保存的推流目标 / 直接填写的临时地址 |

### 录像

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/api/recordings` | 列表（含时长、大小、是否录制中） |
| POST | `/api/recordings` | 开始录制 `{"stream":"live/camera01"}` |
| POST | `/api/recordings/{id}/stop` | 停止录制（停止后自动转封装） |
| POST | `/api/recordings/{id}/repair` | 重新封装：修复索引/时长，让浏览器能完整播放与拖动 |
| POST | `/api/recordings/{id}/hls` | 生成 HLS 切片（`/hls/<id>/index.m3u8`），供手机/微信内置浏览器播放 |
| GET/DELETE | `/api/recordings/{id}` | 详情/删除 |
| GET | `/media/{file}` | 播放（支持 Range）/ `?download=1` 下载 |

### 诊断

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/api/diag` | 推流/播放地址、在线流、录像健康、**最近 60 条访问日志**（含 Range、实际发送字节、UA、是否中断） |

访问日志同时落盘在 `/data/logs/access.log`。排查"播放只播一点""预览黑屏"时，先看这里：
如果日志显示服务器已经把整个文件发给客户端（`sent` 接近文件大小），那就是客户端/浏览器的问题；
如果 `aborted=true` 或 `sent` 很小，则是连接被中断。

### 设置

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET/PUT | `/api/settings` | `auto_record`、`record_segment_minutes`、`public_base`、`srs_api`、`srs_http`、`rtmp_base` |

### SRS 回调（内部）

SRS 的 `http_hooks` 会在推流上线/下线时回调 `/hooks/on_publish`、`/hooks/on_unpublish`，
用于触发自动录制与自动转推；回调失败会被控制面兜底轮询补上。

## 目录与数据

| 容器路径 | 说明 |
| --- | --- |
| `/data/config.json` | 推流目标、任务、设置的持久化配置 |
| `/data/recordings/` | 录像 `*.mp4` 与同名 `*.json` 元数据 |
| `/data/logs/job-<id>.log` | 每个任务（编码器 + 各中继）的运行日志 |
| `/data/assets/circle_mask.png` | 自动生成的圆窗口遮罩 |
| `/usr/local/srs` | SRS 安装目录（`objs/srs`、`conf/`、`objs/nginx/html` 播放器） |

录像写入时使用分片 MP4（断电/强制停止也不丢），停止后自动转封装成 `faststart` 标准 MP4，浏览器拖动进度条无需等待。

## 容器网络与推流目标地址

容器有独立的网络命名空间，**容器里的 `127.0.0.1` 是容器自己，不是宿主机**。因此：

| 想推到哪里 | 正确写法 | 错误写法 |
| --- | --- | --- |
| 宿主机上另一个 SRS / 平台（如 `srs6` 容器映射的 1935） | `rtmp://<宿主机IP>:1935/live/xxx`（如 `rtmp://192.168.0.14:1935/live/xxx`） | ❌ `rtmp://127.0.0.1:1935/live/xxx` |
| 同上，但不想写死 IP | `rtmp://host.docker.internal:1935/live/xxx`（容器需 `--add-host=host.docker.internal:host-gateway`，本项目 `make run` / compose 已带） | |
| 本容器自己的 SRS 的另一路（很少用） | `rtmp://127.0.0.1:1935/<app>/<另一个流名>` | ❌ 流名与源相同时会形成自环 |

把 `127.0.0.1` 当宿主机地址是最常见的坑：看起来"连上了"，其实是推回自己，
SRS 会因流名重复立即断开，界面上表现为**一直"重连中"**（ffmpeg 日志是 `Input/output error`）。

本项目已做防护：

- 创建/修改任务时**检测自环**（回环地址 + 端口匹配本容器 SRS + 流名与源相同）→ 直接报错并给出可用地址；
- 目标卡片上直接显示 ffmpeg 的真实报错（如"连接失败：127.0.0.1 在容器内指向容器自己…"）；
- 新增推流地址时会提示不要用 `127.0.0.1`。

## 常见问题

**预览是黑屏 / 看不到画面**

1. 先看「直播流」页该流的状态是否为「推流中」——没有推流自然是黑屏。
2. 看「视频」列的编码：如果是 **H.265/HEVC**，浏览器 HTTP-FLV 基本播不了（弹窗里也会提示），
   请把推流端改回 H.264，或用 VLC/PotPlayer 打开弹窗里的 FLV 地址。
3. 点弹窗里的「新窗口打开」，看播放器页里有什么报错（例如 404 说明流名/端口不对）。
4. 确认控制台能访问到 SRS 播放服务：设置页里的「SRS 播放服务地址」默认 `http://127.0.0.1:8080`，
   在容器内应始终可访问；若你改过它，改回默认值。

**录像"播放只能播一点点" / 拖不动进度条**

录制时为了让断电、强杀也不丢数据，写的是**分片 MP4（fragmented MP4）**：边写边可用，但索引不完整、时长未知。
浏览器打开这类文件通常只播到已写入的位置就停住——这不是录像坏了，而是还没"收尾"。本项目的处理：

- **录制中**：录像库不再提供播放器，只显示「正在录制 · 已录 N 分钟 · 大小」，避免被误判成坏文件；
- **正常停止**（推流下线 / 点停止）：自动转封装成带完整索引的 `faststart` MP4，可正常显示时长、拖动进度条；
- **异常中断**（容器被强杀、进程崩溃）：下次启动时自动扫描并重新封装（>2GB 的大文件跳过，避免占磁盘）；
- **历史文件**：录像卡片上点「修复并封装」可手动重建索引，几秒钟即可。

若某个卡片带「未收尾」徽标，说明它还没被修复，点一下「修复并封装」即可。

**推流已经停了，录像还在"录制中"**

理论上推流下线会触发 SRS 的 `on_unpublish` 回调让录制收尾；如果回调丢失（SRS 重启、网络抖动），
控制面还有一层兜底：**每 10 秒巡检一次**，发现"流已下线但还在录制"就自动停止并转封装。
早期的版本这里有个 bug——回调里同步执行了耗时操作导致 SRS hook 超时，已修复。

**播放一会儿就断（约 2~5 秒）**

控制台每 5 秒自动刷新一次数据。早期版本每次刷新都用 `innerHTML` 全量重建列表 DOM，
正在播放的 `<video>` 元素会被销毁重建 —— 表现就是"刚播几秒就断、进度回到 0"。
现已改为**内容指纹 + 播放保护**：

- 数据没变化时完全不重建 DOM；
- 有视频正在播放时，即使数据变化也跳过重绘（右上角会显示「播放中（已暂停自动刷新）」）；
- 播放/拖动后 15 秒内也不重绘；
- 设置页在输入框获得焦点时不重建，避免输入被冲掉。

回归测试：`node scripts/test-ui-refresh.js`（无需浏览器，9 项断言）。

**录像"手机/微信里播不动，电脑上正常"**

手机内置浏览器（尤其微信 X5）对上百 MB 的整段 MP4 支持很差。点录像卡片上的「手机播放」，
会生成 HLS 切片（`-c copy`，不转码）并用播放器播放，兼容性好得多；生成的切片在 `/data/hls/<id>/`，可随时删除。

**录像库是空的 / 之前的录像不见了**

录像存在容器的 `/data/recordings`。用命名卷或宿主机目录挂载时，`docker rm` + `docker run` 不会丢；
**只有删除数据卷（`docker volume rm`）或删除宿主机挂载目录才会丢**。

## 运维提示

- **文件句柄**：SRS 要求 `connections*1.128+128 <= nofile`。入口脚本会自动按实际 `nofile` 下调
  `max_connections`，但仍建议 `--ulimit nofile=65535:65535` 以支持更多连接。
- **CPU**：`direct` 模式几乎零转码开销；`mix` 模式每路任务只做一次 x264 编码（`veryfast` + `zerolatency`）。
  1280×720@25fps 双路混流约占 1~1.5 核，可按需提高 `video_bitrate` 与分辨率。
- **网络**：容器需要能出网访问第三方推流服务器（TCP 1935 或 443 for rtmps）。
- **端口占用**：若宿主机已有 SRS，请改映射端口，如 `-p 19350:1935 -p 18080:8080 -p 19850:1985 -p 19860:1986`。
- **WebRTC**：镜像默认关闭 RTC/SRT 以减小体积。需要时在 Dockerfile 中改
  `SRS_CONFIGURE_ARGS` 为 `--rtc=on --ffmpeg-fit=on --srt=on`（构建时间会明显变长），并在 `conf/srs.conf` 增加 `rtc_server` 段。
- **鉴权**：控制面默认不鉴权（内网使用）。对外暴露时请设置 `AUTH_TOKEN`，或在前面加一层反向代理做认证。

## 自测

```bash
# 在容器内跑（会自己推流、建任务、录制、校验播放，结束后清理）
docker exec srs-restream bash /tmp/smoke-test.sh
```

覆盖：服务健康 → 两路推流 → 单流 copy 转推（校验原分辨率）→ 圆窗口混流（校验输出规格）
→ 录制与 Range 播放 → 源流下线后任务不崩溃。

前端刷新逻辑（防止自动刷新打断播放/输入）有独立的无需浏览器测试：

```bash
node scripts/test-ui-refresh.js    # 或 make test-ui
```

## 与 SRS 的关系

本项目的改动全部在 **SRS 之外**：SRS 源码只做编译，`conf/srs.conf` 增加了 `http_hooks`
（推流上下线回调）、HLS 参数与秒开优化。转推、混流、录制、播放列表等逻辑都在 `control/`（Go 控制面）里，
通过 SRS 的 HTTP API 与 RTMP 接口协作，因此升级 SRS 版本不会影响业务代码。

## 源码结构

```
.
├── Dockerfile                 # 三段构建：SRS、控制面、运行镜像
├── docker-compose.yml
├── Makefile
├── docker/
│   ├── srs.conf               # SRS 运行配置（容器内使用）
│   └── entrypoint.sh          # 入口：拉起 SRS + 控制面，优雅退出
├── control/                   # Go 控制面（无第三方依赖，静态编译）
│   ├── main.go                # 启动、路由、圆窗遮罩生成
│   ├── api.go                 # REST 接口
│   ├── store.go               # 推流目标/任务/设置的持久化
│   ├── srs.go                 # SRS HTTP API 客户端
│   ├── ffmpeg.go              # 转推/混流的 ffmpeg 参数与布局算法
│   ├── jobs.go                # 任务调度：编码器 + 每目标中继 + 重连
│   ├── record.go              # 录制、收尾转封装、录像列表
│   └── web/index.html         # 单页控制台（内嵌进二进制）
├── jet/                       # 捷兔桌面 .jet 包模板（双容器编排）
│   ├── jdconfig               # 清单：type=compose，endpointPort=1986
│   ├── docker-compose.yml     # srs + control 两个服务
│   ├── srs/                   # SRS 引擎镜像（Dockerfile + srs.conf + 源码由脚本注入）
│   ├── control/               # 控制面镜像（Dockerfile + entrypoint）
│   └── help/index.md          # 包内使用说明书
├── docs/
│   └── trail-app-settings.md  # 捷兔行迹 App 推流设置项（本次改造说明）
├── scripts/
│   ├── prepare-srs-source.sh  # 把 SRS 源码快照为可构建的自包含副本
│   ├── build-jet.sh           # 组装并打包 .jet
│   ├── smoke-test.sh          # 端到端自测
│   └── test-ui-refresh.js     # 前端刷新逻辑测试
└── build/srs-src/             # SRS 源码快照（由脚本生成）
```
