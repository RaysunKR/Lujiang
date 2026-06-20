package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// opencodeBackend 通过 `opencode run --format json --dangerously-skip-permissions`
// 调起 opencode CLI。stdout NDJSON，每行一个 opencodeEvent。
// 协议参考：multica-main/server/pkg/agent/opencode.go。
type opencodeBackend struct {
	cfg Config
}

func newOpencodeBackend(cfg Config) *opencodeBackend {
	return &opencodeBackend{cfg: cfg}
}

func (b *opencodeBackend) Execute(ctx context.Context, prompt string, opts ExecOptions) (*Session, error) {
	execPath, err := resolveExecPath(b.cfg, "opencode")
	if err != nil {
		return nil, fmt.Errorf("opencode executable not found: %w", err)
	}

	runCtx, cancel := runContext(ctx, 0)
	args := []string{"run", "--format", "json", "--dangerously-skip-permissions"}
	if opts.Model != "" {
		args = append(args, "-m", opts.Model)
	}
	args = append(args, prompt)

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

	b.cfg.Logger.Info("opencode started", "pid", cmd.Process.Pid, "cwd", opts.Cwd, "model", opts.Model)

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
			var evt opencodeEvent
			if err := json.Unmarshal([]byte(line), &evt); err != nil {
				continue
			}

			if evt.SessionID != "" {
				sessionID = evt.SessionID
			}

			switch evt.Type {
			case "text":
				if evt.Part.Text != "" {
					output.WriteString(evt.Part.Text)
					trySend(eventCh, Event{Type: EventText, Content: evt.Part.Text})
				}
			case "tool_use":
				var input map[string]any
				if evt.Part.State != nil && evt.Part.State.Input != nil {
					_ = json.Unmarshal(evt.Part.State.Input, &input)
				}
				trySend(eventCh, Event{
					Type:   EventToolUse,
					Tool:   evt.Part.Tool,
					CallID: evt.Part.CallID,
					Input:  input,
				})
				if evt.Part.State != nil && evt.Part.State.Status == "completed" {
					trySend(eventCh, Event{
						Type:   EventToolResult,
						Tool:   evt.Part.Tool,
						CallID: evt.Part.CallID,
						Output: extractOpencodeToolOutput(evt.Part.State.Output),
					})
				}
			case "step_start":
				trySend(eventCh, Event{Type: EventStatus, Status: "running"})
			case "error":
				errMsg := "unknown opencode error"
				if evt.Error != nil {
					if m := evt.Error.Message(); m != "" {
						errMsg = m
					}
				}
				trySend(eventCh, Event{Type: EventError, Content: errMsg})
				finalStatus = "failed"
				finalError = errMsg
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
			finalError = fmt.Sprintf("opencode exited with error: %v", exitErr)
		}
		if finalError != "" {
			finalError = withStderrTail(finalError, "opencode", stderrT.Tail())
		}

		b.cfg.Logger.Info("opencode finished", "status", finalStatus, "duration", duration.Round(time.Millisecond).String())

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

func (b *opencodeBackend) RespondPermission(ctx context.Context, reqID, decision string, updatedInput map[string]any) error {
	// opencode --dangerously-skip-permissions 默认全部放行；没有运行时 seam。
	return fmt.Errorf("opencode: permission seam not supported")
}

func (b *opencodeBackend) Interrupt(ctx context.Context) error { return nil }
func (b *opencodeBackend) Close() error                       { return nil }

func extractOpencodeToolOutput(output any) string {
	if output == nil {
		return ""
	}
	if s, ok := output.(string); ok {
		return s
	}
	data, _ := json.Marshal(output)
	return string(data)
}

// ── opencode event JSON types ──

type opencodeEvent struct {
	Type      string            `json:"type"`
	Timestamp int64             `json:"timestamp,omitempty"`
	SessionID string            `json:"sessionID,omitempty"`
	Part      opencodeEventPart `json:"part"`
	Error     *opencodeError    `json:"error,omitempty"`
}

type opencodeEventPart struct {
	Text   string             `json:"text,omitempty"`
	Tool   string             `json:"tool,omitempty"`
	CallID string             `json:"callID,omitempty"`
	State  *opencodeToolState `json:"state,omitempty"`
}

type opencodeToolState struct {
	Status string          `json:"status,omitempty"`
	Input  json.RawMessage `json:"input,omitempty"`
	Output any             `json:"output,omitempty"`
}

type opencodeError struct {
	Name string           `json:"name,omitempty"`
	Data *opencodeErrData `json:"data,omitempty"`
}

func (e *opencodeError) Message() string {
	if e.Data != nil && e.Data.Message != "" {
		return e.Data.Message
	}
	if e.Name != "" {
		return e.Name
	}
	return ""
}

type opencodeErrData struct {
	Message string `json:"message,omitempty"`
}
