import type { ReactNode } from "react";
import { Lexer, type Token, type Tokens } from "marked";

// Markdown is a playbook's body as its preview shows it (Guardrails › Playbooks): marked's lexer reads the Markdown,
// a real parser, and its tokens become React elements here, so no text an admin writes is ever HTML on the page —
// raw HTML in the body shows as its own text, an image as its alt text with no fetch, and a link goes only to
// http, https or mailto.
export function Markdown({ text }: { text: string }) {
  return <div className="text-base break-words">{blocks(Lexer.lex(text))}</div>;
}

const SAFE = /^(https?:|mailto:)/i;

function blocks(tokens: Token[]): ReactNode[] {
  return tokens.map((token, i) => block(token, i));
}

function block(token: Token, key: number): ReactNode {
  const t = token as Tokens.Generic;
  switch (t.type) {
    case "heading": {
      const size = ["text-title", "text-lg", "text-base", "text-base", "text-md", "text-md"][(t as Tokens.Heading).depth - 1] ?? "text-md";
      return <p key={key} role="heading" aria-level={(t as Tokens.Heading).depth} className={`mt-3 font-semibold ${size}`}>{inline((t as Tokens.Heading).tokens)}</p>;
    }
    case "paragraph": return <p key={key} className="mt-2">{inline((t as Tokens.Paragraph).tokens)}</p>;
    case "code": return <pre key={key} className="mt-2 overflow-x-auto rounded-lg bg-surface-2 px-3 py-2 font-mono text-sm">{(t as Tokens.Code).text}</pre>;
    case "blockquote": return <blockquote key={key} className="mt-2 border-l-2 border-border pl-3 text-text-dim">{blocks((t as Tokens.Blockquote).tokens)}</blockquote>;
    case "list": {
      const list = t as Tokens.List;
      const items = list.items.map((item, i) => <li key={i}>{item.tokens.flatMap((one, j) => (one.type === "text" ? [inline((one as Tokens.Text).tokens ?? [one], j)] : [block(one, j)]))}</li>);
      return list.ordered
        ? <ol key={key} start={list.start === "" ? undefined : list.start} className="mt-2 list-decimal pl-6">{items}</ol>
        : <ul key={key} className="mt-2 list-disc pl-6">{items}</ul>;
    }
    case "table": {
      const table = t as Tokens.Table;
      return (
        <table key={key} className="mt-2 text-md">
          <thead><tr>{table.header.map((cell, i) => <th key={i} className="pr-3 text-left">{inline(cell.tokens)}</th>)}</tr></thead>
          <tbody>{table.rows.map((row, i) => <tr key={i}>{row.map((cell, j) => <td key={j} className="pr-3">{inline(cell.tokens)}</td>)}</tr>)}</tbody>
        </table>
      );
    }
    case "hr": return <hr key={key} className="mt-3 border-border" />;
    case "space": case "def": return null;
    case "html": return <pre key={key} className="mt-2 font-mono text-sm whitespace-pre-wrap text-text-dim">{t.raw}</pre>;
    default: return t.tokens !== undefined ? <p key={key} className="mt-2">{inline(t.tokens)}</p> : <p key={key} className="mt-2">{t.raw}</p>;
  }
}

function inline(tokens: Token[], at?: number): ReactNode {
  const out = tokens.map((token, i) => {
    const t = token as Tokens.Generic;
    switch (t.type) {
      case "text": return (t as Tokens.Text).tokens !== undefined ? <span key={i}>{inline((t as Tokens.Text).tokens!)}</span> : (t as Tokens.Text).text;
      case "escape": return (t as Tokens.Escape).text;
      case "strong": return <strong key={i}>{inline((t as Tokens.Strong).tokens)}</strong>;
      case "em": return <em key={i}>{inline((t as Tokens.Em).tokens)}</em>;
      case "del": return <del key={i}>{inline((t as Tokens.Del).tokens)}</del>;
      case "codespan": return <code key={i} className="rounded bg-surface-2 px-1 font-mono text-sm">{(t as Tokens.Codespan).text}</code>;
      case "br": return <br key={i} />;
      case "link": {
        const link = t as Tokens.Link;
        return SAFE.test(link.href) ? <a key={i} className="text-link" href={link.href} rel="noreferrer noopener" target="_blank">{inline(link.tokens)}</a> : <span key={i}>{inline(link.tokens)}</span>;
      }
      case "image": return (t as Tokens.Image).text;
      default: return t.raw; // raw HTML among them: its own text, never markup
    }
  });
  return at === undefined ? out : <span key={at}>{out}</span>;
}
