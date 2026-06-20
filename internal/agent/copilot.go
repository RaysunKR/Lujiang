package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// copilotBackend 通过 `copilot -p PROMPT --output-format json --allow-all --no-ask-user`
// 调起 GitHub Copilot CLI。stdout NDJSON（"dotted.event.name" 风格）。
// 协议参考：multica-main/server/pkg/agent/copilot.go。
type copilotBackend struct {
	cfg Config
}

func newCopilotBackend(cfg Config) *copilotBackend {
	return &copilotBackend{cfg: cfg}
}

func (b *copilotBackend) Execute(ctx context.Context, prompt string, opts ExecOptions) (*Session, error) {
	execPath, err := resolveExecPath(b.cfg, "copilot")
	if err != nil {
		return nil, fmt.Errorf("copilot executable not found: %w", err)
	}

	runCtx, cancel := runContext(ctx, 0)
	args := []string{
		"-p", prompt,
		"--output-format", "json",
		"--allow-all",
		"--no-ask-user",
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

	b.cfg.Logger.Info("copilot started", "pid", cmd.Process.Pid, "cwd", opts.Cwd, "model", opts.Model)

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
			var evt copilotEvent
			if err := json.Unmarshal([]byte(line), &evt); err != nil {
				continue
			}

			switch evt.Type {
			case "result":
				// 最后一行：top-level 字段，不是 data-driven。
				if evt.SessionID != "" {
					sessionID = evt.SessionID
				}
				if evt.ExitCode != 0 {
					finalStatus = "failed"
					finalError = fmt.Sprintf("copilot exit code %d", evt.ExitCode)
				}
			case "session.start":
				var d copilotSessionStart
				_ = json.Unmarshal(evt.Data, &d)
				if d.SessionID != "" {
					sessionID = d.SessionID
				}
				trySend(eventCh, Event{Type: EventStatus, Status: "running", Content: sessionID})
			case "session.error":
				var d copilotSessionError
				_ = json.Unmarshal(evt.Data, &d)
				if d.Message != "" {
					finalStatus = "failed"
					finalError = d.Message
					trySend(eventCh, Event{Type: EventError, Content: d.Message})
				}
			case "assistant.reasoning", "assistant.reasoning_delta":
				var d copilotReasoning
				_ = json.Unmarshal(evt.Data, &d)
				t := d.Content
				if t == "" {
					t = d.DeltaContent
				}
				if t != "" {
					trySend(eventCh, Event{Type: EventThinking, Content: t})
				}
			case "assistant.message":
				var d copilotAssistantMessage
				_ = json.Unmarshal(evt.Data, &d)
				if d.Content != "" {
					output.WriteString(d.Content)
					trySend(eventCh, Event{Type: EventText, Content: d.Content})
				}
				for _, tr := range d.ToolRequests {
					var args map[string]any
					if tr.Arguments != nil {
						_ = json.Unmarshal(tr.Arguments, &args)
					}
					trySend(eventCh, Event{
						Type:   EventToolUse,
						Tool:   tr.Name,
						CallID:  tr.ToolCallID,
						Input:  args,
					})
				}
			case "assistant.message_delta":
				var d copilotMessageDelta
				_ = json.Unmarshal(evt.Data, &d)
				if d.DeltaContent != "" {
					output.WriteString(d.DeltaContent)
					trySend(eventCh, Event{Type: EventText, Content: d.DeltaContent})
				}
			case "tool.execution_complete":
				var d copilotToolExecComplete
				_ = json.Unmarshal(evt.Data, &d)
				out := ""
				if d.Result != nil {
					if d.Result.DetailedContent != "" {
						out = d.Result.DetailedContent
					} else {
						out = d.Result.Content
					}
				}
				if !d.Success && d.Error != nil {
					out = fmt.Sprintf("%s\nerror: %s", out, d.Error.Message)
				}
				trySend(eventCh, Event{
					Type:   EventToolResult,
					CallID:  d.ToolCallID,
					Output: out,
				})
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
			finalError = fmt.Sprintf("copilot exited with error: %v", exitErr)
		}
		if finalError != "" {
			finalError = withStderrTail(finalError, "copilot", stderrT.Tail())
		}

		b.cfg.Logger.Info("copilot finished", "status", finalStatus, "duration", duration.Round(time.Millisecond).String())

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

func (b *copilotBackend) RespondPermission(ctx context.Context, reqID, decision string, updatedInput map[string]any) error {
	// Copilot CLI 没有 running permission seam；--allow-all 默认放行。
	return fmt.Errorf("copilot: permission seam not supported")
}

func (b *copilotBackend) Interrupt(ctx context.Context) error { return nil }
func (b *copilotBackend) Close() error                       { return nil }

// ── copilot JSONL event types ──

type copilotEvent struct {
	Type      string          `json:"type"`
	Data      json.RawMessage `json:"data,omitempty"`
	Timestamp string          `json:"timestamp,omitempty"`

	SessionID string `json:"sessionId,omitempty"`
	ExitCode  int    `json:"exitCode,omitempty"`
}

type copilotSessionStart struct {
	SessionID     string `json:"sessionId"`
	SelectedModel string `json:"selectedModel"`
}

type copilotAssistantMessage struct {
	Content      string                 `json:"content"`
	ToolRequests []copilotToolRequest   `json:"toolRequests"`
}

type copilotToolRequest struct {
	ToolCallID string          `json:"toolCallId"`
	Name       string          `json:"name"`
	Arguments  json.RawMessage `json:"arguments"`
}

type copilotMessageDelta struct {
	DeltaContent string `json:"deltaContent"`
}

type copilotToolExecComplete struct {
	ToolCallID string             `json:"toolCallId"`
	Success    bool               `json:"success"`
	Result     *copilotToolResult `json:"result,omitempty"`
	Error      *copilotToolError  `json:"error,omitempty"`
}

type copilotToolResult struct {
	Content         string `json:"content"`
	DetailedContent string `json:"detailedContent,omitempty"`
}

type copilotToolError struct {
	Message string `json:"message"`
}

type copilotReasoning struct {
	Content      string `json:"content,omitempty"`
	DeltaContent string `json:"deltaContent,omitempty"`
}

type copilotSessionError struct {
	ErrorType string `json:"errorType"`
	Message   string `json:"message"`
}
