# 生产部署

本文采用“单机 Docker Compose API 控制面 + 独立 SPA + 宿主机 Nginx + 多台 systemd Agent”的基线。只有 `qcontrol-web` 发布到回环地址，Nginx 负责公网 TLS；控制面 API 只在 Compose 内部网络可达，控制面到 PostgreSQL 使用项目内部后端网络，数据库持久化到命名卷。

## 1. 准备控制面主机

建议使用受支持的 Linux 发行版，并安装 Docker Engine、Docker Compose v2、Nginx、OpenSSL 与证书管理工具。防火墙只对管理来源和 Agent 网络开放 TCP 443；不要开放 8080 或 5432。

初始化随机密钥：

```bash
make init-env
chmod 600 .env
```

把 `.env` 中的 PostgreSQL 密码和管理员令牌保存到密码管理器。确认以下生产设置：

```dotenv
QCH_BEHIND_TLS_PROXY=true
QCH_ALLOW_INSECURE_HTTP=false
QCH_ALLOW_INSECURE_DATABASE=true
QCH_CONTROL_PROXY_SUBNET=172.30.254.0/24
QCH_CONTROL_PROXY_GATEWAY=172.30.254.1
QCH_WEB_PROXY_ADDRESS=172.30.254.2
QCH_CONTROL_PLANE_PROXY_ADDRESS=172.30.254.3
QCH_TRUSTED_PROXY_CIDRS=172.30.254.2/32,172.30.254.1/32
QCH_BIND_ADDRESS=127.0.0.1
QCH_PORT=8080
POSTGRES_PORT=5432
QCH_CORS_ORIGINS=https://qcontrolhub.example.com
# 可选：配置正文与修订的 AES-256-GCM 落盘加密密钥（任意非空字符串）。
# 开启后旧明文行仍可透明读取，新写入自动加密；密钥丢失将无法解密，务必备份。
QCH_CONFIG_ENCRYPTION_KEY=replace-with-a-long-random-secret
```

`QCH_CORS_ORIGINS` 仅在浏览器从另一个 origin 调用 JSON API 时需要；使用同域 Web 控制台可以留空。官方拓扑包含宿主 Nginx 与 `qcontrol-web` 两跳代理：控制面直接看到固定的 `QCH_WEB_PROXY_ADDRESS`，转发链中真实客户端右侧还包含固定的 `QCH_CONTROL_PROXY_GATEWAY`。`QCH_TRUSTED_PROXY_CIDRS` 必须只列出这两个精确 `/32` 端点，控制面才能从右向左安全剥离完整代理链，同时忽略客户端伪造在链左侧的值。若网段冲突，subnet、gateway、两个容器地址及信任列表必须一起修改，禁止改成整个私网或任意来源网段。若手工设置 PostgreSQL 密码，必须对 URL 保留字符进行百分号编码；`make init-env` 生成的十六进制密码可直接用于 Compose URL。

## 2. 启动 PostgreSQL 与控制面

```bash
make up
docker compose ps
curl --fail http://127.0.0.1:8080/healthz
curl --fail http://127.0.0.1:8080/readyz
```

控制面启动时会自动建立或升级当前 schema。Compose 的 PostgreSQL 连接在单机内部网络使用 SCRAM 密码认证；`QCH_ALLOW_INSECURE_DATABASE=true` 只为这个隔离 bridge 上的 `sslmode=disable` 连接提供显式豁免，不适用于跨主机数据库。

首次部署后执行一致性备份，并设置定期备份。例如逻辑备份可以在受保护目录中运行：

```bash
set -a
. ./.env
set +a
umask 077
docker compose exec -T postgres pg_dump \
  -U "$POSTGRES_USER" \
  -d "$POSTGRES_DB" \
  --format=custom > qcontrolhub.dump
```

备份含完整配置正文，必须加密并限制访问。恢复演练应在隔离环境完成。

## 3. 配置 Nginx 与 TLS

1. 获取 `qcontrolhub.example.com` 的有效证书。
2. 复制 [Nginx 示例](../deploy/nginx/qcontrolhub.conf) 到 `/etc/nginx/conf.d/qcontrolhub.conf`。
3. 替换其中的域名与证书路径。
4. 如果并非所有子域都强制 HTTPS，评估并调整 HSTS 的 `includeSubDomains`。
5. 校验并平滑重载：

```bash
sudo nginx -t
sudo systemctl reload nginx
curl --fail https://qcontrolhub.example.com/healthz
```

