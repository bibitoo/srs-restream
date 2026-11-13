# 捷兔行迹 App · 推流设置项

> 对应改动：设置首页把**推流设置**与**语音播报（TTS / CosyVoice）**拆成两个分组。
> 推流设置页不再包含任何 CosyVoice 相关项。

## 一、设置首页分组

`app/src/main/res/xml/preferences_headers.xml`

| 顺序 | 分组 | Fragment type | 说明 |
| --- | --- | --- | --- |
| 1 | 账号与云端 | `account` | 登录、云端轨迹 |
| 2 | 设备管理 | （Activity） | 蓝牙传感器、相机信息 |
| 3 | **推流与视频规格** | `streaming` | ← **仅推流**，本次清理的范围 |
| 4 | **语音播报** | `tts` | ← 新增分组，CosyVoice 三项迁到这里 |
| 5 | 轨迹与码表 | `track` | 体重、3D 导出 |
| 6 | 运动记录模式 | （弹窗） | 记录模式 |
| 7 | 通用 | `general` | 其它 |

## 二、推流设置项清单（`preferences_streaming.xml`）

| 设置项 | key | 控件 | 默认值 | 说明 |
| --- | --- | --- | --- | --- |
| 推流协议 | `streamingProtocolPref` | ListPreference | `0`（RTMP） | RTMP / SRT / WebRTC |
| 清晰度 | `streamingQualityPref` | ListPreference | `1080p` | 480p/720p/1080p/4K，同时决定分辨率与码率；改完推流页状态条会跟着变 |
| RTMP 地址 | `streamingUrlRtmpPref` | `StreamUrlPreference` | `rtmp://192.168.0.14/live/stream` | 完整推流地址 |
| SRT 地址 | `streamingUrlSrtPref` | `StreamUrlPreference` | `srt://` | 协议选 SRT 时使用 |
| WebRTC 地址 | `streamingUrlWebrtcPref` | `StreamUrlPreference` | `https://` | WHIP 推流地址 |
| 视频稳定 | `videoStabilizationPref` | CheckBox | `false` | 电子防抖 |
| 推流时本地保存 | `saveLocallyWhileStreamingPref` | CheckBox | `false` | 边推边存本地 |
| 语音控制 | `streamingVoiceControl` | CheckBox | `true` | 语音指令开关 |
| 人脸模糊 | `streaming_face_blur` | CheckBox | `false` | 推流时对人脸打码 |

> 键名与旧工程一致，历史配置可直接复用。
> 绑定逻辑：`SettingsFragment#bindStreaming()`。

### 对接「捷兔流媒体服务」时的推荐值

| 场景 | 推荐配置 |
| --- | --- |
| 同机（手机与流媒体服务在同一台电脑/局域网） | RTMP 地址填 `rtmp://<电脑IP>:1935/live/行迹01` |
| 走局域网 | 手机与电脑同一 Wi-Fi，IP 用电脑的局域网地址（如 `192.168.1.10`） |
| 编码要求 | **H.264**（服务端浏览器预览不支持 H.265/HEVC） |
| 清晰度 | 1080p（服务端默认转推/混流按 1280×720 或 1920×1080 输出，可在服务端调） |

推流地址在流媒体服务的控制台「直播流」页顶部可直接复制，端口若改过以那里显示的为准。

## 三、语音播报设置项（`preferences_tts.xml`，本次从推流页迁出）

| 设置项 | key | 控件 | 存储 | 说明 |
| --- | --- | --- | --- | --- |
| 播报文稿 | `ttsScriptPref` | Preference | — | 打开 `TtsScriptActivity`（文稿/引擎/音色） |
| 启用语音播报 | `ttsBroadcastEnabled` | CheckBox | `tts_broadcast` 偏好文件的 `tts_broadcast` 键（`persistent=false`） | 播报混入直播流；开启后推流强制走自建（遮罩）管线 |
| 语音服务地址 | `cosyUrl` | EditText | 同上（`persistent=false`） | CosyVoice 服务地址 |

绑定逻辑：`SettingsFragment#bindTts()`，只在 `type=tts` 时调用。

## 四、改动的文件

```
app/src/main/res/xml/preferences_streaming.xml   移除 3 个 TTS 项，只留推流
app/src/main/res/xml/preferences_tts.xml         新增：TTS 三项
app/src/main/res/xml/preferences_headers.xml     新增「语音播报」分组（type=tts）
app/src/main/java/.../SettingsFragment.java      新增 TYPE_TTS 分支；bindStreaming 不再调 bindTts
app/src/main/res/values/strings_settings.xml     新增 settings_header_tts / _summary
app/src/main/res/values/values-zh/strings_settings.xml  同上（中文）
app/src/main/res/values/strings_tts.xml          新增 tts_pref_category_title
app/src/main/res/values-zh/strings_tts.xml       同上（中文）
```

## 五、验证要点

1. 设置首页出现「语音播报」分组，点进去只有 3 项，且能正常读写（开关状态、地址回显）；
2. 「推流与视频规格」分组里**不再出现**播报文稿 / 启用语音播报 / 语音服务地址；
3. 推流页的「语音播报」入口按钮行为不变（仍读 `TtsBroadcaster`）；
4. 已有用户的配置不受影响：三项 key 未变，`tts_broadcast` 偏好文件照旧。
