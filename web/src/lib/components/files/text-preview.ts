export function decodeText(bytes: Uint8Array, contentType = '', truncated = false): string {
  let encoding = /charset\s*=\s*["']?([^\s;"']+)/i.exec(contentType)?.[1] ?? 'utf-8';
  if (bytes[0] === 0xef && bytes[1] === 0xbb && bytes[2] === 0xbf) encoding = 'utf-8';
  else if (bytes[0] === 0xff && bytes[1] === 0xfe) encoding = 'utf-16le';
  else if (bytes[0] === 0xfe && bytes[1] === 0xff) encoding = 'utf-16be';
  let decoder: TextDecoder;
  try { decoder = new TextDecoder(encoding); }
  catch { decoder = new TextDecoder('utf-8'); }
  return decoder.decode(bytes, { stream: truncated });
}

const MAX_PREVIEW_CELLS = 20_000;

export async function parseDelimited(text: string, delimiter = '', maxRows = 200, inputTruncated = false): Promise<{ header: string[]; rows: string[][]; truncated: boolean; cellLimit: boolean; columnsTruncated: boolean; hasErrors: boolean }> {
  const { default: Papa } = await import('papaparse');
  // Records may end in LF, CRLF or CR within one file: unify terminators outside quoted fields, whose content is kept.
  const fieldStart = `(?<=^|[\r\n]|${(delimiter ? [delimiter] : [',', '\t', '|', ';', '\x1e', '\x1f']).map((d) => d.replace(/[\\^$.*+?()[\]{}|/-]/g, '\\$&')).join('|')})`, prefix = text.replace(new RegExp(`(${fieldStart}"(?:[^"]|"")*(?:"|$))|\r\n?`, 'g'), (_, quoted) => quoted ?? '\n');
  // Guess with blank lines ignored, then retain every parsed record.
  const resolved = delimiter || Papa.parse(prefix, { newline: '\n', preview: 1, skipEmptyLines: true }).meta.delimiter;
  const result = Papa.parse<string[]>(prefix, { delimiter: resolved, newline: '\n', preview: maxRows + 2 });
  // A final record terminator produces one extra empty row.
  const last = result.data.at(-1);
  if (!result.meta.truncated && prefix.endsWith(result.meta.linebreak) && last?.length === 1 && last[0] === '') result.data.pop();
  const hasErrors = result.errors.some((error) => error.type === 'Quotes' && !(inputTruncated && !result.meta.truncated && error.code === 'MissingQuotes' && error.row === result.data.length - 1));
  // A byte prefix can end inside a field, including a quoted multiline field.
  // Only a parsed record terminator proves the final record is complete.
  if (inputTruncated && !result.meta.truncated && (!prefix.endsWith(result.meta.linebreak) || result.errors.some((error) => error.code === 'MissingQuotes'))) result.data.pop();
  const visible = result.data.slice(0, maxRows + 1);
  const columns = Math.max(0, ...visible.map((row) => row.length));
  const header = Array.from({ length: Math.min(columns, MAX_PREVIEW_CELLS) }, (_, index) => visible[0]?.[index] ?? '');
  const rows: string[][] = [];
  let cells = header.length;
  for (const row of visible.slice(1)) {
    if (cells + row.length > MAX_PREVIEW_CELLS) break;
    rows.push(row);
    cells += row.length;
  }
  return {
    header, rows, cellLimit: rows.length < visible.length - 1, columnsTruncated: columns > header.length,
    truncated: result.data.length > maxRows + 1 || result.meta.truncated,
    hasErrors,
  };
}