控制面根据 `QCH_BEHIND_TLS_PROXY=true` 把连接视为安全传输，并直接设置 Secure Cookie 与 HSTS，不依赖代理传入的协议头。只有 TLS 确实在本机受信反向代理终止时才能设置该值；不要为“解决登录问题”启用 `QCH_ALLOW_INSECURE_HTTP`。SPA 登录调用 `/api/v1/auth/login`，浏览器后续写请求自动携带会话 CSRF 头。

Nginx 示例按真实客户端 IP 对 `/api/v1/auth/login` 和 `/agent/v1/enroll` 做额外限速。控制面仅在直接 TCP 对端匹配 `QCH_TRUSTED_PROXY_CIDRS` 时解析 `X-Forwarded-For`，并从右向左剥离可信代理。官方 Compose 将 `qcontrol-web` 和宿主入口各自固定为一个精确信任端点；不要删除其中任何一跳，也不要把整个私网或 `0.0.0.0/0` 加入该列表。

Agent 使用 `/agent/v1/connect` 的长期 WSS 会话。Nginx 示例已转发 `Upgrade`/`Connection`，并把上游读取空闲超时提高到一小时；删除这些设置会导致 Agent 无法升级或在无任务时周期性断线。

## 4. 安装远程 Agent

在受控构建主机执行：

```bash
make build VERSION=0.1.0
```

将 `bin/qagent` 安全传输到 Agent 主机，然后以 root 安装：

```bash
sudo install -o root -g root -m 0755 qagent /usr/local/bin/qagent
sudo install -d -o root -g root -m 0700 /etc/qcontrolhub /var/lib/qcontrolhub
sudo install -d -o root -g root -m 0750 /etc/qagent/mihomo /etc/qagent/xray /etc/qagent/sing-box /etc/qagent/shadowsocks-rust /etc/qcontrolhub/tls
sudo install -d -o root -g root -m 0755 /usr/local/lib/qagent/cores
sudo install -o root -g root -m 0644 \
  deploy/systemd/qagent.service \
  /etc/systemd/system/qagent.service
sudo install -o root -g root -m 0600 \
  deploy/systemd/agent.env.example \
  /etc/qcontrolhub/agent.env

# 空白节点还没有内核 unit 和初始配置时执行；已有文件不会被覆盖。
sudo deploy/bootstrap-core-services.sh
```

TLS 入站默认引用 `/etc/qcontrolhub/tls/server.crt` 与 `/etc/qcontrolhub/tls/server.key`。QControlHub 不代替 ACME 或站点证书管理；使用 TLS、TUIC、Hysteria2、Trojan 或 AnyTLS 前，应把适用的证书链和私钥安装到这两个路径（私钥权限建议 `0600`），或在方案表单中改为站点的既有绝对路径。

如果文件是从另一台构建主机复制来的，请把示例文件的本地路径替换为实际路径。先登录控制台，在“远程节点”页为目标节点生成添加命令；原始凭证只显示一次，可重复安装，直到删除对应的添加记录。然后编辑 `/etc/qcontrolhub/agent.env`：

- `QCH_SERVER_URL` 应使用控制面的 `wss://` origin；Agent 会从同一 origin 派生首次注册所需的 HTTPS 地址。
- 公网可信证书无需额外配置；私有 CA 或自签名证书必须复制为 root 所有的普通 PEM 文件，并通过 `QCH_TLS_CA_FILE` 指定绝对路径。Agent 不提供跳过证书验证的模式。
- 安装或重装时临时把该节点的添加凭证填入 `QCH_ENROLLMENT_TOKEN`。
- 设置有辨识度的节点名与标签。
- `QCH_AGENT_ENGINES` 只列出本机真实安装的内核。
- 先核对节点权限、内核路径和 systemd 单元，再从面板提交校验任务；Agent 任务均为真实执行。
- 核对每个内核的 binary、config、service 三项覆盖值。

默认值与覆盖变量：

| 内核 | 默认二进制 | 默认配置路径 | 默认服务 | 覆盖前缀 |
| --- | --- | --- | --- | --- |
| Mihomo | `/usr/local/lib/qagent/cores/mihomo` | `/etc/qagent/mihomo/config.yaml` | `qagent-mihomo.service` | `QCH_MIHOMO_*` |
| Xray | `/usr/local/lib/qagent/cores/xray` | `/etc/qagent/xray/config.json` | `qagent-xray.service` | `QCH_XRAY_*` |
| sing-box | `/usr/local/lib/qagent/cores/sing-box` | `/etc/qagent/sing-box/config.json` | `qagent-sing-box.service` | `QCH_SING_BOX_*` |
| Shadowsocks Rust | `/usr/local/lib/qagent/cores/ssserver` | `/etc/qagent/shadowsocks-rust/config.json` | `qagent-shadowsocks-rust.service` | `QCH_SS_RUST_*` |

