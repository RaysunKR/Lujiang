import { createSignal, Show, For, onCleanup, onMount, createEffect } from "solid-js";
import { useNavigate, useParams } from "@solidjs/router";
import { useAuth } from "../context/auth";
import { Ev, type AgentEvent } from "../gen/events";
import MessageTimeline from "../components/session/message-timeline";
import Composer from "../components/session/composer";
import PermissionDock from "../components/session/permission-dock";

// ProjectPage 是 agent 工作区：左侧 session 列表 + 中间 message timeline + 底部 composer。
// URL：/project/:clientID?cwd=...&backend=...&model=...&session_id=...
//
// 多会话支持：
//   - 左侧列出客户端持久化的最近 N 条 session（live / done 都列）。
//   - 点 "新对话" 进入空白 composer；提交后服务端建 session 并自动 attach。
//   - 点历史 session：用 agent.resume 打开（重放历史 + 如仍活则 tail 新事件）。
//
// Agent 独立于 web：
//   - WS 断开不杀 agent；浏览器重新打开 session 走 resume 路径即可继续看。
//   - 异常断开会自动 resume 一次；用户主动切走再回来点 sidebar 也能恢复。

type SessionInfo = {
  id: string;
  backend: string;
  cwd: string;
  created_at: number;
  ended_at?: number;
  status?: string;
  last_seq: number;
  live: boolean;
  provider_session_id?: string;
  title?: string;
};

async function listSessions(clientId: string, cwdFilter?: string): Promise<SessionInfo[]> {
  const qs = new URLSearchParams({ limit: "50" });
  if (cwdFilter) qs.set("cwd", cwdFilter);
  const r = await fetch(
    `/api/session/${encodeURIComponent(clientId)}?${qs}`,
    { credentials: "include" },
  );
  if (!r.ok) {
    throw new Error(`list sessions: ${r.status}`);
  }
  const data = await r.json();
  return (data.sessions ?? []) as SessionInfo[];
}

