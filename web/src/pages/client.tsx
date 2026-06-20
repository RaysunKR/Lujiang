import { Show, For, createEffect, createSignal, on, lazy } from "solid-js";
import { useNavigate, useParams } from "@solidjs/router";
import { useAuth } from "../context/auth";
import Editor from "../components/editor";
import { fsList, fsRead, fsWrite, fsMkdir, type FSEntry } from "../api/fs";

// 仅在打开终端时才加载 xterm（减小初始 bundle）。
const TerminalView = lazy(() => import("../components/terminal"));

type EditState = {
  path: string;
  content: string;
  original: string;
  language?: string;
  binary?: boolean;
};

export default function Client() {
  const params = useParams();
  const auth = useAuth();
  const nav = useNavigate();
  const clientId = () => params.id;

  const [cwd, setCwd] = createSignal<string>(".");
  // listedPath：最近一次 fsList 成功返回的绝对路径。enter() 用它拼下级路径，
  // 不读 cwd() —— 否则用户连点同一行时 SolidJS 同步更新的 cwd() 会让每次 click
  // 都在前一次的 append 结果上再 append，路径就退化成 /hello/hello/hello/...
  const [listedPath, setListedPath] = createSignal<string>("");
  const [entries, setEntries] = createSignal<FSEntry[]>([]);
  const [error, setError] = createSignal<string | null>(null);
  const [loading, setLoading] = createSignal(false);
  const [edit, setEdit] = createSignal<EditState | null>(null);
  const [saving, setSaving] = createSignal(false);
  const [dirty, setDirty] = createSignal(false);

  // 终端面板：可隐藏；shell 选择由 /api/clients 元数据提供。
  const [termOpen, setTermOpen] = createSignal(false);
  const [termShell, setTermShell] = createSignal<string>("");
  const [termKey, setTermKey] = createSignal(1); // 切换 shell / 重启时 remount
  const [termHeight, setTermHeight] = createSignal(280); // 拖拽高度
  const [shells, setShells] = createSignal<string[]>([]);

  // clientId 变化（含初始挂载）：拉 shell 元数据 + 必要时重置 cwd。
  // 切换 client 时如果保留旧 cwd，会拿前一个 client 的绝对路径去 list 新 client，
  // 要么 404 要么列错目录，所以切换时一律回 "."。
  createEffect(on(clientId, async (id, prevId) => {
    if (prevId !== undefined && id !== prevId) {
      setCwd(".");
    }
    try {
      const r = await fetch("/api/clients", { credentials: "include" });
      if (!r.ok) return;
      const list = (await r.json()) as { id: string; shells?: string[] }[];
      const me = list.find((c) => c.id === id);
      if (me?.shells && me.shells.length > 0) {
        setShells(me.shells);
        setTermShell(me.shells[0]);
      }
    } catch {
      // 元数据不可得时让终端用客户端默认 shell。
    }
  }, { defer: false }));

  async function reload() {
    setLoading(true);
    setError(null);
    try {
      const res = await fsList(clientId(), cwd());
      setCwd(res.path);
      setListedPath(res.path);
      setEntries(sortEntries(res.entries ?? []));
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setLoading(false);
    }
  }

  // cwd 变化（初始 "."、点击目录、breadcrumb、上级、跨 client 重置）→ 自动 reload。
  // 旧版本只在 clientId 变化时 reload，导致点击文件夹 / breadcrumb / 上级按钮全部静默失败。
  createEffect(on(cwd, reload, { defer: false }));

  function enter(e: FSEntry) {
    // 用 listedPath()（上次成功 listing 的绝对路径）做 base，不用 cwd()：
    // cwd() 在同一事件循环里被 setCwd 同步更新，连点同一行会累积 append。
    // listedPath 只在 reload 完成后才更新，连点期间保持稳定。
    const base = listedPath() || cwd();
    if (e.type === "dir") {
      setCwd(joinPath(base, e.name));
    } else if (e.type === "file") {
      openFile(joinPath(base, e.name));
    }
  }

  async function openFile(path: string) {
    setError(null);
    try {
      const res = await fsRead(clientId(), path);
      if (res.encoding === "base64") {
        setEdit({ path, content: "", original: "", binary: true });
      } else {
        setEdit({
          path,
          content: res.content,
          original: res.content,
        });
        setDirty(false);
      }
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    }
  }

  async function save() {
    const cur = edit();
    if (!cur || cur.binary) return;
    setSaving(true);
    setError(null);
    try {
      await fsWrite(clientId(), cur.path, cur.content);
      setEdit({ ...cur, original: cur.content });
      setDirty(false);
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setSaving(false);
    }
  }

  async function newFolder() {
    const name = window.prompt("请输入新文件夹名（不存在的目录名）");
    if (!name) return;
    try {
      await fsMkdir(clientId(), joinPath(cwd(), name));
      await reload();
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    }
  }

  async function newFile() {
    const name = window.prompt("请输入新文件名（可含相对路径，如 src/foo.txt）");
    if (!name) return;
    try {
      await fsWrite(clientId(), joinPath(cwd(), name), "");
      await reload();
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    }
  }

  // breadcrumbs：把绝对路径拆成可点击的段。要处理 POSIX 根（/）和
  // Windows 盘符根（D:/）。filepath.ToSlash 在 Windows 上把 D:\foo → D:/foo，
  // 所以协议里始终是 POSIX-style；我们只要认 D: 这种盘符前缀。
  const breadcrumbs = () => {
    const path = cwd();
    const out: { name: string; path: string }[] = [];

    if (path.startsWith("/")) {
      out.push({ name: "/", path: "/" });
      let acc = "";
      for (const p of path.split("/").filter(Boolean)) {
        acc += "/" + p;
        out.push({ name: p, path: acc });
      }
      return out;
    }

    const m = path.match(/^([A-Za-z]:)(\/.*)?$/);
    if (m) {
      const driveRoot = m[1] + "/";
      out.push({ name: driveRoot, path: driveRoot });
      let acc = driveRoot;
      if (m[2]) {
        for (const p of m[2].split("/").filter(Boolean)) {
          acc = acc.endsWith("/") ? acc + p : acc + "/" + p;
          out.push({ name: p, path: acc });
        }
      }
      return out;
    }

    return out;
  };

  return (
    <div class="h-screen flex flex-col">
      <header class="flex items-center gap-2 sm:gap-4 px-3 sm:px-4 py-2 border-b border-[var(--color-border)]">
        <button class="text-xs sm:text-sm hover:underline shrink-0" onClick={() => nav("/clients")}>
          ← <span class="hidden sm:inline">客户端列表</span><span class="sm:hidden">返回</span>
        </button>
        <div class="text-xs sm:text-sm text-neutral-400 truncate">{clientId()}</div>
        <div class="flex-1" />
        <span class="text-xs sm:text-sm text-neutral-400 hidden sm:inline">{auth.session()?.username}</span>
        <button
          class="text-xs sm:text-sm text-neutral-300 hover:text-white shrink-0"
          onClick={async () => {
            await auth.logout();
            nav("/login", { replace: true });
          }}
        >
          登出
        </button>
      </header>

      <div class="flex items-center gap-2 px-3 sm:px-4 py-2 border-b border-[var(--color-border)] text-sm overflow-x-auto">
        <button class="rounded border border-[var(--color-border)] px-2 py-1 hover:bg-white/5 shrink-0 min-h-[32px]" onClick={() => setCwd(parentOf(cwd()))} title="上级">
          ↑ <span class="hidden sm:inline">上级</span>
        </button>
        <button class="rounded border border-[var(--color-border)] px-2 py-1 hover:bg-white/5 shrink-0 min-h-[32px]" onClick={reload} title="刷新">
          🔄 <span class="hidden sm:inline">刷新</span>
        </button>
        <button class="rounded border border-[var(--color-border)] px-2 py-1 hover:bg-white/5 shrink-0 min-h-[32px]" onClick={newFolder} title="新建文件夹">
          📁 <span class="hidden sm:inline">新建文件夹</span>
        </button>
        <button class="rounded border border-[var(--color-border)] px-2 py-1 hover:bg-white/5 shrink-0 min-h-[32px]" onClick={newFile} title="新建文件">
          📄 <span class="hidden sm:inline">新建文件</span>
        </button>
        <button
          class="rounded border border-[var(--color-border)] px-2 py-1 hover:bg-white/5 shrink-0 min-h-[32px]"
          classList={{ "bg-[var(--color-accent)]/20": termOpen() }}
          onClick={() => setTermOpen((v) => !v)}
        >
          {termOpen() ? "✕ 关闭终端" : "🖥 终端"}
        </button>
        <button
          class="rounded border border-[var(--color-border)] px-2 py-1 hover:bg-white/5 shrink-0 min-h-[32px]"
          onClick={() => nav(`/project/${clientId()}?cwd=${encodeURIComponent(cwd())}`)}
        >
          🤖 <span class="hidden sm:inline">Agent</span>
        </button>
        <Show when={termOpen()}>
          <select
            class="rounded border border-[var(--color-border)] bg-[var(--color-panel)] px-2 py-1 text-xs shrink-0 min-h-[32px]"
            value={termShell()}
            onChange={(e) => {
              setTermShell(e.currentTarget.value);
              setTermKey((k) => k + 1); // 切 shell 即重启会话
            }}
          >
            <For each={shells()} fallback={<option value="">default</option>}>
              {(s) => <option value={s}>{s}</option>}
            </For>
          </select>
        </Show>
        <div class="truncate px-2 text-neutral-300 min-w-0">
          <For each={breadcrumbs()}>
            {(b, i) => {
              // 根段（POSIX "/" 或 Windows "D:/"）的名字自带尾斜杠，再插
              // " / " 分隔符会变成 "/ / etc" / "D:/ / foo"。所以根段后面
              // 不插分隔符，其它段之间才插。
              const prev = i() > 0 ? breadcrumbs()[i() - 1] : null;
              const skipSep = !!prev && (prev.name === "/" || /^[A-Za-z]:\/$/.test(prev.name));
              return (
                <>
                  {i() > 0 && !skipSep ? <span class="text-neutral-500"> / </span> : null}
                  <button class="hover:underline" onClick={() => setCwd(b.path)}>
                    {b.name}
                  </button>
                </>
              );
            }}
          </For>
        </div>
      </div>

      <Show when={error()}>
        <div class="border-b border-red-500/40 bg-red-500/10 px-4 py-2 text-sm text-red-300">
          {error()}
        </div>
      </Show>

      <div class="flex-1 min-h-0 overflow-auto">
        <table class="w-full text-sm">
          <tbody>
            <For each={entries()}>
              {(e) => (
                <tr
                  class="cursor-pointer border-b border-[var(--color-border)]/40 hover:bg-white/5"
                  onClick={() => enter(e)}
                >
                  <td class="px-3 sm:px-4 py-2 sm:py-1.5 w-8 text-center">
                    {iconFor(e)}
                  </td>
                  <td class="px-2 py-2 sm:py-1.5 truncate">{e.name}</td>
                  <td class="px-2 py-2 sm:py-1.5 text-right text-neutral-400 hidden sm:table-cell w-32">
                    {e.type === "dir" ? "—" : formatSize(e.size)}
                  </td>
                  <td class="px-2 py-2 sm:py-1.5 text-right text-neutral-500 hidden md:table-cell w-44">
                    {new Date(e.mod_time).toLocaleString()}
                  </td>
                </tr>
              )}
            </For>
          </tbody>
        </table>
        <Show when={!loading() && entries().length === 0}>
          <div class="p-8 text-center text-neutral-500">空目录</div>
        </Show>
      </div>

      <Show when={termOpen()}>
        <div class="border-t border-[var(--color-border)] flex flex-col min-h-0" style={{ height: `${termHeight()}px` }}>
          <div
            class="h-1 cursor-row-resize bg-[var(--color-border)]/30 hover:bg-[var(--color-accent)]/40"
            onMouseDown={(e) => startResize(e, termHeight, setTermHeight)}
          />
          <div class="flex-1 min-h-0">
            <Show when={termKey()} keyed>
              <TerminalView
                clientId={clientId()}
                shell={termShell()}
                cwd={cwd()}
                onClose={() => setTermOpen(false)}
              />
            </Show>
          </div>
        </div>
      </Show>

      <Show when={edit()}>
        {(cur) => (
          <div class="absolute inset-0 bg-black/60 flex items-center justify-center z-10 p-2" onClick={() => !dirty() && setEdit(null)}>
            <div
              class="w-full h-full sm:w-[90vw] sm:h-[85vh] rounded-lg border border-[var(--color-border)] bg-[var(--color-panel)] flex flex-col"
              onClick={(e) => e.stopPropagation()}
            >
              <div class="flex items-center gap-3 px-4 py-2 border-b border-[var(--color-border)] text-sm">
                <span class="truncate flex-1">{cur().path}</span>
                <Show when={dirty()}>
                  <span class="text-amber-400">未保存</span>
                </Show>
                <button
                  class="rounded bg-[var(--color-accent)] text-black px-3 py-1 disabled:opacity-50"
                  disabled={cur().binary || saving() || !dirty()}
                  onClick={save}
                >
                  {saving() ? "保存中…" : "保存 (Ctrl+S)"}
                </button>
                <button
                  class="rounded border border-[var(--color-border)] px-3 py-1"
                  onClick={() => {
                    if (dirty() && !window.confirm("有未保存更改，确定关闭？")) return;
                    setEdit(null);
                  }}
                >
                  关闭
                </button>
              </div>
              <Show
                when={!cur().binary}
                fallback={<div class="p-4 text-neutral-400">二进制文件，不支持编辑。</div>}
              >
                <div class="flex-1">
                  <Editor
                    path={cur().path}
                    value={cur().content}
                    onChange={(v) => {
                      setEdit({ ...cur(), content: v });
                      setDirty(v !== cur().original);
                    }}
                    onSave={save}
                  />
                </div>
              </Show>
            </div>
          </div>
        )}
      </Show>
    </div>
  );
}

