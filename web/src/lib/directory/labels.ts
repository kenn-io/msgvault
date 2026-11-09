import type { ChipTone } from '@kenn-io/kit-ui';
import { OrganizationCreateBodyKind } from '../api/generated/models/organizationCreateBodyKind';
import { sentenceCase } from '../explore/labels';
import { sourcePhrase, type ProvenanceLine } from '../components/directory/history-format';
import { formatDateTime, formatShortDate } from '../util/format';
import { readablePartialDate } from './dates';

export const organizationKindOptions = Object.values(OrganizationCreateBodyKind).map(value => ({ value, label: sentenceCase(value) }));

const CONTACT_STATES: Record<string, string> = { active: 'Active', inactive: 'Inactive' };
// The directory projection uses ActivityChannel (internal/store/activity.go).
const CHANNELS: Record<string, string> = {
  email: 'Email', chat: 'Chat', meeting: 'Meeting', other: 'Other'
};
export const PRIMARY_CHANNELS = Object.keys(CHANNELS);
// Shared status vocabulary: amber needs attention, green finished, gray off.
const REVIEW_STATES: Record<string, { label: string; tone: ChipTone }> = {
  candidate: { label: 'Candidate', tone: 'warning' },
  conflict: { label: 'Conflict', tone: 'warning' },
  pending: { label: 'Pending', tone: 'warning' },
  accepted: { label: 'Accepted', tone: 'success' },
  rejected: { label: 'Rejected', tone: 'muted' },
};
const DATE_FORMAT: Intl.DateTimeFormatOptions = { year: 'numeric', month: 'short', day: 'numeric' };

export const contactStateLabel = (code: string): string => CONTACT_STATES[code] ?? sentenceCase(code);
export const channelLabel = (code: string): string => CHANNELS[code] ?? sentenceCase(code);
export const reviewStateChip = (code: string): { label: string; tone: ChipTone } =>
  REVIEW_STATES[code] ?? { label: sentenceCase(code), tone: 'neutral' };

type SpanDate = { year?: number; month?: number; day?: number } | null | undefined;

/**
 * A job's or a connection's dates as one range, so Work and Connections read alike: "Mar 2024 – present",
 * "2020 – 2023", "From 2019", "Until 2021". `later` lowercases "From" and "Until" for a later middot segment.
 */
export function dateSpan(start: SpanDate, end: SpanDate, current: boolean, { later = false, precision = 'month' }: { later?: boolean; precision?: 'month' | 'day' } = {}): string {
  const when = (value: SpanDate) => readablePartialDate(value && { year: value.year, month: value.month, ...(precision === 'day' ? { day: value.day } : {}) });
  const from = when(start), until = when(end) || (current ? 'present' : '');
  const text = from && until ? (from === until ? from : `${from} – ${until}`) : from ? `From ${from}` : until && until !== 'present' ? `Until ${until}` : '';
  return later ? text.replace(/^(From|Until) /, word => word.toLowerCase()) : text;
}

/**
 * When the saved version in a conflict was saved, under its Saved column: "Saved at 8:34 AM" today, "Saved on Nov 5 at 8:34 AM"
 * before. msgvault can't tell the person's other windows or devices apart, so a change the owner made names no one; one from an
 * import or msgvault itself names it: "Saved from CardDAV contacts at 8:34 AM", "Saved by msgvault at 8:34 AM".
 */
export function conflictSaved(at: string, source = 'user'): ProvenanceLine {
  const date = new Date(at);
  if (Number.isNaN(date.valueOf())) return { text: 'Saved', title: '' };
  const time = date.toLocaleTimeString(undefined, { hour: 'numeric', minute: '2-digit' });
  const when = date.toDateString() === new Date().toDateString() ? `at ${time}` : `on ${formatShortDate(at)} at ${time}`;
  return { text: ['Saved', sourcePhrase(source), when].filter(Boolean).join(' '), title: `Saved ${formatDateTime(at)}` };
}

export function formatContactDate(iso: string): string {
  const date = new Date(iso);
  return Number.isNaN(date.valueOf()) ? iso : date.toLocaleDateString('en-US', DATE_FORMAT);
}

export function formatDay(day: string): string {
  return new Date(`${day}T00:00:00Z`).toLocaleDateString('en-US', { ...DATE_FORMAT, timeZone: 'UTC' });
}
