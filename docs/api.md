# HTTP API

本机 Compose 默认地址为 `http://127.0.0.1:8080`；生产基线通过 Nginx TLS 对外提供 `https://qcontrolhub.example.com`，下文示例按该生产地址书写。所有请求和响应使用 UTF-8；JSON 请求应发送 `Content-Type: application/json`。

## 鉴权模型

- 管理端点 `/api/v1/*`：自动化调用使用 `Authorization: Bearer <令牌>`；SPA 使用 `/api/v1/auth/login` 建立 HttpOnly 会话 Cookie。个人账号只有 admin/user 两种身份，user 的能力由 `permissions` 列表决定；旧版低权限令牌仅作为兼容入口。
- 添加或重装节点 `/agent/v1/enroll`：`Authorization: Bearer <节点绑定的 QCH_ENROLLMENT_TOKEN>`。
- WSS `/agent/v1/connect`：仅供官方 Agent 使用，握手要求 Ed25519 签名头、时间戳和 nonce；不要用管理员令牌调用。
- `/healthz`：无鉴权，仅返回服务存活状态，不包含数据库详情或秘密。
- `/readyz`：无鉴权；仅在控制面能连接 PostgreSQL 时返回 200，不暴露数据库错误详情。
- `GET /api/v1/agent-installer`：通过 `X-QControlHub-Enrollment` 头提交有效的添加节点凭证后返回一键安装脚本。
- `GET /api/v1/agent-binary`：通过 `X-QControlHub-Enrollment` 头提交有效的添加节点凭证后返回 Agent 可执行文件。

### 身份与权限

用户管理只保留两种身份；管理员拥有全部能力，用户按 `permissions` 逐项授权。旧版低权限令牌仍兼容映射到能力集合：

| 角色 | 读取 | 任务/配置写操作 | 节点与添加命令、设置、删除配置 |
| --- | --- | --- | --- |
| user（用户，按 permissions） | 按授权能力 | 按授权能力 | — |
| admin（管理员） | ✓ | ✓ | ✓ |

权限不足时返回 `403`。同一令牌在 Web 会话与 Bearer 请求中的角色一致。

失败响应通常是：

```json
{"error":"message"}
```