### 已有 Xray / sing-box VPS 映射

一键安装脚本会尝试识别下列常见组合：

| 内核 | 活动服务 | 二进制候选 | 配置候选 |
| --- | --- | --- | --- |
| Xray | `xray.service` | `/usr/local/bin/xray`、`/usr/bin/xray` | `/usr/local/etc/xray/config.json`、`/etc/xray/config.json` |
| sing-box | `sing-box.service`、`singbox.service` | `/usr/local/bin/sing-box`、`/usr/bin/sing-box` | `/etc/sing-box/config.json`、`/usr/local/etc/sing-box/config.json` |

只有同时满足以下条件才会映射：通用服务当前为 active；systemd 只有一个明确的 `ExecStart`，其实际 executable token 与发现信息完全相同；Xray 使用受支持的单文件参数，sing-box 使用 `run -c <file>`、`run --config <file>`，或产品确认的固定顺序 `run -c <file> -C <directory>`；二进制、配置源及父链均为 root 所有、不是符号链接且不可被组/其他用户写入；全部配置源与合并快照均不超过 2 MiB；QAgent 对完整合并快照执行结构和真实内核校验；对应的 `qagent-*` 专用单元不存在或处于 inactive/failed。未知附加参数、相似路径前缀、多个 `-c`/`-C`、其他参数顺序、多个启动命令和活动的专用单元都会安全回退。

sing-box 的 `-C` 表示配置目录，而不是工作目录。QAgent 按 sing-box 的路径排序、对象递归合并、数组追加和“较早标量优先”规则读取主文件及目录中全部 `.json`，拒绝符号链接、非普通 JSON 条目、权限不安全或读取期间发生变化的目录，并分别用原始 `-c/-C` 参数和合并后的单文件快照执行真实内核校验。这样页面展示并迁移的是完整生效配置，不会遗漏目录片段。

二进制路径默认仍必须是受保护的普通文件。对于常见的 `/usr/local/bin/sing-box` 符号链接，只额外接受两种可证明的形式：直接解析到受保护真实二进制，或解析到内容严格等于 `#!/bin/sh` 加 `exec <受保护真实二进制> "$@"` 的固定转发器。QAgent 记录 systemd 使用的 executable token，但只校验、复制真实二进制；包含条件、环境展开、前后置命令或其他 shell 逻辑的任意 wrapper 不会显示为可迁移入口，也绝不会被复制成内核。

脚本只把核验后的 binary、config、可选 config-directory、service executable 和 service 写成精确的 `QCH_EXISTING_*` 只读发现信息，并禁用尚未运行的对应 `qagent-*` 空白单元；不会停止、禁用、替换或修改原通用服务。注册请求不包含配置正文，也不会创建配置、修订或部署记录。节点上线后，管理员在 Web 控制台“手动配置”页查看实时读取的节点快照，再显式选择“手动导入并迁移”。

已有节点通过控制面“升级 Agent”替换二进制并重启后，即使环境文件中没有 `QCH_EXISTING_*`，新版 Agent 也会在启动阶段执行同一套只读发现与真实内核校验。自动发现结果原子保存到 Agent state 文件旁的 `0600` 状态文件，只使用 `/var/lib/qcontrolhub` 既有受保护写权限；每次重启都会按当前 service/ExecStart/配置源刷新，不写原服务配置、二进制或任意 `/etc` 路径。显式配置的 `QCH_EXISTING_*` 始终优先，不会被自动结果覆盖；持久映射只在已有 `migrating` marker 需要崩溃恢复时保留。

如果标准服务及 `-c`/`-C` 配置形式已被检测到，但 executable 是复杂 wrapper、多跳 symlink、路径/权限不安全，或多个标准服务同时 active，Agent 不会执行 wrapper、读取不完整快照或创建迁移映射。该状态及原因会随心跳上报；节点页与“手动配置”页显示“检测到但不可迁移”，禁用配置读取、服务操作和版本变更。管理员应先把单元调整为直接真实二进制、一跳真实二进制链接或前述固定两行转发器，再重启 Agent 触发刷新。

