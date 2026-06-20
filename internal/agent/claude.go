package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// claudeBackend spawns Claude Code CLI with stream-json input/output.
type claudeBackend struct {
	cfg Config

	mu          sync.Mutex
	active      *claudeRun // 当前活动执行；Interrupt/RespondPermission 走它
}

func newClaudeBackend(cfg Config) *claudeBackend {
	return &claudeBackend{cfg: cfg}
}

type claudeRun struct {
	ctx       context.Context
	cancel    context.CancelFunc
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	stdout    io.ReadCloser
	stderr    *stderrTail
	sessionID string

	// permCh 把权限请求送给 host；respCh 把 host 回复送回 scanner 循环。
	permCh chan PermissionRequest
	respCh chan PermissionResponse

	writeMu sync.Mutex // 保护 stdin 串行写
}

// Execute 启动 claude，把 stream-json 翻译为统一 Event。
//
// 注意点（来自 multica 的踩坑）：
//   - 必须 --verbose，否则 stdout 第一行不是 init frame 而是 banner，scanner
//     会跳过空行 + unmarshal fail。
//   - stdin 不能在 prompt 写完后立即 close：claude 可能在执行中发 control_request
//     等回复，关 stdin 会让它死等到自己 timeout。
//   - prompt 写入与 stdout 读取必须并行（claude 先吐 banner 再读 stdin），
//     串行会死锁。
func (b *claudeBackend) Execute(ctx context.Context, prompt string, opts ExecOptions) (*Session, error) {
	execPath, err := resolveExecPath(b.cfg, "claude")
	if err != nil {
		return nil, fmt.Errorf("claude executable not found: %w", err)
	}

	runCtx, cancel := runContext(ctx, 0)
	args := buildClaudeArgs(opts)

	cmd := exec.CommandContext(runCtx, execPath, args...)
	if opts.Cwd != "" {
		cmd.Dir = opts.Cwd
	}
	cmd.Env = buildClaudeEnv(b.cfg.Env)
	cmd.WaitDelay = 10 * time.Second

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("claude stdout pipe: %w", err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("claude stdin pipe: %w", err)
	}
	stderrT := newStderrTail(64 * 1024)
	cmd.Stderr = stderrT

	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("start claude: %w", err)
	}

	b.cfg.Logger.Info("claude started", "pid", cmd.Process.Pid, "cwd", opts.Cwd, "model", opts.Model)

	permCh := make(chan PermissionRequest, 8)
	respCh := make(chan PermissionResponse, 8)

	run := &claudeRun{
		ctx:    runCtx,
		cancel: cancel,
		cmd:    cmd,
		stdin:  stdin,
		stdout: stdout,
		stderr: stderrT,
		permCh: permCh,
		respCh: respCh,
	}

	b.mu.Lock()
	b.active = run
	b.mu.Unlock()

	eventCh := make(chan Event, 256)
	resultCh := make(chan Result, 1)

	go b.runLoop(run, prompt, opts, eventCh, resultCh)

	return &Session{
		Events:          eventCh,
		Result:          resultCh,
		cancel:          cancel,
		permissionCh:    permCh,
		permissionRespCh: respCh,
		backend:         b,
	}, nil
}

