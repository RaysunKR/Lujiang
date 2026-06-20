import { For, Show, type Component } from "solid-js";
import { Ev, type AgentEvent } from "../../gen/events";
import Markdown from "../markdown";

// MessageTimeline 把事件流折成"消息块"再渲染。
// P5 简化：直接把事件按顺序列出，不做合并去重。P7 再做虚拟化 + delta 合并。

type Props = {
  events: AgentEvent[];
};

const MessageTimeline: Component<Props> = (props) => {
  return (
    <div class="px-3 sm:px-6 py-4 space-y-3 max-w-4xl mx-auto min-w-0">
      <For each={props.events} fallback={<div class="text-center text-neutral-500 py-8">向 agent 提问以开始</div>}>
        {(ev) => <EventRow ev={ev} />}
      </For>
    </div>
  );
};

const EventRow: Component<{ ev: AgentEvent }> = (props) => {
  const ev = () => props.ev;
  return (
    <Show when={visible(ev())} fallback={<></>}>
      <div class="rounded border border-[var(--color-border)]/40 bg-[var(--color-panel)] px-3 sm:px-4 py-2 text-sm min-w-0 overflow-hidden">
        <Show when={ev().type === Ev.UserPrompt}>
          <div class="flex flex-col gap-1">
            <div class="text-[10px] uppercase tracking-wide text-[var(--color-accent)]/80">你</div>
            <div class="whitespace-pre-wrap break-words text-neutral-100">{ev().text}</div>
          </div>
        </Show>
        <Show when={ev().type === Ev.TextDelta}>
          <div class="prose prose-invert max-w-none text-neutral-200 min-w-0">
            <Markdown text={ev().text || ""} />
          </div>
        </Show>
        <Show when={ev().type === Ev.ReasoningDelta}>
          <div class="text-neutral-500 italic text-xs whitespace-pre-wrap border-l-2 border-neutral-700 pl-3">
            {ev().reason}
          </div>
        </Show>
        <Show when={ev().type === Ev.ToolCalled}>
          <div class="flex items-center gap-2 text-xs text-neutral-300">
            <span class="text-[var(--color-accent)]">🔧 {ev().tool}</span>
            <code class="text-neutral-400 truncate flex-1">{summarizeInput(ev().input)}</code>
          </div>
        </Show>
        <Show when={ev().type === Ev.ToolSuccess}>
          <details class="text-xs text-neutral-400">
            <summary class="cursor-pointer text-green-400">✓ tool result</summary>
            <pre class="mt-1 whitespace-pre-wrap break-all">{ev().output}</pre>
          </details>
        </Show>
        <Show when={ev().type === Ev.ToolFailed}>
          <div class="text-xs text-red-400">✗ tool failed: {ev().error}</div>
        </Show>
        <Show when={ev().type === Ev.SessionStatus}>
          <div class="text-xs text-neutral-500">
            status: <span class="text-neutral-300">{ev().status}</span>
            <Show when={ev().error}>
              <span class="text-red-400"> — {ev().error}</span>
            </Show>
          </div>
        </Show>
        <Show when={ev().type === Ev.SessionError}>
          <div class="text-xs text-red-400">error: {ev().error}</div>
        </Show>
        <Show when={ev().type === Ev.SessionCreated}>
          <div class="text-xs text-neutral-500">session {ev().session_id?.slice(0, 8)}…</div>
        </Show>
      </div>
    </Show>
  );
};

function visible(ev: AgentEvent): boolean {
  switch (ev.type) {
    case Ev.UserPrompt:
    case Ev.TextDelta:
    case Ev.ReasoningDelta:
    case Ev.ToolCalled:
    case Ev.ToolSuccess:
    case Ev.ToolFailed:
    case Ev.SessionStatus:
    case Ev.SessionError:
    case Ev.SessionCreated:
      return true;
    default:
      return false;
  }
}

function summarizeInput(input: unknown): string {
  if (!input || typeof input !== "object") return "";
  const obj = input as Record<string, unknown>;
  if (typeof obj.command === "string") return obj.command;
  if (typeof obj.file_path === "string") return obj.file_path;
  if (typeof obj.pattern === "string") return obj.pattern;
  if (typeof obj.query === "string") return obj.query;
  try {
    return JSON.stringify(input).slice(0, 200);
  } catch {
    return "";
  }
}

export default MessageTimeline;
