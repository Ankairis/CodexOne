# CodexOne

一个面向个人自用的单账号 Codex 反向代理。它把自己的 ChatGPT/Codex 登录转换为 OpenAI 兼容的 `/v1` Base URL，并提供一套完全独立编写的管理后台。

**Co-author: [YnSaki](https://github.com/YnSaki)**

> CodexOne 是非官方社区项目，与 OpenAI 无隶属或背书关系。请仅代理你本人有权使用的账号，并自行遵守适用的服务条款。项目不提供账号池、订阅共享、多人额度分发、计费或转售功能。

## 核心边界

- **物理单账号**：数据库只有固定 `id = 1` 的账号行，并带约束；重新登录只会替换该账号，不存在账号列表、轮询、故障切换或权重。
- **独立前端**：React 管理后台从零编写，没有复用 CLIProxyAPI 或 sub2api 的前端代码与资源。
- **固定 Codex 身份**：转发到上游时强制覆盖 `User-Agent`、`Originator`、`Version`、`Authorization` 和 ChatGPT Account ID，下游无法透传这些值。
- **不是透明代理**：Responses 请求会按 Codex 后端要求标准化；Chat Completions 会转换为 Responses 协议后再转换回来。
- **只记录元数据**：请求日志保存时间、模型、状态、耗时和 token 数，不保存提示词或响应正文。

这里的客户端身份处理只是应用层请求头与请求体兼容，不承诺“不可检测”，也不会改变账号权限或服务端规则。

## 功能

管理后台只有三个主页面：

1. **总览**：当天请求数、成功率、平均耗时、token 统计、逐条请求详情和应用日志。
2. **账号**：Codex 浏览器 PKCE 登录（粘贴 localhost 回调 URL）、设备码登录、导入官方 `auth.json`、额度查看与刷新、管理员密码修改。
3. **API Key**：创建、查看和撤销 `sk-codexone-*` Key。明文只在创建时显示一次，数据库只存 SHA-256 摘要。

兼容入口：

- `GET /v1/models`
- `POST /v1/responses`
- `POST /v1/responses/compact`
- `POST /v1/responses/input_tokens`
- `POST /v1/chat/completions`

Responses 与 Chat Completions 均支持流式和非流式调用。Chat Completions 会透传 `reasoning_effort`（未指定时默认 `medium`），并把 Codex 推理摘要映射为 `reasoning_content`，同时返回 `completion_tokens_details.reasoning_tokens`。

## 快速部署：SQLite

要求 Docker Compose。默认只监听宿主机 `127.0.0.1:8080`，建议在前面放 Caddy 或 Nginx 提供 HTTPS。

```bash
cp .env.example .env
# 编辑 .env，把 PUBLIC_URL 改为你的 HTTPS 域名
docker compose up -d --build
docker compose logs codexone
```

若 `PUBLIC_URL` 仍是示例值 `https://xxx.xxx.com`，服务会直接拒绝启动，避免出现“能登录但后台写操作被 Origin 校验拒绝”的半可用状态。

首次启动如果未设置 `ADMIN_PASSWORD`，日志会且只会显示一次随机后台密码。打开 `PUBLIC_URL` 登录后应立即修改密码。

SQLite 数据库、加密主密钥和日志都保存在 `codexone-data` volume。**必须同时备份数据库与 `master.key`**；丢失主密钥后，已保存的 OAuth Token 无法恢复。

## PostgreSQL + Redis 部署

此模式把账号、API Key、请求记录和设置存入 PostgreSQL；Redis 只保存管理员会话与尚未完成的 OAuth/设备码登录状态。

先在 `.env` 中设置：

```dotenv
PUBLIC_URL=https://xxx.xxx.com
POSTGRES_PASSWORD=replace-with-a-long-random-password
MASTER_KEY=replace-with-32-byte-base64-key
```

生成主密钥：

```bash
openssl rand -base64 32
```

然后启动：

```bash
docker compose -f docker-compose.postgres.yml up -d --build
docker compose -f docker-compose.postgres.yml logs codexone
```

生产环境使用外部 PostgreSQL/Redis 时，只需设置 `STORAGE_DRIVER=postgres`、`DATABASE_URL`、`REDIS_URL` 与 `MASTER_KEY`。

## 配置 HTTPS 域名

将 [`deploy/Caddyfile`](deploy/Caddyfile) 中的 `xxx.xxx.com` 换成真实域名，并确保域名解析到服务器。Base URL 就是：

```text
https://xxx.xxx.com/v1
```

Caddy 配置已关闭反向代理响应缓冲，以便 SSE 实时输出。若使用 Nginx，也要为 `/v1` 关闭 buffering。

Docker 镜像自带 `/healthz` 健康检查，会验证应用、数据库和会话存储；请求元数据会在启动时及其后每 24 小时按保留期清理一次。

## 客户端调用

```bash
curl https://xxx.xxx.com/v1/responses \
  -H "Authorization: Bearer sk-codexone-你的Key" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-5.4","input":"hello","stream":false}'
```

把兼容客户端的 Base URL 设为 `https://xxx.xxx.com/v1`，API Key 使用后台创建的值即可。

## 本地开发

需要 Go 1.26.6 或更高补丁版本、Node.js 24 和 npm。

```bash
cd web
npm ci
npm run build
cd ..
go test ./...
go run ./cmd/server
```

前端生产文件会输出到 `internal/web/dist`，随后由 Go 二进制嵌入。开发模式可分别运行后端与 `npm run dev`。

默认本地配置：

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `LISTEN_ADDR` | `:8080` | HTTP 监听地址 |
| `PUBLIC_URL` | `http://localhost:8080` | 后台公开地址；Base URL 自动追加 `/v1` |
| `STORAGE_DRIVER` | `sqlite` | `sqlite` 或 `postgres` |
| `SQLITE_PATH` | `./data/codexone.db` | SQLite 文件位置 |
| `MASTER_KEY_FILE` | 与 SQLite 同目录的 `master.key` | SQLite 模式自动生成 |
| `LOG_PATH` | `./data/codexone.log` | JSON 行日志 |
| `APP_TIMEZONE` | `Asia/Shanghai` | “当天”统计所用时区 |
| `TRUSTED_PROXY_CIDRS` | 空 | 可提供 `X-Forwarded-For` 的直连反向代理 IP/CIDR，逗号分隔 |
| `REQUEST_RETENTION_DAYS` | `30` | 请求元数据保留天数 |
| `MAX_REQUEST_MIB` | `32` | 单个代理请求正文上限 |
| `SESSION_TTL_HOURS` | `24` | 后台登录有效期 |
| `CODEX_CLIENT_VERSION` | `0.146.0` | 上游请求中的 Codex 版本 |

`ADMIN_PASSWORD` 仅在数据库第一次初始化时生效；后续修改环境变量不会覆盖后台中保存的密码。

## 安全说明

- OAuth Token 使用 AES-256-GCM 加密保存；API Key 明文不落盘。
- 后台 Cookie 使用 HttpOnly、SameSite=Strict；`PUBLIC_URL` 为 HTTPS 时自动启用 Secure。
- 状态修改接口校验浏览器 Origin；登录失败有本机地址级限速。
- 默认忽略 `X-Forwarded-For`。只有在 `TRUSTED_PROXY_CIDRS` 中明确列出的直连代理才能提供客户端地址；不要填写不受你控制的宽泛网段。
- 不要直接把 8080 端口暴露到公网，不要公开 `auth.json`、`.env`、数据库或 `master.key`。
- SQLite 模式重启后管理员需要重新登录，未完成的设备码流程也会失效；持久数据不受影响。

## 来源与许可证

实现过程中曾将 [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) 与 [sub2api](https://github.com/Wei-Shaw/sub2api) 作为协议互操作性参考。详细归属见 [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md)。

CodexOne 采用 [MIT License](LICENSE)。
