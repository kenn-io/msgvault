import { sentenceCase } from '../../explore/labels';

const WHEN = new Intl.DateTimeFormat('en-US', { month: 'short', day: 'numeric', year: 'numeric', hour: 'numeric', minute: '2-digit' });

/** "Nov 1, 2026, 4:44 AM" in the viewer's time zone. */
export function historyTime(iso: string | undefined): string {
  if (!iso) return '';
  const date = new Date(iso);
  return Number.isNaN(date.valueOf()) ? iso : WHEN.format(date);
}

/** Who wrote a value: "You" for edits made here, otherwise the importer. */
export function historySource(source: string): string {
  return source === 'user' ? 'You' : source === 'carddav' ? 'Address book' : sentenceCase(source);
}
