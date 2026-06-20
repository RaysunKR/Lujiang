// events.ts —— Go internal/proto/agent.go 的 TS 镜像（手写）。
// scripts/gen-proto.ts 在 P7 会自动生成，现阶段手维护。

export type AgentEvent = {
  type: string;
  seq: number;
  ts: number;
  session_id?: string;
  message_id?: string;
  part_id?: string;
  text?: string;
  reason?: string;
  tool?: string;
  call_id?: string;
  input?: unknown;
  output?: string;
  status?: string;
  error?: string;
  permission?: { request_id: string; action: string; resources: string[] };
  provider_session_id?: string;
};

export const Ev = {
  SessionCreated: "session.created",
  SessionStatus: "session.status",
  SessionIdle: "session.idle",
  SessionError: "session.error",
  UserPrompt: "session.user.prompt",
  TextStarted: "session.next.text.started",
  TextDelta: "session.next.text.delta",
  TextEnded: "session.next.text.ended",
  ReasoningStarted: "session.next.reasoning.started",
  ReasoningDelta: "session.next.reasoning.delta",
  ReasoningEnded: "session.next.reasoning.ended",
  ToolStarted: "session.next.tool.input.started",
  ToolDelta: "session.next.tool.input.delta",
  ToolEnded: "session.next.tool.input.ended",
  ToolCalled: "session.next.tool.called",
  ToolSuccess: "session.next.tool.success",
  ToolFailed: "session.next.tool.failed",
  StepStarted: "session.next.step.started",
  StepEnded: "session.next.step.ended",
  StepFailed: "session.next.step.failed",
  PermissionAsked: "permission.v2.asked",
  PermissionReplied: "permission.v2.replied",
} as const;