## 管理端点

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| `GET` | `/api/v1/overview` | 节点、配置和任务计数 |
| `POST` | `/api/v1/auth/login` | 使用管理令牌创建 SPA 会话，返回角色和 CSRF token |
| `GET` | `/api/v1/auth/session` | 读取当前 SPA 会话 |
| `POST` | `/api/v1/auth/logout` | 注销当前 SPA 会话 |
| `GET` | `/api/v1/agents` | 列出未撤销 Agent |
| `DELETE` | `/api/v1/agents/{id}` | 永久撤销 Agent、立即断开 WSS 并终止其未完成任务 |
| `POST` | `/api/v1/agents/{id}/enrollment-token` | 为该节点新增一条独立、可重复使用的 Agent 安装凭据；已有凭据继续有效（enrollment.manage） |
| `GET` | `/api/v1/agents/{id}/configs` | 列出节点已有的内核配置 |
| `GET` | `/api/v1/agents/{id}/configs/{engine}` | 读取节点绑定的内核配置 |
| `PUT` | `/api/v1/agents/{id}/configs/{engine}` | 以乐观版本锁创建或更新节点配置 |
| `GET` | `/api/v1/agents/{id}/configs/{engine}/workspace` | 读取服务端入站、字段目录和节点配置工作区数据 |
| `POST` | `/api/v1/agents/{id}/configs/{engine}/plans` | 生成带安全随机凭据的服务端入站方案；可传当前 `input` 以保留用户选择并重新生成随机字段 |
| `POST` | `/api/v1/agents/{id}/configs/{engine}/server-inbounds` | 新增、修改或删除服务端入站并创建校验/部署任务 |
| `GET` | `/api/v1/agents/{id}/configs/{engine}/fields/{key}` | 读取官方目录中的一个顶级配置字段 |
| `POST` | `/api/v1/agents/{id}/configs/{engine}/fields/{key}` | 新增、修改或删除顶级配置字段并创建任务 |
| `GET` | `/api/v1/deployments` | 列出每个节点/内核最近一次真实成功部署 |
| `GET` | `/api/v1/client-access` | 从已部署入站生成客户端连接资料 |
| `GET` | `/api/v1/core-logs` | 查询面板集中保存的内核运行日志 |
| `PUT` | `/api/v1/agents/{id}/client-address` | 设置或清除客户端访问节点时使用的域名/IP（agents.manage） |
| `GET` | `/api/v1/config-catalogs/{engine}` | 读取内核官方配置字段和服务端协议目录 |
| `GET` | `/api/v1/configs` | 列出配置及正文 |
| `POST` | `/api/v1/configs` | 创建配置 |
| `PUT` | `/api/v1/configs/{id}` | 更新配置并增加版本号 |
| `DELETE` | `/api/v1/configs/{id}` | 软删除配置 |
| `GET` | `/api/v1/configs/{id}/revisions?limit=` | 列出最近 1–100 个配置修订 |
| `GET` | `/api/v1/configs/{id}/revisions/{version}` | 读取指定修订正文 |
| `POST` | `/api/v1/configs/{id}/revisions/{version}/restore` | 将指定修订恢复为新的当前修订 |
| `GET` | `/api/v1/tasks?agent_id=&status=&action=&limit=` | 按节点、状态和动作筛选任务；`limit` 为 1–500，默认 100 |
| `POST` | `/api/v1/tasks` | 创建远程任务 |
| `GET` | `/api/v1/tasks/{id}` | 读取单个任务及结果 |
| `GET` | `/api/v1/tasks/{id}/config-snapshot` | 读取已成功 `read-config` 任务的短期配置快照 |
| `DELETE` | `/api/v1/tasks/{id}` | 取消尚未领取的任务 |
| `POST` | `/api/v1/tasks/{id}/retry` | 按当前配置重试失败或已取消任务 |
| `GET` | `/api/v1/enrollment-tokens` | 列出添加节点记录，不返回原始凭证（admin） |
| `POST` | `/api/v1/enrollment-tokens` | 创建节点绑定、可重复安装的添加命令 |
| `DELETE` | `/api/v1/enrollment-tokens/{id}` | 删除添加节点记录并立即使命令失效 |
| `GET` | `/api/v1/settings` | 读取面板设置 |
| `PUT` | `/api/v1/settings` | 保存面板设置（admin） |
| `GET` | `/api/v1/audit?limit=` | 读取最近审计记录 |
| `GET` | `/api/v1/metrics/{agent_id}` | 读取节点最近 24 小时资源样本 |
| `GET` | `/api/v1/traffic-policies` | 读取所有端口流量配额及 Agent 最新计数（traffic.read） |
| `POST` | `/api/v1/traffic-policies` | 创建端口流量配额（traffic.manage） |
| `PUT` | `/api/v1/traffic-policies/{id}` | 更新端口、协议、周期或额度（traffic.manage） |
| `POST` | `/api/v1/traffic-policies/{id}/reset` | 立即清零当前周期并解封端口（traffic.manage） |
| `DELETE` | `/api/v1/traffic-policies/{id}` | 停止监控并移除该端口的 QControlHub 封禁规则（traffic.manage） |
| `GET` | `/api/v1/templates` | 列出配置模板 |
| `POST` | `/api/v1/templates` | 创建配置模板 |
| `DELETE` | `/api/v1/templates/{id}` | 删除配置模板（admin） |
| `POST` | `/api/v1/templates/{id}/apply` | 渲染模板并保存到指定节点 |
| `GET` | `/api/v1/agent-installer` | 下载添加节点凭证保护的一键安装脚本 |
| `GET` | `/api/v1/agent-binary` | 下载添加节点凭证保护的 Agent 可执行文件 |

