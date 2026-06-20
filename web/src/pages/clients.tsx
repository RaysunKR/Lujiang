import { For, Show, createResource, onCleanup } from "solid-js";
import { useNavigate } from "@solidjs/router";
import { useAuth } from "../context/auth";

type ClientMeta = {
  id: string;
  hostname: string;
  os: string;
  arch: string;
  shells?: string[];
};

async function fetchClients(): Promise<ClientMeta[]> {
  const r = await fetch("/api/clients", { credentials: "include" });
  if (!r.ok) throw new Error(`list clients failed (${r.status})`);
  return (await r.json()) as ClientMeta[];
}

export default function Clients() {
  const auth = useAuth();
  const nav = useNavigate();
  const [clients, { refetch }] = createResource(fetchClients);

  // 后台轮询：每 5s 拉一次，及时反映客户端上线/下线。
  // yamux 自带 30s keepalive 但浏览器不知道；轮询是最简单的"上下线提示"。
  const poll = setInterval(() => {
    refetch();
  }, 5000);
  onCleanup(() => clearInterval(poll));

  return (
    <div class="min-h-screen p-4 sm:p-6 lg:p-8">
      <header class="flex items-center justify-between mb-4 sm:mb-6 gap-2">
        <h1 class="text-lg sm:text-xl font-semibold truncate">在线客户端</h1>
        <div class="flex items-center gap-2 sm:gap-4 shrink-0">
          <span class="text-xs sm:text-sm text-neutral-400 truncate max-w-[8rem] sm:max-w-none">{auth.session()?.username}</span>
          <button
            class="text-xs sm:text-sm text-neutral-300 hover:text-white px-2 py-1 min-h-[36px]"
            onClick={async () => {
              await auth.logout();
              nav("/login", { replace: true });
            }}
          >
            登出
          </button>
        </div>
      </header>

      <div class="mb-4 flex items-center gap-2 sm:gap-3 flex-wrap">
        <button
          class="rounded-md border border-[var(--color-border)] px-3 py-1.5 text-sm hover:bg-white/5 min-h-[36px]"
          onClick={() => refetch()}
        >
          刷新
        </button>
        <Show when={clients.loading}>
          <span class="text-sm text-neutral-400">载入中…</span>
        </Show>
        <span class="text-xs text-neutral-500 hidden sm:inline">每 5 秒自动刷新</span>
      </div>

      <Show when={clients.error}>
        <div class="rounded-md border border-red-500/40 bg-red-500/10 p-3 text-sm text-red-300">
          {String(clients.error)}
        </div>
      </Show>

      <Show
        when={!clients.loading && (clients() ?? []).length > 0}
        fallback={
          <Show when={!clients.loading} fallback={<span />}>
            <div class="text-neutral-400">暂无在线客户端。</div>
          </Show>
        }
      >
        <div class="grid gap-3">
          <For each={clients()}>
            {(c) => (
              <div class="rounded-lg border border-[var(--color-border)] bg-[var(--color-panel)] p-3 sm:p-4 flex items-center justify-between gap-3">
                <div class="min-w-0">
                  <div class="font-medium flex items-center gap-2 truncate">
                    <span class="inline-block h-2 w-2 rounded-full bg-green-500 shrink-0" title="online" />
                    <span class="truncate">{c.id}</span>
                  </div>
                  <div class="text-xs sm:text-sm text-neutral-400 truncate">
                    {c.hostname} · {c.os}/{c.arch}
                    <Show when={c.shells && c.shells.length > 0}>
                      {" · "}
                      <span class="hidden sm:inline">shells: {c.shells!.join(", ")}</span>
                      <span class="sm:hidden">{c.shells!.length} shells</span>
                    </Show>
                  </div>
                </div>
                <button
                  class="rounded-md bg-[var(--color-accent)] text-black text-sm font-medium px-3 py-1.5 min-h-[36px] shrink-0"
                  onClick={() => nav(`/client/${c.id}`)}
                >
                  浏览文件
                </button>
              </div>
            )}
          </For>
        </div>
      </Show>
    </div>
  );
}
