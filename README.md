# QControlHub

QControlHub 是面向 Linux 节点的配置与远程运维平台，由 Go 控制面、Go Agent、静态 Web 控制台和 PostgreSQL 组成，用于集中管理代理内核、配置、任务与运行状态。

> **安全提示：** QControlHub 可以下发敏感配置、控制 systemd 服务并管理端口流量规则，属于高权限基础设施。生产环境必须使用 HTTPS、限制管理入口、妥善保管凭据并定期验证备份。

## 核心能力

### 节点与任务

- 通过出站 WSS 接入 Linux Agent，使用 Ed25519 身份、签名时间窗和持久化 nonce 防止握手重放。
- 集中查看节点在线状态、CPU、内存、磁盘、网络与流量趋势，并保留任务结果和审计记录。
- 远程执行配置校验与部署、服务启停、状态查询、配置读取和内核安装；节点侧任务均为真实执行。

### 配置与内核

- 管理 Mihomo、Xray、sing-box 和 Shadowsocks Rust 的节点配置、版本修订、差异与模板。
- 提供服务端入站方案和源码编辑入口，配置在控制面检查结构后仍由目标内核完成真实校验。
- Agent 通过受限路径、原子替换、备份和失败回滚部署配置与内核二进制，并通过专用 systemd 单元管理服务。

### 接入、日志与流量

- 汇集托管内核日志，并从已认证的 Agent 通道刷新运行指标和服务状态。
- 根据节点配置生成带遮罩的客户端分享 URI 与逐项接入参数。
- 使用 QAgent 专用 nftables 表统计端口收发流量，支持按月或按年额度、周期重置和超额封禁。

### 权限与数据

- PostgreSQL 持久化节点、配置、任务、指标、流量策略和审计数据。
- 管理 API 使用 Bearer 令牌；Web 控制台使用服务端会话、HttpOnly Cookie 和 CSRF 防护，并支持按能力授权的管理员与用户账号。
- 可选用 AES-256-GCM 加密配置正文与修订，并通过 HMAC-SHA256 签名 Webhook 通知。

## 本地快速开始

### 环境要求

- Linux
- Docker Engine
- Docker Compose v2（`docker compose`）
- OpenSSL

### 启动控制面

```bash
git clone https://github.com/qimaoww/qcontrolhub.git
cd qcontrolhub
make init-env
make dev-up
```

`make init-env` 会创建权限为 `0600` 的 `.env` 并生成本地所需密钥；`make dev-up` 从当前源码构建镜像，只在回环地址启用开发用 HTTP。

访问 `http://127.0.0.1:8080`，使用 `.env` 中的 `QCH_ADMIN_TOKEN` 登录。停止并保留 PostgreSQL 数据：

```bash
make down
```

生产环境不要使用 `make dev-up`，也不要把回环端口直接暴露到公网。

## 生产部署与 Agent 接入

### 生产部署

Linux 控制面主机可以运行 [`deploy/quick-start.sh`](deploy/quick-start.sh) 初始化内置或外部 PostgreSQL 模式：

```bash
./deploy/quick-start.sh
```

该脚本负责密钥、Compose 服务和就绪检查，不替代 TLS、反向代理、数据库保护、备份与恢复演练。上线前按 [生产部署指南](docs/production.md) 完成全部步骤，并核对 [安全基线](docs/security.md)。

### Agent 接入

控制面可用后，在 Web 控制台为目标节点生成添加命令，并在受控 Linux 节点执行该命令。安装流程会下载受凭据保护的 Agent 与配套资源，写入受限环境文件，安装 systemd 单元并启动 QAgent。

一键安装器可以识别符合严格安全检查的标准 Xray 或 sing-box 单文件服务；现有配置不会在注册时自动导入或切换服务，管理员可在“手动配置”页查看节点快照并显式迁移到 QAgent 专用服务。迁移失败会恢复原服务，无法精确识别时则保留隔离式 QAgent 配置。支持边界见 [生产部署指南](docs/production.md#4-安装远程-agent)。

Agent 以受限的 root 服务运行，远程任务会真实修改配置、服务、内核二进制或 QAgent 专用流量规则。接入前请核对证书、权限、内核路径和服务名；完整步骤见 [安装远程 Agent](docs/production.md#4-安装远程-agent)。

## 开发与验证

| 命令 | 用途 |
| --- | --- |
| `make build` | 构建 `bin/qcontrol-plane` 和 `bin/qagent` |
| `make test` | 运行 Go 测试 |
| `make check` | 检查 gofmt，运行前端模块 smoke、`go vet ./...` 和 `go test ./...` |
| `make compose-config` | 校验 Compose 渲染结果；需要先初始化 `.env` |

开发环境、宿主机运行方式和 PostgreSQL 集成测试说明见 [开发指南](docs/development.md)。

## 文档导航

### 运行与安全

- [开发指南](docs/development.md)
- [生产部署](docs/production.md)
- [鉴权与安全基线](docs/security.md)

### 接口与配置

- [HTTP API 与 Agent 协议](docs/api.md)
- [服务端入站方案](docs/server-plans.md)
- [最小配置样例](examples/configs/)

## License

QControlHub 依据 [GNU General Public License version 3](LICENSE) 发布，SPDX 标识为 `GPL-3.0-only`。