`GET /api/v1/overview` 中的 `configs` 只统计可在“配置档案”工作区跨节点下发的全局配置；`node_configs` 单独统计绑定到具体 Agent/内核的节点配置，避免将两类配置混为一个不可解释的总数。为兼容既有调用方，`tasks_pending` 仍表示 `pending + running` 的活动任务总数；`tasks_queued` 和 `tasks_running` 分别给出排队与执行中的精确数量。

### 端口流量配额

创建与更新请求使用相同结构：

```json
{
  "agent_id": "agt_0123456789abcdef",
  "name": "Reality 入口",
  "engine": "xray",
  "port": 443,
  "protocol": "both",
  "cycle": "monthly",
  "cycle_anchor": "2026-08-01T00:00:00Z",
  "limit_bytes": 107374182400
}
```

`protocol` 为 `tcp`、`udp` 或 `both`，`cycle` 为 `monthly` 或 `yearly`。`cycle_anchor` 必须是当天或过去的 UTC 日期；月末和闰年按日历末日自动对齐。额度是接收与发送字节之和，同一节点同一端口只能配置一次。修改端口、协议、周期或起始日期会开始新的计数；只调整名称、内核归属或额度会保留当前已用流量。响应中的 `enforcement_available`、`enforcement_error`、`blocked`、当前周期与收发计数均来自 Agent 最新心跳。

删除 Agent 是不可逆的身份吊销：控制面先删除该节点的配置与修订、将其未完成任务标记为失败，再主动关闭当前认证 WSS；相同 Ed25519 身份的后续握手返回 `401`。添加节点记录仍存在时，可重新执行原命令原位注册新身份。

### 添加节点

```json
{
  "name": "edge-01"
}
```

`name` 同时是凭证绑定的节点名称。接口始终创建无有效期、可重复安装的添加节点命令；重复注册会更新原节点的密钥并复用节点 ID。为已有节点再次生成命令时会新增独立凭据，不会删除或覆盖已有凭据。创建响应中的 `token` 只返回一次并带有 `Cache-Control: no-store`，控制面只保存摘要。删除某条添加记录后，仅对应命令立即失效；删除节点会使该节点的全部安装命令失效。

### 创建配置

请求字段：

```json
{
	"version": 1,
	"name": "edge-xray",
  "description": "local smoke test",
  "engine": "xray",
  "content": "{\"log\":{\"loglevel\":\"warning\"},\"inbounds\":[],\"outbounds\":[]}"
}
```

更新配置时必须提交当前 `version`；版本不匹配会返回 `409`，防止两个编辑器静默覆盖彼此的修改。创建配置时忽略该字段。

`engine` 只能是 `mihomo`、`xray`、`sing-box` 或 `ss-rust`。Mihomo 正文必须是非空 YAML mapping；另外三类必须是非空 JSON object。正文上限 2 MiB。控制面的结构解析不能替代真实内核校验，应先创建 `validate` 任务。`ss-rust` 的结构校验基于官方 JSON 配置格式；由于 `ssserver` 没有非运行检查模式，真实 Agent 的 `validate` 不会启动服务。

使用仓库样例创建配置时，推荐用 `jq` 负责 JSON 转义，避免 shell 插值破坏正文：

```bash
jq -n \
  --arg name xray-smoke \
  --arg engine xray \
  --rawfile content examples/configs/xray-minimal.json \
  '{name:$name, engine:$engine, content:$content}' \
| curl --fail-with-body \
    -X POST \
    -H "Authorization: Bearer ${QCH_ADMIN_TOKEN}" \
    -H 'Content-Type: application/json' \
    --data-binary @- \
    https://qcontrolhub.example.com/api/v1/configs
```

### 配置修订与恢复

全局配置档案和节点绑定配置每次成功创建、更新或恢复时，都会在同一数据库事务中保留一份完整修订。修订列表按版本号倒序返回，默认 20 条，`limit` 可设为 1–100：