function sortEntries(entries: FSEntry[]): FSEntry[] {
  const out = [...entries];
  out.sort((a, b) => {
    if (a.type === "dir" && b.type !== "dir") return -1;
    if (a.type !== "dir" && b.type === "dir") return 1;
    return a.name.localeCompare(b.name);
  });
  return out;
}

function iconFor(e: FSEntry): string {
  switch (e.type) {
    case "dir":
      return "📁";
    case "symlink":
      return "↪";
    default:
      return "📄";
  }
}

function formatSize(n: number): string {
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KiB`;
  if (n < 1024 * 1024 * 1024) return `${(n / 1024 / 1024).toFixed(1)} MiB`;
  return `${(n / 1024 / 1024 / 1024).toFixed(1)} GiB`;
}

// joinPath / parentOf：客户端路径在协议里是 opaque 字符串，Windows 上是
// 反斜杠。这里仅做最少的字符串拼接（分隔符容忍），由客户端 filepath.Abs 兜底。
function joinPath(base: string, name: string): string {
  if (!base) return name;
  if (base.endsWith("/") || base.endsWith("\\")) return base + name;
  return base + "/" + name;
}

// parentOf 返回上一级目录。到根（POSIX / 或 Windows D:/）就停。
// 旧实现把 D:/ 截成 D:，filepath.Abs("D:") 在 Windows 上解释成"当前 D 盘工作目录"，
// 不是盘符根，所以会列错目录。
function parentOf(path: string): string {
  // POSIX 根 / Windows 盘符根（D:/ 或 D:）— 已到顶。
  if (path === "/" || /^[A-Za-z]:\/?$/.test(path)) return path;

  // 去掉末尾分隔符（避免 D:/foo/ 这种尾斜杠干扰）。
  const p = path.replace(/[\\/]+$/, "");
  const idx = Math.max(p.lastIndexOf("/"), p.lastIndexOf("\\"));
  // "/tmp" 这种顶层目录：idx=0（开头的 /）。父目录是 "/"，不是 path 本身。
  // 旧实现 `idx <= 0 return path` 让用户从 /tmp 永远回不到 /。
  if (idx < 0) return path;
  if (idx === 0) return p[0] === "/" ? "/" : path;

  let parent = p.slice(0, idx);
  if (parent === "") parent = "/";            // POSIX 根边界
  if (/^[A-Za-z]:$/.test(parent)) parent += "/"; // D: → D:/
  return parent;
}

// startResize：垂直拖拽调整底栏高度。绑定到 window 上的 mousemove/up。
function startResize(
  e: MouseEvent,
  get: () => number,
  set: (v: number) => void,
) {
  e.preventDefault();
  const startY = e.clientY;
  const startH = get();
  const onMove = (ev: MouseEvent) => {
    const delta = startY - ev.clientY;
    const next = Math.min(Math.max(startH + delta, 80), window.innerHeight - 200);
    set(next);
  };
  const onUp = () => {
    window.removeEventListener("mousemove", onMove);
    window.removeEventListener("mouseup", onUp);
  };
  window.addEventListener("mousemove", onMove);
  window.addEventListener("mouseup", onUp);
}
