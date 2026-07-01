import { Fragment, type ReactNode } from "react";

// Markdown renders a small, safe markdown subset for dashboard text panels:
// #–#### headings, -/* bullet lists, 1. numbered lists, ``` code fences,
// paragraphs, and inline **bold** / *italic* / `code` / [link](https://…).
// It builds React elements directly (never dangerouslySetInnerHTML), so
// arbitrary HTML in the text renders as text — safe by construction.

// inline parses the inline spans of one line.
function inline(text: string, key = 0): ReactNode {
  // Tokenize left-to-right on the first match of any inline pattern.
  const re = /(\*\*([^*]+)\*\*)|(\*([^*]+)\*)|(`([^`]+)`)|(\[([^\]]+)\]\((https?:\/\/[^\s)]+)\))/;
  const parts: ReactNode[] = [];
  let rest = text;
  let k = key;
  for (;;) {
    const m = re.exec(rest);
    if (!m) {
      if (rest) parts.push(rest);
      break;
    }
    if (m.index > 0) parts.push(rest.slice(0, m.index));
    if (m[2] != null) parts.push(<b key={k++}>{m[2]}</b>);
    else if (m[4] != null) parts.push(<i key={k++}>{m[4]}</i>);
    else if (m[6] != null) parts.push(<code key={k++}>{m[6]}</code>);
    else if (m[8] != null)
      parts.push(
        <a key={k++} href={m[9]} target="_blank" rel="noreferrer noopener">
          {m[8]}
        </a>,
      );
    rest = rest.slice(m.index + m[0].length);
  }
  return <>{parts}</>;
}

export function Markdown({ text }: { text: string }) {
  const lines = text.split(/\r?\n/);
  const blocks: ReactNode[] = [];
  let i = 0;
  let key = 0;

  while (i < lines.length) {
    const line = lines[i];

    if (line.trim() === "") {
      i++;
      continue;
    }

    // ``` code fence — verbatim until the closing fence.
    if (line.startsWith("```")) {
      const code: string[] = [];
      i++;
      while (i < lines.length && !lines[i].startsWith("```")) code.push(lines[i++]);
      i++; // closing fence
      blocks.push(
        <pre key={key++} className="md-code">
          {code.join("\n")}
        </pre>,
      );
      continue;
    }

    // Headings.
    const h = /^(#{1,4})\s+(.*)$/.exec(line);
    if (h) {
      const level = h[1].length;
      const content = inline(h[2]);
      blocks.push(
        level === 1 ? <h1 key={key++}>{content}</h1>
        : level === 2 ? <h2 key={key++}>{content}</h2>
        : level === 3 ? <h3 key={key++}>{content}</h3>
        : <h4 key={key++}>{content}</h4>,
      );
      i++;
      continue;
    }

    // Lists (consecutive -/* or N. lines).
    const bullet = /^\s*[-*]\s+/;
    const ordered = /^\s*\d+\.\s+/;
    if (bullet.test(line) || ordered.test(line)) {
      const isOrdered = ordered.test(line);
      const marker = isOrdered ? ordered : bullet;
      const items: ReactNode[] = [];
      while (i < lines.length && marker.test(lines[i])) {
        items.push(<li key={key++}>{inline(lines[i].replace(marker, ""))}</li>);
        i++;
      }
      blocks.push(isOrdered ? <ol key={key++}>{items}</ol> : <ul key={key++}>{items}</ul>);
      continue;
    }

    // Paragraph: consecutive plain lines joined with spaces.
    const para: string[] = [];
    while (
      i < lines.length &&
      lines[i].trim() !== "" &&
      !/^(#{1,4})\s/.test(lines[i]) &&
      !lines[i].startsWith("```") &&
      !bullet.test(lines[i]) &&
      !ordered.test(lines[i])
    ) {
      para.push(lines[i]);
      i++;
    }
    blocks.push(<p key={key++}>{inline(para.join(" "))}</p>);
  }

  return <div className="markdown">{blocks.map((b, j) => <Fragment key={j}>{b}</Fragment>)}</div>;
}
