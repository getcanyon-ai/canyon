// Renders each ```diagram block of a page to HTML at build time: cards, arrows and bands drawn by
// src/styles/diagrams.css, so the words are the page's own text, in its type and at its size, read in
// order by a screen reader, and a wide layout stacks on a phone instead of shrinking. The block is
// YAML (README.md, "Diagrams"); its info line names the kind and the caption:
//   ```diagram kind=flow title="…"
// A key the kind doesn't know, a missing one, or text holding a link stops the build: a diagram says
// exactly what it was written to say, or the site isn't built.
import { micromark, parse, postprocess, preprocess } from "micromark";
import { parse as parseYaml } from "yaml";

const TONES = ["canyon", "person", "blocked", "passed"];

// Small line icons, drawn in the card's tone; each is decoration beside words that say the same.
const ICONS = {
  agent: '<rect x="2" y="3" width="12" height="9" rx="1.5"/><path d="M5 7l1.5 1.2L5 9.4M8 9.5h3"/>',
  canyon: '<path d="M8 1.8l5.2 2v4c0 3.2-2.2 5.4-5.2 6.4-3-1-5.2-3.2-5.2-6.4v-4z"/>',
  person: '<circle cx="8" cy="5.2" r="2.6"/><path d="M2.8 14c.6-2.8 2.6-4.3 5.2-4.3s4.6 1.5 5.2 4.3"/>',
  people: '<circle cx="6" cy="5.4" r="2.3"/><path d="M1.6 13.6c.5-2.5 2.2-3.8 4.4-3.8s3.9 1.3 4.4 3.8"/><path d="M10.4 3.4a2.3 2.3 0 0 1 0 4.2M12 9.9c1.3.5 2.1 1.7 2.4 3.7"/>',
  passed: '<path d="M3 8.5l3.2 3L13 4.5"/>',
  blocked: '<circle cx="8" cy="8" r="6.2"/><path d="M3.6 12.4l8.8-8.8"/>',
  laptop: '<rect x="2.5" y="3" width="11" height="7.5" rx="1"/><path d="M1 13h14"/>',
  cloud: '<path d="M4.5 12.5h7.2a3 3 0 0 0 .3-6 4.2 4.2 0 0 0-8-.6 3.3 3.3 0 0 0 .5 6.6z"/>',
  server: '<rect x="2.5" y="2.5" width="11" height="4.5" rx="1"/><rect x="2.5" y="9" width="11" height="4.5" rx="1"/><path d="M5 4.75h.01M5 11.25h.01"/>',
  key: '<circle cx="5.5" cy="10.5" r="3"/><path d="M7.6 8.4L13.5 2.5M11.5 4.5l1.8 1.8M9.8 6.2l1.3 1.3"/>',
  loop: '<path d="M13 8a5 5 0 1 1-1.6-3.7"/><path d="M13 2.5v2.8h-2.8"/>',
};
const icon = (name) => {
  if (!(name in ICONS)) throw new Error(`diagram: no icon "${name}": one of ${Object.keys(ICONS).join(", ")}`);
  return `<svg class="cd-icon" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">${ICONS[name]}</svg>`;
};
const DOWN = '<svg class="cd-down" viewBox="0 0 12 20" aria-hidden="true"><path d="M6 0v17M1.5 12.5L6 17.5l4.5-5" fill="none" stroke="currentColor" stroke-width="1.6"/></svg>';
const ACROSS = '<svg class="cd-across" viewBox="0 0 20 12" aria-hidden="true"><path d="M0 6h17M12.5 1.5L17.5 6l-5 4.5" fill="none" stroke="currentColor" stroke-width="1.6"/></svg>';

const escape = (s) => s.replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" })[c]);

// One line of Markdown (code, emphasis) as inline HTML. A link would skip the base the site is served
// under and the link checker, so text with one is refused; a card links with its own link: key.
function inline(text, where) {
  if (typeof text !== "string" || text.trim() === "") throw new Error(`diagram: ${where} must be text`);
  const events = postprocess(parse().document().write(preprocess()(text, undefined, true)));
  const refused = ["link", "image", "autolink", "definition", "htmlText", "htmlFlow", "atxHeading", "setextHeading", "listOrdered", "listUnordered", "blockQuote", "codeFenced", "codeIndented", "thematicBreak"];
  const found = events.filter(([kind, token]) => kind === "enter" && refused.includes(token.type)).map(([, token]) => token.type);
  if (found.length) throw new Error(`diagram: ${where} holds ${found.join(", ")}: one line of text, code or emphasis; a card links with link:`);
  if (events.filter(([kind, token]) => kind === "enter" && token.type === "paragraph").length !== 1) throw new Error(`diagram: ${where} must be one paragraph`);
  const html = micromark(text).trimEnd();
  if (!html.startsWith("<p>") || !html.endsWith("</p>")) throw new Error(`diagram: ${where} must be one paragraph`);
  return html.slice("<p>".length, -"</p>".length);
}

