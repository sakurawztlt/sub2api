<div align="center">

<img src="assets/logo.svg" alt="Sub2API Logo" width="128" />

# Sub2API

[![Go](https://img.shields.io/badge/Go-1.26.6-00ADD8.svg)](https://golang.org/)
[![Vue](https://img.shields.io/badge/Vue-3.4+-4FC08D.svg)](https://vuejs.org/)
[![PostgreSQL](https://img.shields.io/badge/PostgreSQL-15+-336791.svg)](https://www.postgresql.org/)
[![Redis](https://img.shields.io/badge/Redis-7+-DC382D.svg)](https://redis.io/)
[![Docker](https://img.shields.io/badge/Docker-Ready-2496ED.svg)](https://www.docker.com/)

<a href="https://trendshift.io/repositories/21823" target="_blank"><img src="https://trendshift.io/api/badge/repositories/21823" alt="Wei-Shaw%2Fsub2api | Trendshift" width="250" height="55"/></a>

**AI API 网关平台 - 订阅配额分发管理**

[English](README.md) | 中文 | [日本語](README_JA.md)

</div>


## ⚠️ 重要提醒

使用本项目前，请务必仔细阅读以下内容：

- **🚨 服务条款风险**：使用本项目可能违反 Anthropic 等上游服务商的服务条款。请在使用前仔细阅读相关服务商的用户协议，由此产生的一切风险由用户自行承担。
- **⚖️ 合规使用**：请在符合您所在国家或地区法律法规的前提下使用本项目，严禁将其用于任何违法违规用途。
- **📖 免责声明**：本项目仅供技术学习与研究使用，作者不对因使用本项目导致的账户封禁、服务中断、数据丢失或其他任何直接或间接损失承担责任。

## 项目概述

Sub2API 是一个 AI API 网关平台，用于分发和管理 AI 产品订阅的 API 配额。用户通过平台生成的 API Key 调用上游 AI 服务，平台负责鉴权、计费、负载均衡和请求转发。

## 核心功能

- **多账号管理** - 支持多种上游账号类型（OAuth、API Key）
- **API Key 分发** - 为用户生成和管理 API Key
- **精确计费** - Token 级别的用量追踪和成本计算
- **智能调度** - 智能账号选择，支持粘性会话
- **并发控制** - 用户级和账号级并发限制
- **速率限制** - 可配置的请求和 Token 速率限制
- **内置支付系统** - 支持 EasyPay 易支付、支付宝官方、微信官方、Stripe，用户自助充值，无需独立部署支付服务（[配置指南](docs/PAYMENT_CN.md)）
- **管理后台** - Web 界面进行监控和管理
- **外部系统集成** - 支持通过 iframe 嵌入外部系统（如工单等），扩展管理后台功能

## ❤️ 赞助商

> [想出现在这里？](mailto:support@pincc.ai)

<table>

<tr>
<td width="180"><a href="https://www.openmodel.ai?ref=sub2api"><img src="assets/partners/logos/openmodel.jpg" alt="openmodel" width="150"></a></td>
<td>一个API，顶级模型随便用！<a href="https://www.openmodel.ai?ref=sub2api">OpenModel</a> 专注于生产级、高可用的 AI API 网关，让你的应用真正做到高速稳定：自动故障转移、智能选最优渠道、生产级 SLA 保障。远超单一供应商的 SLA，让稳定性成为您的核心竞争力。</td>
</tr>

<tr>
<td width="180"><a href="https://poixe.com/i/sub2api"><img src="assets/partners/logos/poixe.png" alt="PoixeAI" width="150"></a></td>
<td>感谢 Poixe AI 赞助了本项目！Poixe AI 提供可靠的 AI 模型接口服务，您可以使用平台提供的 LLM API 接口轻松构建 AI 产品，同时也可以成为供应商，为平台提供大模型资源以赚取收益。通过 <a href="https://poixe.com/i/sub2api">此链接</a> 专属链接注册，充值额外赠送 $5 美金</td>
</tr>

<tr>
<td width="180"><a href="https://ctok.ai"><img src="assets/partners/logos/ctok.png" alt="CTok" width="150"></a></td>
<td>感谢 CTok.ai 赞助了本项目！CTok.ai 致力于打造一站式 AI 编程工具服务平台。我们提供 Claude Code 专业套餐及技术社群服务，同时支持 Google Gemini 和 OpenAI Codex。通过精心设计的套餐方案和专业的技术社群，为开发者提供稳定的服务保障和持续的技术支持，让 AI 辅助编程真正成为开发者的生产力工具。点击<a href="https://ctok.ai">这里</a>注册！</td>
</tr>

<tr>
<td width="180"><a href="https://code.silkapi.com/"><img src="assets/partners/logos/silkapi.png" alt="silkapi" width="150"></a></td>
<td>感谢 丝绸API 赞助了本项目！ <a href="https://code.silkapi.com/">丝绸API</a> 是基于 Sub2API 搭建的中转服务，专注于提供 Codex 高速稳定API中转。</td>
</tr>

<tr>
<td width="180"><a href="https://ylscode.com/"><img src="assets/partners/logos/ylscode.png" alt="ylscode" width="150"></a></td>
<td>感谢 伊莉思Code 赞助了本项目！ <a href="https://ylscode.com/">伊莉思Code</a> 致力于构建安全的企业级Coding Agent生产力服务，提供稳定快速的 Codex / Claude / Gemini 订阅服务与即用即付API多种方案灵活选择，限时注册赠送 3 天 Codex 试用福利！</td>
</tr>

<tr>
<td width="180"><a href="https://aigocode.com/invite/SUB2API"><img src="assets/partners/logos/aigocode.png" alt="AIGoCode" width="150"></a></td>
<td>感谢 AIGoCode 赞助了本项目！AIGoCode 是一站式集成 Claude Code、Codex 以及最新 Gemini 模型的综合平台，为您提供稳定、高效、高性价比的 AI 编程服务。平台提供灵活的订阅方案，零封号风险，免 VPN 直连，响应极速。AIGoCode 为 sub2api 用户准备了专属福利：通过<a href="https://aigocode.com/invite/SUB2API">此链接</a>注册，首次充值可额外获得 10% 赠送额度！</td>
</tr>

<tr>
<td width="180"><a href="https://codex-everywhere.com"><img src="assets/partners/logos/codex-everywhere.jpg" alt="CodexEverywhere" width="150"></a></td>
<td>Real GPT-5.6 series at 3% of OpenAI pricing — <a href="https://codex-everywhere.com">CodexEverywhere</a> is democratizing access to frontier models for developers worldwide. We believe in transparency and honesty, with model quality verified by active community oversight for months. USD and crypto friendly. Start with a free $20 trial at <a href="https://codex-everywhere.com">codex-everywhere.com</a>.</td>
</tr>

<tr>
<td width="180"><a href="https://shop.bmoplus.com/?utm_source=github"><img src="assets/partners/logos/bmoplus.jpg" alt="bmoplus" width="150"></a></td>
<td>感谢 BmoPlus 赞助了本项目！BmoPlus 是一家专为AI订阅重度用户打造的可靠 AI 账号代充服务商，提供稳定的 ChatGPT Plus / ChatGPT Pro(全程质保) / Claude Pro / Super Grok / Gemini Pro 的官方代充&成品账号。 通过<a href="https://shop.bmoplus.com/?utm_source=github">BmoPlus AI成品号专卖/代充</a>注册下单的用户，可享GPT 官网订阅一折 的震撼价格！</td>
</tr>

<tr>
<td width="180"><a href="https://bestproxy.com/?keyword=a2e8iuol"><img src="assets/partners/logos/bestproxy.png" alt="bestproxy" width="150"></a></td>
<td>感谢 Bestproxy 赞助了本项目！<a href="https://bestproxy.com/?keyword=a2e8iuol">Bestproxy</a> 是一家提供高纯度住宅IP，支持一号一IP独享，结合真实家庭网络与指纹隔离，可实现链路环境隔离，降低关联风控概率。</td>
</tr>

<tr>
<td width="180"><a href="https://pateway.ai/?ch=1tsfr51"><img src="assets/partners/logos/pateway.png" alt="pateway" width="150"></a></td>
<td>感谢 PatewayAI 赞助了本项目！PatewayAI 是一家面向重度 AI 开发者、专注官方直连的高品质模型 API 中转服务商。提供 Claude 全系列与 Codex 系列模型，100% 官方源直供，不掺假不注水，欢迎检验。计费透明，Token 级账单可逐笔核验。
同时支持企业级高并发，并为企业客户提供了专业的管理平台，企业客户可签订正式合同并开具发票，更多详情进入官网获取联系方式。
现在通过 <a href="https://pateway.ai/?ch=1tsfr51">此链接</a> 注册即送 $3 试用额度，用户充值低至 6 折，邀请好友双向赠送，邀请奖励可达 $150。</td>
</tr>

</table>

## 生态项目

围绕 Sub2API 的社区扩展与集成项目：

| 项目 | 说明 | 功能 |
|------|------|------|
| ~~[Sub2ApiPay](https://github.com/touwaeriol/sub2apipay)~~ | ~~自助支付系统~~ | **已内置** — 支付功能已集成到 Sub2API 中，无需独立部署。详见 [支付配置指南](docs/PAYMENT_CN.md) |
| [sub2api-mobile](https://github.com/ckken/sub2api-mobile) | 移动端管理控制台 | 跨平台应用（iOS/Android/Web），支持用户管理、账号管理、监控看板、多后端切换；基于 Expo + React Native 构建 |

## 技术栈

| 组件 | 技术 |
|------|------|
| 后端 | Go 1.26.6, Gin, Ent |
| 前端 | Vue 3.4+, Vite 5+, TailwindCSS |
| 数据库 | PostgreSQL 15+ |
| 缓存/队列 | Redis 7+ |

---

## Nginx 反向代理注意事项

通过 Nginx 反向代理 Sub2API（或 CRS 服务）并搭配 Codex CLI 使用时，需要在 Nginx 配置的 `http` 块中添加：

```nginx
underscores_in_headers on;
```

Nginx 默认会丢弃名称中含下划线的请求头（如 `session_id`），这会导致多账号环境下的粘性会话功能失效。

---

## 部署方式

### 方式一：脚本安装（推荐）

一键安装脚本，自动从 GitHub Releases 下载预编译的二进制文件。

#### 前置条件

- Linux 服务器（amd64 或 arm64）
- PostgreSQL 15+（已安装并运行）
- Redis 7+（已安装并运行）
- Root 权限

#### 安装步骤

```bash
curl -sSL https://raw.githubusercontent.com/Wei-Shaw/sub2api/main/deploy/install.sh | sudo bash
```

脚本会自动：
1. 检测系统架构
2. 下载最新版本
3. 安装二进制文件到 `/opt/sub2api`
4. 创建 systemd 服务
5. 配置系统用户和权限

#### 安装后配置

```bash
# 1. 启动服务
sudo systemctl start sub2api

# 2. 设置开机自启
sudo systemctl enable sub2api

# 3. 在浏览器中打开设置向导
# http://你的服务器IP:8080
```

设置向导将引导你完成：
- 数据库配置
- Redis 配置
- 管理员账号创建

#### 升级

可以直接在 **管理后台** 左上角点击 **检测更新** 按钮进行在线升级。

网页升级功能支持：
- 自动检测新版本
- 一键下载并应用更新
- 支持回滚

#### 常用命令

```bash
# 查看状态
sudo systemctl status sub2api

# 查看日志
sudo journalctl -u sub2api -f

# 重启服务
sudo systemctl restart sub2api

# 卸载
curl -sSL https://raw.githubusercontent.com/Wei-Shaw/sub2api/main/deploy/install.sh | sudo bash -s -- uninstall -y
```

---

### 方式二：Docker Compose（推荐）

使用 Docker Compose 部署，包含 PostgreSQL 和 Redis 容器。

#### 前置条件

- Docker 20.10+
- Docker Compose v2+

#### 快速开始（一键部署）

使用自动化部署脚本快速搭建：

```bash
# 创建部署目录
mkdir -p sub2api-deploy && cd sub2api-deploy

# 下载并运行部署准备脚本
curl -sSL https://raw.githubusercontent.com/Wei-Shaw/sub2api/main/deploy/docker-deploy.sh | bash

# 启动服务
docker compose up -d

# 查看日志
docker compose logs -f sub2api
```

**脚本功能：**
- 下载 `docker-compose.local.yml`（本地保存为 `docker-compose.yml`）和 `.env.example`
- 自动生成安全凭证（JWT_SECRET、TOTP_ENCRYPTION_KEY、POSTGRES_PASSWORD）
- 创建 `.env` 文件并填充自动生成的密钥
- 创建数据目录（使用本地目录，便于备份和迁移）
- 显示生成的凭证供你记录

#### 手动部署

如果你希望手动配置：

```bash
# 1. 克隆仓库
git clone https://github.com/Wei-Shaw/sub2api.git
cd sub2api/deploy

# 2. 复制环境配置文件
cp .env.example .env
chmod 600 .env

# 3. 编辑配置（生成安全密码）
nano .env
```

**`.env` 必须配置项：**

```bash
# PostgreSQL 密码（必需）
POSTGRES_PASSWORD=your_secure_password_here

# JWT 密钥（推荐 - 重启后保持用户登录状态）
JWT_SECRET=your_jwt_secret_here

# TOTP 加密密钥（推荐 - 重启后保留双因素认证）
TOTP_ENCRYPTION_KEY=your_totp_key_here

# 可选：管理员账号
ADMIN_EMAIL=admin@example.com
ADMIN_PASSWORD=your_admin_password

# 可选：自定义端口
SERVER_PORT=8080
```

**生成安全密钥：**
```bash
# 生成 JWT_SECRET
openssl rand -hex 32

# 生成 TOTP_ENCRYPTION_KEY
openssl rand -hex 32

# 生成 POSTGRES_PASSWORD
openssl rand -hex 32
```

```bash
# 4. 创建数据目录（本地版）
mkdir -p data postgres_data redis_data

# 5. 启动所有服务
# 选项 A：本地目录版（推荐 - 易于迁移）
docker compose -f docker-compose.local.yml up -d

# 选项 B：命名卷版（简单设置）
docker compose up -d

# 6. 查看状态
docker compose -f docker-compose.local.yml ps

# 7. 查看日志
docker compose -f docker-compose.local.yml logs -f sub2api
```

#### 部署版本对比

| 版本 | 数据存储 | 迁移便利性 | 适用场景 |
|------|---------|-----------|---------|
| **docker-compose.local.yml** | 本地目录 | ✅ 简单（打包整个目录） | 生产环境、频繁备份 |
| **docker-compose.yml** | 命名卷 | ⚠️ 需要 docker 命令 | 简单设置 |

**推荐：** 使用 `docker-compose.local.yml`（脚本部署）以便更轻松地管理数据。

#### 启用“数据管理”功能（datamanagementd）

如需启用管理后台“数据管理”，需要额外部署宿主机数据管理进程 `datamanagementd`。

关键点：

- 主进程固定探测：`/tmp/sub2api-datamanagement.sock`
- 只有该 Socket 可连通时，数据管理功能才会开启
- Docker 场景需将宿主机 Socket 挂载到容器同路径

详细部署步骤见：`deploy/DATAMANAGEMENTD_CN.md`

#### 访问

在浏览器中打开 `http://你的服务器IP:8080`

如果管理员密码是自动生成的，在日志中查找：
```bash
docker compose -f docker-compose.local.yml logs sub2api | grep "admin password"
```

#### 升级

```bash
# 拉取最新镜像并重建容器
docker compose -f docker-compose.local.yml pull
docker compose -f docker-compose.local.yml up -d
```

#### 轻松迁移（本地目录版）

使用 `docker-compose.local.yml` 时，可以轻松迁移到新服务器：

```bash
# 源服务器
docker compose -f docker-compose.local.yml down
cd ..
tar czf sub2api-complete.tar.gz sub2api-deploy/

# 传输到新服务器
scp sub2api-complete.tar.gz user@new-server:/path/

# 新服务器
tar xzf sub2api-complete.tar.gz
cd sub2api-deploy/
docker compose -f docker-compose.local.yml up -d
```

#### 常用命令

```bash
# 停止所有服务
docker compose -f docker-compose.local.yml down

# 重启
docker compose -f docker-compose.local.yml restart

# 查看所有日志
docker compose -f docker-compose.local.yml logs -f

# 删除所有数据（谨慎！）
docker compose -f docker-compose.local.yml down
rm -rf data/ postgres_data/ redis_data/
```

---

### 方式三：Apple container（macOS）

Apple 芯片 Mac 在 macOS 26 上可使用 Apple `container` 1.1.0 或更高版本运行完整的 Sub2API、PostgreSQL 和 Redis：

```bash
git clone https://github.com/Wei-Shaw/sub2api.git
cd sub2api/deploy
./apple-container.sh init
./apple-container.sh up
./apple-container.sh status
```

这是由运维人员管理的本地运行方式；生产环境仍建议使用 Docker Compose。生命周期命令、持久化、升级和运行时限制见 [deploy/APPLE_CONTAINER.md](deploy/APPLE_CONTAINER.md)。

---

### 方式四：源码编译

从源码编译安装，适合开发或定制需求。

#### 前置条件

- Go 1.21+
- Node.js 18+
- PostgreSQL 15+
- Redis 7+

#### 编译步骤

```bash
# 1. 克隆仓库
git clone https://github.com/Wei-Shaw/sub2api.git
cd sub2api

# 2. 安装 pnpm（如果还没有安装）
npm install -g pnpm

# 3. 编译前端
cd frontend
pnpm install
pnpm run build
# 构建产物输出到 ../backend/internal/web/dist/

# 4. 编译后端（嵌入前端）
cd ../backend
VERSION="$(./scripts/resolve-version.sh)"
go build -tags embed -ldflags="-X main.Version=${VERSION}" -o sub2api ./cmd/server

# 5. 创建配置文件
cp ../deploy/config.example.yaml ./config.yaml

# 6. 编辑配置
nano config.yaml
```

> **注意：** `-tags embed` 参数会将前端嵌入到二进制文件中。不使用此参数编译的程序将不包含前端界面。

**`config.yaml` 关键配置：**

```yaml
server:
  host: "0.0.0.0"
  port: 8080
  mode: "release"

database:
  host: "localhost"
  port: 5432
  user: "postgres"
  password: "your_password"
  dbname: "sub2api"

redis:
  host: "localhost"
  port: 6379
  password: ""

jwt:
  secret: "change-this-to-a-secure-random-string"
  expire_hour: 24

default:
  user_concurrency: 5
  user_balance: 0
  api_key_prefix: "sk-"
  rate_multiplier: 1.0
```

**首次创建管理员：** 只要 `config.yaml` 已存在，Web 初始化向导就会被跳过；`default.admin_email` / `default.admin_password` 也不会创建管理员。全新的源码安装可跳过“创建/编辑配置”步骤，直接运行 `./sub2api` 并访问 `http://<host>:8080`，由向导创建配置和管理员；也可改用 `./sub2api -setup` / `AUTO_SETUP` 初始化。只有目标数据库已经有管理员时，才应手工预先创建配置。

`config.yaml` 还支持以下安全相关配置：

- `cors.allowed_origins` 配置 CORS 白名单
- `security.url_allowlist` 配置上游/价格数据/CRS 主机白名单
- `security.url_allowlist.enabled` 可关闭 URL 校验（慎用）
- `security.url_allowlist.allow_insecure_http` 关闭校验时允许 HTTP URL
- `security.url_allowlist.allow_private_hosts` 允许私有/本地 IP 地址
- `security.response_headers.enabled` 可启用可配置响应头过滤（关闭时使用默认白名单）
- `security.csp` 配置 Content-Security-Policy
- `billing.circuit_breaker` 计费异常时 fail-closed
- `server.trusted_proxies` 配置 Gin 信任的直接反向代理 IP/CIDR；留空即不信任代理链
- `security.trust_forwarded_ip_for_api_key_acl` 让 API Key IP 白/黑名单改用旧式原始转发头提取，而不是 Gin 的可信代理链（默认 `false`）
- `turnstile.required` 在 release 模式强制启用 Turnstile

API Key ACL 默认使用 Gin 的可信代理链。建议保持旧式接管关闭，并在 `server.trusted_proxies` 中只填写直接连接 Sub2API 的反代地址。如果兼容性要求启用原始 `CF-Connecting-IP`、`X-Real-IP` 或 `X-Forwarded-For` 提取，必须把源站防火墙限制到代理/CDN，并由边缘覆盖这些外部传入的头。

**网关防御纵深建议（重点）**

- `gateway.upstream_response_read_max_bytes`：限制非流式上游响应读取大小（默认 `8MB`），用于防止异常响应导致内存放大。
- `gateway.proxy_probe_response_read_max_bytes`：限制代理探测响应读取大小（默认 `1MB`）。
- `gateway.gemini_debug_response_headers`：默认 `false`，仅在排障时短时开启，避免高频请求日志开销。
- `/auth/register`、`/auth/login`、`/auth/login/2fa`、`/auth/send-verify-code` 已提供服务端兜底限流（Redis 故障时 fail-close）。
- 推荐将 WAF/CDN 作为第一层防护，服务端限流与响应读取上限作为第二层兜底；两层同时保留，避免旁路流量与误配置风险。

**⚠️ 安全警告：HTTP URL 配置**

当 `security.url_allowlist.enabled=false` 时，当前默认只执行最小 URL 校验并**允许 HTTP URL**。这便于本地或可信内网调试，但生产环境应显式收紧为仅允许 HTTPS：

```yaml
security:
  url_allowlist:
    enabled: false                # 禁用白名单检查
    allow_insecure_http: false    # 仅允许 HTTPS（生产环境推荐）
```

**或通过环境变量：**

```bash
SECURITY_URL_ALLOWLIST_ENABLED=false
SECURITY_URL_ALLOWLIST_ALLOW_INSECURE_HTTP=false
```

**允许 HTTP 的风险：**
- API 密钥和数据以**明文传输**（可被截获）
- 易受**中间人攻击 (MITM)**
- **不适合生产环境**

**适用场景：**
- ✅ 开发/测试环境的本地服务器（http://localhost）
- ✅ 内网可信端点
- ✅ 获取 HTTPS 前测试账号连通性
- ❌ 生产环境（仅使用 HTTPS）

**设置 `allow_insecure_http: false` 后访问 HTTP URL 的错误示例：**
```
Invalid base URL: invalid url scheme: http
```

如关闭 URL 校验或响应头过滤，请加强网络层防护：
- 出站访问白名单限制上游域名/IP
- 阻断私网/回环/链路本地地址
- 强制仅允许 TLS 出站
- 在反向代理层移除敏感响应头

#### OpenAI Responses WebSocket 模式与 HTTP bridge

Codex 风格的 Responses WebSocket 入口支持按账号选择 `off`、`ctx_pool`、`passthrough` 和 `http_bridge`。在账号表单中选择这些模式前，需要先开启 v2 模式路由：

```yaml
gateway:
  openai_ws:
    mode_router_v2_enabled: true
    ingress_mode_default: ctx_pool
    http_bridge_enabled: true
    http_bridge_threshold_bytes: 15728640 # 15 MiB
```

`off` 回退到 HTTP/SSE，`ctx_pool` 使用受管的上游 WebSocket 连接池，`passthrough` 通过上游 WebSocket 适配器透传客户端会话；`http_bridge` 则保留客户端 WebSocket、但让上游 turn 走 HTTP/SSE。开启 bridge 后，超过阈值的大帧可自动切换；Grok WebSocket 入口会使用 HTTP bridge 访问 xAI 的 HTTP/SSE Responses 上游。紧急全局回滚可设置 `gateway.openai_ws.force_http=true`。

```bash
# 7. 运行应用
./sub2api
```

#### HTTP/2 (h2c) 与 HTTP/1.1 回退

主服务仅在 `server.h2c.enabled=true` 时启用明文 HTTP/2；`deploy/config.example.yaml` 已开启该项。HTTP/1.1 始终保留，用于 WebSocket 升级与旧客户端。浏览器通常不直接使用 h2c，收益主要在反向代理或内网链路。

```yaml
server:
  h2c:
    enabled: true
```

**反向代理示例（Caddy）：**

```caddyfile
transport http {
	versions h2c h1
}
```

**验证：**

```bash
# h2c prior knowledge
curl --http2-prior-knowledge -I http://localhost:8080/health
# HTTP/1.1 回退
curl --http1.1 -I http://localhost:8080/health
# WebSocket 回退验证（需管理员 token）
websocat -H="Sec-WebSocket-Protocol: sub2api-admin, jwt.<ADMIN_TOKEN>" ws://localhost:8080/api/v1/admin/ops/ws/qps
```

#### 开发模式

```bash
# 后端（支持热重载）
cd backend
go run ./cmd/server

# 前端（支持热重载）
cd frontend
pnpm run dev
```

#### 代码生成

修改 `backend/ent/schema` 后，需要重新生成 Ent + Wire：

```bash
cd backend
go generate ./ent
go generate ./cmd/server
```

---

## 简易模式

简易模式适合个人开发者或内部团队快速使用，不依赖完整 SaaS 功能。

- 启用方式：设置环境变量 `RUN_MODE=simple`
- 功能差异：隐藏 SaaS 相关功能，跳过计费流程
- 安全注意事项：生产环境需同时设置 `SIMPLE_MODE_CONFIRM=true` 才允许启动

---

## Grok / xAI 支持

Sub2API 同时支持通过 xAI OAuth 接入的 Grok 订阅账号和标准 xAI API Key 账号，并按账号类型转发到对应的 xAI 上游。

### 支持范围

- 平台名：`grok`；账号类型：OAuth 订阅账号、xAI API Key 账号
- Responses：`/v1/responses`、`/responses`、`/backend-api/codex/responses`
- Claude 兼容：`/v1/messages`（转换为 xAI Responses，再返回 Anthropic Messages 结构）
- Chat Completions：`/v1/chat/completions`、`/chat/completions`
- Codex 风格 Responses WebSocket 入口通过 HTTP bridge 访问 xAI HTTP/SSE 上游
- 文本模型包括 `grok-4.5`、`grok-4.3`、`grok-build-0.1`、`grok-composer-2.5-fast` 和 Grok 4.20 reasoning/non-reasoning/multi-agent 系列
- Grok 分组支持图片生成/编辑与视频生成/编辑/扩展/状态查询；生成、编辑和扩展需要开启分组图片生成权限
- 媒体模型包括 `grok-imagine`、`grok-imagine-image-quality`、`grok-imagine-image`、`grok-imagine-image-2.0`、`grok-imagine-edit`、`grok-imagine-video`、`grok-imagine-video-1.5`
- JSON 图片编辑和视频生成请求可在 `image`、`images`、`reference_images`、`mask` 中传入图片；推荐使用 xAI 兼容的 `url`，旧 `image_url` 会在转发前规范化

### OAuth 与 API Key 配置

OAuth 使用 PKCE，不需要提交私有客户端密钥。常用环境变量如下：

| 变量 | 默认值 |
|------|--------|
| `XAI_OAUTH_SCOPE` | `openid profile email offline_access grok-cli:access api:access` |
| `XAI_OAUTH_REDIRECT_URI` | `http://127.0.0.1:56121/callback` |
| `XAI_BASE_URL` | `https://api.x.ai/v1`（转发以账号 `base_url` 为准） |
| `XAI_GROK_CLI_VERSION` | `0.2.114`；低于该版本下限的覆盖值会被忽略 |

管理员可在控制台创建或重新授权 Grok OAuth 账号，也可直接创建 Grok API Key 账号。OAuth 默认推断到 `https://cli-chat-proxy.grok.com/v1`；API Key 账号默认使用官方 `https://api.x.ai/v1`，并复用账号的 `base_url` 与 `api_key` 字段。

### Grok Build CLI 配置

把 Grok 账号加入 Grok 分组并创建对应 Sub2API API Key 后，可在用户 API Key 页面选择 **使用密钥 → Grok CLI** 自动生成配置。手动配置可保存为 `~/.grok/config.toml`（Windows：`%USERPROFILE%\.grok\config.toml`）：

```toml
[models]
default = "grok"
web_search = "grok"

[model."grok"]
model = "grok-4.5"
base_url = "https://your-sub2api.example.com/v1"
name = "Grok 4.5"
api_key = "sk-your-sub2api-key"
api_backend = "responses"
context_window = 1000000
supports_backend_search = true
```

配置文件包含 API Key，请限制文件权限，并使用 `grok inspect` 验证。`base_url` 应是以 `/v1` 结尾的公开 Sub2API 地址，而不是 xAI 内部上游地址。

### 用量、额度与媒体资格

xAI 额度采用被动观测：只有上游实际返回受信的 rate-limit 响应头时才记录；首次获得有效观测前，控制台显示额度未知，同时仍展示 Sub2API 本地用量。`401`、`403`、`429` 会分别按凭据失效、访问/权益失败和短期限流语义处理。

新图片/视频生成还会执行媒体资格检查：API Key 账号默认合格；OAuth 账号必须由 xAI 计费探测确认付费权益。Free、禁止访问、缺失、格式错误或不确定的结果不会参与新的媒体生成；无合格账号时返回 HTTP `503` 和 `grok_media_no_eligible_account`。管理员可通过账号字段 `extra.grok_media_eligible` 覆盖自动判断。

---

## Antigravity 使用说明

Sub2API 支持 [Antigravity](https://antigravity.so/) 账户，授权后可通过专用端点访问 Claude 和 Gemini 模型。

### 专用端点

| 端点 | 模型 |
|------|------|
| `/antigravity/v1/messages` | Claude 模型 |
| `/antigravity/v1beta/` | Gemini 模型 |

### Claude Code 配置示例

```bash
export ANTHROPIC_BASE_URL="http://localhost:8080/antigravity"
export ANTHROPIC_AUTH_TOKEN="sk-xxx"
```

### 混合调度模式

Antigravity 账户支持可选的**混合调度**功能。开启后，通用端点 `/v1/messages` 和 `/v1beta/` 也会调度该账户。

> **⚠️ 注意**：Anthropic Claude 和 Antigravity Claude **不能在同一上下文中混合使用**，请通过分组功能做好隔离。


### 已知问题
在 Claude Code 中，无法自动退出Plan Mode。（正常使用原生Claude Api时，Plan 完成后，Claude Code会弹出弹出选项让用户同意或拒绝Plan。） 
解决办法：shift + Tab，手动退出Plan mode，然后输入内容 告诉 Claude Code 同意或拒绝 Plan
---

## 项目结构

```
sub2api/
├── backend/                  # Go 后端服务
│   ├── cmd/server/           # 应用入口
│   ├── internal/             # 内部模块
│   │   ├── config/           # 配置管理
│   │   ├── model/            # 数据模型
│   │   ├── service/          # 业务逻辑
│   │   ├── handler/          # HTTP 处理器
│   │   └── gateway/          # API 网关核心
│   └── resources/            # 静态资源
│
├── frontend/                 # Vue 3 前端
│   └── src/
│       ├── api/              # API 调用
│       ├── stores/           # 状态管理
│       ├── views/            # 页面组件
│       └── components/       # 通用组件
│
└── deploy/                   # 部署文件
    ├── docker-compose.yml    # Docker Compose 配置
    ├── .env.example          # Docker Compose 环境变量
    ├── config.example.yaml   # 二进制部署完整配置文件
    └── install.sh            # 一键安装脚本
```

## 免责声明

> **使用本项目前请仔细阅读：**
>
> :rotating_light: **服务条款风险**: 使用本项目可能违反 Anthropic 的服务条款。请在使用前仔细阅读 Anthropic 的用户协议，使用本项目的一切风险由用户自行承担。
>
> :book: **免责声明**: 本项目仅供技术学习和研究使用，作者不对因使用本项目导致的账户封禁、服务中断或其他损失承担任何责任。

---

## Star History

<a href="https://star-history.dera.page/#Wei-Shaw/sub2api&Date">
 <picture>
   <source media="(prefers-color-scheme: dark)" srcset="https://star-history.dera.page/svg?repos=Wei-Shaw/sub2api&type=Date&theme=dark" />
   <source media="(prefers-color-scheme: light)" srcset="https://star-history.dera.page/svg?repos=Wei-Shaw/sub2api&type=Date" />
   <img alt="Star History Chart" src="https://star-history.dera.page/svg?repos=Wei-Shaw/sub2api&type=Date" />
 </picture>
</a>

---

## 许可证

本项目基于 [GNU 宽通用公共许可证 v3.0](LICENSE)（或更高版本）授权。

Copyright (c) 2026 Wesley Liddick

---

<div align="center">

**如果觉得有用，请给个 Star 支持一下！**

</div>
