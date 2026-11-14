# 捷兔行迹 App · 推流地址发现与登录态

> 原则：**发现只做辅助**。候选仅用于填充与提示，用户随时可改成任何地址；
> 原设置项一个没删。注销则相反——**三处必须一起退**。

## 一、候选的三个来源

点「设置 → 推流与视频规格 → RTMP 地址」：

```
手动输入新地址…                          ▼
📡 捷兔流媒体服务 · 192.168.0.14          ← 局域网扫描 /api/discover
☁️ 捷兔流媒体服务 · 192.168.0.14          ← 捷兔社区 GET /vapi/agent/apps
✅ rtmp://192.168.0.14:19351/live/stream  ← 推流成功过（历史）
```

| 来源 | 实现 | 凭证 | 缓存 |
| --- | --- | --- | --- |
| 📡 局域网 | `StreamServiceDiscovery` 扫 /24 网段的 `1986/api/discover`（24 并发、350ms 超时） | 无 | 24 小时 |
| ☁️ 社区 | `CommunityAppsDiscovery` 调 `GET {社区}/vapi/agent/apps?applicationId=jetool-trail`，再反查每个后端的 `/api/discover` 补 RTMP 端口 | **A 仓**社区 JWT | 10 分钟 |
| ✅ 历史 | `StreamUrlHistory`（原有，仅连接成功时记录） | 无 | 12 条/协议 |

设置列表页副标题实时显示：`RTMP直播推流地址 ｜ 局域网 1 个 / 社区 1 个，点击选择`

## 二、自动填入策略

```java
shouldAutoFill() = 值为空  ||  偏好里从未保存过这个 key
```

| 情况 | 行为 |
| --- | --- |
| 从没设置过（仅 XML 默认值） | 自动填入第一个发现的服务（Toast 提示可改） |
| 值为空 | 自动填入 |
| 用户保存过（哪怕与默认值相同） | **只进候选，不覆盖** |

## 三、登录态：一次 SSO，两仓分存

```
A 仓（社区）  JetDeskSharedSession.read()
              content://cn.jetool.commune.jetdesk.session/session
              → 社区 JWT + baseUrl
              用途：/vapi/agent/apps 等社区接口

B 仓（行迹）  AuthStore / jietu_auth
              → 行迹 access/refresh token
              用途：行迹自己的后台（/api/v1/tracks…）
```

用户在捷兔社区 App 登录一次即可，两边**各存各的、各取各的**：社区 token 不能打行迹后台，反之亦然。

## 四、注销：三处齐退（`LogoutHelper.logoutAll`）

| 身份 | 存放 | 动作 |
| --- | --- | --- |
| 行迹令牌（B 仓） | `AuthStore` | `AuthStore.clear()` |
| 社区会话（A 仓） | jetdesk-discovery 组件 | `JetDeskLink.logout()`（同时停轮询） |
| 统一账号 SSO | Casdoor 会话 cookie | `GET {ssoBase}/api/logout` |

执行顺序（顺序错了服务端会话就退不掉）：

```java
String ssoBase   = AuthStore.ssoBase(app);    // ① 先按值取出
String ssoCookie = AuthStore.ssoCookie(app);  //    （clear() 会连带清掉）
AuthStore.clear(app);                         // ② 清行迹本地
JetDeskLink.logout();                         // ③ 退社区会话
new Thread(() -> SsoClient.logoutSso(ssoBase, ssoCookie)).start();  // ④ SSO /api/logout
StreamServiceDiscovery.clear(app);            // ⑤ 清发现缓存
CommunityAppsDiscovery.clear(app);
```

所有注销入口都走它：账号页、轨迹列表页（后者原先只清了本地，漏掉组件与 SSO）。

## 五、applicationId 必须逐字一致

```
jdconfig.applicationIds = ["jetool-trail"]
JetDeskLink.APPLICATION_ID = "jetool-trail"     ← 组件常量
CommunityAppsDiscovery.APPLICATION_ID = JetDeskLink.APPLICATION_ID
```

差一个字符（如写成 `jetooltrail`）不会报错，只是**永远发现不到**。

## 六、改动文件

```
新增  jietu/LogoutHelper.java                三处齐退的统一入口
新增  stream/StreamServiceDiscovery.java     局域网扫描 + 缓存
新增  stream/CommunityAppsDiscovery.java     社区发现（A 仓凭证）
修改  stream/StreamUrlPreference.java        三类候选 / 自动填策略 / 双来源 summary
修改  jietu/JetDeskSharedSession.java        Session 增加 baseUrl
修改  jietu/SettingsFragment.java            注销改调 LogoutHelper
修改  ui/TrackListActivity.java              注销补上组件与 SSO（原来漏了）
修改  res/values(-zh)/strings_stream.xml     新增候选与状态文案
未动  preferences_streaming.xml / TTS 全套 / StreamUrlHistory / 推流管线
```

## 七、排障

```bash
adb logcat -s CommunityApps JetoolTrailLink
```

| 日志 | 含义 |
| --- | --- |
| `无社区登录态，跳过社区发现` | 社区 App 未装 / 未登录 / 签名不同 |
| `parse failed` | 社区返回结构变化 |
| `fetch failed` | 网络或社区基址问题 |

服务端自检：浏览器打开 `http://<后端IP>:1986/api/discover`，能看到 `"service":"jetstream"` 即可。


## 附：jdconfig `models.services.*` 必须显式声明 `models`

实体机安装时如果报：

```
models.services.control 没有声明models
```

说明 `models.services.<服务名>` 里缺 `models` 键。**即使本软件不需要任何大模型能力，
也要写空数组**，否则客户端校验不通过：

```json
"models": {
  "requires": [],
  "delivery": "http",
  "services": {
    "control": { "models": [], "configPath": "/jetdesk/config", "configPort": 1986, "configToken": "jetool" }
  }
}
```

`models: []` 的语义是「这个服务不需要任何模型能力」，但 `configPath` 仍然有效 ——
桌面照样会把 `instanceId` / `communityBaseUrl` / 密钥等基础配置 POST 到 `/jetdesk/config`。
