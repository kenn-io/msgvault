import type { RelationshipRow } from '../api/generated/models';
import type { ChipTone } from '@kenn-io/kit-ui';
import { sentenceCase } from '../explore/labels';

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

export function formatContactDate(iso: string): string {
  const date = new Date(iso);
  return Number.isNaN(date.valueOf()) ? iso : date.toLocaleDateString('en-US', DATE_FORMAT);
}

export function formatDay(day: string): string {
  return new Date(`${day}T00:00:00Z`).toLocaleDateString('en-US', { ...DATE_FORMAT, timeZone: 'UTC' });
}

/** Only the saved directory projection supplies cadence; stale dates are provisional. */
export function cadenceLabel(cadence: NonNullable<RelationshipRow['cadence']>): string {
  const frequency = cadence.list ? `List ${cadence.list}` : cadence.frequency_days ? `Every ${cadence.frequency_days} ${cadence.frequency_days === 1 ? 'day' : 'days'}` : 'No list';
  if (cadence.stale) return `${frequency} · Due date updating`;
  if (!cadence.frequency_days) return frequency;
  if (!cadence.due_at) return `${frequency} · Due date unknown`;
  const date = formatDay(cadence.due_at.slice(0, 10));
  return `${frequency} · ${cadence.status === 'overdue' ? `Overdue · ${date}` : `Due ${date}`}`;
}