func (b *claudeBackend) runLoop(run *claudeRun, prompt string, opts ExecOptions, eventCh chan<- Event, resultCh chan<- Result) {
	defer close(eventCh)
	defer close(resultCh)
	defer close(run.permCh)
	defer close(run.respCh)
	defer run.cancel()

	// 1. 写 prompt（async，避免与 stdout 读互相死锁）。
	writeDone := make(chan error, 1)
	go func() {
		writeDone <- writeClaudeInput(run, prompt)
	}()

	// ctx 取消时关 stdout，让 scanner 退出。
	go func() {
		<-run.ctx.Done()
		closeOnce(run.stdin)
		_ = run.stdout.Close()
	}()

	startTime := time.Now()
	var output strings.Builder
	var sessionID string
	finalStatus := "completed"
	var finalError string

	scanner := bufio.NewScanner(run.stdout)
	scanner.Buffer(make([]byte, 0, 1024*1024), 10*1024*1024)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var msg claudeSDKMessage
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			continue
		}

		switch msg.Type {
		case "assistant":
			handleClaudeAssistant(msg, eventCh, &output)
		case "user":
			handleClaudeUser(msg, eventCh)
		case "system":
			// Claude 在一个 session 里会发多条 system frame（每个 turn 一条）。
			// 只在第一次拿到 sessionID 时发 status，避免 timeline 被重复 running 灌爆。
			if msg.SessionID != "" && sessionID == "" {
				sessionID = msg.SessionID
				trySend(eventCh, Event{Type: EventStatus, Status: "running", Content: sessionID})
			}
		case "result":
			sessionID = msg.SessionID
			if msg.ResultText != "" {
				output.Reset()
				output.WriteString(msg.ResultText)
			}
			if msg.IsError {
				finalStatus = "failed"
				finalError = msg.ResultText
			}
			closeOnce(run.stdin)
		case "log":
			if msg.Log != nil {
				trySend(eventCh, Event{Type: EventLog, Level: msg.Log.Level, Content: msg.Log.Message})
			}
		case "control_request":
			b.handleControlRequest(run, msg, eventCh)
		}
	}

	closeOnce(run.stdin)
	exitErr := run.cmd.Wait()
	duration := time.Since(startTime)
	writeErr := <-writeDone

	switch {
	case run.ctx.Err() == context.Canceled:
		finalStatus = "aborted"
		finalError = "execution cancelled"
	case writeErr != nil && finalStatus == "completed" && sessionID == "":
		finalStatus = "failed"
		finalError = fmt.Sprintf("write claude input: %v", writeErr)
	case exitErr != nil && finalStatus == "completed":
		finalStatus = "failed"
		finalError = fmt.Sprintf("claude exited with error: %v", exitErr)
	}
	if finalError != "" {
		finalError = withStderrTail(finalError, "claude", run.stderr.Tail())
	}

	b.cfg.Logger.Info("claude finished", "status", finalStatus, "duration", duration.Round(time.Millisecond).String())

	resultCh <- Result{
		Status:     finalStatus,
		Output:     output.String(),
		Error:      finalError,
		DurationMs: duration.Milliseconds(),
		SessionID:  sessionID,
	}
}

