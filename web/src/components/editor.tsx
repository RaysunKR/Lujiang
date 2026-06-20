import { createSignal, onCleanup, onMount, Show } from "solid-js";
import loader from "@monaco-editor/loader";
import type { editor as MonacoEditorNS } from "monaco-editor";

// P3 直接用 loader 从 jsdelivr CDN 加载 Monaco，免去 worker 配置。
// P7 会切换到 bundled workers（离线可用）。
loader.config({ paths: { vs: "https://cdn.jsdelivr.net/npm/monaco-editor@0.52.2/min/vs" } });

export type EditorProps = {
  path: string;
  value: string;
  language?: string;
  onChange?: (value: string) => void;
  onSave?: () => void;
};

// 浏览器会抢占这些 Ctrl/Cmd 组合（保存网页、查找、新标签…）。
// 当 Monaco 编辑器获得焦点时主动 preventDefault，确保编辑器拿到事件。
// 不在 focus 时放回默认行为，避免误伤页面其它 input。
const BROWSER_INTERCEPTED = new Set([
  "ctrl+s", "cmd+s",
  "ctrl+f", "cmd+f",
  "ctrl+h", "cmd+h",
  "ctrl+o", "cmd+o",
  "ctrl+p", "cmd+p",
  "ctrl+g", "cmd+g",
  "ctrl+k", "cmd+k",
  "ctrl+d", "cmd+d",
]);

function keyCombo(e: KeyboardEvent): string {
  const parts: string[] = [];
  if (e.ctrlKey) parts.push("ctrl");
  if (e.metaKey) parts.push("cmd");
  if (e.altKey) parts.push("alt");
  if (e.shiftKey) parts.push("shift");
  parts.push(e.key.toLowerCase());
  return parts.join("+");
}

// isMobile / isLikelyMonacoBroken：移动端浏览器（iOS Safari、Android Chrome）
// 在加载 jsdelivr 上的 Monaco + Web Worker 时常常卡死或白屏。原因是 Monaco
// 用 blob:worker 启动 TS worker，移动端对 worker URL 的 CSP / 跨域 / 内存
// 限制比桌面严得多；CDN 不可达时 loader.init() 会无限期挂起。
// 直接走 textarea fallback，保留 Ctrl+S 与高亮基本排版，避免用户看到空白模态。
function isMobile(): boolean {
  if (typeof navigator === "undefined") return false;
  return /Android|iPhone|iPad|iPod|Mobile|Windows Phone/i.test(navigator.userAgent)
    || (navigator.maxTouchPoints > 1 && window.innerWidth < 900);
}

