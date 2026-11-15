# .jet 端口 / 地址契约

> 适用：捷兔桌面（JetDesk）Docker Compose 类软件包
> 版本：v1（2026-11-15），首个落地实现：捷兔流媒体服务 1.4.0

## 一、要解决的问题

`jdconfig` 过去只有一个 `endpointPort`，包里所有"对外地址"都是写死的：

```jsonc
// 老写法：宿主端口写死在 compose 的 environment 里
environment:
  PUBLIC_RTMP: rtmp://127.0.0.1:${SRS_RTMP_PORT:-1935}/
```

真机上的连锁反应（2026-11-15 实测）：

1. 本机 `1935` 已被另一个进程（`media.exe`）占用；
2. 捷兔桌面做端口冲突自动分配，把 SRS 的**宿主端口**改成 `1936`，桌面与社区都按 `1936` 上报；
3. 容器对此一无所知，控制面 `/api/discover` 仍按 `127.0.0.1:1935` 返回 —— 手机照着推到 `1935`，也就是 `media.exe`；
4. SRS 控制台看不到这路流；
5. 手动把控制面的 `rtmp_public` 改成 `1936` 才恢复，**容器一重建又回到 1935**。

根因不是某个包写错，而是契约缺失：包内没有区分"容器内端口"和"宿主端口"，
而"宿主端口"这件事**只有桌面知道**。Windows 上还多一层端口转发（宿主端口 → WSL → 容器），
容器更不可能自己推断出来。

## 二、契约三条规则

| 规则 | 说明 |
| --- | --- |
| 包里只声明**容器内端口** | `containerPort` 是包自己的事，永远不变，与宿主无关 |
| **宿主端口由桌面分配**，并必须回告容器 | 桌面负责选一个可用端口，然后通过通道（见第四节）告诉容器 |
| 对外地址用「**请求方看到的 Host + 解析出的宿主端口**」拼 | 绝不在包里写死 `127.0.0.1` 或某个宿主端口 |

判据：如果把 compose 里所有宿主端口整体平移 1000（1935→2935、1986→2986），
包的功能和它对外报出的地址都应该**自动跟着变**，不需要改包、不需要人改设置。

## 三、jdconfig 新增 `endpoints[]`（可选，老包不写保持原行为）

```json
"endpoints": [
  {
    "name": "http",
    "service": "control",
    "protocol": "http",
    "containerPort": 1986,
    "primary": true,
    "path": "/",
    "keyQuery": "key"
  },
  {
    "name": "rtmp",
    "service": "srs",
    "protocol": "rtmp",
    "containerPort": 1935,
    "path": "/live/",
    "publishPath": "/live/stream",
    "keyQuery": "key"
  }
]
```

| 字段 | 必填 | 说明 |
| --- | --- | --- |
| `name` | 是 | 稳定标识（建议小写），用于环境变量名与上报，例如 `rtmp` → `JETDESK_ENDPOINT_RTMP_PORT` |
| `service` | 否 | compose 服务名；不写按主服务处理 |
| `protocol` | 是 | `http` / `https` / `rtmp` / `rtmps` / `tcp` / `udp` |
| `containerPort` | 是 | **容器内**端口 |
| `primary` | 否 | 首页主入口，**最多一个**；不写时取第一个 http 端点 |
| `path` | 否 | URL 路径前缀，如 `/`、`/live/` |
| `publishPath` | 否 | 推流等完整路径，如 `/live/stream`，用于给出"可直接用"的推流地址 |
| `keyQuery` | **需要口令时必填** | 该端点携带访问口令的 query 参数名（本服务是 `key`）。桌面按它把口令拼进入口 URL |

### 为什么 `keyQuery` 必须配

桌面打开首页入口时，会按 `keyQuery` 把访问口令拼进 URL，例如
`http://192.168.1.10:1986/?key=jetool`。不配这个字段，桌面就不会拼口令 ——
而控制台是受口令保护的，于是前端一进去就被 401 挡住（表现：白屏 / 一直弹输入框）。

两边必须对齐：

1. 声明里写 `keyQuery: "key"` ↔ 服务端认 `?key=`（`accessKeyOf` 里的 query 取值）；
2. 服务端前端要**首屏从 URL 读口令**并立刻从地址栏移除（否则口令留在浏览历史里）；
3. 推流端点同样配 `keyQuery`，拼出来的推流地址是 `rtmp://host:1936/live/stream?key=<推流口令>`
   ——注意推流用的是 `publish_key`，不是控制台的 `access_key`，两者刻意分开。

