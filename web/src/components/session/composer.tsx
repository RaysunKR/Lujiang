import { createSignal, Show, For, onMount, createEffect, type Component } from "solid-js";
import { fsList } from "../../api/fs";

// Composer 是底部输入区。Enter 发送，Shift+Enter 换行。
// P5 简化：只支持新对话（无中断态追加 prompt）。

type Props = {
  disabled?: boolean;
  onSubmit: (prompt: string, backend: string, model: string, mode: string) => void;
  // P8 新增：slash 命令补全要用。clientId + cwd 用来从客户端 .claude/commands/
  // 扫项目自定义命令；不传则只用内置命令表。
  clientId?: string;
  cwd?: string;
};

// Claude Code 内置 slash 命令（截取常用）。完整列表随 CLI 版本变，这里
// 只列稳定的、用户最容易按到的。覆盖 90% 场景；剩下 10% 用户自己手输。
// 不在这里加技能（skills）：技能太多、变更频繁、按平台不同；做自动发现
// 收益太低。
const BUILTIN_COMMANDS: { name: string; desc: string }[] = [
  { name: "/clear", desc: "清空当前对话上下文" },
  { name: "/compact", desc: "压缩对话历史以节省 token" },
  { name: "/resume", desc: "恢复历史会话" },
  { name: "/agents", desc: "管理 subagents" },
  { name: "/model", desc: "选择模型" },
  { name: "/cost", desc: "查看 token 使用统计" },
  { name: "/help", desc: "查看帮助" },
  { name: "/init", desc: "初始化项目（生成 CLAUDE.md）" },
  { name: "/review", desc: "代码 review" },
  { name: "/mcp", desc: "管理 MCP 服务器" },
  { name: "/memory", desc: "编辑 CLAUDE.md 记忆" },
  { name: "/permissions", desc: "管理权限规则" },
  { name: "/config", desc: "编辑配置" },
  { name: "/status", desc: "查看 Claude 状态" },
  { name: "/login", desc: "登录账户" },
  { name: "/logout", desc: "退出账户" },
  { name: "/vim", desc: "进入 vim 编辑模式" },
  { name: "/release-notes", desc: "查看 release notes" },
  { name: "/doctor", desc: "诊断安装问题" },
];

type Command = { name: string; desc: string; custom?: boolean };

