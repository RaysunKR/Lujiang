# Lujiang 鹭江

[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
[![Go](https://img.shields.io/badge/Go-1.24+-00ADD8?logo=go)](https://go.dev/)
[![SolidJS](https://img.shields.io/badge/SolidJS-1-2c4f7c?logo=solidjs)](https://www.solidjs.com/)

**[中文](#中文) · [English](#english)**

---

<a id="english"></a>
## English

A remote-access gateway for AI coding agents. Users log in to a Go-hosted web UI, pick an online client (also Go, running on the controlled machine), and drive that client's local agents / terminal / filesystem / editor from the browser. The client carries all execution and state; the server only authenticates and proxies. Clients reverse-dial the server over a long-lived WebSocket tunnel multiplexed with `hashicorp/yamux`.

### Why

You want to use Claude Code / Codex / etc. on a remote machine (your homelab, a Windows box at the office, a cloud VM behind NAT) without exposing SSH, without copying files around, and without leaving credentials on a laptop. Lujiang makes the remote machine's filesystem, shell, and agent CLIs accessible from a single browser tab.

### Architecture

```
Browser ──HTTPS/WSS──> Server <──WSS/yamux── Client ──> local agent CLI / PTY / FS
                       │                       │
                       │                       └─ SQLite (session / event persistence)
                       └─ In-memory registry (clientID → *ClientConn)
```

- **Tunnel**: WebSocket + `hashicorp/yamux`. The client reverse-dials the server over WSS; the connection is wrapped as a yamux session. Each logical RPC is one yamux stream.
- **Browser ↔ server**: WebSocket (not SSE). Bidirectional, supports permission replies and session interrupts with low token-stream latency.
- **PTY**: Windows uses ConPTY, Linux/macOS uses `creack/pty`. Per-stream resize is debounced.
- **Storage**: `modernc.org/sqlite` (pure Go), WAL mode + single writer goroutine. No CGO → static cross-compilation.
- **Web assets**: embedded into the server binary via `embed.FS` → single-file deployment.
- **Permission surface**: agent control requests are surfaced to the web user via a permission dock (Allow once / Allow always / Deny). Lujiang never auto-approves.

### Repository layout

```
cmd/
  lujiang-server/         Server entrypoint
  lujiang-client/         Client entrypoint
internal/
  proto/                  Shared protocol types (agent / fs / pty / tunnel frames)
  agent/                  Agent adapters (Claude Code shipped; Codex / Gemini / Cursor / Copilot / OpenCode stubbed)
  server/
    auth/                 bcrypt login + HMAC session cookie
    tls/                  Load cert or self-sign ECDSA fallback
    tunnel/               Client registry + WSS handler
    web/                  REST + WS routes + embed.FS static assets
  client/
    dial/                 WSS dial + yamux client + exponential backoff reconnect
    handlers/             stream op → handler dispatch (fs / pty / agent)
    store/                SQLite wrapper (schema + session/event persistence)
    shell/                Enumerate available shells per platform
    winpty/               Cross-platform PTY (Windows ConPTY + Unix creack/pty)
  tunnelmux/              yamux server/client wrappers + length-prefixed JSON framing
web/                      SolidJS Web UI (Vite + Tailwind v4)
scripts/
  build.ps1               Windows build matrix
  build.sh                Unix build matrix
  deploy/                 Deploy helpers (ssh_exec.py / ssh_upload.py / systemd units)
configs/
  server.example.yaml
  client.example.yaml
```

### Quick start

#### 1. Build

```powershell
# Windows (PowerShell)
scripts\build.ps1
# Output: dist/lujiang-{server,client}.windows-amd64.exe
```

```bash
# Linux / macOS
./scripts/build.sh
# Output: dist/lujiang-{server,client}.{linux|darwin}-{amd64|arm64}
```

Cross-compilation (works because the SQLite driver is pure Go — set `CGO_ENABLED=0`):

```powershell
scripts\build.ps1 -Os linux -Arch amd64
scripts\build.ps1 -Os darwin -Arch arm64
```

#### 2. Configure

Copy the examples:

```bash
cp configs/server.example.yaml server.yaml
cp configs/client.example.yaml client.yaml
```

Edit `server.yaml`:
- Set `web_users[].password_hash`. Generate a bcrypt hash with:
  `htpasswd -nbBC 12 "" 'yourpass' | cut -d: -f2`
- Set `clients[].token` — a shared secret that must match the client.
- For production: `auto_tls: true` (self-signed) or supply `cert_file` / `key_file`, and set a 32+ byte random `session_secret`.

Edit `client.yaml`:
- `id` and `token` must match one of the server's `clients[]` entries.
- `server_url` points at the server (`wss://` once TLS is on).
- `tls_skip_verify: true` only for self-signed dev setups.

#### 3. Run

```bash
# Terminal 1: server
./lujiang-server -config configs/server.yaml

# Terminal 2: client (same machine or another)
./lujiang-client -config configs/client.yaml

# Browser
open https://localhost:7443
```

On first visit you'll get a self-signed cert warning (skip it for dev; bring a real cert or put a reverse proxy in front for production).

#### 4. Use

1. Log in with the account from `web_users`.
2. On the "Online clients" page, pick a client.
3. Browse / edit files (Monaco, Ctrl+S to save), open a terminal (xterm.js), or click "Agent" to start a Claude Code session.
4. In the agent workspace: prompt at the bottom, watch streaming markdown + tool calls. When the agent asks for permission (runtime `control_request`), a permission dock appears — choose Allow once / Allow always / Deny.

### Implementation status

| Module | Status |
|---|---|
| Tunnel (WSS + yamux + reconnect) | ✅ |
| Auth (bcrypt + session cookie) | ✅ |
| Self-signed TLS | ✅ |
| File service (list / read / write / mkdir / stat) | ✅ |
| Monaco editor (Ctrl+S save) | ✅ |
| PTY terminal (ConPTY + creack/pty) | ✅ |
| Agent — Claude Code | ✅ |
| Permission dock (control_request path) | ✅ |
| Client online/offline indicator | ✅ |
| Cross-platform build matrix | ✅ (win/linux/darwin × amd64/arm64) |
| Mobile layout | ✅ |
| Agent — Codex / Gemini / Cursor / Copilot / OpenCode | ⏳ stubbed, not implemented |
| Session resume (event replay after reconnect) | ⏳ pending |
| SQLite migrations | ⏳ pending (single-version schema) |
| Markdown upgrade (Shiki + remend + morphdom) | ⏳ pending (currently `marked`) |
| Automatic TS type generation | ⏳ pending (`web/src/gen/events.ts` is hand-maintained) |

### Known limitations

- **Claude Code 2.1.132 + GLM models** in stream-json mode do not surface permission requests via `control_request`; they return them as `tool_result` errors. The permission dock will not fire naturally in that setup. Switch to Anthropic-native models or older Claude Code to trigger it.
- **Session resume is not implemented**: if the browser WS drops, you cannot continue the existing session. Current behavior is to submit the prompt again.
- **PTY Ctrl-C via automation**: real keyboard input works; synthesized Ctrl-C events from browser-automation tools may not reach xterm's hidden textarea. Affects testing, not normal use.

### Security notes

Lujiang is designed to expose a remote machine's shell and filesystem to authenticated browser sessions. Treat both binaries as privileged:
- The server holds web login hashes and the HMAC session secret. Put it behind TLS (real cert recommended) and don't share the binary with embedded production secrets.
- The client spawns PTY children and executes file writes as its own user. Run it as a dedicated low-privilege account.
- `configs/*.prod.yaml` and `configs/*-local.yaml` are gitignored — keep real tokens and hashes out of version control.

### License

[MIT](LICENSE) © RaysunKR

---

<a id="中文"></a>
## 中文

面向 AI 编程 agent 的远程网关。用户登录 Go 实现的 Web UI，选择一台在线客户端（同样 Go 实现、跑在被控设备上），通过该客户端调用其本地 AI agents / 终端 / 文件 / 文本编辑器。客户端承载所有执行与状态，服务端只做鉴权与协议透传。客户端反向 dial 服务端，长连接隧道用 `hashicorp/yamux` 多路复用。

### 为什么做这个

如果你想在远程机器上用 Claude Code / Codex 等工具（家里的服务器、办公室的 Windows、NAT 后面的云主机），又不想开 SSH、不想来回拷文件、不想在笔记本上留凭据 —— Lujiang 把那台远程机器的文件系统、shell、agent CLI 都收进一个浏览器 tab。

### 架构

```
浏览器 ──HTTPS/WSS──> 服务端 <──WSS/yamux── 客户端 ──> 本地 agent CLI / PTY / FS
                        │                          │
                        │                          └─ SQLite（session/event 持久化）
                        └─ 内存 registry（clientID → *ClientConn）
```

- **隧道传输**：WebSocket + `hashicorp/yamux` 多路复用。客户端反向 WSS dial 服务端，连接包裹为 yamux session。每个逻辑 RPC = 一条 yamux stream。
- **浏览器↔服务端**：WebSocket 双向（不是 SSE）。天然支持权限回复和会话中断，token 流延迟低。
- **PTY**：Windows 用 ConPTY，Linux/macOS 用 `creack/pty`。每路 resize 单独 debounce。
- **存储**：`modernc.org/sqlite`（pure Go），WAL 模式 + 单 writer goroutine。无需 CGO → 静态交叉编译。
- **Web 资源**：`embed.FS` 内嵌进 server 二进制 → 单文件部署。
- **权限面板**：agent 的 control_request 透传到 Web 用户，弹出 permission dock（允许一次 / 始终允许 / 拒绝）。Lujiang 绝不自动批准。

### 仓库结构

```
cmd/
  lujiang-server/         服务端入口
  lujiang-client/         客户端入口
internal/
  proto/                  双端共享协议（agent / fs / pty / tunnel 帧）
  agent/                  agent adapter（Claude Code 已落地；Codex / Gemini / Cursor / Copilot / OpenCode 占位）
  server/
    auth/                 bcrypt 登录 + HMAC session cookie
    tls/                  加载证书 / 自签 ECDSA 兜底
    tunnel/               客户端注册表 + WSS handler
    web/                  REST + WS 路由 + embed.FS 静态资源
  client/
    dial/                 WSS dial + yamux client + 指数退避重连
    handlers/             stream op → handler 分发（fs / pty / agent）
    store/                SQLite 包装（schema + session/event 持久化）
    shell/                按平台枚举可用 shell
    winpty/               PTY 跨平台抽象（Windows ConPTY + Unix creack/pty）
  tunnelmux/              yamux server/client 包装 + 长度前缀 JSON 帧编解码
web/                      SolidJS Web UI（Vite + Tailwind v4）
scripts/
  build.ps1               Windows 构建矩阵
  build.sh                Unix 构建矩阵
  deploy/                 部署助手（ssh_exec.py / ssh_upload.py / systemd unit）
configs/
  server.example.yaml
  client.example.yaml
```

### 快速开始

#### 1. 构建

```powershell
# Windows（PowerShell）
scripts\build.ps1
# 产物：dist/lujiang-{server,client}.windows-amd64.exe
```

```bash
# Linux / macOS
./scripts/build.sh
# 产物：dist/lujiang-{server,client}.{linux|darwin}-{amd64|arm64}
```

交叉编译（SQLite 是 pure Go，`CGO_ENABLED=0` 即可）：

```powershell
scripts\build.ps1 -Os linux -Arch amd64
scripts\build.ps1 -Os darwin -Arch arm64
```

#### 2. 配置

复制示例配置：

```bash
cp configs/server.example.yaml server.yaml
cp configs/client.example.yaml client.yaml
```

编辑 `server.yaml`：
- 改 `web_users[].password_hash`，用 bcrypt：
  `htpasswd -nbBC 12 "" 'yourpass' | cut -d: -f2`
- 改 `clients[].token` —— 与客户端共享的预共享密钥。
- 生产环境：`auto_tls: true`（自签）或自带 `cert_file` / `key_file`，并设置 32+ 字节随机 `session_secret`。

编辑 `client.yaml`：
- `id` 与 `token` 必须与服务端 `clients[]` 中的一项匹配。
- `server_url` 指向服务端（启用 TLS 后用 `wss://`）。
- `tls_skip_verify: true` 仅在自签的开发环境用。

#### 3. 运行

```bash
# 终端 1：服务端
./lujiang-server -config configs/server.yaml

# 终端 2：客户端（同机或另一台机器）
./lujiang-client -config configs/client.yaml

# 浏览器
open https://localhost:7443
```

首次访问会跳过自签证书警告（生产环境请自带证书或前置反向代理）。

#### 4. 使用

1. 用 `web_users` 里配置的账号登录。
2. 在"在线客户端"页面选择一台客户端。
3. 浏览/编辑文件（Monaco，Ctrl+S 保存），打开终端（xterm.js），或点 "Agent" 进入 Claude Code 会话。
4. Agent 工作区：底部输入 prompt，看到流式 markdown + 工具调用；当 agent 请求权限时（运行时 control_request），底部出现 permission dock 供用户决策（允许一次 / 始终允许 / 拒绝）。

### 当前实现状态

| 模块 | 状态 |
|---|---|
| 隧道（WSS + yamux + 重连） | ✅ |
| 鉴权（bcrypt + session cookie） | ✅ |
| 自签 TLS | ✅ |
| 文件服务（list / read / write / mkdir / stat） | ✅ |
| Monaco 编辑器（Ctrl+S 保存） | ✅ |
| PTY 终端（ConPTY + creack/pty） | ✅ |
| Agent — Claude Code | ✅ |
| Permission dock（control_request 路径） | ✅ |
| 客户端上下线提示 | ✅ |
| 跨平台构建矩阵 | ✅（win/linux/darwin × amd64/arm64） |
| 移动端布局 | ✅ |
| Agent — Codex / Gemini / Cursor / Copilot / OpenCode | ⏳ 占位，未实现 |
| Session resume（断线重连重放） | ⏳ 待实现 |
| SQLite 迁移机制 | ⏳ 待实现（schema 当前为单版本） |
| Markdown 升级（Shiki + remend + morphdom） | ⏳ 待实现（当前用 marked） |
| 自动 TS 类型生成 | ⏳ 待实现（`web/src/gen/events.ts` 手维护） |

### 已知限制

- **Claude Code 2.1.132 + GLM 模型** 在 stream-json 模式下不通过 `control_request` surface 权限请求，而是直接以 tool_result error 返回。Permission dock 在该环境下不会自然触发；切换到 Anthropic 官方模型或旧版 Claude Code 可触发。
- **Session resume 尚未实现**：浏览器 WS 断开后无法续传已有 session。当前行为是用户重新发起一次 prompt。
- **PTY Ctrl-C**：真实键盘输入工作正常；浏览器自动化工具合成的 Ctrl-C 事件可能无法被 xterm 的隐藏 textarea 正确接收。仅影响测试，不影响实际使用。

### 安全说明

Lujiang 的设计目的是把远程机器的 shell 和文件系统暴露给已鉴权的浏览器会话。两个二进制都要按特权软件对待：
- 服务端持有 web 登录 hash 和 HMAC session 密钥。一定要走 TLS（生产建议用正式证书），不要把带生产凭据的二进制分发出去。
- 客户端会以自身用户身份 spawn PTY 子进程、写文件。用专用低权限账号运行。
- `configs/*.prod.yaml` 和 `configs/*-local.yaml` 已在 `.gitignore` —— 真实 token 和 hash 不要进版本库。

### 协议

[MIT](LICENSE) © RaysunKR