// handleControlRequest 处理 Claude Code 的运行时权限请求。
//
// P6：发 EventPermission surface 给 host（→ 浏览器 permission dock），
// 然后阻塞等用户回复（run.respCh 由 Backend.RespondPermission 填）。
// 拿到回复后写 control_response 到 claude stdin。
// 上下文取消 → 直接返回（claude 会被 cancel 杀掉）。
func (b *claudeBackend) handleControlRequest(run *claudeRun, msg claudeSDKMessage, eventCh chan<- Event) {
	var req claudeControlRequestPayload
	if err := json.Unmarshal(msg.Request, &req); err != nil {
		return
	}

	var inputMap map[string]any
	if req.Input != nil {
		_ = json.Unmarshal(req.Input, &inputMap)
	}
	if inputMap == nil {
		inputMap = map[string]any{}
	}

	resources := claudePermissionResources(req.ToolName, inputMap)
	trySend(eventCh, Event{
		Type:      EventPermission,
		RequestID: msg.RequestID,
		Tool:      req.ToolName,
		Resources: resources,
		Input:     inputMap,
	})

	// 阻塞等用户回复；ctx 取消 → 直接返回（不要写 response，让 claude 退出）。
	var resp PermissionResponse
	select {
	case resp = <-run.respCh:
	case <-run.ctx.Done():
		return
	}

	finalInput := inputMap
	if resp.UpdatedInput != nil {
		finalInput = resp.UpdatedInput
	}

	var payload map[string]any
	switch resp.Decision {
	case "deny":
		payload = map[string]any{
			"type": "control_response",
			"response": map[string]any{
				"subtype":    "success",
				"request_id": msg.RequestID,
				"response": map[string]any{
					"behavior":     "deny",
					"message":      "user denied",
					"updatedInput": finalInput,
				},
			},
		}
	default: // allow_once / allow_always / 空 → allow
		payload = map[string]any{
			"type": "control_response",
			"response": map[string]any{
				"subtype":    "success",
				"request_id": msg.RequestID,
				"response": map[string]any{
					"behavior":     "allow",
					"updatedInput": finalInput,
				},
			},
		}
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	data = append(data, '\n')
	run.writeMu.Lock()
	_, _ = run.stdin.Write(data)
	run.writeMu.Unlock()
}

// claudePermissionResources 从 tool input 里抽出对用户友好的资源路径列表。
func claudePermissionResources(tool string, input map[string]any) []string {
	switch tool {
	case "Bash", "bash":
		if cmd, ok := input["command"].(string); ok && cmd != "" {
			return []string{cmd}
		}
	case "Edit", "edit", "Write", "write", "MultiEdit", "NotebookEdit":
		if p, ok := input["file_path"].(string); ok && p != "" {
			return []string{p}
		}
	}
	return nil
}

// RespondPermission 把用户决策注入对应 control_request 的等待者。
// handleControlRequest 在 run.respCh 上阻塞读，匹配 reqID 由调用方保证
// （claude 一次只会有一个 in-flight control_request）。
func (b *claudeBackend) RespondPermission(ctx context.Context, reqID, decision string, updatedInput map[string]any) error {
	b.mu.Lock()
	run := b.active
	b.mu.Unlock()
	if run == nil {
		return fmt.Errorf("claude: no active run for permission reply")
	}
	select {
	case run.respCh <- PermissionResponse{RequestID: reqID, Decision: decision, UpdatedInput: updatedInput}:
		return nil
	case <-run.ctx.Done():
		return run.ctx.Err()
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Interrupt 通过 cancel ctx 让 cmd.Wait 返回；cmd.WaitDelay 兜底 SIGKILL。
func (b *claudeBackend) Interrupt(ctx context.Context) error {
	b.mu.Lock()
	run := b.active
	b.mu.Unlock()
	if run == nil {
		return nil
	}
	run.cancel()
	return nil
}

func (b *claudeBackend) Close() error {
	// 每次 Execute 用完自己清理；这里只是接口实现。
	return nil
}

// ── 输入/输出辅助 ──

func writeClaudeInput(run *claudeRun, prompt string) error {
	data, err := buildClaudeInput(prompt)
	if err != nil {
		return err
	}
	run.writeMu.Lock()
	defer run.writeMu.Unlock()
	_, err = run.stdin.Write(data)
	return err
}

func buildClaudeInput(prompt string) ([]byte, error) {
	payload := map[string]any{
		"type": "user",
		"message": map[string]any{
			"role": "user",
			"content": []map[string]string{
				{"type": "text", "text": prompt},
			},
		},
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal claude input: %w", err)
	}
	return append(data, '\n'), nil
}

func buildClaudeArgs(opts ExecOptions) []string {
	args := []string{
		"-p",
		"--output-format", "stream-json",
		"--input-format", "stream-json",
		"--verbose",
		"--strict-mcp-config",
	}
	// --resume <id>：续上一段 Claude 对话。配合 stream-json 输入时，新 prompt
	// 仍走 stdin；Claude 加载历史上下文后处理新 prompt。
	if opts.ResumeFrom != "" {
		args = append(args, "--resume", opts.ResumeFrom)
	}
	mode := opts.PermissionMode
	if mode == "" {
		mode = "acceptEdits"
	}
	args = append(args, "--permission-mode", mode)
	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}
	if opts.MaxTurns > 0 {
		args = append(args, "--max-turns", fmt.Sprintf("%d", opts.MaxTurns))
	}
	return args
}

func buildClaudeEnv(extra map[string]string) []string {
	out := []string{}
	for _, entry := range buildEnv(extra) {
		k, _, _ := strings.Cut(entry, "=")
		if isFilteredChildEnvKey(k) {
			continue
		}
		out = append(out, entry)
	}
	return out
}

func handleClaudeAssistant(msg claudeSDKMessage, ch chan<- Event, output *strings.Builder) {
	var content claudeMessageContent
	if err := json.Unmarshal(msg.Message, &content); err != nil {
		return
	}
	for _, block := range content.Content {
		switch block.Type {
		case "text":
			if block.Text != "" {
				output.WriteString(block.Text)
				trySend(ch, Event{Type: EventText, Content: block.Text})
			}
		case "thinking":
			if block.Text != "" {
				trySend(ch, Event{Type: EventThinking, Content: block.Text})
			}
		case "tool_use":
			var input map[string]any
			if block.Input != nil {
				_ = json.Unmarshal(block.Input, &input)
			}
			trySend(ch, Event{
				Type:  EventToolUse,
				Tool:  block.Name,
				CallID: block.ID,
				Input: input,
			})
		}
	}
}

func handleClaudeUser(msg claudeSDKMessage, ch chan<- Event) {
	var content claudeMessageContent
	if err := json.Unmarshal(msg.Message, &content); err != nil {
		return
	}
	for _, block := range content.Content {
		if block.Type == "tool_result" {
			resultStr := ""
			if block.Content != nil {
				resultStr = string(block.Content)
			}
			// Claude Code 2.1.x 把权限拒绝表达成 tool_result(is_error=true)，
			// 文本形如 "Claude requested permissions to ... but you haven't granted"。
			// 嗅探这种 case 把它翻译成 EventPermission，让前端 dock 显示出来
			// （而不是只当普通 tool 失败被埋没在 timeline 里）。
			if block.IsError {
				if action, resources := sniffClaudePermissionDenial(resultStr); action != "" {
					trySend(ch, Event{
						Type:      EventPermission,
						RequestID: "denied:" + block.ToolUseID,
						Tool:      action,
						Resources: resources,
						Output:    resultStr,
					})
				}
			}
			trySend(ch, Event{
				Type:   EventToolResult,
				CallID: block.ToolUseID,
				Output: resultStr,
			})
		}
	}
}

// sniffClaudePermissionDenial 从 Claude 的权限拒绝文本里抽出 action 和
// 涉及资源。空 action 表示不像权限拒绝。
//
// 例子：
//
//	"Claude requested permissions to use Bash, but you haven't granted it..."
//	  → action="Bash"
//	"Claude requested permissions to read from /etc/passwd, but you haven't..."
//	  → action="Read", resources=["/etc/passwd"]
func sniffClaudePermissionDenial(text string) (action string, resources []string) {
	const prefix = "Claude requested permissions"
	idx := strings.Index(text, prefix)
	if idx < 0 {
		return "", nil
	}
	rest := text[idx+len(prefix):]
	// rest 形如：" to use Bash, but ..." / " to read from /etc/foo, but ..."
	rest = strings.TrimPrefix(rest, " to ")
	// 第一个逗号前的部分描述动作。
	if comma := strings.Index(rest, ","); comma >= 0 {
		rest = rest[:comma]
	}
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return "", nil
	}
	// "use Bash" → action=Bash
	// "read from /etc/foo" → action=Read, resource=/etc/foo
	// "edit /etc/foo" → action=Edit, resource=/etc/foo
	parts := strings.SplitN(rest, " ", 3)
	switch len(parts) {
	case 1:
		return parts[0], nil
	case 2:
		// "use X" / "read X" / "edit X"
		if parts[0] == "use" {
			return parts[1], nil
		}
		return parts[0], []string{parts[1]}
	default:
		// "read from /path/with spaces" / "edit /path"
		if parts[0] == "use" {
			// "use Bash from /foo" → 不太常见；保守处理。
			return parts[1], []string{parts[2]}
		}
		path := strings.Join(parts[1:], " ")
		path = strings.TrimPrefix(path, "from ")
		return parts[0], []string{path}
	}
}

// trySend 非阻塞 send；满了丢一条（host 应消费得足够快）。
func trySend(ch chan<- Event, e Event) {
	select {
	case ch <- e:
	default:
	}
}

// closeOnce 让 stdin 在多次关闭调用下幂等。
func closeOnce(c io.Closer) {
	type cinterface = interface{ Close() error }
	if c == nil {
		return
	}
	if s, ok := c.(interface{ Close() error }); ok {
		// exec.Cmd 的 StdinPipe 返回的 *closeOnce 包装已经幂等。
		_ = s.Close()
	}
}

// ── Claude SDK JSON types ──

type claudeSDKMessage struct {
	Type      string          `json:"type"`
	Message   json.RawMessage `json:"message,omitempty"`
	SessionID string          `json:"session_id,omitempty"`
	Model     string          `json:"model,omitempty"`

	ResultText string `json:"result,omitempty"`
	IsError    bool   `json:"is_error,omitempty"`

	Log *claudeLogEntry `json:"log,omitempty"`

	RequestID string          `json:"request_id,omitempty"`
	Request   json.RawMessage `json:"request,omitempty"`
}

type claudeControlRequestPayload struct {
	ToolName string          `json:"tool_name"`
	Input    json.RawMessage `json:"input"`
}

type claudeLogEntry struct {
	Level   string `json:"level"`
	Message string `json:"message"`
}

type claudeMessageContent struct {
	Role    string               `json:"role"`
	Model   string               `json:"model"`
	Content []claudeContentBlock `json:"content"`
}

type claudeContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}
