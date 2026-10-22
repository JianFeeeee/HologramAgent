/**
 * Markdown 解析器（无 UI 依赖）。
 *
 * 从 components/StaticMarkdown.ets 抽出：解析与渲染分家后，
 * 解析规则可以单独被复用/测试，渲染组件也回到可读长度。
 *
 * 覆盖：标题、段落、代码围栏、无序/有序列表、引用块、分隔线、表格，
 * 以及行内的粗体/斜体/行内代码/链接。
 */

// ── Types ──────────────────────────────────────────────────────────────────────

export interface MdBlock {
  type: string; // 'heading' | 'code' | 'list' | 'ol' | 'blockquote' | 'hr' | 'table' | 'para'
  level?: number;
  items?: string[];
  text?: string;
  lang?: string;
  codeLines?: string[];
  headers?: string[];
  rows?: string[][];
}

export interface MdSpan {
  text: string;
  bold?: boolean;
  italic?: boolean;
  code?: boolean;
  link?: boolean;
  linkUrl?: string;
}

// ── Inline parser ──────────────────────────────────────────────────────────────

export function parseInline(text: string): MdSpan[] {
  const spans: MdSpan[] = [];
  let i: number = 0;
  while (i < text.length) {
    // Inline code (backtick)
    if (text[i] === '`') {
      const end: number = text.indexOf('`', i + 1);
      if (end > i) {
        spans.push({ text: text.substring(i + 1, end), code: true });
        i = end + 1;
        continue;
      }
    }
    // Bold: **text**
    if (text[i] === '*' && i + 1 < text.length && text[i + 1] === '*') {
      const end: number = text.indexOf('**', i + 2);
      if (end > i + 1) {
        spans.push({ text: text.substring(i + 2, end), bold: true });
        i = end + 2;
        continue;
      }
    }
    // Italic: *text* (single asterisk)
    if (text[i] === '*' && (i + 1 >= text.length || text[i + 1] !== '*')) {
      const end: number = text.indexOf('*', i + 1);
      if (end > i) {
        spans.push({ text: text.substring(i + 1, end), italic: true });
        i = end + 1;
        continue;
      }
    }
    // Link: [text](url)
    if (text[i] === '[') {
      const cb: number = text.indexOf(']', i + 1);
      if (cb > i && cb + 1 < text.length && text[cb + 1] === '(') {
        const cp: number = text.indexOf(')', cb + 2);
        if (cp > cb + 1) {
          spans.push({ text: text.substring(i + 1, cb), link: true, linkUrl: text.substring(cb + 2, cp) });
          i = cp + 1;
          continue;
        }
      }
    }
    // Plain run
    let j: number = i + 1;
    while (j < text.length && text[j] !== '`' && text[j] !== '*' && text[j] !== '[') {
      j++;
    }
    spans.push({ text: text.substring(i, j) });
    i = j;
  }
  return spans;
}

// ── Block parser helpers ───────────────────────────────────────────────────────

function isHr(line: string): boolean {
  if (line.length < 3) {
    return false;
  }
  const ch: string = line[0];
  if (ch !== '-' && ch !== '*' && ch !== '_') {
    return false;
  }
  for (let k = 0; k < line.length; k++) {
    if (line[k] !== ch) {
      return false;
    }
  }
  return true;
}

function isOlStart(line: string): boolean {
  if (line.length < 3) {
    return false;
  }
  let k: number = 0;
  while (k < line.length && line[k] >= '0' && line[k] <= '9') {
    k++;
  }
  return k > 0 && k + 1 < line.length && line[k] === '.' && line[k + 1] === ' ';
}

function isTableSep(line: string): boolean {
  if (!line.includes('-')) {
    return false;
  }
  for (let k = 0; k < line.length; k++) {
    const c: string = line[k];
    if (c !== '|' && c !== '-' && c !== ':' && c !== ' ' && c !== '\t') {
      return false;
    }
  }
  return true;
}

/** 表格行 → 单元格数组（去掉首尾空串产生的空单元格）。 */
function parseTableRow(row: string): string[] {
  const cells: string[] = [];
  const parts: string[] = row.split('|');
  for (let p = 0; p < parts.length; p++) {
    const c: string = parts[p].trim();
    if (c.length > 0) {
      cells.push(c);
    }
  }
  return cells;
}

