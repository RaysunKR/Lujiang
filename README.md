# Lujiang 鹭江

远程网关：用户通过浏览器登录 Go 服务端，选择一台在线客户端（Go 实现、跑在被控设备上），通过该客户端调用其本地 AI agents / 终端 / 文件 / 文本编辑器。客户端承载所有执行与状态，服务端只做鉴权与协议透传。客户端反向 dial 服务端，长连接隧道承载所有交互。

## 架构

```
浏览器 ──HTTPS/WSS──> 服务端 <──WSS/yamux── 客户端 ──> 本地 agent CLI / PTY / FS
                        │                          │
                        │                          └─ SQLite（session/event 持久化）
                        └─ 内存 registry（clientID → *ClientConn）
```

- **隧道传输**：WebSocket + `hashicorp/yamux` 多路复用。客户端反向 WSS dial 服务端，连接包裹为 yamux session。每个逻辑 RPC = 一条 yamux stream。
- **浏览器↔服务端**：WebSocket 双向，承载 agent 事件流 + 控制消息（interrupt / permission.reply）。
- **PTY**：Windows 用 ConPTY，Linux/macOS 用 `creack/pty`。
- **存储**：`modernc.org/sqlite`（pure Go），WAL 模式 + 单 writer goroutine。
- **Web 资源**：`embed.FS` 内嵌进 server 二进制，单文件部署。

## 仓库结构

```
cmd/
  lujiang-server/         服务端入口
  lujiang-client/         客户端入口
internal/
  proto/                  双端共享协议（agent / fs / pty / tunnel 帧）
  agent/                  agent adapter（claude 已落地；codex/gemini/cursor/copilot/opencode 占位）
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
web/                      SolidJS Web UI（vite + tailwind v4）
scripts/
  build.ps1               Windows 构建矩阵
  build.sh                Unix 构建矩阵
configs/
  server.example.yaml
  client.example.yaml
```

## 快速开始

### 1. 构建

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

跨平台交叉编译（`modernc.org/sqlite` 是 pure Go，`CGO_ENABLED=0` 即可）：

```powershell
scripts\build.ps1 -Os linux -Arch amd64
scripts\build.ps1 -Os darwin -Arch arm64
```

### 2. 配置

复制示例配置：

```bash
cp configs/server.example.yaml server.yaml
cp configs/client.example.yaml client.yaml
```

编辑 `server.yaml`：
- 改 `web_users[].password_hash`（用 `htpasswd -nbBC 12 "" 'yourpass' | cut -d: -f2` 生成 bcrypt hash）
- 改 `clients[].token`（与服务端共享的预共享密钥）
- 生产环境：`auto_tls: true` 或自带 `cert_file` / `key_file`，并设置 32+ 字节随机 `session_secret`

编辑 `client.yaml`：
- `id` 与 `token` 与服务端 `clients[]` 中的一项匹配
- `server_url` 指向服务端（启用 TLS 后用 `wss://`）

### 3. 运行

```bash
# 终端 1：服务端
./lujiang-server

# 终端 2：客户端（另一台机器或同机均可）
./lujiang-client

# 浏览器
open https://localhost:7443
```

首次访问会跳过自签证书警告（生产环境请自带证书或前置反向代理）。

### 4. 使用

1. 用 `web_users` 里配置的账号登录。
2. 在"在线客户端"页面选择一台客户端。
3. 浏览/编辑文件（Monaco），打开终端（xterm.js），或点 "Agent" 进入 Claude Code 会话。
4. Agent 工作区：底部输入 prompt，看到流式 markdown + 工具调用；当 agent 请求权限时（运行时 control_request），底部出现 permission dock 供用户决策。

## 当前实现状态

| 模块 | 状态 |
|---|---|
| 隧道（WSS + yamux + 重连） | ✅ |
| 鉴权（bcrypt + session cookie） | ✅ |
| 自签 TLS | ✅ |
| 文件服务（list/read/write/mkdir/stat） | ✅ |
| Monaco 编辑器（Ctrl+S 保存） | ✅ |
| PTY 终端（ConPTY + creack/pty） | ✅ |
| Agent — Claude Code | ✅ |
| Permission dock | ✅（control_request 路径） |
| Agent — Codex / Gemini / Cursor / Copilot / OpenCode | ⏳ 占位，未实现 |
| 跨平台构建矩阵 | ✅（win/linux/darwin × amd64/arm64） |
| 客户端掉线提示 | ✅（客户端列表轮询 + 项目页 banner） |
| Session resume（断线重连重放） | ⏳ 待实现 |
| SQLite 迁移机制 | ⏳ 待实现（schema 当前为单版本） |
| Markdown 升级（Shiki + remend + morphdom） | ⏳ 待实现（当前用 marked） |
| 自动 TS 类型生成 | ⏳ 待实现（`web/src/gen/events.ts` 手维护） |

## 已知限制

- **Claude Code 2.1.132 + GLM 模型** 在 stream-json 模式下不通过 `control_request` surface 权限请求，而是直接以 tool_result error 返回。Permission dock 在该环境下不会自然触发；切换到Anthropic 官方模型或旧版 Claude Code 可触发。
- **Session resume** 尚未实现：浏览器 WS 断开后无法续传已有 session。当前行为是用户重新发起一次 prompt。
- **PTY Ctrl-C**：通过 xterm.js 用户键盘输入工作正常；自动化测试工具（如 agent-browser）合成的 Ctrl-C 事件不能被 xterm 的隐藏 textarea 正确接收，仅影响测试，不影响实际使用。
