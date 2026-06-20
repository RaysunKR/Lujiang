import { onCleanup, onMount } from "solid-js";
import { Terminal } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";
import "@xterm/xterm/css/xterm.css";

// 终端会话协议（browser ↔ server）：
//   - 连接：/api/pty/{clientID}/ws?shell=...&cwd=...&cols=...&rows=...
//   - 二进制消息 = pty 数据（双向）。
//   - 文本消息 = 控制 JSON：{"type":"resize","cols":N,"rows":N} / {"type":"close"}。

export type TerminalProps = {
  clientId: string;
  shell?: string;
  cwd?: string;
  onClose?: () => void;
};

export default function TerminalView(props: TerminalProps) {
  let host!: HTMLDivElement;
  let ws: WebSocket | null = null;
  let term: Terminal | null = null;
  let fit: FitAddon | null = null;
  let resizeTimer: ReturnType<typeof setTimeout> | null = null;

  const wsURL = () => {
    const proto = location.protocol === "https:" ? "wss:" : "ws:";
    const params = new URLSearchParams();
    if (props.shell) params.set("shell", props.shell);
    if (props.cwd) params.set("cwd", props.cwd);
    // cols/rows 在 open 后用 fit 补上，避免初始握手取错。
    return `${proto}//${location.host}/api/pty/${encodeURIComponent(props.clientId)}/ws?${params}`;
  };

  onMount(() => {
    term = new Terminal({
      fontFamily: "ui-monospace, SFMono-Regular, Consolas, 'Liberation Mono', monospace",
      fontSize: 13,
      cursorBlink: true,
      allowProposedApi: true,
      // 完整 16 色 ANSI 调色板（Nord-ish）+ 8 种亮色。
      // 关键点：xterm.js 在收到 ESC[3<m> / ESC[9<m>（前景色）或 ESC[4<m> /
      // ESC[10<m>（背景色）时会从 theme 的 black/red/.../bright* 字段取色。
      // 旧 theme 只设了 background/foreground/cursor，结果 ls / grep / tmux
      // 等发出来的颜色码全被 xterm 当 "亮色前景" 渲染成 foreground 灰白，
      // 视觉上就是"一切内容都是单一颜色"。
      theme: {
        background: "#0b0d12",
        foreground: "#d8dee9",
        cursor: "#d8dee9",
        cursorAccent: "#0b0d12",
        selectionBackground: "#4c566a80",
        black: "#3b4252",
        red: "#bf616a",
        green: "#a3be8c",
        yellow: "#ebcb8b",
        blue: "#81a1c1",
        magenta: "#b48ead",
        cyan: "#88c0d0",
        white: "#e5e9f0",
        brightBlack: "#4c566a",
        brightRed: "#bf616a",
        brightGreen: "#a3be8c",
        brightYellow: "#ebcb8b",
        brightBlue: "#81a1c1",
        brightMagenta: "#b48ead",
        brightCyan: "#8fbcbb",
        brightWhite: "#eceff4",
      },
    });
    fit = new FitAddon();
    term.loadAddon(fit);
    term.open(host);
    try {
      fit.fit();
    } catch {
      // 容器尺寸未就绪；onResize 时再 fit。
    }

    const initialCols = term.cols ?? 80;
    const initialRows = term.rows ?? 24;
    const url = `${wsURL()}&cols=${initialCols}&rows=${initialRows}`;
    ws = new WebSocket(url);
    ws.binaryType = "arraybuffer";

    ws.onopen = () => {
      term!.writeln("\x1b[32m[lujiang]\x1b[0m connected");
    };
    ws.onmessage = (e) => {
      if (typeof e.data === "string") {
        // 当前没用到 server → browser 的控制消息；忽略。
        return;
      }
      const bytes = new Uint8Array(e.data as ArrayBuffer);
      term!.write(bytes, () => {
        /* 写完，不需要回调 */
      });
    };
    ws.onerror = () => {
      term!.writeln("\x1b[31m[lujiang]\x1b[0m websocket error");
    };
    ws.onclose = (e) => {
      term!.writeln(`\x1b[33m[lujiang]\x1b[0m disconnected (code ${e.code})`);
    };

    term.onData((data) => {
      if (ws && ws.readyState === WebSocket.OPEN) {
        ws.send(new TextEncoder().encode(data));
      }
    });
    term.onResize(({ cols, rows }) => {
      if (!ws || ws.readyState !== WebSocket.OPEN) return;
      // 250ms debounce，避免 Windows ConPTY 在连续 resize 时丢字。
      if (resizeTimer) clearTimeout(resizeTimer);
      resizeTimer = setTimeout(() => {
        ws!.send(JSON.stringify({ type: "resize", cols, rows }));
      }, 250);
    });

    // 容器尺寸变化时重新 fit。
    const onWindowResize = () => {
      try {
        fit!.fit();
      } catch {}
    };
    window.addEventListener("resize", onWindowResize);

    // focus 上点击。
    host.addEventListener("mousedown", () => term!.focus());

    onCleanup(() => {
      window.removeEventListener("resize", onWindowResize);
      if (resizeTimer) clearTimeout(resizeTimer);
      if (ws && ws.readyState === WebSocket.OPEN) {
        ws.send(JSON.stringify({ type: "close" }));
      }
      ws?.close();
      term?.dispose();
    });
  });

  return (
    <div class="relative h-full w-full bg-[#0b0d12]">
      <div ref={host} class="h-full w-full p-2 overflow-hidden" />
      <button
        class="absolute top-2 right-2 rounded border border-[var(--color-border)] bg-[var(--color-panel)] px-2 py-1 text-xs hover:bg-white/10"
        onClick={() => props.onClose?.()}
      >
        关闭终端
      </button>
    </div>
  );
}