export default function ProjectPage() {
  const params = useParams();
  const auth = useAuth();
  const nav = useNavigate();
  const clientId = () => params.clientID;
  const cwd = () => new URLSearchParams(location.search).get("cwd") || "";

  const [events, setEvents] = createSignal<AgentEvent[]>([]);
  const [ws, setWs] = createSignal<WebSocket | null>(null);
  const [busy, setBusy] = createSignal(false);
  const [pendingPerm, setPendingPerm] = createSignal<AgentEvent | null>(null);
  const [offline, setOffline] = createSignal<string | null>(null);

  // session 列表 + 当前打开的 session id（空表示 "新对话" 空白态）。
  const [sessions, setSessions] = createSignal<SessionInfo[]>([]);
  const [currentSessionId, setCurrentSessionId] = createSignal<string>("");
  // sidebar scope：cwd = 只看当前目录的 session；all = 全部目录混在一起。
  // 默认 cwd —— 用户进 /project?cwd=/etc 时只关心 /etc 的历史；切到 all 才能
  // 看见别的目录的 session。
  const [sessionScope, setSessionScope] = createSignal<"cwd" | "all">("cwd");
  // 桌面默认展开 sidebar；移动端默认收起（overlay 弹出，避免吃掉 60% 宽度）。
  // 用 matchMedia 而不是 UA，桌面窗口拉窄时也跟着切。
  const [sessionsOpen, setSessionsOpen] = createSignal(
    typeof window !== "undefined" && window.matchMedia("(min-width: 1024px)").matches,
  );

  // providerSessionId 是 Claude 自己的 session id（如 session-xxx）。
  // 多轮对话时下一条 prompt 用它做 resume_from，保留 provider 端完整上下文。
  // 由 session.status(running) 事件或打开历史 session 时填充。
  const [providerSessionId, setProviderSessionId] = createSignal<string>("");

  const replied = new Set<string>();
  const sessionState = { id: "", lastSeq: 0, done: false };
  // wsGen: 每次 openWS 自增；wireSock 注册的 handler 捕获当时的 gen，
  // 过期 WS 的 onclose / onmessage 检测到 gen !== wsGen.current 就直接 return。
  // 没有这个守卫，旧 WS 关闭时会触发 setBusy(false)，污染新会话的 UI。
  const wsGen = { current: 0 };
  // resumeAttempts：当前 session 自动重连连续失败计数。每次 onclose 触发自动
  // resumeSession 时 +1，每次成功收到事件或用户手动切换 session 时重置。
  // 上限 3 次后停止重连，避免 server 端 bug 把前端拖进无限循环。
  const resumeGuard = { attempts: 0, max: 3 };
  const lastSubmit = { prompt: "", backend: "claude", model: "", mode: "acceptEdits" };

  function append(ev: AgentEvent) {
    setEvents((prev) => [...prev, ev]);
    if (ev.seq && ev.seq > sessionState.lastSeq) {
      sessionState.lastSeq = ev.seq;
    }
    // 收到任意事件 = WS 健康，自动重连计数清零。
    resumeGuard.attempts = 0;
    // Claude 在 system frame 里上报自己的 session id；存下来供下一轮 --resume。
    if (
      ev.type === Ev.SessionStatus &&
      ev.status === "running" &&
      ev.provider_session_id
    ) {
      setProviderSessionId(ev.provider_session_id);
    }
    if (ev.type === Ev.PermissionAsked && ev.permission) {
      if (!replied.has(ev.permission.request_id)) {
        setPendingPerm(ev);
      }
    }
  }

  async function refreshSessions() {
    try {
      const filter = sessionScope() === "cwd" ? cwd() : "";
      const list = await listSessions(clientId(), filter);
      setSessions(list);
    } catch (e) {
      // 静默失败 —— sidebar 不应阻塞主功能区。
      console.warn(e);
    }
  }

  // sidebar scope 切换时立即刷一次。createEffect 首次也会跑，等价于 onMount。
  createEffect(() => {
    sessionScope();
    refreshSessions();
  });

  // 兜底：busy 期间每 2.5s 主动刷一次 sidebar，防止漏掉 session.status 事件
  // 导致 sidebar 还显示上一轮结束态。
  createEffect(() => {
    if (!busy()) return;
    const id = setInterval(refreshSessions, 2500);
    onCleanup(() => clearInterval(id));
  });

  function startSession(prompt: string, backend: string, model: string, mode: string) {
    // 多轮对话：已有 Lujiang session id 且 backend 没换时，下一条 prompt 走
    // continue_session_id，服务端复用同一 session（seq 续编号）并自动给 Claude
    // 传 --resume。timeline / sidebar 不分裂。首次提交 / backend 切换 / "新对话"
    // 按钮 → 视为新对话。
    const continuation =
      !!currentSessionId() && backend === lastSubmit.backend && !!lastSubmit.backend;

    if (!continuation) {
      setEvents([]);
      setProviderSessionId("");
      setCurrentSessionId("");
    }
    setPendingPerm(null);
    replied.clear();
    setBusy(true);
    setOffline(null);
    // 续轮：sessionState.id 保留，seq 命名空间延续。新对话：重置等 server 回 id。
    if (!continuation) {
      sessionState.id = "";
      sessionState.lastSeq = 0;
    }
    sessionState.done = false;
    lastSubmit.prompt = prompt;
    lastSubmit.backend = backend;
    lastSubmit.model = model;
    lastSubmit.mode = mode;

    const params: Record<string, string> = {
      backend,
      cwd: cwd(),
      prompt,
      model,
      permission_mode: mode,
    };
    if (continuation) {
      params.continue_session_id = currentSessionId()!;
    }
    openWS(new URLSearchParams(params), false);
  }

  function openSession(id: string) {
    if (id === currentSessionId() && busy()) return;
    // 移动端打开 session 后自动收起 sidebar，否则 timeline 被遮挡。
    if (typeof window !== "undefined" && !window.matchMedia("(min-width: 1024px)").matches) {
      setSessionsOpen(false);
    }
    setEvents([]);
    setProviderSessionId("");
    setPendingPerm(null);
    replied.clear();
    setBusy(true);
    setOffline(null);
    sessionState.id = id;
    sessionState.lastSeq = 0;
    sessionState.done = false;
    setCurrentSessionId(id);

    // 从 sidebar 的 session info 预填 provider id 和 backend —— 后续若用户在该
    // session 上发新 prompt，就走 --resume 续同一对话；replay 的 session.status
    // 也会再覆盖 provider id。
    const info = sessions().find((s) => s.id === id);
    if (info?.provider_session_id) {
      setProviderSessionId(info.provider_session_id);
    }
    if (info?.backend) {
      lastSubmit.backend = info.backend;
    }

    const qs = new URLSearchParams({
      session_id: id,
      last_seq: "0",
    });
    openWS(qs, true);
  }

  function newSession() {
    if (busy()) {
      if (!window.confirm("当前对话还在跑，切换会中断显示（agent 会继续在后台跑）。继续？")) {
        return;
      }
    }
    // 移动端点完"新对话"也要收 sidebar，否则 composer 被压。
    if (typeof window !== "undefined" && !window.matchMedia("(min-width: 1024px)").matches) {
      setSessionsOpen(false);
    }
    ws()?.close();
    setEvents([]);
    setProviderSessionId("");
    setPendingPerm(null);
    setOffline(null);
    setBusy(false);
    sessionState.id = "";
    sessionState.lastSeq = 0;
    sessionState.done = true;
    setCurrentSessionId("");
  }

  function openWS(qs: URLSearchParams, isResume: boolean) {
    // 先把旧 WS 的 handler 全部摘掉再关，避免旧 onclose 触发污染新会话状态。
    // 场景：denied 权限点"允许"会触发 startSession → 旧 WS 还没 EOF，
    // 几百 ms 后才 close；这段窗口里它的 handler 仍会跑。
    const old = ws();
    if (old) {
      try {
        old.onmessage = null;
        old.onerror = null;
        old.onclose = null;
        old.close();
      } catch {
        /* ignore */
      }
    }
    wsGen.current += 1;
    const gen = wsGen.current;
    const proto = location.protocol === "https:" ? "wss:" : "ws:";
    const url = `${proto}//${location.host}/api/session/${encodeURIComponent(clientId())}/ws?${qs}`;
    const sock = new WebSocket(url);
    setWs(sock);
    wireSock(sock, isResume, gen);
  }

  function resumeSession() {
    if (!sessionState.id || sessionState.done) return;
    const qs = new URLSearchParams({
      session_id: sessionState.id,
      last_seq: String(sessionState.lastSeq),
    });
    openWS(qs, true);
  }

  function wireSock(sock: WebSocket, isResume: boolean, gen: number) {
    sock.onmessage = (e) => {
      if (gen !== wsGen.current) return; // 过期 WS 的事件，忽略
      try {
        const ev = JSON.parse(e.data) as AgentEvent;
        if (isResume && ev.seq && ev.seq <= sessionState.lastSeq) {
          return;
        }
        if (!isResume && ev.session_id && !sessionState.id) {
          sessionState.id = ev.session_id;
          setCurrentSessionId(ev.session_id);
          // 服务端返回的 session id 是新对话的真实 id；刷新 sidebar 高亮它。
          refreshSessions();
        }
        append(ev);
        if (ev.type === Ev.SessionIdle || ev.type === Ev.SessionError) {
          sessionState.done = true;
          setBusy(false);
          // 会话结束就清 dock —— Claude 超时放弃权限等待时，browser 拿到
          // idle 后 dock 还挂着会让用户以为连接卡住。denied: 类的也一起清。
          setPendingPerm(null);
          refreshSessions();
        } else if (ev.type === Ev.SessionStatus) {
          // session.status(running/completed/...) 都触发一次 sidebar 刷新，
          // 保证 sidebar 的 live / status 徽章与 timeline 同步。
          refreshSessions();
        }
      } catch {
        // ignore non-JSON
      }
    };
    sock.onerror = () => {
      // 不在这里 setOffline —— onerror 之后浏览器必定会 fire onclose，
      // 由 close code 决定是否 offline，避免误把 NormalClosure 当成掉线。
    };
    sock.onclose = (e) => {
      if (gen !== wsGen.current) return; // 过期 WS 的 close，忽略
      setBusy(false);
      if (e.code === 1000) {
        if (isResume) setOffline(null);
        // NormalClosure 也清一下 dock —— 万一服务端没发 idle 就关了
        // （比如 session.resume 已结束会话时漏发 idle 的历史 bug）。
        setPendingPerm(null);
        refreshSessions();
        return;
      }
      // 异常关闭：如果已拿到 session id 且未结束，尝试自动 resume。
      // 但限制最多 3 次连续重试 —— 服务端如果反复用非 1000 close code
      // 关 WS（比如 resume done session 时漏发 idle），不要再无限重连。
      if (sessionState.id && !sessionState.done && resumeGuard.attempts < resumeGuard.max) {
        resumeGuard.attempts += 1;
        setOffline(`重连中（${resumeGuard.attempts}/${resumeGuard.max}）…`);
        setTimeout(() => resumeSession(), 800);
        return;
      }
      const reason = e.reason?.trim();
      setOffline(reason || `连接异常关闭（code ${e.code}）`);
    };
  }

  function interrupt() {
    ws()?.send(JSON.stringify({ type: "interrupt" }));
  }

  function replyPermission(reqID: string, decision: string) {
    if (reqID.startsWith("denied:")) {
      setPendingPerm(null);
      replied.add(reqID);
      if (decision === "deny") return;
      const nextMode = decision === "allow_always" ? "bypassPermissions" : "acceptEdits";
      if (lastSubmit.prompt) {
        startSession(lastSubmit.prompt, lastSubmit.backend, lastSubmit.model, nextMode);
      }
      return;
    }
    const sock = ws();
    // WS 已经关闭（最常见：Claude 自己 ~30s 等不到回复就退出，
    // server 走 idle → NormalClosure）。这时再 send 会被浏览器静默丢掉，
    // 用户看到 dock 消失但 agent 没动静，体感像"断线"。直接告知用户。
    if (!sock || sock.readyState !== WebSocket.OPEN) {
      setPendingPerm(null);
      setOffline("权限回复未送达：agent 已结束本轮（可能等超时了）。请重发 prompt 再试。");
      return;
    }
    try {
      sock.send(
        JSON.stringify({ type: "permission.reply", request_id: reqID, decision }),
      );
    } catch {
      setPendingPerm(null);
      setOffline("权限回复发送失败（WS 异常）。请重发 prompt 再试。");
      return;
    }
    replied.add(reqID);
    setPendingPerm(null);
  }

  onCleanup(() => {
    ws()?.close();
  });

  return (
    <div class="h-screen flex flex-col">
      <header class="flex items-center gap-2 sm:gap-4 px-3 sm:px-4 py-2 border-b border-[var(--color-border)]">
        <button class="text-xs sm:text-sm hover:underline shrink-0" onClick={() => nav(`/client/${clientId()}`)}>
          ← <span class="hidden sm:inline">文件</span>
        </button>
        <div class="text-xs sm:text-sm text-neutral-400 truncate shrink-0">{clientId()}</div>
        <div class="text-xs sm:text-sm text-neutral-500 truncate min-w-0 flex-1 hidden md:block">{cwd() || "(no cwd)"}</div>
        <Show when={busy()}>
          <button
            class="rounded border border-red-500/40 bg-red-500/10 px-3 py-1 text-xs sm:text-sm text-red-300 hover:bg-red-500/20 shrink-0 min-h-[32px]"
            onClick={interrupt}
          >
            中断
          </button>
        </Show>
        <button
          class="text-xs sm:text-sm text-neutral-300 hover:text-white shrink-0 lg:hidden"
          onClick={() => setSessionsOpen((v) => !v)}
          title="会话列表"
        >
          ☰
        </button>
        <span class="text-xs sm:text-sm text-neutral-400 hidden sm:inline shrink-0">{auth.session()?.username}</span>
      </header>

      <div class="flex-1 min-h-0 flex relative">
        {/* 会话侧栏
         * 桌面 (lg+)：static，占据 flex 流，w-56 + 右边框。
         * 移动：absolute overlay，从左滑入，背后加半透明 backdrop 点一下关闭。
         * 旧实现两种尺寸共用 static 布局，sidebar 在手机上吃掉 56*4=224px，
         * 主区只剩 166px 没法用。
         */}
        <Show when={sessionsOpen()}>
          <div
            class="absolute inset-0 z-20 bg-black/50 lg:hidden"
            onClick={() => setSessionsOpen(false)}
          />
          <aside class="w-64 sm:w-56 shrink-0 border-r border-[var(--color-border)] flex flex-col min-h-0 bg-[var(--color-panel)] absolute inset-y-0 left-0 z-30 lg:static lg:z-auto">
            <div class="px-3 py-2 border-b border-[var(--color-border)] flex items-center gap-2">
              <span class="text-xs text-neutral-400 flex-1">会话</span>
              <button
                class="text-xs rounded border border-[var(--color-border)] px-2 py-1 hover:bg-white/5 min-h-[28px]"
                onClick={refreshSessions}
                title="刷新"
              >
                🔄
              </button>
              <button
                class="text-xs rounded border border-[var(--color-border)] px-2 py-1 hover:bg-white/5 min-h-[28px] lg:hidden"
                onClick={() => setSessionsOpen(false)}
                title="收起"
              >
                ✕
              </button>
            </div>
            <div class="px-2 py-2 border-b border-[var(--color-border)]">
              <button
                class="w-full rounded bg-[var(--color-accent)]/20 border border-[var(--color-accent)]/40 px-2 py-1.5 text-sm hover:bg-[var(--color-accent)]/30"
                classList={{ "bg-[var(--color-accent)]/40": currentSessionId() === "" }}
                onClick={newSession}
              >
                ＋ 新对话
              </button>
              <div class="mt-2 flex rounded border border-[var(--color-border)] overflow-hidden text-xs">
                <button
                  class="flex-1 px-2 py-1 min-h-[28px] hover:bg-white/5"
                  classList={{ "bg-[var(--color-accent)]/25 text-neutral-100": sessionScope() === "cwd" }}
                  onClick={() => setSessionScope("cwd")}
                  title={cwd() ? `只看 ${cwd()}` : "只看当前目录"}
                >
                  当前目录
                </button>
                <button
                  class="flex-1 px-2 py-1 min-h-[28px] hover:bg-white/5 border-l border-[var(--color-border)]"
                  classList={{ "bg-[var(--color-accent)]/25 text-neutral-100": sessionScope() === "all" }}
                  onClick={() => setSessionScope("all")}
                  title="列出所有目录的会话"
                >
                  全部
                </button>
              </div>
            </div>
            <div class="flex-1 min-h-0 overflow-auto">
              <Show when={sessions().length > 0} fallback={
                <div class="p-3 text-xs text-neutral-500">暂无历史会话</div>
              }>
                <For each={sessions()}>
                  {(s) => (
                    <button
                      class="w-full text-left px-3 py-2 border-b border-[var(--color-border)]/40 hover:bg-white/5 flex flex-col gap-0.5"
                      classList={{ "bg-[var(--color-accent)]/15": s.id === currentSessionId() }}
                      onClick={() => openSession(s.id)}
                    >
                      <div class="flex items-center gap-2 text-xs">
                        <span
                          class="truncate flex-1 text-neutral-200"
                          title={s.title || `${s.backend} · ${s.cwd}`}
                        >
                          {s.title || sessionFallbackLabel(s)}
                        </span>
                        <Show when={s.live}>
                          <span class="text-[10px] rounded bg-green-500/20 text-green-300 px-1.5 py-0.5 shrink-0 animate-pulse">
                            运行中
                          </span>
                        </Show>
                        <Show when={!s.live && s.status}>
                          <span class="text-[10px] text-neutral-500 shrink-0">{statusLabel(s.status)}</span>
                        </Show>
                      </div>
                      <div class="text-[11px] text-neutral-500 truncate flex items-center gap-1">
                        <span class="text-neutral-600">{s.backend}</span>
                        <span class="text-neutral-700">·</span>
                        <span class="truncate">{shortCwd(s.cwd)}</span>
                      </div>
                      <div class="text-[10px] text-neutral-600">{formatTime(s.created_at)}</div>
                    </button>
                  )}
                </For>
              </Show>
            </div>
          </aside>
        </Show>

        {/* 主区域：timeline + composer
         * min-w-0 必须有：flex item 默认 min-width:auto=content 的 min-content，
         * 长代码块/URL 会把主区撑出 viewport。加 min-w-0 才能被 flex 收缩。 */}
        <div class="flex-1 min-h-0 flex flex-col min-w-0">
          <div class="flex-1 min-h-0 overflow-auto">
            <MessageTimeline events={events()} />
          </div>

          <Show when={offline()}>
            <div class="border-t border-amber-500/40 bg-amber-500/10 px-6 py-2 text-sm text-amber-200 flex items-center gap-3">
              <span>⚠ {offline()}</span>
              <button
                class="ml-auto rounded border border-amber-500/40 px-2 py-0.5 text-xs hover:bg-amber-500/20"
                onClick={() => nav(`/clients`)}
              >
                返回列表
              </button>
            </div>
          </Show>

          <PermissionDock pending={pendingPerm()} onReply={replyPermission} />

          <div class="border-t border-[var(--color-border)]">
            <Composer
              disabled={busy()}
              onSubmit={startSession}
              clientId={clientId()}
              cwd={cwd()}
            />
          </div>
        </div>
      </div>
    </div>
  );
}

function shortCwd(p: string): string {
  if (!p) return "(no cwd)";
  const parts = p.split(/[\\/]/).filter(Boolean);
  if (parts.length <= 2) return p;
  return ".../" + parts.slice(-2).join("/");
}

// sessionFallbackLabel：老 session（迁移前没存 title）兜底显示。
function sessionFallbackLabel(s: SessionInfo): string {
  return `${s.backend} · ${shortCwd(s.cwd)}`;
}

// statusLabel：把 server 的英文 status 翻成中文显示给用户。
function statusLabel(s?: string): string {
  switch (s) {
    case "completed": return "完成";
    case "failed": return "失败";
    case "interrupted": return "中断";
    case "running": return "运行中";
    case "": return "";
    default: return s || "";
  }
}

function formatTime(ms: number): string {
  if (!ms) return "";
  const d = new Date(ms);
  const now = new Date();
  const sameDay = d.toDateString() === now.toDateString();
  if (sameDay) {
    return d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
  }
  return d.toLocaleDateString([], { month: "2-digit", day: "2-digit" }) +
    " " + d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
}