// ── Block parser ───────────────────────────────────────────────────────────────

export function parseBlocks(content: string): MdBlock[] {
  if (content.length === 0) {
    return [];
  }
  const lines: string[] = content.split('\n');
  const blocks: MdBlock[] = [];
  let i: number = 0;

  while (i < lines.length) {
    const line: string = lines[i];

    // Empty line
    if (line.trim().length === 0) {
      i++;
      continue;
    }

    // Code fence
    if (line.startsWith('```')) {
      const langEnd: number = line.indexOf('`', 3);
      const lang: string = langEnd > 3 ? line.substring(3, langEnd).trim() : '';
      const codeLines: string[] = [];
      i++;
      while (i < lines.length && !lines[i].trimStart().startsWith('```')) {
        codeLines.push(lines[i]);
        i++;
      }
      if (i < lines.length) {
        i++;
      }
      blocks.push({ type: 'code', lang: lang, codeLines: codeLines });
      continue;
    }

    // Heading
    if (line.startsWith('#')) {
      let level: number = 0;
      while (level < line.length && line[level] === '#') {
        level++;
      }
      if (level <= 6 && level < line.length && line[level] === ' ') {
        blocks.push({ type: 'heading', level: level, text: line.substring(level + 1).trim() });
        i++;
        continue;
      }
    }

    // Horizontal rule
    if (isHr(line.trim())) {
      blocks.push({ type: 'hr' });
      i++;
      continue;
    }

    // Unordered list
    if ((line.startsWith('- ') || line.startsWith('* ')) && !line.startsWith('- [')) {
      const items: string[] = [];
      while (i < lines.length && (lines[i].startsWith('- ') || lines[i].startsWith('* ')) && !lines[i].startsWith('- [')) {
        items.push(lines[i].substring(2));
        i++;
      }
      blocks.push({ type: 'list', items: items });
      continue;
    }

    // Ordered list
    if (isOlStart(line)) {
      const items: string[] = [];
      while (i < lines.length && isOlStart(lines[i])) {
        const dotIdx: number = lines[i].indexOf('. ');
        items.push(lines[i].substring(dotIdx + 2));
        i++;
      }
      blocks.push({ type: 'ol', items: items });
      continue;
    }

    // Blockquote
    if (line.startsWith('> ')) {
      const qLines: string[] = [];
      while (i < lines.length && lines[i].startsWith('> ')) {
        qLines.push(lines[i].substring(2));
        i++;
      }
      blocks.push({ type: 'blockquote', text: qLines.join('\n') });
      continue;
    }

    // Table
    if (line.trimStart().startsWith('|') && !isTableSep(line)) {
      const tLines: string[] = [];
      while (i < lines.length && lines[i].trimStart().startsWith('|')) {
        tLines.push(lines[i]);
        i++;
      }
      if (tLines.length >= 2) {
        const headers: string[] = parseTableRow(tLines[0]);
        const rows: string[][] = [];
        for (let k = 1; k < tLines.length; k++) {
          if (!isTableSep(tLines[k].trim())) {
            rows.push(parseTableRow(tLines[k]));
          }
        }
        if (headers.length > 0) {
          blocks.push({ type: 'table', headers: headers, rows: rows });
        }
      }
      continue;
    }

    // Paragraph: collect consecutive non-special lines
    {
      const paraLines: string[] = [];
      while (i < lines.length) {
        const ln: string = lines[i];
        if (ln.trim().length === 0) {
          break;
        }
        if (ln.startsWith('```') || ln.startsWith('#') || isHr(ln.trim())) {
          break;
        }
        if (ln.startsWith('- ') || ln.startsWith('* ') || isOlStart(ln) || ln.startsWith('> ')) {
          break;
        }
        if (ln.trimStart().startsWith('|') && !isTableSep(ln)) {
          break;
        }
        paraLines.push(ln);
        i++;
      }
      if (paraLines.length > 0) {
        blocks.push({ type: 'para', text: paraLines.join('\n') });
      }
    }
  }
  return blocks;
}
