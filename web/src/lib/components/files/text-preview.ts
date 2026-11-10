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

export async function parseDelimited(text: string, delimiter = '', maxRows = 200, inputTruncated = false): Promise<{ header: string[]; rows: string[][]; truncated: boolean; hasErrors: boolean }> {
  const { default: Papa } = await import('papaparse');
  const linebreak = text.endsWith('\r') ? Papa.parse(text + '\n', { delimiter, preview: maxRows + 2, skipEmptyLines: true }).meta.linebreak : '';
  const prefix = text.endsWith('\r') && linebreak === '\n' ? text.slice(0, -1) + '\n' : text.endsWith('\r') && linebreak === '\r\n' ? text + '\n' : text;
  const result = Papa.parse<string[]>(prefix, { delimiter, preview: maxRows + 2, skipEmptyLines: true });
  const hasErrors = result.errors.some((error) => error.type === 'Quotes' && !(inputTruncated && !result.meta.truncated && error.code === 'MissingQuotes' && error.row === result.data.length - 1));
  // A byte prefix can end inside a field, including a quoted multiline field.
  // Only a parsed record terminator proves the final record is complete.
  if (inputTruncated && !result.meta.truncated && (!prefix.endsWith(result.meta.linebreak) || result.errors.some((error) => error.code === 'MissingQuotes'))) result.data.pop();
  const visible = result.data.slice(0, maxRows + 1);
  const columns = Math.max(0, ...visible.map((row) => row.length));
  const header = Array.from({ length: columns }, (_, index) => visible[0]?.[index] ?? '');
  return {
    header, rows: visible.slice(1),
    truncated: result.data.length > maxRows + 1 || result.meta.truncated,
    hasErrors,
  };
}
