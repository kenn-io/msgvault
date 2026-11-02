import { sentenceCase } from '../../explore/labels';
import { formatDateTime } from '../../util/format';

type HistoryValue = { source: string; source_ref?: string; active_from?: string; active_until?: string; superseded_at?: string; created_at: string; updated_at?: string; actor?: string; confidence?: number };

/** Source and audit information retained for a historical value. */
export function historyProvenance(value: HistoryValue): string {
  return [
    historySource(value.source),
    value.actor ? `Actor: ${value.actor}` : undefined,
    value.source_ref ? `Reference: ${value.source_ref}` : undefined,
    value.confidence === undefined ? undefined : `Confidence: ${value.confidence}`,
    value.active_from ? `Valid from: ${formatDateTime(value.active_from)}` : undefined,
    value.active_until ? `Valid until: ${formatDateTime(value.active_until)}` : undefined,
    value.superseded_at ? `Superseded: ${formatDateTime(value.superseded_at)}` : undefined,
    `${value.updated_at ? 'Updated' : 'Created'}: ${formatDateTime(value.updated_at ?? value.created_at)}`,
  ].filter(Boolean).join(' · ');
}

/** Who wrote a value: "You" for edits made here, otherwise the importer. */
export function historySource(source: string): string {
  return source === 'user' ? 'You' : source === 'carddav_import' ? 'Address book' : sentenceCase(source);
}