```text
GET /api/v1/configs/cfg_0123456789abcdef/revisions?limit=20
GET /api/v1/configs/cfg_0123456789abcdef/revisions/2
```

恢复不会把版本号倒退，也不会覆盖既有历史。调用方必须提交当前配置版本；例如当前为 v3、恢复 v1 时，会创建内容来自 v1 的新 v4：

```http
POST /api/v1/configs/cfg_0123456789abcdef/revisions/1/restore
Content-Type: application/json

{"expected_version":3}
```

`expected_version` 不匹配返回 `409`，无效版本号返回 `400`，配置或修订不存在（包括配置已删除）返回 `404`。升级到带修订历史的版本时，迁移会把每个现有活动配置的当前版本写为第一条可用修订，但无法重建升级前已经被覆盖的旧正文。

修订包含完整配置和代理凭据，应与当前配置正文使用相同的数据库、备份和访问保护。删除配置档案或撤销其所属节点时会永久删除对应修订，避免已删除凭据继续留存在修订表中。

### 列出任务

任务列表筛选条件可组合使用。`agent_id` 接受完整 Agent ID；`status` 允许 `pending`、`running`、`succeeded`、`failed`、`canceled`；`action` 允许 `validate`、`deploy`、`import-existing`、`read-config`、`start`、`stop`、`restart`、`status`、`install`。`limit` 默认为 100，最大为 500；无效的 `status` 或 `action` 返回 `400`。

### 创建任务

部署或校验任务需要 `config_id`，且配置内核必须与任务内核相同：

```json
{
  "agent_id": "agt_0123456789abcdef",
  "action": "validate",
  "engine": "xray",
  "config_id": "cfg_0123456789abcdef"
}
```

`read-config` 不接受 `config_id`。Agent 只会读取该内核启动配置中预先配置的绝对白名单路径，拒绝符号链接、不安全归属或权限、非 UTF-8 以及超过 2 MiB 的文件，并在返回前调用目标节点上的真实内核校验。读取结果作为短期配置快照保存，不出现在普通任务列表响应中；同一节点和内核新的成功读取会清除上一份快照。

服务动作不使用 `config_id`：

```json
{
  "agent_id": "agt_0123456789abcdef",
  "action": "status",
  "engine": "xray"
}
```

安装或切换内核版本同样不使用 `config_id`。`core_version` 可以是 `stable`、`development`，也可以是去掉 `v` 前缀的严格版本号：

```json
{
  "agent_id": "agt_0123456789abcdef",
  "action": "install",
  "engine": "sing-box",
  "core_version": "1.14.0-beta.3"
}
```

允许的 `action` 为 `validate`、`deploy`、`import-existing`、`read-config`、`start`、`stop`、`restart`、`status`、`install`。Agent 必须在注册能力中声明对应内核。`import-existing` 只接受该节点自己保存的配置快照，并且只在 Agent 已精确识别、仍等待管理员确认的现有 Xray 或 sing-box 服务上执行；常规使用应从“手动配置”页提交。版本安装只使用四个内核各自的官方 GitHub Release，不接受 URL；`development` 没有官方 prerelease 时任务会失败而不会降级到稳定版。

任务成功响应表示目标节点已完成对应操作；失败响应会保留节点返回的错误信息。部署任务只有在目标节点真实写入配置并成功重启服务后，才会进入节点的最新部署记录。

成功的 `read-config` 任务不会在普通任务列表或任务详情中返回配置正文。读取完成后，使用 `GET /api/v1/tasks/{id}/config-snapshot` 获取 `{ "content": "..." }`；当快照已被同一节点和内核的后续成功读取清理时返回 `404`。

### 状态码

