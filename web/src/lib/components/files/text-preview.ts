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
  const result = Papa.parse<string[]>(text, { delimiter, preview: maxRows + 2, skipEmptyLines: true });
  // A byte prefix can end inside a field, including a quoted multiline field.
  // Only a parsed record terminator proves the final record is complete.
  if (inputTruncated && !result.meta.truncated && (!text.endsWith(result.meta.linebreak) || result.errors.some((error) => error.code === 'MissingQuotes'))) result.data.pop();
  return {
    header: result.data[0] ?? [],
    rows: result.data.slice(1, maxRows + 1),
    truncated: result.data.length > maxRows + 1 || result.meta.truncated,
    hasErrors: result.errors.length > 0,
  };
}
