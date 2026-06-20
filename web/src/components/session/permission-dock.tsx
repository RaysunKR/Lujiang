import { Show, For, type Component } from "solid-js";
import type { AgentEvent } from "../../gen/events";

// PermissionDock 在 dock 位置渲染 pending 的权限请求。
// 三按钮：Allow once / Allow always / Deny，借鉴 opencode session-permission-dock。
//
// 决策通过 onReply 回传到 host（→ WS permission.reply → 客户端 Backend.RespondPermission）。
// host 拿到回复后会触发下一条 agent 事件，pending 自动清空。

type Props = {
  pending: AgentEvent | null;
  onReply: (reqID: string, decision: string) => void;
};

const PermissionDock: Component<Props> = (props) => {
  const req = () => props.pending?.permission;
  // denied:X 是 Claude tool_result-as-error 翻译出来的；不是真正的 seam，
  // "允许" 的语义是用 bypassPermissions/acceptEdits 重跑 prompt。
  const isClaudeDenied = () => !!req() && req()!.request_id.startsWith("denied:");

  return (
    <Show when={req()}>
      {(r) => (
        <div class="border-t border-amber-500/40 bg-amber-500/5 px-3 sm:px-6 py-3">
          <div class="flex flex-col sm:flex-row sm:items-start gap-3">
            <div class="mt-0.5 text-amber-400 text-lg shrink-0">⚠</div>
            <div class="flex-1 min-w-0">
              <div class="text-sm text-amber-200">
                <Show
                  when={!isClaudeDenied()}
                  fallback={
                    <>
                      Claude 拒绝执行 <span class="font-mono font-semibold">{r().action || "tool"}</span>（权限不足）
                    </>
                  }
                >
                  Agent 请求执行 <span class="font-mono font-semibold">{r().action || "tool"}</span>
                </Show>
              </div>
              <Show when={r().resources?.length}>
                <div class="mt-1 text-xs text-neutral-300 font-mono">
                  <For each={r().resources}>
                    {(res) => (
                      <div class="truncate" title={res}>
                        {res}
                      </div>
                    )}
                  </For>
                </div>
              </Show>
              <Show when={isClaudeDenied()}>
                <div class="mt-1 text-xs text-neutral-400">
                  点"允许"用更高权限模式重跑同一条 prompt
                </div>
              </Show>
            </div>
            <div class="flex items-center gap-2 shrink-0">
              <button
                class="rounded bg-[var(--color-accent)] text-black px-3 py-1.5 text-sm font-medium hover:brightness-110 min-h-[36px]"
                onClick={() => props.onReply(r().request_id, "allow_once")}
              >
                允许一次
              </button>
              <button
                class="rounded border border-[var(--color-border)] px-3 py-1.5 text-sm hover:bg-white/5 min-h-[36px]"
                onClick={() => props.onReply(r().request_id, "allow_always")}
              >
                总是允许
              </button>
              <button
                class="rounded border border-red-500/50 px-3 py-1.5 text-sm text-red-300 hover:bg-red-500/10 min-h-[36px]"
                onClick={() => props.onReply(r().request_id, "deny")}
              >
                拒绝
              </button>
            </div>
          </div>
        </div>
      )}
    </Show>
  );
};

export default PermissionDock;