兼容性：老桌面不认 `endpoints` 时按未知字段静默忽略，仍走 `endpointPort` + 容器内端口，
行为与升级前一致（代价是宿主端口被改号后仍会报错 —— 这是旧契约的固有缺陷）。

`endpointPort` 继续保留，语义不变（首页入口的容器内端口），保证老桌面可用。

## 四、桌面 → 容器：宿主端口回告通道

容器接受以下通道，优先级从高到低：

| 通道 | 形式 | 说明 |
| --- | --- | --- |
| A（推荐） | `POST /jetdesk/config` body: `{"endpoints":[{"name":"rtmp","hostPort":1936}]}` | 与现有的"推配置"同一个通道，桌面本来就每 2 秒重推一次，天然幂等、能随时改口 |
| B | 环境变量 `JETDESK_PORT_<containerPort>`，如 `JETDESK_PORT_1935=1936` | 适合在 compose 里显式传递 |
| C | 环境变量 `JETDESK_ENDPOINT_<NAME>_PORT`，如 `JETDESK_ENDPOINT_RTMP_PORT=1936` | 按端点名传，端点名可读性更好 |
| D | 环境变量 `JETDESK_ENDPOINTS='[{"name":"rtmp","containerPort":1935,"hostPort":1936}]'` | 一次传整套声明 + 端口 |
| E | 历史变量 `SRS_RTMP_PORT` / `UI_PORT` | 仅在**显式设置**时采信；compose 里的 `:-1935` 缺省值不算"告知" |
| F | 请求 Host 里的端口 | 仅主入口 http 端点：桌面打开界面时一定带上真实宿主端口 |
| G | 都没有 | 回退容器内端口，并在 `/api/diag` 标 `portSource=container-port` |

推送里还可以带 `host`（对外主机名/IP），用于桌面知道更好的地址时覆盖请求 Host。

**报文形状（2026-11-15 起固定）**：

```json
{
  "self": "control",
  "accessKey": "<当前 access_key>",
  "instanceId": "d-xxx:service-id",
  "endpoints": [
    {"name":"http","service":"control","protocol":"http","containerPort":1986,"hostPort":2986},
    {"name":"rtmp","service":"srs","protocol":"rtmp","containerPort":1935,"hostPort":19366}
  ]
}
```

- `self` 是 **compose 服务名 / 实例 key**，不是 `instanceId`；
- `endpoints` **只放端口事实**，不放 `url`/`path`：容器用 `hostPort` + 自己 jdconfig 的
  `path`/`publishPath`/`keyQuery` 拼地址（谁声明谁拼，避免两套地址打架）；
- 换口令那一轮：**头部是旧口令、报文是新口令**；容器只从头部取口令、只从报文取新值；
- 容器回报时 `endpoints` 也只给端口事实，地址放在 `resolved` 里供排查。

**鉴权硬规则（口令头只留一个）**：

| 用途 | 唯一载体 |
| --- | --- |
| 用户访问密钥 | `X-Access-Key`（**唯一口径**；`X-API-Key`/`X-JetDesk-Key`/`X-Auth-Token` 已删除，无过渡） |
| 无头客户端 | `?key=` / `Cookie: jsk=` |
| 标准 API 客户端 | `Authorization: Bearer <access_key>`，**只在 `/api/*`** |
| configToken（公开常量） | `Authorization: Bearer <configToken>`，**只在 `<configPath>`** |
| 推流口令 | RTMP URL 的 `?key=<publish_key>` |

`<configPath>` 上**必须先看 `X-Access-Key`**：那里的 `Authorization` 是公开的 configToken，
拿它比用户口令等于让公开常量持有改配置的权限。401 文案必须含「访问密钥」。

配置推送的放行口**只有两个**（无过渡后门）：

1. 服务本身没配口令（`access_key` 为空）；
2. `X-Access-Key` == 当前生效口令。

已删除：出厂弱口令 `jetool` 无条件放行、报文自证（body 里带口令）、
`Authorization` 等值口令放行。`JETDESK_CONFIG_STRICT=0` 是运维排障开关，不是兼容通道。