export default function Editor(props: EditorProps) {
  const [failed, setFailed] = createSignal(false);
  // 移动端直接走 fallback，省得 loader.init() 挂起后用户看着空白等超时。
  const preferFallback = isMobile();

  let container!: HTMLDivElement;
  let editor: MonacoEditorNS.IStandaloneCodeEditor | null = null;

  onMount(async () => {
    if (preferFallback) {
      setFailed(true);
      return;
    }
    // loader.init() 在 CDN 不可达 / worker 启动失败时会无限挂起。
    // 给它 8s，超时就 fallback，至少让用户能编辑。
    const timeout = new Promise<never>((_, reject) =>
      setTimeout(() => reject(new Error("monaco load timeout")), 8000),
    );
    try {
      const monaco = await Promise.race([loader.init(), timeout]);
      editor = monaco.editor.create(container, {
        value: props.value,
        language: props.language ?? guessLang(props.path),
        automaticLayout: true,
        fontSize: 13,
        minimap: { enabled: false },
        scrollBeyondLastLine: false,
        tabSize: 2,
        theme: "vs-dark",
        // VSCode 默认的行为：自动闭合括号、自动缩进、sticky scroll 等。
        autoClosingBrackets: "always",
        autoClosingQuotes: "always",
        autoIndent: "full",
        formatOnPaste: true,
        formatOnType: true,
        stickyScroll: { enabled: true },
        multiCursorModifier: "altKeyCmdCtrlKey",
        linkedEditing: true,
        matchBrackets: "always",
        renderWhitespace: "selection",
        wordBasedSuggestions: "currentDocument",
      });

      editor.onDidChangeModelContent(() => {
        props.onChange?.(editor!.getValue());
      });

      // addAction 在 Monaco 0.50+ 才会真正进 keybinding resolver。
      // editor.addCommand 在新版 monaco-editor 上是 no-op（resolver 里查不到
      // 对应 dispatch part），Ctrl+S / Ctrl+P 全部不触发。实测过：addCommand
      // 注册后 _standaloneKeybindingService._getResolver()._map 里没有 ctrl+s；
      // 换成 addAction 后立刻出现。
      //
      // KeyMod / KeyCode 必须从 loader.init() 返回的 monaco 命名空间拿，
      // 不能从 "monaco-editor" import —— monaco-editor 的 ESM 入口里这俩只是
      // type，被 verbatimModuleSyntax/isolatedModules 当 type-only import 抠掉，
      // 运行时是 undefined（旧版本报 ReferenceError: KeyMod is not defined）。
      const { KeyMod, KeyCode } = monaco;
      editor.addAction({
        id: "lujiang.save",
        label: "Save",
        keybindings: [KeyMod.CtrlCmd | KeyCode.KeyS],
        run: () => props.onSave?.(),
      });
      editor.addAction({
        id: "lujiang.quickCommand",
        label: "Command Palette",
        keybindings: [
          KeyMod.CtrlCmd | KeyCode.KeyP,
          KeyMod.CtrlCmd | KeyMod.Shift | KeyCode.KeyP,
        ],
        run: () => editor!.getAction("editor.action.quickCommand")?.run(),
      });

      // Monaco 编辑器获得焦点时拦浏览器原生快捷键（Ctrl+S 保存网页、Ctrl+P 打印等）。
      // 只调 preventDefault —— 千万不要 stopPropagation：capture 阶段在 window 上
      // 调 stopPropagation 会让事件不再传播到 Monaco 的 DOM，Monaco 自己注册的
      // Ctrl+S（addAction）就永远不会触发，save 调用丢失。
      // 旧版本就是踩了这个坑。
      const onKeyDown = (e: KeyboardEvent) => {
        if (!editor) return;
        const dom = editor.getDomNode();
        if (!dom || !dom.contains(document.activeElement)) return;
        if (BROWSER_INTERCEPTED.has(keyCombo(e))) {
          e.preventDefault();
        }
      };
      window.addEventListener("keydown", onKeyDown, true);
      onCleanup(() => window.removeEventListener("keydown", onKeyDown, true));
    } catch (e) {
      console.warn("Monaco load failed, falling back to textarea:", e);
      setFailed(true);
    }
  });

  onCleanup(() => {
    editor?.dispose();
  });

  return (
    <Show
      when={!failed()}
      fallback={<TextareaEditor {...props} />}
    >
      <div ref={container} class="h-full w-full" />
    </Show>
  );
}

// TextareaEditor：Monaco 不可用时的兜底编辑器。保留 Ctrl+S、Tab 缩进、
// 等宽字体、行号（用 CSS counter 简单做）。够看够改够保存，不指望它有 IDE 体验。
function TextareaEditor(props: EditorProps) {
  let ta!: HTMLTextAreaElement;
  const onKeyDown = (e: KeyboardEvent) => {
    // Ctrl/Cmd+S：保存。textarea 不会拦浏览器原生 Ctrl+S，得自己 preventDefault。
    const combo = keyCombo(e);
    if (combo === "ctrl+s" || combo === "cmd+s") {
      e.preventDefault();
      props.onSave?.();
      return;
    }
    // Tab 插入两个空格（不让焦点跳走）。
    if (e.key === "Tab") {
      e.preventDefault();
      const start = ta.selectionStart;
      const end = ta.selectionEnd;
      const next = ta.value.slice(0, start) + "  " + ta.value.slice(end);
      ta.value = next;
      ta.selectionStart = ta.selectionEnd = start + 2;
      props.onChange?.(next);
    }
  };
  return (
    <textarea
      ref={ta}
      class="h-full w-full bg-[#1e1e1e] text-neutral-100 font-mono text-[13px] leading-[1.5] p-3 border-0 resize-none outline-none"
      spellcheck={false}
      autocomplete="off"
      autocapitalize="off"
      autocorrect="off"
      value={props.value}
      onInput={(e) => props.onChange?.(e.currentTarget.value)}
      onKeyDown={onKeyDown}
    />
  );
}

function guessLang(path: string): string {
  const ext = path.split(".").pop()?.toLowerCase();
  switch (ext) {
    case "ts":
    case "tsx":
      return "typescript";
    case "js":
    case "jsx":
    case "mjs":
    case "cjs":
      return "javascript";
    case "go":
      return "go";
    case "py":
      return "python";
    case "rs":
      return "rust";
    case "json":
      return "json";
    case "yaml":
    case "yml":
      return "yaml";
    case "md":
      return "markdown";
    case "sh":
    case "bash":
      return "shell";
    case "ps1":
      return "powershell";
    case "html":
      return "html";
    case "css":
      return "css";
    default:
      return "plaintext";
  }
}
