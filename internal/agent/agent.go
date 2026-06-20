// Package agent 提供统一的 coding-agent Backend 接口，把 Claude Code / Codex /
// 等等各家 CLI 的输出翻译成统一的事件流。
//
// 接口借鉴 multica-main/server/pkg/agent，但权限路径是 Lujiang 风格：
// multica 在 daemon 里自动 allow，Lujiang 必须 surface 给 Web 用户（P6 实现），
// 因此 Backend 增加 RespondPermission 钩子。
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Backend 是单个 coding-agent CLI 的抽象。
//
//   - Execute 启动一次 prompt 执行，返回事件流。
//   - RespondPermission 在 ControlRequest / 审批事件触发时回复（P6 用）。
//   - Interrupt 中断当前执行。
//   - Close 释放底层资源（如果 backend 是有状态的）。
type Backend interface {
	Execute(ctx context.Context, prompt string, opts ExecOptions) (*Session, error)
	RespondPermission(ctx context.Context, reqID, decision string, updatedInput map[string]any) error
	Interrupt(ctx context.Context) error
	Close() error
}

// ExecOptions 是单次执行的参数。
type ExecOptions struct {
	Cwd            string
	Model          string
	PermissionMode string // "acceptEdits" | "plan" | "bypassPermissions"
	// MaxTurns 限制 Claude Code 的对话轮数（Claude 特有，其它 backend 忽略）。
	MaxTurns int
	// ResumeFrom 非空时，backend 会续上一段 provider 端的对话（如 Claude --resume）。
	// 用于多轮对话：浏览器在已存在的 session 上发新 prompt 时带上 provider session id。
	ResumeFrom string
}

// Session 是一次执行的句柄。
type Session struct {
	Events <-chan Event
	Result <-chan Result

	cancel context.CancelFunc

	// permissionCh 把 Execute 内部遇到的权限请求 surface 给 host，host 在
	// 另一条 stream 上拿到用户回复后调 RespondPermission。
	permissionCh chan<- PermissionRequest

	// permissionRespCh 把 host 的回复送回 Execute goroutine。
	permissionRespCh <-chan PermissionResponse

	// backend 用于 Interrupt/RespondPermission 转发到具体 adapter 的私有方法。
	backend Backend
}

// EventType 标识事件种类。借鉴 multica 的 MessageType，但 Lujiang 在
// handlers/agent.go 里翻译为 proto.AgentEvent，不直接对外暴露。
type EventType string

const (
	EventText       EventType = "text"        // 模型文本输出
	EventThinking   EventType = "thinking"    // reasoning
	EventToolUse    EventType = "tool-use"    // tool 调用开始
	EventToolResult EventType = "tool-result" // tool 返回
	EventStatus     EventType = "status"      // 状态变化（如拿到 session_id）
	EventError      EventType = "error"       // 致命错误
	EventPermission EventType = "permission"  // 等待用户授权
	EventLog        EventType = "log"
)

// Event 是统一事件。Host 收到后翻译为 proto.AgentEvent 写到 yamux stream。
type Event struct {
	Type    EventType
	Content string         // text / error / log
	Tool    string         // tool 名
	CallID  string         // tool call ID
	Input   map[string]any // tool 输入
	Output  string         // tool 输出
	Status  string         // 状态字符串
	Level   string         // 日志级别
	// RequestID 仅 EventPermission 用：对应 Claude control_request ID。
	RequestID string
	Resources []string // 仅 EventPermission：相关资源（文件路径等）
}

// PermissionRequest 是 backend → host 的权限询问（async）。
type PermissionRequest struct {
	RequestID string
	Action    string   // "bash"、"edit" 等
	Resources []string // 文件路径等
	RawInput  map[string]any
}

// PermissionResponse 是 host → backend 的回复。
type PermissionResponse struct {
	RequestID    string
	Decision     string         // allow_once | allow_always | deny
	UpdatedInput map[string]any // 可能被用户编辑过的 input
}

// Result 是最终结果。
type Result struct {
	Status     string // "completed" | "failed" | "aborted" | "timeout"
	Output     string
	Error      string
	DurationMs int64
	SessionID  string
}