| 变量 | 作用域 | 说明 |
| --- | --- | --- |
| `JETDESK_ENDPOINT_<NAME>_PORT/_HOST/_CONTAINER_PORT/_BASE/_URL` | 端口所属服务 | 端点级事实。只有 HTTP(S) 的 `_URL` 带 `?key=<access_key>`；RTMP/RTMPS 的 `_URL` 不带 key |
| `JETDESK_PORT_<容器端口>` | 所有目标服务 | 控制面要同时知道 1986 与 1935 的宿主端口 |
| `JETDESK_ACCESS_KEY` / `JETDESK_LAN_IP` / `JETDESK_COMMUNITY_BASE` / `JETDESK_INSTANCE_ID` | 所有目标服务 | `ACCESS_KEY` 只当首次启动初始值，持久化值优先 |

compose 里必须**显式引用**这些变量（`VAR: "${VAR:-}"`），否则宿主环境的值传不进容器；
ports 必须显式映射宿主端口（`"${VAR:-默认}:容器端口"`），只写容器端口会让桌面报「没有发布」。

**重要**：不要用 `PUBLIC_RTMP` 或 `JETDESK_PORT_1935` 的**缺省值**去"猜"宿主端口。
`JETDESK_PORT_1935="${SRS_RTMP_PORT}"`（没有 `:-1935`）是正确写法：
没设置就是空，容器如实回答"不知道"；写成 `:-1935` 会把"不知道"伪装成"就是 1935"，
等于又把宿主端口写死了一次。

## 五、包内实现要点（本仓库）

- `control/endpoints.go`：端点解析器，负责声明表、通道解析、URL 拼装；
- 宿主端口绑定落盘到 `data/endpoints.json`：容器重建后到首次推送之间也能报对端口，
  随后被桌面推送覆盖；
- **发现接口分级（2026-11-15 修订）**：
  - `GET /api/discover`：**免密钥**，只返回 `service` / `name` / `version` / `auth_required:true` /
    `endpoints_path:"/api/endpoints"` / `auth_header:"X-Access-Key"`。
    **不返回** `rtmp_base`、`publish_key`、`endpoints[]`、`access_key` —— 它面向整个局域网，
    放推流口令等于把「能推流」公开；
  - `GET /api/endpoints`：**需要访问密钥**，返回 `endpoints[]`（`hostPort` / `portSource` /
    `keyQuery` / `url` / `publishUrl`）、`rtmp_base` / `rtmp_port` / `ui` / `publish_key` / `auto_record`；
  - 老客户端若只读 discover，会拿不到地址（这是有意的破坏性变更加安全收益）。
- `/api/diag` 输出每个端点的 `containerPort` / `hostPort` / `portSource`，
  真机排查时一眼看出"桌面到底有没有告诉我们"；
- 设置页的「对外推流地址」降级为**可选覆盖**：留空即用解析结果。

## 六、Windows 端口转发相关

- Windows 上宿主端口经端口转发进 WSL 再到容器，容器内始终只看到自己的 `containerPort`，
  所以宿主端口只能靠第四节的通道得到，不能"探测"；
- 容器要访问**宿主**上的服务（例如推给宿主机上的另一套 RTMP），必须用
  `host.docker.internal`（compose 里加 `extra_hosts: ["host.docker.internal:host-gateway"]`），
  `127.0.0.1` 永远指向容器自己；端口也要用宿主端口，不是容器内端口。

## 七、自测（真机复现步骤）

```bash
# 1. 让 SRS 的宿主端口不是 1935（模拟被占用后自动改号）
SRS_RTMP_PORT=1936 docker compose up -d

# 2. 看容器是否被告知（带口令查 /api/endpoints，两种都应看到 hostPort=1936）
curl -s -H 'X-Access-Key: jetool' localhost:1986/api/endpoints \
  | python3 -m json.tool | grep -A5 '"name": "rtmp"'
curl -s -H 'X-Access-Key: jetool' localhost:1986/api/diag \
  | python3 -m json.tool | grep portSource

# 3. 用 /api/endpoints 给出的 publishUrl（已带推流口令）推流，SRS 控制台应能看到流
```

验收标准：`/api/endpoints` 里的 `rtmp_base` 端口 = 宿主实际映射端口，
且 `portSource` 不为 `container-port`。

> 注意：`/api/discover` 现在**只做最小发现**（`service`/`name`/`version` + `auth_required` +
> `endpoints_path`），地址与推流口令一律走 `/api/endpoints`（要访问密钥）。
