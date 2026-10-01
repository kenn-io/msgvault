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
