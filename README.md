<div align="center">

<img src="assets/logo.svg" alt="Sub2API Logo" width="128" />

# Sub2API

[![Go](https://img.shields.io/badge/Go-1.26.6-00ADD8.svg)](https://golang.org/)
[![Vue](https://img.shields.io/badge/Vue-3.4+-4FC08D.svg)](https://vuejs.org/)
[![PostgreSQL](https://img.shields.io/badge/PostgreSQL-15+-336791.svg)](https://www.postgresql.org/)
[![Redis](https://img.shields.io/badge/Redis-7+-DC382D.svg)](https://redis.io/)
[![Docker](https://img.shields.io/badge/Docker-Ready-2496ED.svg)](https://www.docker.com/)

<a href="https://trendshift.io/repositories/21823" target="_blank"><img src="https://trendshift.io/api/badge/repositories/21823" alt="Wei-Shaw%2Fsub2api | Trendshift" width="250" height="55"/></a>

**AI API Gateway Platform for Subscription Quota Distribution**

English | [中文](README_CN.md) | [日本語](README_JA.md)

</div>

## ⚠️ Important Notice

Please read the following carefully before using this project:

- **🚨 Terms of Service Risk**: Using this project may violate the terms of service of Anthropic and other upstream providers. Please review the relevant providers' user agreements before use; all risks arising from such use are borne solely by the user.
- **⚖️ Compliant Use**: Use this project only in compliance with the laws and regulations of your country or region. Any unlawful use is strictly prohibited.
- **📖 Disclaimer**: This project is provided for technical learning and research purposes only. The authors assume no liability for account bans, service interruptions, data loss, or any other direct or indirect damages resulting from the use of this project.

## Overview

Sub2API is an AI API gateway platform designed to distribute and manage API quotas from AI product subscriptions. Users can access upstream AI services through platform-generated API Keys, while the platform handles authentication, billing, load balancing, and request forwarding.

## Features

- **Multi-Account Management** - Support multiple upstream account types (OAuth, API Key)
- **API Key Distribution** - Generate and manage API Keys for users
- **Precise Billing** - Token-level usage tracking and cost calculation
- **Smart Scheduling** - Intelligent account selection with sticky sessions
- **Concurrency Control** - Per-user and per-account concurrency limits
- **Rate Limiting** - Configurable request and token rate limits
- **Built-in Payment System** - Supports EasyPay, Alipay, WeChat Pay, and Stripe for user self-service top-up, no separate payment service needed ([Configuration Guide](docs/PAYMENT.md))
- **Admin Dashboard** - Web interface for monitoring and management
- **Composite Groups** - Admin routing layer that resolves requested models to concrete providers for multi-provider groups ([Operator Guide](docs/COMPOSITE_GROUPS.md))
- **External System Integration** - Embed external systems (e.g. ticketing) via iframe to extend the admin dashboard

## ❤️ Sponsors

> [Want to appear here?](mailto:support@pincc.ai)

<table>

<tr>
<td width="180"><a href="https://www.openmodel.ai?ref=sub2api"><img src="assets/partners/logos/openmodel.jpg" alt="openmodel" width="150"></a></td>
<td>One API, every top model! <a href="https://www.openmodel.ai?ref=sub2api">OpenModel</a> is a production-grade, high-availability AI API gateway that makes your applications truly fast and stable: automatic failover, smart routing to the best-performing channel, and a production-grade SLA. An SLA that far surpasses any single provider — making stability your core competitive advantage. Works directly with Claude Code, Codex, and Gemini CLI. Register via this link to get started.</td>
</tr>

<tr>
<td width="180"><a href="https://poixe.com/i/sub2api"><img src="assets/partners/logos/poixe.png" alt="PoixeAi" width="150"></a></td>
<td>Thanks to Poixe Ai for sponsoring this project! Poixe AI provides reliable LLM API services. You can leverage the platform's API endpoints to seamlessly build AI-powered products. Additionally, you can become a vendor by providing AI API resources to the platform and earn revenue. Register through the exclusive <a href="https://poixe.com/i/sub2api">sub2api</a> referral link and receive a bonus of $5 USD on your first top-up.</td>
</tr>

<tr>
<td width="180"><a href="https://ctok.ai"><img src="assets/partners/logos/ctok.png" alt="CTok" width="150"></a></td>
<td>Thanks to CTok.ai for sponsoring this project! CTok.ai is dedicated to building a one-stop AI programming tool service platform. We offer professional Claude Code packages and technical community services, with support for Google Gemini and OpenAI Codex. Through carefully designed plans and a professional tech community, we provide developers with reliable service guarantees and continuous technical support, making AI-assisted programming a true productivity tool. Click <a href="https://ctok.ai">here</a> to register!</td>
</tr>

<tr>
<td width="180"><a href="https://code.silkapi.com/"><img src="assets/partners/logos/silkapi.png" alt="silkapi" width="150"></a></td>
<td>Thanks to SilkAPI for sponsoring this project! <a href="https://code.silkapi.com/">SilkAPI</a> is a relay service built on Sub2API, specializing in providing high-speed and stable Codex API relay.</td>
</tr>

<tr>
<td width="180"><a href="https://ylscode.com/"><img src="assets/partners/logos/ylscode.png" alt="ylscode" width="150"></a></td>
<td>Thanks to YLS Code for sponsoring this project! <a href="https://ylscode.com/">YLS Code</a> is dedicated to building secure enterprise-grade Coding Agent productivity services, offering stable and fast Codex / Claude / Gemini subscription services along with pay-as-you-go API options for flexible choices. Register now for a limited-time 3-day Codex trial bonus!</td>
</tr>

<tr>
<td width="180"><a href="https://aigocode.com/invite/SUB2API"><img src="assets/partners/logos/aigocode.png" alt="AIGoCode" width="150"></a></td>
<td>Thanks to AIGoCode for sponsoring this project! AIGoCode is an all-in-one platform that integrates Claude Code, Codex, and the latest Gemini models, providing you with stable, efficient, and highly cost-effective AI coding services. The platform offers flexible subscription plans, zero risk of account suspension, direct access with no VPN required, and lightning-fast responses. AIGoCode has prepared a special benefit for sub2api users: if you register via <a href="https://aigocode.com/invite/SUB2API">this link</a>, you'll receive an extra 10% bonus credit on your first top-up!</td>
</tr>

<tr>
<td width="180"><a href="https://shop.bmoplus.com/?utm_source=github"><img src="assets/partners/logos/bmoplus.jpg" alt="bmoplus" width="150"></a></td>
<td>Huge thanks to BmoPlus for sponsoring this project! BmoPlus is a highly reliable AI account provider built strictly for heavy AI users and developers. They offer rock-solid, ready-to-use accounts and official top-up services for ChatGPT Plus / ChatGPT Pro (Full Warranty) / Claude Pro / Super Grok / Gemini Pro. By registering and ordering through <a href="https://shop.bmoplus.com/?utm_source=github">BmoPlus - Premium AI Accounts & Top-ups</a>, users can unlock the mind-blowing rate of 10% of the official GPT subscription price (90% OFF)</td>
</tr>

<tr>
<td width="180"><a href="https://bestproxy.com/?keyword=a2e8iuol"><img src="assets/partners/logos/bestproxy.png" alt="bestproxy" width="150"></a></td>
<td>Thanks to Bestproxy for sponsoring this project! <a href="https://bestproxy.com/?keyword=a2e8iuol">Bestproxy</a> provides high-purity residential IPs with dedicated one-IP-per-account support. By combining real home networks with fingerprint isolation, it enables link environment isolation and reduces the probability of association-based risk control.</td>
</tr>

<tr>
<td width="180"><a href="https://pateway.ai/?ch=1tsfr51"><img src="assets/partners/logos/pateway.png" alt="pateway" width="150"></a></td>
<td>Thanks to PatewayAI for sponsoring this project! PatewayAI is a premium model API relay service provider built for heavy AI developers, focused on direct official connections. Offering the full Claude series and Codex series models, 100% sourced directly from official providers — no dilution, no substitution, open to verification. Billing is fully transparent with token-level invoices that can be audited line by line.
Enterprise-grade high concurrency is also supported, with a dedicated management platform for enterprise clients. Enterprise customers can sign formal contracts and receive invoices. Visit the official website for more details and contact information.
Register now via <a href="https://pateway.ai/?ch=1tsfr51">this link</a> to receive $3 in trial credits. User top-ups start as low as 60% off, and referring friends earns both parties rewards — referral bonuses up to $150.</td>
</tr>

</table>

## Ecosystem

Community projects that extend or integrate with Sub2API:

| Project | Description | Features |
|---------|-------------|----------|
| ~~[Sub2ApiPay](https://github.com/touwaeriol/sub2apipay)~~ | ~~Self-service payment system~~ | **Now Built-in** — Payment is now integrated into Sub2API, no separate deployment needed. See [Payment Configuration Guide](docs/PAYMENT.md) |
| [sub2api-mobile](https://github.com/ckken/sub2api-mobile) | Mobile admin console | Cross-platform app (iOS/Android/Web) for user management, account management, monitoring dashboard, and multi-backend switching; built with Expo + React Native |

## Tech Stack

| Component | Technology |
|-----------|------------|
| Backend | Go 1.26.6, Gin, Ent |
| Frontend | Vue 3.4+, Vite 5+, TailwindCSS |
| Database | PostgreSQL 15+ |
| Cache/Queue | Redis 7+ |

---

## Nginx Reverse Proxy Note

When using Nginx as a reverse proxy for Sub2API (or CRS) with Codex CLI, add the following to the `http` block in your Nginx configuration:

```nginx
underscores_in_headers on;
```

Nginx drops headers containing underscores by default (e.g. `session_id`), which breaks sticky session routing in multi-account setups.

---

## Deployment

### Method 1: Script Installation (Recommended)

One-click installation script that downloads pre-built binaries from GitHub Releases.

#### Prerequisites

- Linux server (amd64 or arm64)
- PostgreSQL 15+ (installed and running)
- Redis 7+ (installed and running)
- Root privileges

#### Installation Steps

```bash
curl -sSL https://raw.githubusercontent.com/Wei-Shaw/sub2api/main/deploy/install.sh | sudo bash
```

The script will:
1. Detect your system architecture
2. Download the latest release
3. Install binary to `/opt/sub2api`
4. Create systemd service
5. Configure system user and permissions

#### Post-Installation

```bash
# 1. Start the service
sudo systemctl start sub2api

# 2. Enable auto-start on boot
sudo systemctl enable sub2api

# 3. Open Setup Wizard in browser
# http://YOUR_SERVER_IP:8080
```

The Setup Wizard will guide you through:
- Database configuration
- Redis configuration
- Admin account creation

#### Upgrade

You can upgrade directly from the **Admin Dashboard** by clicking the **Check for Updates** button in the top-left corner.

The web interface will:
- Check for new versions automatically
- Download and apply updates with one click
- Support rollback if needed

#### Useful Commands

```bash
# Check status
sudo systemctl status sub2api

# View logs
sudo journalctl -u sub2api -f

# Restart service
sudo systemctl restart sub2api

# Uninstall
curl -sSL https://raw.githubusercontent.com/Wei-Shaw/sub2api/main/deploy/install.sh | sudo bash -s -- uninstall -y
```

---

### Method 2: Docker Compose (Recommended)

Deploy with Docker Compose, including PostgreSQL and Redis containers.

#### Prerequisites

- Docker 20.10+
- Docker Compose v2+

#### Quick Start (One-Click Deployment)

Use the automated deployment script for easy setup:

```bash
# Create deployment directory
mkdir -p sub2api-deploy && cd sub2api-deploy

# Download and run deployment preparation script
curl -sSL https://raw.githubusercontent.com/Wei-Shaw/sub2api/main/deploy/docker-deploy.sh | bash

# Start services
docker compose up -d

# View logs
docker compose logs -f sub2api
```

**What the script does:**
- Downloads `docker-compose.local.yml` (saved as `docker-compose.yml`) and `.env.example`
- Generates secure credentials (JWT_SECRET, TOTP_ENCRYPTION_KEY, POSTGRES_PASSWORD)
- Creates `.env` file with auto-generated secrets
- Creates data directories (uses local directories for easy backup/migration)
- Displays generated credentials for your reference

#### Manual Deployment

If you prefer manual setup:

```bash
# 1. Clone the repository
git clone https://github.com/Wei-Shaw/sub2api.git
cd sub2api/deploy

# 2. Copy environment configuration
cp .env.example .env
chmod 600 .env

# 3. Edit configuration (generate secure passwords)
nano .env
```

**Required configuration in `.env`:**

```bash
# PostgreSQL password (REQUIRED)
POSTGRES_PASSWORD=your_secure_password_here

# JWT Secret (RECOMMENDED - keeps users logged in after restart)
JWT_SECRET=your_jwt_secret_here

# TOTP Encryption Key (RECOMMENDED - preserves 2FA after restart)
TOTP_ENCRYPTION_KEY=your_totp_key_here

# Optional: Admin account
ADMIN_EMAIL=admin@example.com
ADMIN_PASSWORD=your_admin_password

# Optional: Custom port
SERVER_PORT=8080
```

**Generate secure secrets:**
```bash
# Generate JWT_SECRET
openssl rand -hex 32

# Generate TOTP_ENCRYPTION_KEY
openssl rand -hex 32

# Generate POSTGRES_PASSWORD
openssl rand -hex 32
```

```bash
# 4. Create data directories (for local version)
mkdir -p data postgres_data redis_data

# 5. Start all services
# Option A: Local directory version (recommended - easy migration)
docker compose -f docker-compose.local.yml up -d

# Option B: Named volumes version (simple setup)
docker compose up -d

# 6. Check status
docker compose -f docker-compose.local.yml ps

# 7. View logs
docker compose -f docker-compose.local.yml logs -f sub2api
```

#### Deployment Versions

| Version | Data Storage | Migration | Best For |
|---------|-------------|-----------|----------|
| **docker-compose.local.yml** | Local directories | ✅ Easy (tar entire directory) | Production, frequent backups |
| **docker-compose.yml** | Named volumes | ⚠️ Requires docker commands | Simple setup |

**Recommendation:** Use `docker-compose.local.yml` (deployed by script) for easier data management.

#### Access

Open `http://YOUR_SERVER_IP:8080` in your browser.

If admin password was auto-generated, find it in logs:
```bash
docker compose -f docker-compose.local.yml logs sub2api | grep "admin password"
```

#### Upgrade

```bash
# Pull latest image and recreate container
docker compose -f docker-compose.local.yml pull
docker compose -f docker-compose.local.yml up -d
```

#### Easy Migration (Local Directory Version)

When using `docker-compose.local.yml`, migrate to a new server easily:

```bash
# On source server
docker compose -f docker-compose.local.yml down
cd ..
tar czf sub2api-complete.tar.gz sub2api-deploy/

# Transfer to new server
scp sub2api-complete.tar.gz user@new-server:/path/

# On new server
tar xzf sub2api-complete.tar.gz
cd sub2api-deploy/
docker compose -f docker-compose.local.yml up -d
```

#### Useful Commands

```bash
# Stop all services
docker compose -f docker-compose.local.yml down

# Restart
docker compose -f docker-compose.local.yml restart

# View all logs
docker compose -f docker-compose.local.yml logs -f

# Remove all data (caution!)
docker compose -f docker-compose.local.yml down
rm -rf data/ postgres_data/ redis_data/
```

---

### Method 3: Apple container (macOS)

Apple-silicon Macs running macOS 26 can run the full Sub2API, PostgreSQL, and Redis stack with Apple `container` 1.1.0 or newer:

```bash
git clone https://github.com/Wei-Shaw/sub2api.git
cd sub2api/deploy
./apple-container.sh init
./apple-container.sh up
./apple-container.sh status
```

This is an operator-managed local workflow; Docker Compose remains the recommended production path. See [deploy/APPLE_CONTAINER.md](deploy/APPLE_CONTAINER.md) for lifecycle commands, persistence, upgrades, and runtime limitations.

---

### Method 4: Build from Source

Build and run from source code for development or customization.

#### Prerequisites

- Go 1.21+
- Node.js 18+
- PostgreSQL 15+
- Redis 7+

#### Build Steps

```bash
# 1. Clone the repository
git clone https://github.com/Wei-Shaw/sub2api.git
cd sub2api

# 2. Install pnpm (if not already installed)
npm install -g pnpm

# 3. Build frontend
cd frontend
pnpm install
pnpm run build
# Output will be in ../backend/internal/web/dist/

# 4. Build backend with embedded frontend
cd ../backend
VERSION="$(./scripts/resolve-version.sh)"
go build -tags embed -ldflags="-X main.Version=${VERSION}" -o sub2api ./cmd/server

# 5. Create configuration file
cp ../deploy/config.example.yaml ./config.yaml

# 6. Edit configuration
nano config.yaml
```

> **Note:** The `-tags embed` flag embeds the frontend into the binary. Without this flag, the binary will not serve the frontend UI.

**Key configuration in `config.yaml`:**

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
  username: ""
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

**First-run administrator:** An existing `config.yaml` skips the web setup wizard, and `default.admin_email` / `default.admin_password` do not create an administrator. On a fresh source install, either omit the create/edit configuration steps, start `./sub2api`, and let `http://<host>:8080` create the configuration and administrator, or bootstrap with `./sub2api -setup` / `AUTO_SETUP`. Only pre-create the configuration manually when the target database already has an administrator.

Additional security-related options are available in `config.yaml`:

- `cors.allowed_origins` for CORS allowlist
- `security.url_allowlist` for upstream/pricing/CRS host allowlists
- `security.url_allowlist.enabled` to disable URL validation (use with caution)
- `security.url_allowlist.allow_insecure_http` to allow HTTP URLs when validation is disabled
- `security.url_allowlist.allow_private_hosts` to allow private/local IP addresses
- `security.response_headers.enabled` to enable configurable response header filtering (disabled uses default allowlist)
- `security.csp` to control Content-Security-Policy headers
- `billing.circuit_breaker` to fail closed on billing errors
- `server.trusted_proxies` to define the exact proxy IPs/CIDRs trusted by Gin; an empty list disables proxy trust
- `security.trust_forwarded_ip_for_api_key_acl` to opt API-key IP allow/deny lists into legacy raw forwarded-header extraction instead of the Gin trusted-proxy chain (default `false`)
- `turnstile.required` to require Turnstile in release mode

API-key ACLs use Gin's trusted-proxy chain by default. Keep the legacy takeover disabled and configure only the reverse proxies that connect directly to Sub2API in `server.trusted_proxies`. If compatibility requires enabling raw `CF-Connecting-IP`, `X-Real-IP`, or `X-Forwarded-For` extraction, firewall the origin to the proxy/CDN and make the edge overwrite those headers.

**⚠️ Security Warning: HTTP URL Configuration**

When `security.url_allowlist.enabled=false`, the current default performs minimal validation and **allows HTTP URLs**. This is useful for local or private-network development, but production deployments should explicitly require HTTPS:

```yaml
security:
  url_allowlist:
    enabled: false                # Disable allowlist checks
    allow_insecure_http: false    # HTTPS only (recommended for production)
```

**Or via environment variable:**

```bash
SECURITY_URL_ALLOWLIST_ENABLED=false
SECURITY_URL_ALLOWLIST_ALLOW_INSECURE_HTTP=false
```

**Risks of allowing HTTP:**
- API keys and data transmitted in **plaintext** (vulnerable to interception)
- Susceptible to **man-in-the-middle (MITM) attacks**
- **NOT suitable for production** environments

**When to use HTTP:**
- ✅ Development/testing with local servers (http://localhost)
- ✅ Internal networks with trusted endpoints
- ✅ Testing account connectivity before obtaining HTTPS
- ❌ Production environments (use HTTPS only)

**Example error for an HTTP URL when `allow_insecure_http: false` is set:**
```
Invalid base URL: invalid url scheme: http
```

If you disable URL validation or response header filtering, harden your network layer:
- Enforce an egress allowlist for upstream domains/IPs
- Block private/loopback/link-local ranges
- Enforce TLS-only outbound traffic
- Strip sensitive upstream response headers at the proxy

#### OpenAI Responses WebSocket modes and HTTP bridge

Codex-style Responses WebSocket ingress can use the per-account v2 modes `off`, `ctx_pool`, `passthrough`, and `http_bridge`. Enable the v2 mode router before selecting these modes in the account form:

```yaml
gateway:
  openai_ws:
    mode_router_v2_enabled: true
    ingress_mode_default: ctx_pool
    http_bridge_enabled: true
    http_bridge_threshold_bytes: 15728640 # 15 MiB
```

`off` falls back to HTTP/SSE, `ctx_pool` uses the managed upstream WebSocket pool, and `passthrough` forwards the client session through the upstream WebSocket adapter. `http_bridge` keeps the client-facing WebSocket but runs the upstream turn over HTTP/SSE. With the bridge enabled, oversized request frames can hand off automatically at the configured threshold; Grok WebSocket ingress uses the HTTP bridge because xAI is reached through its HTTP/SSE Responses endpoint. Set `gateway.openai_ws.force_http=true` for an emergency global rollback.

```bash
# 7. Run the application
./sub2api
```

#### HTTP/2 (h2c) and HTTP/1.1 fallback

The main cleartext listener enables h2c when `server.h2c.enabled=true` (`deploy/config.example.yaml` enables it). HTTP/1.1 remains available for WebSocket upgrades and legacy clients:

```yaml
server:
  h2c:
    enabled: true
```

For a Caddy hop, advertise both protocols with `versions h2c h1`. Verify the backend with `curl --http2-prior-knowledge -I http://localhost:8080/health` and `curl --http1.1 -I http://localhost:8080/health`.

#### Development Mode

```bash
# Backend (with hot reload)
cd backend
go run ./cmd/server

# Frontend (with hot reload)
cd frontend
pnpm run dev
```

#### Code Generation

When editing `backend/ent/schema`, regenerate Ent + Wire:

```bash
cd backend
go generate ./ent
go generate ./cmd/server
```

---

## Simple Mode

Simple Mode is designed for individual developers or internal teams who want quick access without full SaaS features.

- Enable: Set environment variable `RUN_MODE=simple`
- Difference: Hides SaaS-related features and skips billing process
- Security note: In production, you must also set `SIMPLE_MODE_CONFIRM=true` to allow startup

---

## Grok / xAI Support

Sub2API supports both Grok subscription accounts through xAI OAuth and standard xAI API-key accounts. Both account types forward OpenAI-compatible traffic to their account-specific xAI upstream.

### Supported Scope

- Platform name: `grok`
- Account types: OAuth subscription accounts and xAI API-key accounts
- Public Responses targets: `/v1/responses`, `/responses`, and `/backend-api/codex/responses`, forwarded to the Grok subscription proxy for OAuth accounts or `https://api.x.ai/v1/responses` for API-key accounts
- Public Claude-compatible target: `/v1/messages`, converted to xAI Responses and returned as Anthropic Messages output for Claude CLI style clients
- Public Chat Completions targets: `/v1/chat/completions` and `/chat/completions`, forwarded to the account-type-specific xAI upstream
- Codex CLI style Responses WebSocket ingress is accepted on the Responses targets and bridged to xAI HTTP/SSE Responses upstream
- Text models: `grok-4.5`, `grok-4.3`, `grok-build-0.1`, `grok-composer-2.5-fast`, `grok-4.20-0309-reasoning`, `grok-4.20-0309-non-reasoning`, and `grok-4.20-multi-agent-0309`
- Media targets for Grok groups: `/v1/images/generations`, `/images/generations`, `/v1/images/edits`, `/images/edits`, `/v1/videos/generations`, `/videos/generations`, `/v1/videos/edits`, `/videos/edits`, `/v1/videos/extensions`, `/videos/extensions`, `/v1/videos/{request_id}`, and `/videos/{request_id}`. Generation, editing, and extension requests require the group image-generation permission.
- Media models: `grok-imagine`, `grok-imagine-image-quality`, `grok-imagine-image`, `grok-imagine-image-2.0`, `grok-imagine-edit`, `grok-imagine-video`, and `grok-imagine-video-1.5`
- JSON image-edit and video-generation requests accept image references in `image`, `images`, `reference_images`, and `mask` objects. Use `url` for xAI-compatible payloads; the legacy `image_url` field remains accepted and is normalized to `url` before forwarding.
- Out of scope for this provider: TTS, transcription, browser automation, cookies, and Grok web scraping

### OAuth Configuration

The Grok OAuth flow uses PKCE and does not require committing private secrets. The default client details follow the public xAI OAuth flow used by compatible clients, and every value can be overridden by environment variable:

| Variable | Default |
|----------|---------|
| `XAI_OAUTH_CLIENT_ID` | Public xAI OAuth client ID |
| `XAI_OAUTH_SCOPE` | `openid profile email offline_access grok-cli:access api:access` |
| `XAI_OAUTH_REDIRECT_URI` | `http://127.0.0.1:56121/callback` |
| `XAI_OAUTH_AUTHORIZE_URL` | `https://auth.x.ai/oauth2/authorize` |
| `XAI_OAUTH_TOKEN_URL` | `https://auth.x.ai/oauth2/token` |
| `XAI_BASE_URL` | `https://api.x.ai/v1`; runtime-diagnostics override (account `base_url` controls request forwarding) |
| `XAI_GROK_CLI_VERSION` | `0.2.114`; optional override for the client identity sent to `cli-chat-proxy.grok.com`. Overrides below this floor are ignored |

Administrators can create Grok OAuth or API-key accounts from the dashboard. OAuth authorization and reauthorization are also available through the admin API:

| Endpoint | Purpose |
|----------|---------|
| `POST /api/v1/admin/grok/oauth/auth-url` | Generate an xAI OAuth authorization URL |
| `POST /api/v1/admin/grok/oauth/exchange-code` | Exchange a callback URL, query string, or code for OAuth credentials |
| `POST /api/v1/admin/grok/oauth/refresh-token` | Validate or refresh a Grok refresh token |
| `POST /api/v1/admin/grok/accounts/:id/refresh` | Refresh an existing Grok account |

OAuth credential storage reuses the existing account JSON fields: `access_token`, `refresh_token`, `token_type`, `expires_at`, `base_url`, optional `email`, optional `subscription_tier`, and `entitlement_status`. New OAuth accounts default to `https://cli-chat-proxy.grok.com/v1`; explicitly stored official API, regional API, and custom upstreams remain pinned to their configured endpoint.

For API-key accounts, select **Grok → API Key** in the create-account dialog. The official base URL defaults to `https://api.x.ai/v1`; credentials use the existing `base_url` and `api_key` fields.

### Grok Build CLI Configuration

1. Add a Grok OAuth or API-key account, attach it to a Grok group, and create a Sub2API API key for that group.
2. On the user API-key page, click **Use Key → Grok CLI** to generate the correct macOS/Linux or Windows configuration (the OpenCode tab is also supported).
3. For a manual setup, save the following as `~/.grok/config.toml` (Windows: `%USERPROFILE%\.grok\config.toml`):

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

Back up an existing file, restrict its permissions because it contains an API key, then verify it with `grok inspect`. The `base_url` is the public Sub2API URL ending in `/v1`, not an internal xAI URL.

### Usage And Quota Display

xAI quota is passive. Sub2API does not invent subscription quota values; it records whitelisted xAI rate-limit headers from successful or rate-limited upstream responses when xAI sends them. Before the first usable upstream response, the dashboard shows quota as unknown and still displays local Sub2API usage stats.

`401` responses temporarily remove accounts with invalid credentials from scheduling. `403` responses are treated as access or entitlement failures instead of token-refresh loops. `429` responses use `Retry-After` or a short cooldown to temporarily remove the account from scheduling.

New Grok image and video generation requests use a media-specific eligibility check. API-key accounts are eligible by default. OAuth accounts require positive paid-entitlement evidence from the xAI billing probe; Free, forbidden, missing, malformed, and inconclusive observations are excluded from new media generation. If no eligible account remains, the endpoint returns HTTP `503` with error type `grok_media_no_eligible_account`. Administrators can override automatic eligibility with account field `extra.grok_media_eligible`; successful image responses must contain actual image output, otherwise failover is attempted.

---

## Antigravity Support

Sub2API supports [Antigravity](https://antigravity.so/) accounts. After authorization, dedicated endpoints are available for Claude and Gemini models.

### Dedicated Endpoints

| Endpoint | Model |
|----------|-------|
| `/antigravity/v1/messages` | Claude models |
| `/antigravity/v1beta/` | Gemini models |

### Claude Code Configuration

```bash
export ANTHROPIC_BASE_URL="http://localhost:8080/antigravity"
export ANTHROPIC_AUTH_TOKEN="sk-xxx"
```

### Hybrid Scheduling Mode

Antigravity accounts support optional **hybrid scheduling**. When enabled, the general endpoints `/v1/messages` and `/v1beta/` will also route requests to Antigravity accounts.

> **⚠️ Warning**: Anthropic Claude and Antigravity Claude **cannot be mixed within the same conversation context**. Use groups to isolate them properly.

### Known Issues

In Claude Code, Plan Mode cannot exit automatically. (Normally when using the native Claude API, after planning is complete, Claude Code will pop up options for users to approve or reject the plan.)

**Workaround**: Press `Shift + Tab` to manually exit Plan Mode, then type your response to approve or reject the plan.

---

## Project Structure

```
sub2api/
├── backend/                  # Go backend service
│   ├── cmd/server/           # Application entry
│   ├── internal/             # Internal modules
│   │   ├── config/           # Configuration
│   │   ├── model/            # Data models
│   │   ├── service/          # Business logic
│   │   ├── handler/          # HTTP handlers
│   │   └── gateway/          # API gateway core
│   └── resources/            # Static resources
│
├── frontend/                 # Vue 3 frontend
│   └── src/
│       ├── api/              # API calls
│       ├── stores/           # State management
│       ├── views/            # Page components
│       └── components/       # Reusable components
│
└── deploy/                   # Deployment files
    ├── docker-compose.yml    # Docker Compose configuration
    ├── .env.example          # Environment variables for Docker Compose
    ├── config.example.yaml   # Full config file for binary deployment
    └── install.sh            # One-click installation script
```

## Disclaimer

> **Please read carefully before using this project:**
>
> :rotating_light: **Terms of Service Risk**: Using this project may violate Anthropic's Terms of Service. Please read Anthropic's user agreement carefully before use. All risks arising from the use of this project are borne solely by the user.
>
> :book: **Disclaimer**: This project is for technical learning and research purposes only. The author assumes no responsibility for account suspension, service interruption, or any other losses caused by the use of this project.

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

## License

This project is licensed under the [GNU Lesser General Public License v3.0](LICENSE) (or later).

Copyright (c) 2026 Wesley Liddick

---

<div align="center">

**If you find this project useful, please give it a star!**

</div>
