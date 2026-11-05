export function decodeText(bytes: Uint8Array): string {
  return new TextDecoder('utf-8').decode(bytes);
}

export async function parseDelimited(text: string, delimiter: string, maxRows = 200): Promise<{ header: string[]; rows: string[][]; truncated: boolean }> {
  const { default: Papa } = await import('papaparse');
  const result = Papa.parse<string[]>(text, { delimiter, preview: maxRows + 2, skipEmptyLines: true });
  return {
    header: result.data[0] ?? [],
    rows: result.data.slice(1, maxRows + 1),
    truncated: result.data.length > maxRows + 1 || result.meta.truncated,
  };
}
