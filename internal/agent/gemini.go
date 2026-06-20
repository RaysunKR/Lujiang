package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// geminiBackend 通过 `gemini -p PROMPT --yolo -o stream-json` 调起 Google Gemini CLI。
// 协议：stdout NDJSON，事件类型见 geminiStreamEvent。
// 协议参考：multica-main/server/pkg/agent/gemini.go（端口移植，简化）。
type geminiBackend struct {
	cfg Config
}

func newGeminiBackend(cfg Config) *geminiBackend {
	return &geminiBackend{cfg: cfg}
}

func (b *geminiBackend) Execute(ctx context.Context, prompt string, opts ExecOptions) (*Session, error) {
	execPath, err := resolveExecPath(b.cfg, "gemini")
	if err != nil {
		return nil, fmt.Errorf("gemini executable not found: %w", err)
	}

	runCtx, cancel := runContext(ctx, 0)
	args := buildGeminiArgs(prompt, opts)

	cmd, stdout, stderrT, err := spawnCLI(spawnArgs{
		ctx:       runCtx,
		execPath:  execPath,
		args:      args,
		cwd:       opts.Cwd,
		env:       buildGeminiEnv(b.cfg.Env),
		waitDelay: 10 * time.Second,
	})
	if err != nil {
		cancel()
		return nil, err
	}

	b.cfg.Logger.Info("gemini started", "pid", cmd.Process.Pid, "cwd", opts.Cwd, "model", opts.Model)

	eventCh := make(chan Event, 256)
	resultCh := make(chan Result, 1)

	go func() {
		defer close(eventCh)
		defer close(resultCh)
		defer cancel()

		go func() {
			<-runCtx.Done()
			_ = stdout.Close()
		}()

		startTime := time.Now()
		var output strings.Builder
		var sessionID string
		finalStatus := "completed"
		var finalError string

		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 0, 1024*1024), 10*1024*1024)

		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}
			var evt geminiStreamEvent
			if err := json.Unmarshal([]byte(line), &evt); err != nil {
				continue
			}

			switch evt.Type {
			case "init":
				sessionID = evt.SessionID
				trySend(eventCh, Event{Type: EventStatus, Status: "running", Content: sessionID})
			case "message":
				if evt.Role == "assistant" && evt.Content != "" {
					output.WriteString(evt.Content)
					trySend(eventCh, Event{Type: EventText, Content: evt.Content})
				}
			case "tool_use":
				var params map[string]any
				if evt.Parameters != nil {
					_ = json.Unmarshal(evt.Parameters, &params)
				}
				trySend(eventCh, Event{
					Type:   EventToolUse,
					Tool:   evt.ToolName,
					CallID:  evt.ToolID,
					Input:  params,
				})
			case "tool_result":
				trySend(eventCh, Event{
					Type:   EventToolResult,
					CallID:  evt.ToolID,
					Output: evt.Output,
				})
			case "error":
				trySend(eventCh, Event{Type: EventError, Content: evt.Message})
			case "result":
				if evt.Status == "error" && evt.Error != nil {
					finalStatus = "failed"
					finalError = evt.Error.Message
				}
			}
		}

		exitErr := cmd.Wait()
		duration := time.Since(startTime)

		switch {
		case runCtx.Err() == context.Canceled:
			finalStatus = "aborted"
			finalError = "execution cancelled"
		case exitErr != nil && finalStatus == "completed":
			finalStatus = "failed"
			finalError = fmt.Sprintf("gemini exited with error: %v", exitErr)
		}
		if finalError != "" {
			finalError = withStderrTail(finalError, "gemini", stderrT.Tail())
		}

		b.cfg.Logger.Info("gemini finished", "status", finalStatus, "duration", duration.Round(time.Millisecond).String())

		resultCh <- Result{
			Status:     finalStatus,
			Output:     output.String(),
			Error:      finalError,
			DurationMs: duration.Milliseconds(),
			SessionID:  sessionID,
		}
	}()

	return &Session{
		Events:  eventCh,
		Result:  resultCh,
		cancel:  cancel,
		backend: b,
	}, nil
}

func (b *geminiBackend) RespondPermission(ctx context.Context, reqID, decision string, updatedInput map[string]any) error {
	// Gemini CLI 没有运行时权限 seam；权限由 --yolo 默认全部放行。
	// Composer 里的 mode 选择器只影响 prompt 提示语，不调用此方法。
	return fmt.Errorf("gemini: permission seam not supported")
}

func (b *geminiBackend) Interrupt(ctx context.Context) error {
	return nil // ctx 取消已经会杀进程；无需额外动作
}

func (b *geminiBackend) Close() error { return nil }

// ── Gemini stream-json event types ──

type geminiStreamEvent struct {
	Type      string          `json:"type"`
	Timestamp string          `json:"timestamp,omitempty"`
	SessionID string          `json:"session_id,omitempty"`
	Model     string          `json:"model,omitempty"`

	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
	Delta   bool   `json:"delta,omitempty"`

	ToolName   string          `json:"tool_name,omitempty"`
	ToolID     string          `json:"tool_id,omitempty"`
	Parameters json.RawMessage `json:"parameters,omitempty"`

	Status string `json:"status,omitempty"`
	Output string `json:"output,omitempty"`

	Severity string `json:"severity,omitempty"`
	Message  string `json:"message,omitempty"`

	Error *geminiStreamError `json:"error,omitempty"`
}

type geminiStreamError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

func buildGeminiArgs(prompt string, opts ExecOptions) []string {
	args := []string{
		"-p", prompt,
		"--yolo",
		"-o", "stream-json",
	}
	if opts.Model != "" {
		args = append(args, "-m", opts.Model)
	}
	return args
}

// buildGeminiEnv 默认 GEMINI_CLI_TRUST_WORKSPACE=true，跳过 gemini 的
// 文件夹信任门（headless 跑会失败，exit 55）。
func buildGeminiEnv(extra map[string]string) []string {
	const trustKey = "GEMINI_CLI_TRUST_WORKSPACE"
	if _, ok := extra[trustKey]; ok {
		return buildEnv(extra)
	}
	merged := make(map[string]string, len(extra)+1)
	for k, v := range extra {
		merged[k] = v
	}
	merged[trustKey] = "true"
	return buildEnv(merged)
}
