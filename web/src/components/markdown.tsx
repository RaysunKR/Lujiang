import {
  createMemo,
  createSignal,
  onMount,
  createEffect,
  type Component,
} from "solid-js";
import { marked } from "marked";
import { createHighlighterCore, type HighlighterCore } from "shiki/core";
import remend from "remend";
import morphdom from "morphdom";
import DOMPurify from "dompurify";
import bash from "shiki/langs/bash";
import go from "shiki/langs/go";
import json from "shiki/langs/json";
import md from "shiki/langs/markdown";
import python from "shiki/langs/python";
import typescript from "shiki/langs/typescript";
import yaml from "shiki/langs/yaml";
import rust from "shiki/langs/rust";
import shell from "shiki/langs/shell";
import htmlLang from "shiki/langs/html";
import css from "shiki/langs/css";
import sql from "shiki/langs/sql";
import githubDark from "shiki/themes/github-dark";

// Markdown 渲染：marked → remend（tail-block healing）→ DOMPurify → Shiki 高亮。
//
// 设计要点：
//   - marked + remend：流式 markdown 末尾可能不完整（半个代码块/列表），
//     remend 把这种 tail 修复成合法 HTML，避免流式渲染时跳动。
//   - DOMPurify：remend 输出的 HTML 也得过 sanitizer，agent 输出不可信。
//   - Shiki：每渲染完一帧，把 <pre><code class="language-X"> 替换为
//     高亮后的 HTML。highlighter 用 singleton 异步初始化（首帧可能不亮）。
//   - morphdom：增量更新 DOM，避免每次刷新都重建（光标位置/滚动条稳定）。

marked.setOptions({
  gfm: true,
  breaks: false,
});

let highlighterPromise: Promise<HighlighterCore> | null = null;
function getHighlighter() {
  if (!highlighterPromise) {
    highlighterPromise = createHighlighterCore({
      themes: [githubDark],
      langs: [bash, go, json, md, python, typescript, yaml, rust, shell, htmlLang, css, sql],
    }).then((hl) => {
      globalShiki = hl;
      return hl;
    });
  }
  return highlighterPromise;
}

let globalShiki: HighlighterCore | null = null;

type Props = {
  text: string;
};

const Markdown: Component<Props> = (props) => {
  const [ready, setReady] = createSignal(false);
  let containerRef: HTMLDivElement | undefined;

  onMount(async () => {
    try {
      await getHighlighter();
      setReady(true);
    } catch (e) {
      console.warn("shiki init failed", e);
    }
  });

  const rawHtml = createMemo(() => {
    const text = props.text || "";
    try {
      const healed = remend(text);
      const markedHtml = marked.parse(healed, { async: false }) as string;
      return DOMPurify.sanitize(markedHtml, {
        ADD_ATTR: ["target"],
      });
    } catch {
      return escapeHtml(text);
    }
  });

  // ready() 切到 true 后强制重算（让 morphdom 跑一次 shiki 高亮）。
  const html = createMemo(() => rawHtml() + (ready() ? "|shiki:1" : "|shiki:0"));

  createEffect(() => {
    const nextHtml = rawHtml();
    if (!containerRef) return;
    morphdom(containerRef, `<div class="markdown-body text-sm leading-relaxed">${nextHtml}</div>`, {
      onBeforeNodeDiscard: () => true,
    });
    if (globalShiki) highlightCodeBlocks(containerRef);
  });

  return <div ref={containerRef} class="markdown-body text-sm leading-relaxed" />;
};

function highlightCodeBlocks(root: HTMLElement) {
  const blocks = root.querySelectorAll("pre code[class*='language-']");
  blocks.forEach((code) => {
    const el = code as HTMLElement;
    if (el.dataset.lujiangHighlighted === "1" || !globalShiki) return;
    const langMatch = /language-([\w-]+)/.exec(el.className);
    const lang = langMatch?.[1] ?? "text";
    try {
      const html = globalShiki.codeToHtml(el.textContent ?? "", {
        lang: lang === "text" ? "typescript" : lang,
        theme: "github-dark",
      });
      const tmp = document.createElement("div");
      tmp.innerHTML = html;
      const inner = tmp.querySelector("pre code");
      if (inner) {
        el.innerHTML = inner.innerHTML;
        el.dataset.lujiangHighlighted = "1";
      }
    } catch {
      // 不支持的语言 → 留 plaintext。
    }
  });
}

function escapeHtml(s: string): string {
  return s
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;");
}

export default Markdown;
