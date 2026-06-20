package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// cursorBackend 通过 `cursor-agent -p PROMPT --output-format stream-json` 调起
// Cursor Agent CLI。Cursor 协议与 Claude Code 高度相似（NDJSON，事件类型同名）。
// 协议参考：multica-main/server/pkg/agent/cursor.go。
type cursorBackend struct {
	cfg Config
}

func newCursorBackend(cfg Config) *cursorBackend {
	return &cursorBackend{cfg: cfg}
}

func (b *cursorBackend) Execute(ctx context.Context, prompt string, opts ExecOptions) (*Session, error) {
	execPath, err := resolveExecPath(b.cfg, "cursor-agent")
	if err != nil {
		return nil, fmt.Errorf("cursor-agent executable not found: %w", err)
	}

	runCtx, cancel := runContext(ctx, 0)
	args := []string{
		"-p", prompt,
		"--output-format", "stream-json",
	}
	if opts.PermissionMode == "bypass" || opts.PermissionMode == "bypassPermissions" {
		args = append(args, "--dangerously-skip-permissions")
	}
	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}

	cmd, stdout, stderrT, err := spawnCLI(spawnArgs{
		ctx:       runCtx,
		execPath:  execPath,
		args:      args,
		cwd:       opts.Cwd,
		env:       buildEnv(b.cfg.Env),
		waitDelay: 10 * time.Second,
	})
	if err != nil {
		cancel()
		return nil, err
	}

	b.cfg.Logger.Info("cursor-agent started", "pid", cmd.Process.Pid, "cwd", opts.Cwd, "model", opts.Model)

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
		resultSeen := false

		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 0, 1024*1024), 10*1024*1024)

		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}
			var evt cursorStreamEvent
			if err := json.Unmarshal([]byte(line), &evt); err != nil {
				continue
			}

			if sid := evt.readSessionID(); sid != "" {
				sessionID = sid
			}

			switch evt.Type {
			case "system":
				if evt.Subtype == "init" {
					trySend(eventCh, Event{Type: EventStatus, Status: "running", Content: sessionID})
				}
				if evt.Subtype == "error" {
					if msg := cursorErrorText(&evt); msg != "" {
						trySend(eventCh, Event{Type: EventError, Content: msg})
					}
				}
			case "assistant":
				for _, blk := range cursorParseAssistant(evt.Message) {
					switch blk.kind {
					case "text":
						output.WriteString(blk.text)
						trySend(eventCh, Event{Type: EventText, Content: blk.text})
					case "thinking":
						trySend(eventCh, Event{Type: EventThinking, Content: blk.text})
					}
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
			case "result":
				resultSeen = true
				if evt.IsError || evt.Subtype == "error" {
					finalStatus = "failed"
					finalError = cursorErrorText(&evt)
				}
				if evt.ResultText != "" && output.Len() == 0 {
					output.WriteString(evt.ResultText)
				}
			case "error":
				if msg := cursorErrorText(&evt); msg != "" {
					finalError = msg
					trySend(eventCh, Event{Type: EventError, Content: msg})
				}
			case "text":
				if evt.Part != nil {
					var part cursorTextPart
					_ = json.Unmarshal(evt.Part, &part)
					if part.Text != "" {
						output.WriteString(part.Text)
						trySend(eventCh, Event{Type: EventText, Content: part.Text})
					}
				}
			}
		}

		exitErr := cmd.Wait()
		duration := time.Since(startTime)

		switch {
		case runCtx.Err() == context.Canceled && !resultSeen:
			finalStatus = "aborted"
			finalError = "execution cancelled"
		case exitErr != nil && finalStatus == "completed" && !resultSeen:
			finalStatus = "failed"
			finalError = fmt.Sprintf("cursor-agent exited with error: %v", exitErr)
		}
		if finalError != "" {
			finalError = withStderrTail(finalError, "cursor", stderrT.Tail())
		}

		b.cfg.Logger.Info("cursor-agent finished", "status", finalStatus, "duration", duration.Round(time.Millisecond).String())

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

func (b *cursorBackend) RespondPermission(ctx context.Context, reqID, decision string, updatedInput map[string]any) error {
	// cursor-agent 没有运行时权限 seam；权限在 CLI flag 层（--dangerously-skip-permissions）。
	return fmt.Errorf("cursor: permission seam not supported")
}

func (b *cursorBackend) Interrupt(ctx context.Context) error { return nil }
func (b *cursorBackend) Close() error                       { return nil }

// ── cursor event JSON types ──

type cursorStreamEvent struct {
	Type     string          `json:"type"`
	Subtype  string          `json:"subtype,omitempty"`
	SessionID string         `json:"session_id,omitempty"`
	Model    string          `json:"model,omitempty"`

	Message json.RawMessage `json:"message,omitempty"`
	Part    json.RawMessage `json:"part,omitempty"`

	ToolName   string          `json:"tool_name,omitempty"`
	ToolID     string          `json:"tool_id,omitempty"`
	Parameters json.RawMessage `json:"parameters,omitempty"`

	Output string `json:"output,omitempty"`

	IsError bool `json:"is_error,omitempty"`

	ResultText string                 `json:"result_text,omitempty"`
	Error      *cursorErrorPayload    `json:"error,omitempty"`
	SubErr     *cursorSubErrorPayload `json:"cursor_error,omitempty"`
}

type cursorErrorPayload struct {
	Type    string `json:"type,omitempty"`
	Message string `json:"message,omitempty"`
}

type cursorSubErrorPayload struct {
	Type    string `json:"type,omitempty"`
	Message string `json:"message,omitempty"`
}

type cursorTextPart struct {
	Text string `json:"text,omitempty"`
}

func (e *cursorStreamEvent) readSessionID() string {
	if e.SessionID != "" {
		return e.SessionID
	}
	// 某些版本 session_id 在 message 里。
	if e.Message != nil {
		var probe struct {
			SessionID string `json:"session_id"`
		}
		_ = json.Unmarshal(e.Message, &probe)
		return probe.SessionID
	}
	return ""
}

func cursorErrorText(e *cursorStreamEvent) string {
	if e.SubErr != nil && e.SubErr.Message != "" {
		return e.SubErr.Message
	}
	if e.Error != nil && e.Error.Message != "" {
		return e.Error.Message
	}
	if e.ResultText != "" {
		return e.ResultText
	}
	return ""
}

// cursorAssistantBlock 是从 message.content[] 里抽出的简化结构。
type cursorAssistantBlock struct {
	kind string // "text" | "thinking"
	text string
}

func cursorParseAssistant(raw json.RawMessage) []cursorAssistantBlock {
	if raw == nil {
		return nil
	}
	var msg struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		return nil
	}
	out := make([]cursorAssistantBlock, 0, len(msg.Content))
	for _, c := range msg.Content {
		switch c.Type {
		case "output_text", "text":
			if c.Text != "" {
				out = append(out, cursorAssistantBlock{kind: "text", text: c.Text})
			}
		case "thinking":
			if c.Text != "" {
				out = append(out, cursorAssistantBlock{kind: "thinking", text: c.Text})
			}
		}
	}
	return out
}
