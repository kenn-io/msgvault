const PREFIX = 'archive-meeting:';
export const ARCHIVE_MEETING_HISTORY_KEY = 'msgvaultArchivedMeeting';

export interface ArchiveMeetingHistory {
  id: number;
  returnSelectedRow: string | null;
}

export function archiveMeetingSelection(id: number): string {
  if (!Number.isSafeInteger(id) || id < 1) throw new Error('An archived meeting requires a positive safe integer ID.');
  return `${PREFIX}${id}`;
}

export function parseArchiveMeetingSelection(value: string | null): number | undefined {
  if (!value?.startsWith(PREFIX)) return undefined;
  const encoded = value.slice(PREFIX.length);
  if (!/^[1-9][0-9]*$/.test(encoded)) return undefined;
  const id = Number(encoded);
  return Number.isSafeInteger(id) ? id : undefined;
}

/** History ownership belongs only to the exact archive entry being rewritten. */
export function parseArchiveMeetingHistory(value: unknown, selectedRow: string | null): ArchiveMeetingHistory | undefined {
  const id = parseArchiveMeetingSelection(selectedRow);
  if (id === undefined || typeof value !== 'object' || value === null || Array.isArray(value)) return undefined;
  const marker = (value as Record<string, unknown>)[ARCHIVE_MEETING_HISTORY_KEY];
  if (typeof marker !== 'object' || marker === null || Array.isArray(marker)) return undefined;
  const { id: markerID, returnSelectedRow } = marker as Record<string, unknown>;
  if (markerID !== id || (returnSelectedRow !== null && typeof returnSelectedRow !== 'string')) return undefined;
  return { id, returnSelectedRow };
}
