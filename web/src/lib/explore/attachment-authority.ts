const ATTACHMENT_SELECTION_PREFIX = 'attachment:';

export function attachmentSelection(id: number): string {
  if (!Number.isSafeInteger(id) || id < 1) throw new Error('Attachment authority requires a positive safe integer ID.');
  return `${ATTACHMENT_SELECTION_PREFIX}${id}`;
}

export function parseAttachmentSelection(value: string | null): number | undefined {
  if (!value?.startsWith(ATTACHMENT_SELECTION_PREFIX)) return undefined;
  const encoded = value.slice(ATTACHMENT_SELECTION_PREFIX.length);
  if (!/^[1-9][0-9]*$/.test(encoded)) return undefined;
  const id = Number(encoded);
  return Number.isSafeInteger(id) ? id : undefined;
}
