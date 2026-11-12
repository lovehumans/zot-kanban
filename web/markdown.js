// web/markdown.js - the Card's own Markdown reader, and the only Markdown reader this board has.
//
// A Card's body and Comments are Markdown text: they live in the Card's file as the words they were written with,
// the kanban tool reads them as those words, and a browser gets them as those words too.
// The reading happens here, in the browser, so the words that
// travel stay the words a session can search and a human can diff - and this file stays small. A Markdown-to-HTML
// library would be a third-party dependency, and it is also why a Card read by curl or printed would arrive as
// markup it never chose.
//
// The subset is what Cards and Comments on this board actually use, agreed on the Card: paragraphs, headings to
// six levels, flat bullet and numbered lists, fenced code blocks, inline code, **bold**, *italic* and links. What
// is deliberately out: nesting (a list stays flat and an indented inner line stays words), tables, blockquotes,
// images, checkbox rewriting, and underscore emphasis - the last because an identifier like comment_id or
// card_body_view would otherwise be italicised out of every Comment written here. Raw HTML is escaped, so a Card
// holding a script tag reads as that tag. renderMarkdown returns text that is safe to hand to innerHTML.
//
// This file is loaded first: web/board.js calls renderMarkdown when a swap puts a Card's words on the page.
"use strict";

function escapeHTML(text) {
  return String(text).replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]));
}