- `200`：查询或更新成功。
- `101`：Agent WebSocket 升级成功。
- `201`：配置、任务或 Agent 创建成功。
- `204`：删除成功。
- `400`：JSON、字段、内核或动作无效。
- `401`：令牌或 Agent 签名无效。
- `403`：浏览器 Origin 不在 CORS 白名单。
- `404`：目标不存在。
- `409`：重复值或任务状态冲突。
- `429`：鉴权失败次数过多。

## Agent 协议

Agent 协议端点如下：

| 方法 | 路径 | 鉴权 |
| --- | --- | --- |
| `POST` | `/agent/v1/enroll` | 注册 Bearer 令牌 |
| `GET` | `/agent/v1/connect` | Agent 签名的 WebSocket Upgrade |

WSS 握手必须协商子协议 `qcontrolhub.agent.v1`。服务端先发送 `hello` 及该节点的端口流量策略；Agent 定期发送 `heartbeat`，心跳包含内核运行状态、主机资源以及端口配额的收发计数和封禁状态；`runtime.<engine>.existing_config_available` 表示已有服务可在手动配置页读取并迁移，`existing_config_unsupported_reason` 表示检测到已有服务但精确 argv、路径、歧义或 wrapper 安全边界不允许自动读取/接管，控制面只展示该原因并禁用相关操作。服务端下发带随机 lease ID 的 `task`，Agent 返回包含 `success` 和结果正文的 `result`，服务端确认 `result_ack`。连接压缩关闭，服务端要求 50 秒内收到消息，官方 Agent 默认每 15 秒心跳并在断线后指数退避重连。

## Webhook 事件

在设置页配置 Webhook 地址后，控制面会在以下时机向该地址 `POST` 一个 JSON 事件（`Content-Type: application/json`）：

| 事件类型 | 触发时机 |
| --- | --- |
| `task.failed` | 远程任务以失败结束 |
| `agent.offline` | 节点超过 2 分钟未心跳 |
| `agent.online` | 离线节点恢复心跳 |

事件负载示例：

```json
{
  "type": "task.failed",
  "time": "2026-08-14T02:00:00Z",
  "agent_id": "agt_0123456789abcdef",
  "agent": "shanghai-edge-01",
  "engine": "mihomo",
  "task_id": "tsk_0123456789abcdef",
  "action": "deploy",
  "error": "validation failed",
  "message": "任务失败：mihomo 在节点 shanghai-edge-01 上执行 deploy 失败"
}
```

配置了 `QCH_WEBHOOK_SECRET` 时，请求头携带 `X-QControlHub-Signature: sha256=<hex>`，值为 `HMAC-SHA256(secret, body)` 的十六进制摘要；接收端应使用常量时间比较校验签名，并只信任 `2xx` 之外按失败处理。未配置密钥时不带签名头，仅应在可信内网使用。

主机指标不会开放新的 Agent 监听端口。Agent 不上报通配、回环、组播或链路本地地址；控制面用服务器接收时间覆盖 Agent 时间戳，并校验百分比、容量、接口数量、名称和地址边界，只在 PostgreSQL 保存每个节点的最新快照。SPA 通过要求有效管理会话的 `GET /api/v1/metrics/{agent_id}` 获取历史样本；响应设置 `Cache-Control: no-store`。节点运行区只从实际部署版本生成客户端资料，优先使用受控节点标签中的入口地址，否则自动选择默认路由接口地址；配置编辑器不接受或传播客户端连接地址参数。

握手签名 canonical value 由固定字符串 `qcontrolhub-agent-v1`、大写 HTTP 方法、原始转义路径及查询串、Agent ID、Unix 秒时间戳、随机 nonce、空正文 SHA-256 十六进制摘要以换行连接。握手需要以下头：`X-QControlHub-Agent-ID`、`X-QControlHub-Timestamp`、`X-QControlHub-Nonce`、`X-QControlHub-Signature`。有效时间窗为正负 90 秒，nonce 在窗口内只能使用一次。应直接使用官方 Go Agent，避免自行实现导致编码、代理重写、lease 或防重放错误。
