const ATTACHMENT_SELECTION_PREFIX = 'attachment:';

export function parseAttachmentSelection(value: string | null): number | undefined {
  if (!value?.startsWith(ATTACHMENT_SELECTION_PREFIX)) return undefined;
  const encoded = value.slice(ATTACHMENT_SELECTION_PREFIX.length);
  if (!/^[1-9][0-9]*$/.test(encoded)) return undefined;
  const id = Number(encoded);
  return Number.isSafeInteger(id) ? id : undefined;
}