迁移任务只接受 enable 状态为 `enabled`、`enabled-runtime` 或 `disabled` 的原服务和 QAgent 专用服务；`static`、`indirect` 等无法可靠持久禁用或精确恢复的状态会在任何文件或服务变更前安全拒绝，原服务继续运行。QAgent 专用 managed service 必须为 `inactive` 或 `failed`：Agent 在任何临时校验/drop-in/托管文件/marker 变更前检查一次，并在准备完成、停原服务前再次检查；`active`、`activating`、`reloading`、`deactivating` 或期间漂移都会安全拒绝，回滚已准备文件且不把既有进程误报为迁移成功。Agent 还会在准备开始前和停服切换前分别重新读取活动原单元的结构化 `ExecStart`，只有实际 executable token、固定转发关系和受支持的配置参数仍精确匹配发现信息时才继续；同时会重新读取全部配置源并要求合并结果与管理员保存的快照完全相同。通过这些检查后，任务对快照执行普通托管部署的完整策略和真实内核校验，再把受保护的现有真实内核二进制复制到 QAgent 私有目录并写入 QAgent 专用单文件配置。只有这些准备全部成功后，Agent 才停止原通用服务并启动、稳定验证对应的 `qagent-*` 服务；随后启用新服务，并同时清除原服务的 persistent/runtime enable 链接。任何启动、enable/disable 或状态落盘步骤失败，都会停止新服务、恢复原二进制与配置、精确恢复原 enable 层级并重新启动原服务。迁移成功是一次真实部署，控制面会把导入版本记录为当前部署。

自动识别失败时不会降级为猜测式映射。需要手工提供发现信息时，必须同时核对 `QCH_EXISTING_*_BINARY`、`QCH_EXISTING_*_CONFIG` 与 `QCH_EXISTING_*_SERVICE`；sing-box 目录模式还必须核对 `QCH_EXISTING_SING_BOX_CONFIG_DIRECTORY`，转发器布局必须核对 `QCH_EXISTING_SING_BOX_SERVICE_BINARY`。任意 wrapper 无法安全证明时不会提供自动迁移入口，应先由管理员把 systemd 单元改为直接执行受保护真实二进制或上述固定转发形式，再重启 Agent 触发发现；配置仍由管理员在“手动配置”页显式迁移。

迁移前，Agent 不会获得原配置目录或原核心二进制目录的写权限，也会拒绝部署、启停和内核安装任务。迁移后运行的是复制到 QAgent 私有目录的二进制与专用配置，原服务保持 disabled；后续升级和配置管理只作用于 QAgent 专用服务。

私有 CA 示例：

```bash
sudo install -o root -g root -m 0644 control-plane-ca.pem /etc/qcontrolhub/control-plane-ca.pem
sudo sh -c 'printf "%s\n" "QCH_TLS_CA_FILE=/etc/qcontrolhub/control-plane-ca.pem" >> /etc/qcontrolhub/agent.env'
```