// A link only reaches an href with a scheme a page may act on, so a Card cannot become a javascript: prompt.
function isSafeHref(href) {
  return /^(https?:|mailto:|\/|#)/i.test(href.trim());
}

const BLANK_RE = /^\s*$/;
const FENCE_RE = /^ {0,3}(```+|~~~+)/;          // the language after it is an information string, not content
const HEADING_RE = /^ {0,3}(#{1,6})\s+(\S.*)$/; // a seventh hash is a paragraph that starts with hashes
const ITEM_RE = /^ {0,3}([-*+]|\d{1,9}[.)])(\s+)(\S.*)$/;
const INDENTED_RE = /^ {2,}\S/;                 // a list item's own continuation, not a nested list
const CODE_RE = /`([^`\n]+)`/;                  // the old inline form, so a fence can still hold a backtick
const EMPHASIS_RE = /(\*\*|(?<!\*)\*(?!\*))(?=\S)(.+?)(?<=\S)\1/gs;
// The URL alternatives stop before trailing punctuation, so "see https://x.dev." links the address and not the full stop.
const URL_RE = String.raw`https?:\/\/[^\s<>"'` + "`" + String.raw`]*[^\s<>"'` + "`" + String.raw`.,;:!?)\]]`;
const TOKEN_RE = /\u0000(\d+)\u0000/g;

// A Card's words, cut into blocks: blank lines end a paragraph, a line starting a fence or a heading or a list
// starts that block, and a fenced block runs to its closing fence - or to the end of the text when none came.
function splitBlocks(text) {
  const lines = String(text).split("\n");
  const blocks = [];
  let i = 0;
  while (i < lines.length) {
    const line = lines[i];
    if (BLANK_RE.test(line)) { i += 1; continue }
    const fence = line.match(FENCE_RE);
    if (fence) {
      const closedBy = new RegExp("^ {0,3}\\" + fence[1][0] + "{3,} *$");
      const body = [];
      i += 1;
      while (i < lines.length && !closedBy.test(lines[i])) body.push(lines[i++]);
      if (i < lines.length) i += 1; // the closing fence, when the block got one
      while (body.length && BLANK_RE.test(body[body.length - 1])) body.pop(); // padding before the closer is not code
      blocks.push({ kind: "fence", lines: body });
      continue;
    }
    const heading = line.match(HEADING_RE);
    if (heading) { blocks.push({ kind: "heading", level: heading[1].length, text: heading[2] }); i += 1; continue }
    const item = line.match(ITEM_RE);
    if (item) {
      // One list block per marker kind: a bullet list and a numbered list written one after the other stay two
      // lists, so the numbers a human typed are not dropped on the floor. The list is flat - an indented line is
      // the item's own continuation, and a nested marker on it stays words rather than a second list.
      const ordered = /\d/.test(item[1]);
      const items = [];
      while (i < lines.length) {
        const opener = lines[i].match(ITEM_RE);
        if (items.length && !BLANK_RE.test(lines[i]) && INDENTED_RE.test(lines[i])) {
          items[items.length - 1] += "\n" + lines[i].trim();
          i += 1;
          continue;
        }
        if (items.length && BLANK_RE.test(lines[i])) {
          // One blank line between items keeps a list whole - the loose list a human meant - while two of them,
          // or a line that is not another item of this kind, end it.
          let ahead = i;
          while (ahead < lines.length && BLANK_RE.test(lines[ahead])) ahead += 1;
          const next = ahead < lines.length ? lines[ahead].match(ITEM_RE) : null;
          if (ahead - i > 1 || !next || /\d/.test(next[1]) !== ordered) break;
          i = ahead;
          continue;
        }
        if (!opener || /\d/.test(opener[1]) !== ordered) break;
        items.push(lines[i].slice(opener[0].length - opener[3].length).trim());
        i += 1;
      }
      blocks.push({ kind: "list", ordered: ordered, items: items });
      continue;
    }
    const para = [];
    while (i < lines.length && !BLANK_RE.test(lines[i]) && !HEADING_RE.test(lines[i])
      && !ITEM_RE.test(lines[i]) && !FENCE_RE.test(lines[i])) {
      para.push(lines[i++].trim()); // outer space goes; space inside the line and the line break stay
    }
    if (para.length) blocks.push({ kind: "para", text: para.join("\n") });
  }
  return blocks;
}

// Inside a block, in the order a Card's own text demands: a code span's words are never markup, so it goes first
// and nothing later may read inside it; then a link, so its brackets and parentheses are never emphasis bait; then
// the bare address a human pasted; then emphasis, whose inner text is inline content of its own.
//
// Finished HTML is set aside behind a token while the rest of the line is still being read, and read back at the
// end of each pass: a token holds only digits, so no later pattern can bite into it, and a nested pass can restore
// what it put there without disturbing the pass that called it.
function inlineMarkdown(text) {
  const stash = [];
  const hold = (html) => { stash.push(html); return "\u0000" + (stash.length - 1) + "\u0000"; };
  const INLINE_RE = new RegExp(
    CODE_RE.source + "|\\[([^\\]]*)\\]\\(([^()\\s]+)\\)|(" + URL_RE + ")", "g");

  const read = (escaped) => {
    // The finished pieces go behind tokens first, so the emphasis pass below reads a Card's ** pairs across them:
    // **bold [link](https://x.dev) text** is one emphasis, not two stray star runs.
    let marked = "", last = 0;
    escaped.replace(INLINE_RE, (m, code, label, href, url, index) => {
      marked += escaped.slice(last, index);
      last = index + m.length;
      if (code !== undefined) marked += hold("<code>" + code + "</code>");
      else if (url !== undefined) marked += hold('<a href="' + url + '">' + url + "</a>");
      else marked += isSafeHref(href) ? hold('<a href="' + href + '">' + read(label) + "</a>") : m;
    });
    marked += escaped.slice(last);
    const emphasised = marked.replace(EMPHASIS_RE, (m, marker, inner) =>
      hold((marker === "**" ? "<strong>" : "<em>") + read(inner) + (marker === "**" ? "</strong>" : "</em>")));
    return emphasised.replace(TOKEN_RE, (token, n) => stash[Number(n)]);
  };

  return read(escapeHTML(text));
}

// The whole Card text, block by block. A fenced block is escaped whole and its language string is dropped; a list
// is flat; a heading carries inline content the way a paragraph does. Blocks are joined with a line break so the
// HTML a Card produces is as readable as the Markdown it came from.
function renderMarkdown(source) {
  const blocks = splitBlocks(source);
  const out = [];
  for (const block of blocks) {
    if (block.kind === "heading") {
      out.push("<h" + block.level + ">" + inlineMarkdown(block.text) + "</h" + block.level + ">");
    } else if (block.kind === "fence") {
      out.push("<pre><code>" + escapeHTML(block.lines.join("\n")) + "</code></pre>");
    } else if (block.kind === "list") {
      const tag = block.ordered ? "ol" : "ul";
      out.push("<" + tag + ">\n" + block.items.map((item) => "<li>" + inlineMarkdown(item) + "</li>").join("\n")
        + "\n</" + tag + ">");
    } else {
      out.push("<p>" + inlineMarkdown(block.text) + "</p>");
    }
  }
  return out.join("\n");
}

/* Where the markdown tests come in. Node has a `module` and no `window`; a browser has the reverse, so this line is
   inert where a board is painted and costs nothing there. It exists so a Node test can require the reader rather than
   read this file as text and evaluate it, which ties the check to a name inside an eval. */
if (typeof module !== "undefined") {
  module.exports = { renderMarkdown };
}
