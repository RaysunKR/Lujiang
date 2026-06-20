package proto

import "encoding/json"

// Agent op 常量。
//
// 交互模型：
//   - agent.start 在一条新 yamux stream 上发起：opener 写 header + AgentStartReq。
//     client 启动 backend、回写 AgentStartRes{sessionID}，此后该 stream 转为
//     单向（client → opener）事件流：每个事件是 tunnelmux 长度前缀帧包裹的
//     JSON AgentEvent。opener 想中断/回复权限时另开 agent.interrupt /
//     agent.permission.reply 短 RPC。
//   - agent.interrupt / agent.permission.reply 是独立短 RPC，client 回写空
//     JSON `{}` 表示接受（或 AgentError）。
const (
	OpAgentStart           = "agent.start"
	OpAgentResume          = "agent.resume"
	OpAgentInterrupt       = "agent.interrupt"
	OpAgentPermissionReply = "agent.permission.reply"
	OpAgentList            = "agent.list"
)

// AgentStartReq 是 agent.start 的请求体。
type AgentStartReq struct {
	Backend string `json:"backend"` // "claude" 等；P5 只支持 claude
	Cwd     string `json:"cwd"`
	Prompt  string `json:"prompt"`
	Model   string `json:"model,omitempty"`
	// 模式：acceptEdits（默认）| plan | bypassPermissions。
	// 浏览器在 Composer 上选；P5 暂用 acceptEdits 自动 allow。
	PermissionMode string            `json:"permission_mode,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	// ResumeFrom 是 provider 自己的 session id（如 Claude 的 session-xxx）。
	// 非空时 backend 用 --resume <id> 续上一段对话，保留 provider 端的完整上下文。
	// 通常由服务端从 ContinueSessionID 自动推导；浏览器只在特殊场景显式覆盖。
	ResumeFrom string `json:"resume_from,omitempty"`
	// ContinueSessionID 非空时，复用该 Lujiang session id（不新建）。
	// 用于多轮对话：每个 turn 共享同一 session 的 seq 命名空间，sidebar /
	// timeline 不分裂。服务端会从 DB 读出该 session 的 last_seq（续编号）和
	// provider_session_id（作为 Claude --resume 参数，若 ResumeFrom 为空）。
	ContinueSessionID string `json:"continue_session_id,omitempty"`
}

// AgentStartRes 是 agent.start 的响应体。响应写入后 stream 进入事件流阶段。
type AgentStartRes struct {
	SessionID string `json:"session_id"`
}

// AgentResumeReq 是 agent.resume 的请求体。客户端读 store 自 last_seq+1
// 重放历史事件；session 仍活着的话继续 tail 新事件。
type AgentResumeReq struct {
	SessionID string `json:"session_id"`
	LastSeq   int64  `json:"last_seq"`
}

// AgentResumeRes 是 agent.resume 的响应体。
type AgentResumeRes struct {
	SessionID string `json:"session_id"`
	Replayed  int    `json:"replayed"` // 重放了多少条
	Live      bool   `json:"live"`     // session 是否仍活跃（会继续推新事件）
	Done      bool   `json:"done"`     // session 已结束（不会再有新事件）
}

// AgentInterruptReq 中断 agent.start 的执行（对应 ESC / Ctrl-C）。
type AgentInterruptReq struct {
	SessionID string `json:"session_id"`
}

// AgentListReq 列出客户端持久化的 session（最近 N 条）。
type AgentListReq struct {
	Limit int `json:"limit,omitempty"` // 0 = 用默认值（client 自定）
	// Cwd 非空时只返回同目录的 session。空 = 不过滤（全部目录混在一起）。
	// 浏览器在 /project?cwd=... 页面默认带当前 cwd，避免 sidebar 把所有
	// 目录的历史会话都堆在一起。
	Cwd string `json:"cwd,omitempty"`
}

// AgentSessionInfo 是 list 返回的单条 session 摘要。
// Live 表示进程还在跑（仍可继续推事件）；Done 表示已写完最后一帧。
type AgentSessionInfo struct {
	ID        string `json:"id"`
	Backend   string `json:"backend"`
	Cwd       string `json:"cwd"`
	CreatedAt int64  `json:"created_at"` // unix millis
	EndedAt   int64  `json:"ended_at,omitempty"`
	Status    string `json:"status,omitempty"`
	LastSeq   int64  `json:"last_seq"`
	Live      bool   `json:"live"`
	// ProviderSessionID 用于浏览器续对话（作为下一条 prompt 的 resume_from）。
	ProviderSessionID string `json:"provider_session_id,omitempty"`
	// Title 是会话标题（首条 user prompt 截断版），sidebar 主显示。
	// 空表示老 session 迁移过来还没有 title。
	Title string `json:"title,omitempty"`
}

// AgentListRes 是 agent.list 的响应。
type AgentListRes struct {
	Sessions []AgentSessionInfo `json:"sessions"`
}

// AgentPermissionReplyReq 是权限回复（P6 使用，先定义协议）。
type AgentPermissionReplyReq struct {
	SessionID    string         `json:"session_id"`
	RequestID    string         `json:"request_id"`
	Decision     string         `json:"decision"` // allow_once | allow_always | deny
	UpdatedInput map[string]any `json:"updated_input,omitempty"`
}

// AgentError 是 agent.* 失败时的统一响应体。
type AgentError struct {
	Error string `json:"error"`
}

// AgentEvent 是 client → 浏览器的事件。判别联合用 Type 字段。
//
// 事件分类（与 opencode SDK 对齐，便于 P5 直接搬前端组件）：
//   - session.created / session.status / session.idle / session.error
//   - message.updated / message.removed
//   - message.part.delta / message.part.updated / message.part.removed
//   - session.next.text.started / .delta / .ended
//   - session.next.reasoning.started / .delta / .ended
//   - session.next.tool.input.started / .delta / .ended
//   - session.next.tool.called / .progress / .success / .failed
//   - session.next.step.started / .ended / .failed
//   - permission.v2.asked / permission.v2.replied
//   - session.diff / file.edited
//
// P5 只发 text/reasoning/tool 系列与 session.*；permission 系列留给 P6。
type AgentEvent struct {
	Type string `json:"type"`
	Seq  int64  `json:"seq"` // 单调递增，客户端持久化用
	Ts   int64  `json:"ts"`  // unix millis

	// 公共关联字段
	SessionID string `json:"session_id,omitempty"`
	MessageID string `json:"message_id,omitempty"` // 关联到 message.updated
	PartID    string `json:"part_id,omitempty"`    // 关联到 message.part.*

	// 不同 Type 用的 payload；解析端按 Type 选字段。
	Text     string          `json:"text,omitempty"`
	Reason   string          `json:"reason,omitempty"` // reasoning delta
	Tool     string          `json:"tool,omitempty"`
	CallID   string          `json:"call_id,omitempty"`
	Input    json.RawMessage `json:"input,omitempty"`
	Output   string          `json:"output,omitempty"`
	Status   string          `json:"status,omitempty"`
	Error    string          `json:"error,omitempty"`
	Permission *AgentPermissionAsked `json:"permission,omitempty"`

	// ProviderSessionID 是 backend 自己的 session id（如 Claude 的 session-xxx）。
	// session.status 里 status=running 时携带；浏览器续对话时把它作为
	// resume_from 回传，保留 provider 端上下文。
	ProviderSessionID string `json:"provider_session_id,omitempty"`
}

// AgentPermissionAsked 是 permission.v2.asked 事件的 payload。
type AgentPermissionAsked struct {
	RequestID string   `json:"request_id"`
	Action    string   `json:"action"`    // "bash"、"edit" 等
	Resources []string `json:"resources"` // 文件路径等
}

// Agent 事件类型常量（避免字符串散落）。
const (
	EvSessionCreated = "session.created"
	EvSessionStatus  = "session.status"
	EvSessionIdle    = "session.idle"
	EvSessionError   = "session.error"

	// EvUserPrompt 是用户提交的 prompt。每个 turn 开始时由 server 在 claude
	// 启动前 emit 一次，让 timeline 能显示"用户说了什么"。Text 字段携带内容。
	EvUserPrompt = "session.user.prompt"

	EvTextStarted = "session.next.text.started"
	EvTextDelta   = "session.next.text.delta"
	EvTextEnded   = "session.next.text.ended"

	EvReasoningStarted = "session.next.reasoning.started"
	EvReasoningDelta   = "session.next.reasoning.delta"
	EvReasoningEnded   = "session.next.reasoning.ended"

	EvToolStarted = "session.next.tool.input.started"
	EvToolDelta   = "session.next.tool.input.delta"
	EvToolEnded   = "session.next.tool.input.ended"
	EvToolCalled  = "session.next.tool.called"
	EvToolSuccess = "session.next.tool.success"
	EvToolFailed  = "session.next.tool.failed"

	EvStepStarted = "session.next.step.started"
	EvStepEnded   = "session.next.step.ended"
	EvStepFailed  = "session.next.step.failed"

	EvPermissionAsked = "permission.v2.asked"
	EvPermissionReplied = "permission.v2.replied"
)