启动：

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now qagent
sudo journalctl -u qagent -f
```

在控制台确认 Agent 在线，并确认 `/var/lib/qcontrolhub/agent-state.json` 的权限是 `0600`。随后：

1. 从 `/etc/qcontrolhub/agent.env` 删除 `QCH_ENROLLMENT_TOKEN` 行。
2. 通过控制台下发四种内核的 `validate` 与 `status` 测试。
3. 等待两个心跳周期，确认节点卡片显示 CPU、内存、根磁盘、实时上下行速率和累计流量；网速首个样本为 0，第二个样本开始按计数器差值计算。
4. 确认主机已安装 `nft`；在端口流量页创建一个小额度测试策略，验证收发计数、手动清零和自动封禁后再配置正式额度。
5. 核实任务结果及每个固定目标路径；在节点卡片执行稳定版、开发版或自定义版本任务时，Agent 会下载、校验并原子替换文件。
6. 变更内核或服务前确认目标节点处于在线状态，并保留可回滚的上一版本。

```bash
sudo systemctl restart qagent
```

systemd 单元的 `ProtectSystem=strict` 只放行默认的四个配置目录以及 `/usr/local/lib/qagent/cores`，用于在同一文件系统内原子切换配置和私有内核二进制；原服务的配置与二进制路径保持只读，`/usr/local/bin` 不可写，`/usr/local/bin/qagent` 也保持只读。Agent 单元只保留原子部署所需的 `CAP_CHOWN` 与端口计数/封禁所需的 `CAP_NET_ADMIN`；四个非 root 内核单元只保留监听 1-1023 端口所需的 `CAP_NET_BIND_SERVICE`。迁移完成后所有托管内核服务统一使用 `qagent-` 前缀。

现有节点首次通过新版 Agent 启动、重启、部署或升级专用内核时，会为固定的四个 `qagent-*` 单元同步低端口能力 drop-in 并执行 `daemon-reload`；自定义服务名不会被修改。重复运行一键安装脚本也只更新带 `managed by QAgent` 标记的单元，已有配置文件和管理员自建单元继续保留。

端口配额只管理 `inet qcontrolhub` 表，不刷新或改写管理员已有的 nftables 表。每个策略分别统计发往监听端口的接收字节和从该端口发出的发送字节；达到额度后，两条方向规则都切换为 `drop`。计数状态以 `0600` 原子保存在 `/var/lib/qcontrolhub/traffic-state.json`，Agent 或控制面短暂重启不会清零当前周期。

版本切换要求节点已经预置对应配置目录、可通过的初始配置和 systemd 单元。空白 Linux 节点可先运行 `deploy/bootstrap-core-services.sh` 完成这些前置条件；脚本仅创建缺失配置和新的 `qagent-*` unit，不迁移也不操作旧的通用服务或二进制。稳定版使用官方 latest，开发版只使用官方 prerelease，自定义版本必须是类似 `1.19.29` 或 `1.14.0-beta.3` 的完整版本号；不支持自定义下载地址。Agent 在下载后强制核对 GitHub Release API 给出的 SHA-256，运行候选二进制确认版本，随后原子替换并重启服务；失败时恢复上一二进制。

## 5. 运维操作

### 更新控制面

```bash
docker compose build --pull control-plane
docker compose up -d control-plane
docker compose ps
```

控制面重启会让 Web 用户重新登录，但 Agent 身份与任务保存在 PostgreSQL 中。

### 更新 Agent

先在少量节点执行校验任务确认路径和权限，再原子替换二进制并重启：

```bash
sudo install -o root -g root -m 0755 qagent /usr/local/bin/qagent
sudo systemctl restart qagent
sudo systemctl status qagent --no-pager
```

### 撤销与重新注册

从控制台删除 Agent 会立即使其签名身份失效。收到永久身份拒绝后，Agent 会正常退出而不是把它当作瞬时网络故障持续重连；配套的 `Restart=on-failure` 不会再次拉起它。若需重新注册：

```bash
sudo systemctl stop qagent
sudo rm /var/lib/qcontrolhub/agent-state.json
```

然后重新执行该节点的添加命令。控制面会复用原节点 ID、替换旧签名密钥并关闭旧连接；若添加记录已删除，则需要重新创建。删除状态文件是不可逆身份操作，必须先确认控制台中旧身份已撤销。

### 外部 PostgreSQL

`deploy/quick-start.sh -m external` 生成的 Compose 仍使用与 bundled 模式相同的专用代理网络、固定 `qcontrol-web`/控制面地址和两项精确信任列表；数据库改为外部连接不会改变 WSS 来源地址解析边界。重复运行脚本会保留自定义代理网络值，并为旧环境补齐缺失的 `qcontrol-web` 精确信任项。

使用外部数据库时，不应把密码直接写入可读命令行。通过受保护环境或密钥注入设置完整 `QCH_DATABASE_URL`，并使用类似以下参数：

```text
postgresql://qcontrolhub:URL_ENCODED_PASSWORD@db.example.com:5432/qcontrolhub?sslmode=verify-full&sslrootcert=/run/secrets/db-ca.pem
```

同时移除或禁用 Compose 中的 `postgres` 服务，将 `QCH_ALLOW_INSECURE_DATABASE=false`，并确保 CA 文件对控制面容器只读可见。当前仓库的基础 Compose 面向单机内置 PostgreSQL，外部数据库需要站点级 override 文件。

## 6. 监控建议

- 外部探测 `/healthz` 作为进程存活信号、`/readyz` 作为 PostgreSQL 就绪信号，同时监控容器重启次数。
- 告警 Agent 超过 45 秒离线、任务持续失败或任务积压。
- 收集控制面与 Agent 日志，但不要采集 `.env`、Authorization 请求头或配置正文。
- 监控磁盘、WAL 与备份新鲜度；在升级前执行恢复测试。
- 所有控制面和 Agent 主机启用 NTP/chrony，Agent 签名时间窗为正负 90 秒。

更完整的上线检查见 [鉴权与安全基线](security.md#上线核对表)。
