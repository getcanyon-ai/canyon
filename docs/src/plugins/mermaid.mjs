// Renders each ```mermaid block of a page to an inline SVG at build time, so a page loads no diagram
// script and GitHub still draws the same block from the .md source. Colours are Canyon's own tokens
// (tokens.css), so a diagram follows the light and dark theme. A block's title="…" is its caption.
import { renderMermaidSVG } from "beautiful-mermaid";

// The four colours a diagram may use beside its neutral boxes: each means one thing everywhere.
const classes = `
classDef canyon fill:var(--ds-color-accent-dim),stroke:var(--ds-color-accent),color:var(--ds-color-text)
classDef blocked fill:var(--ds-color-critical-dim),stroke:var(--ds-color-critical),color:var(--ds-color-text)
classDef person fill:var(--ds-color-amber-dim),stroke:var(--ds-color-amber),color:var(--ds-color-text)
classDef passed fill:var(--ds-color-ok-dim),stroke:var(--ds-color-ok),color:var(--ds-color-text)`;

const theme = {
  bg: "var(--ds-color-bg)",
  fg: "var(--ds-color-text)",
  line: "var(--ds-color-text-faint)",
  accent: "var(--ds-color-text-dim)",
  muted: "var(--ds-color-text-dim)",
  surface: "var(--ds-color-surface)",
  border: "var(--ds-color-border-strong)",
  font: "IBM Plex Sans", // the page's own face (tokens.css); the renderer would otherwise fetch Inter
  transparent: true,
};

// The renderer always @imports its font from Google; the page already has it, so no reader calls Google.
const noFontImports = (svg) => svg.replace(/@import url\('https:\/\/fonts\.googleapis\.com\/[^']*'\);/g, "");

const escape = (s) => s.replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" })[c]);

export const mermaid = {
  name: "canyon-mermaid",
  code(node, ctx) {
    if (node.lang !== "mermaid") return;
    const title = /title="([^"]*)"/.exec(node.meta ?? "")?.[1];
    if (!title) throw new Error(`a mermaid block needs title="…", its caption and its text for screen readers:\n${node.value}`);
    const svg = noFontImports(renderMermaidSVG(node.value + classes, theme)).replace("<svg ", `<svg role="img" aria-label="${escape(title)}" `);
    ctx.replaceNode(node, { type: "html", value: `<figure class="diagram">${svg}<figcaption>${escape(title)}</figcaption></figure>` });
  },
};