// New 选择 backend。Claude 是 P5 落地的；其它 backend 在 P6/P7 按需补齐。
// 未实现的 backend 返回明确错误，便于 UI / 日志区分"未实现"与"CLI 未安装"。
func New(backend string, cfg Config) (Backend, error) {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	switch backend {
	case "claude":
		return newClaudeBackend(cfg), nil
	case "codex":
		return newCodexBackend(cfg), nil
	case "gemini":
		return newGeminiBackend(cfg), nil
	case "cursor":
		return newCursorBackend(cfg), nil
	case "copilot":
		return newCopilotBackend(cfg), nil
	case "opencode":
		return newOpencodeBackend(cfg), nil
	default:
		return nil, fmt.Errorf("agent: unknown backend %q", backend)
	}
}

// Config 构造 Backend。
type Config struct {
	ExecutablePath string            // 默认按 backend 名 PATH 查找
	Env            map[string]string // 额外环境变量
	Logger         Logger
}

// Logger 是 backend 用的最小日志接口。
type Logger = *slog.Logger

// ── 共用工具：env 合并、CLI 查找、stderr tail ──

// buildEnv 合并 os.Environ() 与 cfg.Env，按 key 去重（后者覆盖）。
func buildEnv(extra map[string]string) []string {
	base := os.Environ()
	out := make([]string, 0, len(base)+len(extra))
	seen := map[string]bool{}
	for k, v := range extra {
		out = append(out, k+"="+v)
		seen[k] = true
	}
	for _, entry := range base {
		key, _, _ := strings.Cut(entry, "=")
		if seen[key] {
			continue
		}
		out = append(out, entry)
	}
	return out
}

// resolveExecPath 返回 backend 的可执行路径：优先 cfg.ExecutablePath，
// 否则在 PATH 里找 name。
func resolveExecPath(cfg Config, name string) (string, error) {
	if cfg.ExecutablePath != "" {
		return cfg.ExecutablePath, nil
	}
	return exec.LookPath(name)
}

// spawnArgs 是 spawnCLI 的入参，集中各 backend 共有的 spawn 配置。
type spawnArgs struct {
	ctx       context.Context
	execPath  string
	args      []string
	cwd       string
	env       []string
	waitDelay time.Duration
}

// spawnCLI 是各 backend 共用的进程启动：开 stdout pipe + 有界 stderr tail。
// 失败时返回 error，调用方负责 cancel()。成功时返回 *exec.Cmd / stdout / stderr。
func spawnCLI(p spawnArgs) (*exec.Cmd, io.ReadCloser, *stderrTail, error) {
	cmd := exec.CommandContext(p.ctx, p.execPath, p.args...)
	if p.cwd != "" {
		cmd.Dir = p.cwd
	}
	if p.env != nil {
		cmd.Env = p.env
	}
	if p.waitDelay > 0 {
		cmd.WaitDelay = p.waitDelay
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("stdout pipe: %w", err)
	}
	stderrT := newStderrTail(64 * 1024)
	cmd.Stderr = stderrT
	if err := cmd.Start(); err != nil {
		return nil, nil, nil, fmt.Errorf("start process: %w", err)
	}
	return cmd, stdout, stderrT, nil
}

// stderrTail 是一个有界 ring buffer，保留最后 N 字节 stderr。
// 失败时把它附加到 Result.Error 帮助 root-cause。
type stderrTail struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func newStderrTail(max int) *stderrTail { return &stderrTail{max: max} }

func (s *stderrTail) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buf = append(s.buf, p...)
	if len(s.buf) > s.max {
		s.buf = s.buf[len(s.buf)-s.max:]
	}
	return len(p), nil
}

func (s *stderrTail) Tail() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return string(s.buf)
}

// withStderrTail 把 stderr 尾巴附加到 err message。
func withStderrTail(msg, label, tail string) string {
	tail = strings.TrimSpace(tail)
	if tail == "" {
		return msg
	}
	if len(tail) > 4096 {
		tail = "... " + tail[len(tail)-4096:]
	}
	return fmt.Sprintf("%s\n--- %s stderr (tail) ---\n%s", msg, label, tail)
}

// runContext 给定 timeout 返回 ctx+cancel；timeout<=0 → 纯 cancel。
func runContext(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout > 0 {
		return context.WithTimeout(ctx, timeout)
	}
	return context.WithCancel(ctx)
}

// inputJSON 把 map 序列化为 RawMessage，便于 embedding。
func inputJSON(m map[string]any) json.RawMessage {
	if len(m) == 0 {
		return nil
	}
	b, _ := json.Marshal(m)
	return b
}