function keys(object, allowed, where) {
  if (object === null || typeof object !== "object" || Array.isArray(object)) throw new Error(`diagram: ${where} must be a map`);
  for (const key of Object.keys(object)) if (!allowed.includes(key)) throw new Error(`diagram: ${where} has "${key}", which isn't one of ${allowed.join(", ")}`);
}
const list = (value, where) => {
  if (!Array.isArray(value) || value.length === 0) throw new Error(`diagram: ${where} must be a list with something in it`);
  return value;
};
const tone = (value, where) => {
  if (value === undefined) return "";
  if (!TONES.includes(value)) throw new Error(`diagram: ${where} tone "${value}" isn't one of ${TONES.join(", ")}`);
  return ` ${value}`;
};

// A card: who (a small label), what (the line that matters), more (a sentence under it), and an
// optional step number, tone, icon and link to another page of the site.
function card(spec, where, base, tag = "li") {
  if (typeof spec === "string") spec = { what: spec };
  keys(spec, ["who", "what", "more", "tone", "icon", "step", "link"], where);
  if (spec.step !== undefined && !Number.isInteger(spec.step)) throw new Error(`diagram: ${where} step must be a number`);
  if (spec.link !== undefined && !/^\/[a-z0-9-]+(\/[a-z0-9.-]+)*\/(#[a-z0-9-]+)?$/.test(spec.link)) {
    throw new Error(`diagram: ${where} link "${spec.link}" must be a page of this site, such as /self-host/docker/`);
  }
  const what = inline(spec.what, `${where} what`);
  const step = spec.step === undefined ? "" : `<span class="cd-step" aria-hidden="true">${spec.step}</span>`;
  const who = spec.who === undefined ? "" : `<span class="cd-who">${spec.icon ? icon(spec.icon) : ""}${inline(spec.who, `${where} who`)}</span>`;
  const title = spec.link === undefined ? what : `<a href="${escape((base ?? "") + spec.link)}">${what}</a>`;
  const more = spec.more === undefined ? "" : `<span class="cd-more">${inline(spec.more, `${where} more`)}</span>`;
  return `<${tag} class="cd-card${tone(spec.tone, where)}">${step}${who}<span class="cd-what">${title}</span>${more}</${tag}>`;
}

// A column of cards, an arrow between each two, labelled where a {via: …} sits between them.
function chain(items, where, base) {
  let html = "";
  let after = false;
  items.forEach((item, i) => {
    if (item && typeof item === "object" && "via" in item) {
      keys(item, ["via"], `${where}[${i}]`);
      if (!after) throw new Error(`diagram: ${where}[${i}] via comes after a card, never first`);
      html += `<li class="cd-link">${DOWN}<span>${inline(item.via, `${where}[${i}] via`)}</span></li>`;
      after = false;
      return;
    }
    if (after) html += `<li class="cd-link" aria-hidden="true">${DOWN}</li>`;
    html += card(item, `${where}[${i}]`, base);
    after = true;
  });
  return html;
}

function flow(spec, base) {
  keys(spec, ["steps"], "flow");
  let html = "";
  let after = false;
  list(spec.steps, "flow steps").forEach((item, i) => {
    const where = `flow steps[${i}]`;
    if (item && typeof item === "object" && "paths" in item) {
      keys(item, ["paths"], where);
      if (after) html += `<li class="cd-link" aria-hidden="true">${DOWN}</li>`;
      const paths = list(item.paths, `${where} paths`).map((path, j) => {
        const at = `${where} paths[${j}]`;
        keys(path, ["label", "tone", "steps", "loop"], at);
        const t = tone(path.tone, at);
        const mark = path.tone === "blocked" ? icon("blocked") : path.tone === "passed" ? icon("passed") : "";
        const loop = path.loop === undefined ? "" : `<p class="cd-loop">${icon("loop")}${inline(path.loop, `${at} loop`)}</p>`;
        return `<li class="cd-path${t}"><p class="cd-label">${mark}${inline(path.label, `${at} label`)}</p><ol>${chain(list(path.steps, `${at} steps`), `${at} steps`, base)}</ol>${loop}</li>`;
      });
      html += `<li><ol class="cd-paths">${paths.join("")}</ol></li>`;
      after = true;
      return;
    }
    if (item && typeof item === "object" && "via" in item) {
      keys(item, ["via"], where);
      html += `<li class="cd-link">${DOWN}<span>${inline(item.via, `${where} via`)}</span></li>`;
      after = false;
      return;
    }
    if (after) html += `<li class="cd-link" aria-hidden="true">${DOWN}</li>`;
    html += card(item, where, base);
    after = true;
  });
  return `<ol class="cd-flow">${html}</ol>`;
}

function compare(spec, base) {
  keys(spec, ["columns", "note"], "compare");
  const columns = list(spec.columns, "compare columns").map((column, i) => {
    const where = `compare columns[${i}]`;
    keys(column, ["label", "icon", "sub", "steps"], where);
    const sub = column.sub === undefined ? "" : `<p class="cd-sub">${inline(column.sub, `${where} sub`)}</p>`;
    return `<div class="cd-column"><p class="cd-heading">${column.icon ? icon(column.icon) : ""}${inline(column.label, `${where} label`)}</p>${sub}<ol>${chain(list(column.steps, `${where} steps`), `${where} steps`, base)}</ol></div>`;
  });
  const note = spec.note === undefined ? "" : `<p class="cd-note">${inline(spec.note, "compare note")}</p>`;
  return `<div class="cd-columns">${columns.join("")}</div>${note}`;
}

function layers(spec, base) {
  keys(spec, ["layers"], "layers");
  let html = "";
  let after = false;
  list(spec.layers, "layers layers").forEach((layer, i) => {
    const where = `layers[${i}]`;
    if (layer && typeof layer === "object" && "via" in layer) {
      keys(layer, ["via"], where);
      html += `<div class="cd-gap">${DOWN}<span>${inline(layer.via, `${where} via`)}</span></div>`;
      after = false;
      return;
    }
    keys(layer, ["band", "note", "tone", "tiles", "flow", "wall"], where);
    if ((layer.tiles === undefined) === (layer.flow === undefined)) throw new Error(`diagram: ${where} has tiles or a flow, one of the two`);
    if (after) html += `<div class="cd-gap" aria-hidden="true">${DOWN}</div>`;
    const note = layer.note === undefined ? "" : ` <span>${inline(layer.note, `${where} note`)}</span>`;
    const body = layer.tiles !== undefined
      ? `<ul class="cd-tiles">${list(layer.tiles, `${where} tiles`).map((tile, j) => card(tile, `${where} tiles[${j}]`, base)).join("")}</ul>`
      : `<ol class="cd-across-flow">${list(layer.flow, `${where} flow`).map((step, j) => (j ? `<li class="cd-link" aria-hidden="true">${DOWN}${ACROSS}</li>` : "") + card(step, `${where} flow[${j}]`, base)).join("")}</ol>`;
    const wall = layer.wall === undefined ? "" : `<p class="cd-wall">${icon("blocked")}${inline(layer.wall, `${where} wall`)}</p>`;
    html += `<div class="cd-band${tone(layer.tone, where)}"><p class="cd-heading">${inline(layer.band, `${where} band`)}${note}</p>${body}${wall}</div>`;
    after = true;
  });
  return html;
}

function choose(spec, base) {
  keys(spec, ["question", "groups"], "choose");
  const groups = list(spec.groups, "choose groups").map((group, i) => {
    const where = `choose groups[${i}]`;
    keys(group, ["label", "sub", "options"], where);
    const sub = group.sub === undefined ? "" : `<p class="cd-sub">${inline(group.sub, `${where} sub`)}</p>`;
    const options = list(group.options, `${where} options`).map((option, j) => card(option, `${where} options[${j}]`, base)).join("");
    return `<div class="cd-group"><p class="cd-heading">${inline(group.label, `${where} label`)}</p>${sub}<ul class="cd-options">${options}</ul></div>`;
  });
  return `<p class="cd-question">${inline(spec.question, "choose question")}</p><div class="cd-groups">${groups.join("")}</div>`;
}

const KINDS = { flow, compare, layers, choose };

// base: the path the site is served under (CANYON_DOCS_BASE), put in front of a card's link.
export const diagram = (base) => ({
  name: "canyon-diagram",
  code(node, ctx) {
    if (node.lang !== "diagram") return;
    const kind = /(?:^|\s)kind=(\w+)/.exec(node.meta ?? "")?.[1];
    const title = /title="([^"]*)"/.exec(node.meta ?? "")?.[1];
    if (!(kind in KINDS)) throw new Error(`a diagram block needs kind=, one of ${Object.keys(KINDS).join(", ")}:\n${node.value}`);
    if (!title) throw new Error(`a diagram block needs title="…", its caption:\n${node.value}`);
    const body = KINDS[kind](parseYaml(node.value), base);
    ctx.replaceNode(node, { type: "html", value: `<figure class="cd cd-${kind} not-content">${body}<figcaption>${inline(title, "title")}</figcaption></figure>` });
  },
});