const Composer: Component<Props> = (props) => {
  const [text, setText] = createSignal("");
  const [backend, setBackend] = createSignal("claude");
  const [model, setModel] = createSignal("");
  const [mode, setMode] = createSignal("acceptEdits");

  // slash 命令补全：输入光标前一个 token 以 / 开头时弹出列表。
  // 上下箭头切换、Enter 选中、Esc 关闭。被 disabled 屏蔽时不弹。
  const [slashOpen, setSlashOpen] = createSignal(false);
  const [slashIndex, setSlashIndex] = createSignal(0);
  const [slashQuery, setSlashQuery] = createSignal("");
  const [customCommands, setCustomCommands] = createSignal<Command[]>([]);
  let textareaRef: HTMLTextAreaElement | undefined;

  const allCommands = (): Command[] => [
    ...BUILTIN_COMMANDS,
    ...customCommands(),
  ];

  const filteredCommands = (): Command[] => {
    const q = slashQuery().toLowerCase();
    if (!q) return allCommands();
    return allCommands().filter((c) => c.name.toLowerCase().includes(q));
  };

  // 扫描客户端 .claude/commands/*.md（任意层级）作为自定义命令。
  // 失败静默 —— 项目没初始化或路径不可达很正常，不应该弹错误。
  // 用户级 ~/.claude/commands/ 这里故意不扫：跨用户机器的命令建议不靠谱。
  async function discoverCustomCommands() {
    if (!props.clientId || !props.cwd) return;
    const targets = [
      joinFsPath(props.cwd, ".claude/commands"),
    ];
    const found: Command[] = [];
    for (const target of targets) {
      try {
        const res = await fsList(props.clientId!, target);
        for (const e of res.entries) {
          if (e.type !== "file") continue;
          const m = e.name.match(/^(.+)\.md$/i);
          if (!m) continue;
          found.push({
            name: "/" + m[1],
            desc: "项目命令",
            custom: true,
          });
        }
      } catch {
        // ignore
      }
    }
    setCustomCommands(found);
  }

  onMount(() => {
    discoverCustomCommands();
  });

  // clientId / cwd 切换（跨 client / 跨目录打开 project）→ 重新扫。
  createEffect(() => {
    const _id = props.clientId;
    const _cwd = props.cwd;
    discoverCustomCommands();
  });

  function submit() {
    const t = text().trim();
    if (!t || props.disabled) return;
    props.onSubmit(t, backend(), model(), mode());
    setText("");
    setSlashOpen(false);
  }

  // 更新光标处的 slash 状态。返回当前光标前一个 token 的起始 offset（供
  // 后续 replace 使用）；-1 表示不在 slash 上下文。
  function updateSlashState(): number {
    const ta = textareaRef;
    if (!ta) return -1;
    const pos = ta.selectionStart;
    const upto = text().slice(0, pos);
    // 从光标往左找最近的空白，切出当前 token。
    const m = upto.match(/(^|\s)(\/[A-Za-z0-9_-]*)$/);
    if (!m) {
      setSlashOpen(false);
      return -1;
    }
    const tokenStart = pos - m[2].length;
    const query = m[2].slice(1); // 去掉前导 /
    setSlashQuery(query);
    setSlashOpen(!props.disabled);
    setSlashIndex(0);
    return tokenStart;
  }

  function insertCommand(cmd: Command) {
    const ta = textareaRef;
    if (!ta) return;
    const start = updateSlashState();
    if (start < 0) return;
    const before = text().slice(0, start);
    const after = text().slice(ta.selectionStart);
    const next = before + cmd.name + " " + after;
    setText(next);
    setSlashOpen(false);
    // 把光标放到命令后面（+ 空格）方便继续输入参数。
    const cursorPos = (before + cmd.name + " ").length;
    queueMicrotask(() => {
      ta.focus();
      ta.selectionStart = ta.selectionEnd = cursorPos;
    });
  }

  function onKeyDown(e: KeyboardEvent) {
    // slash popup 拦截在文本提交前面，避免 Enter 把候选直接送出去。
    if (slashOpen() && filteredCommands().length > 0) {
      if (e.key === "ArrowDown") {
        e.preventDefault();
        const n = filteredCommands().length;
        setSlashIndex((i) => (i + 1) % n);
        return;
      }
      if (e.key === "ArrowUp") {
        e.preventDefault();
        const n = filteredCommands().length;
        setSlashIndex((i) => (i - 1 + n) % n);
        return;
      }
      if (e.key === "Enter" || e.key === "Tab") {
        e.preventDefault();
        const list = filteredCommands();
        if (list.length > 0) {
          insertCommand(list[slashIndex()]);
        }
        return;
      }
      if (e.key === "Escape") {
        e.preventDefault();
        setSlashOpen(false);
        return;
      }
    }
    if (e.key === "Enter" && !e.shiftKey) {
      e.preventDefault();
      submit();
    }
  }

  function onInput(e: InputEvent) {
    const ta = e.currentTarget as HTMLTextAreaElement;
    setText(ta.value);
    updateSlashState();
  }

  // 移动端输入框聚焦时虚拟键盘会遮住输入区；不做处理只依赖 body 滚动
  // 在 iOS 上经常失效。这里在 focus 时把 textarea scrollIntoView 一下，
  // 至少保证 composer 可见。
  function onFocus() {
    if (typeof window !== "undefined" && !window.matchMedia("(min-width: 1024px)").matches) {
      setTimeout(() => textareaRef?.scrollIntoView({ block: "center" }), 200);
    }
  }

  return (
    <div class="px-3 sm:px-6 py-2 sm:py-3 bg-[var(--color-panel)] flex flex-col gap-2 border-t border-[var(--color-border)]">
      {/* 控制栏：桌面一行横排、移动端折成两行（select + select / model + mode）。
       * 旧实现 overflow-x-auto 让移动端横滚选择，体验差。 */}
      <div class="grid grid-cols-2 sm:flex sm:items-center gap-2 sm:gap-3 text-xs text-neutral-400">
        <label class="shrink-0 flex items-center gap-1 min-w-0">
          <span class="truncate">agent</span>
          <select
            class="rounded border border-[var(--color-border)] bg-[var(--color-bg)] px-2 py-1 min-h-[32px] flex-1 min-w-0"
            value={backend()}
            onChange={(e) => setBackend(e.currentTarget.value)}
          >
            <option value="claude">claude</option>
            <option value="codex">codex</option>
            <option value="gemini">gemini</option>
            <option value="cursor">cursor</option>
            <option value="copilot">copilot</option>
            <option value="opencode">opencode</option>
          </select>
        </label>
        <label class="shrink-0 flex items-center gap-1 min-w-0">
          <span class="truncate">model</span>
          <input
            class="w-full sm:w-32 rounded border border-[var(--color-border)] bg-[var(--color-bg)] px-2 py-1 min-h-[32px] min-w-0"
            placeholder="(default)"
            value={model()}
            onInput={(e) => setModel(e.currentTarget.value)}
          />
        </label>
        <label class="col-span-2 sm:shrink-0 sm:flex sm:items-center sm:gap-1 flex items-center gap-1 min-w-0">
          <span class="truncate">mode</span>
          <select
            class="rounded border border-[var(--color-border)] bg-[var(--color-bg)] px-2 py-1 min-h-[32px] flex-1 min-w-0"
            value={mode()}
            onChange={(e) => setMode(e.currentTarget.value)}
          >
            <option value="default">default</option>
            <option value="acceptEdits">acceptEdits</option>
            <option value="plan">plan</option>
            <option value="bypassPermissions">bypass</option>
          </select>
        </label>
      </div>

      {/* textarea + 发送按钮 + slash popup 三件套放在 relative 容器里，
       * popup 绝对定位浮在 textarea 上方。 */}
      <div class="flex gap-2 items-end relative">
        <Show when={slashOpen() && filteredCommands().length > 0}>
          <div class="absolute bottom-full left-0 right-12 sm:right-16 mb-1 bg-[var(--color-bg)] border border-[var(--color-border)] rounded shadow-lg max-h-64 overflow-auto z-10">
            <For each={filteredCommands()}>
              {(cmd, i) => (
                <button
                  class="w-full text-left px-3 py-2 text-sm flex flex-col gap-0.5"
                  classList={{
                    "bg-[var(--color-accent)]/20": i() === slashIndex(),
                    "hover:bg-white/5": i() !== slashIndex(),
                  }}
                  onMouseDown={(e) => {
                    // mousedown 而不是 click：textarea blur 前 fire，避免
                    // 光标丢掉导致 insertCommand 算错位置。
                    e.preventDefault();
                    insertCommand(cmd);
                  }}
                  onMouseEnter={() => setSlashIndex(i())}
                >
                  <div class="flex items-center gap-2">
                    <span class="text-neutral-100 font-mono">{cmd.name}</span>
                    <Show when={cmd.custom}>
                      <span class="text-[10px] text-amber-300 border border-amber-300/40 rounded px-1">自定义</span>
                    </Show>
                  </div>
                  <Show when={cmd.desc}>
                    <div class="text-xs text-neutral-500 truncate">{cmd.desc}</div>
                  </Show>
                </button>
              )}
            </For>
          </div>
        </Show>
        <textarea
          ref={textareaRef}
          class="flex-1 rounded border border-[var(--color-border)] bg-[var(--color-bg)] px-3 py-2 text-sm resize-none min-h-[44px]"
          rows="2"
          placeholder="问 agent 什么？Enter 发送，Shift+Enter 换行。输入 / 触发命令"
          value={text()}
          disabled={props.disabled}
          onInput={onInput}
          onKeyDown={onKeyDown}
          onFocus={onFocus}
          onKeyUp={() => updateSlashState()}
          onClick={() => updateSlashState()}
          onBlur={() => setTimeout(() => setSlashOpen(false), 150)}
        />
        <button
          class="rounded bg-[var(--color-accent)] text-black px-3 sm:px-4 py-2 text-sm font-medium disabled:opacity-50 min-h-[44px] shrink-0"
          disabled={props.disabled || !text().trim()}
          onClick={submit}
        >
          <Show when={!props.disabled} fallback="运行中…">
            发送
          </Show>
        </button>
      </div>
    </div>
  );
};

// joinFsPath：和 client.tsx 里的 joinPath 一样的最简拼接，仅在 composer
// 里用来组 .claude/commands 这种相对路径。客户端 filepath.Abs 兜底。
function joinFsPath(base: string, rel: string): string {
  if (!base) return rel;
  if (base.endsWith("/") || base.endsWith("\\")) return base + rel;
  return base + "/" + rel;
}

export default Composer;
