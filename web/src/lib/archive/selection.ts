export function parseArchiveSelection(value: string | null, prefix: string): number | undefined {
  if (!value?.startsWith(prefix)) return undefined;
  const encoded = value.slice(prefix.length);
  if (!/^[1-9][0-9]*$/.test(encoded)) return undefined;
  const id = Number(encoded);
  return Number.isSafeInteger(id) ? id : undefined;
}
