import { localDateStr, monthLabels } from '@kenn-io/kit-ui';

// Mirrors PartialDate.Validate (internal/store/partialdate.go): year 1–9999,
// month 1–12, and a day that exists in that month. A day without a year is
// checked against leap year 2000, as the store does.
const YEAR_FORMS = /^(\d{4})(?:-(\d{2})(?:-(\d{2}))?)?$|^(\d{4})(\d{2})(\d{2})$/;
const PROFILE_FORMS = /^(\d{4})(?:-(\d{2})(?:-(\d{2}))?)?$|^--(\d{2})(?:-(\d{2}))?$|^---(\d{2})$/;
/** The forms a date field takes, shown in the hint's place while it's empty or being typed. */
export const DATE_HINT = '2024, 2024-03, or 2024-03-15';
/** The error under a date field that isn't one of those forms; it replaces the hint. */
export const DATE_ERROR = 'Use 2024, 2024-03, or 2024-03-15.';
const RANGE_MESSAGE = 'This date does not exist. Check the month and day.';

type DateParts = [number | undefined, number | undefined, number | undefined];

function validParts(year: number | undefined, month: number | undefined, day: number | undefined): boolean {
  if (year !== undefined && (year < 1 || year > 9999)) return false;
  if (month !== undefined && (month < 1 || month > 12)) return false;
  if (day !== undefined && (day < 1 || day > 31)) return false;
  if (month === undefined || day === undefined) return true;
  const probeYear = year ?? 2000;
  const probe = new Date(Date.UTC(probeYear, month - 1, day));
  probe.setUTCFullYear(probeYear);
  return probe.getUTCFullYear() === probeYear && probe.getUTCMonth() === month - 1 && probe.getUTCDate() === day;
}

const num = (value: string | undefined): number | undefined => (value === undefined ? undefined : Number(value));

export function isCalendarDate(value: string): boolean {
  const match = /^(\d{4})-(\d{2})-(\d{2})$/.exec(value);
  return match ? validParts(num(match[1]), num(match[2]), num(match[3])) : false;
}

export function intervalDateError(value: string, start = ''): string | null {
  const trimmed = value.trim();
  if (!trimmed) return null;
  const match = YEAR_FORMS.exec(trimmed);
  if (!match) return DATE_ERROR;
  const [year, month, day] = match[1] ? [match[1], match[2], match[3]] : [match[4], match[5], match[6]];
  if (!validParts(num(year), num(month), num(day))) return DATE_ERROR;
  if (start.trim() && !intervalDateError(start)) {
    const a = start.trim().replaceAll('-', ''), b = trimmed.replaceAll('-', '');
    const precision = Math.min(a.length, b.length);
    if (b.slice(0, precision) < a.slice(0, precision)) return 'End date must be on or after start date.';
  }
  return null;
}

function profileParts(value: string): DateParts | undefined {
  const match = PROFILE_FORMS.exec(value);
  if (!match) return undefined;
  if (match[1]) return [num(match[1]), num(match[2]), num(match[3])];
  if (match[4]) return [undefined, num(match[4]), num(match[5])];
  return [undefined, undefined, num(match[6])];
}

export function profileDateError(value: string): string | null {
  const parts = profileParts(value.trim());
  if (!parts) return null;
  return validParts(...parts) ? null : RANGE_MESSAGE;
}

export function isTextProfileDate(value: string): boolean {
  const trimmed = value.trim();
  return trimmed !== '' && profileParts(trimmed) === undefined;
}

export function formatPartialDate(value: { year?: number; month?: number; day?: number } | undefined): string {
  if (!value) return '';
  return [value.year?.toString().padStart(4, '0'), value.month?.toString().padStart(2, '0'), value.day?.toString().padStart(2, '0')].filter(Boolean).join('-');
}

const MONTHS = monthLabels('short');
type PartialDateParts = { year?: number; month?: number; day?: number };

/**
 * A stored partial date, or a typed one ("2024-03"), as a person reads it: "2024", "Mar 2024", "Mar 5, 2024", and a
 * day this year without its year, "Nov 5".
 */
export function readablePartialDate(value: string | PartialDateParts | null | undefined, today: Date = new Date()): string {
  if (!value) return '';
  let date: PartialDateParts | undefined = typeof value === 'string' ? undefined : value;
  if (typeof value === 'string') {
    const match = /^(\d{4})(?:-(\d{2})(?:-(\d{2}))?)?$/.exec(value.trim().replace(/^(\d{4})(\d{2})(\d{2})$/, '$1-$2-$3'));
    if (!match) return value.trim();
    date = { year: Number(match[1]), month: match[2] ? Number(match[2]) : undefined, day: match[3] ? Number(match[3]) : undefined };
  }
  if (!date?.year) return '';
  const month = date.month ? MONTHS[date.month - 1] : '';
  if (!month) return String(date.year);
  if (!date.day) return `${month} ${date.year}`;
  return date.year === today.getFullYear() ? `${month} ${date.day}` : `${month} ${date.day}, ${date.year}`;
}

/**
 * What sits in the hint's place under a date field: the forms while it's empty
 * or focused, then the date in words once it holds one ("Mar 2024", "Today,
 * Nov 6"). Empty for an invalid value, whose error takes the place.
 */
export function dateNote(value: string, focused: boolean, today: Date = new Date()): string {
  if (intervalDateError(value)) return '';
  const trimmed = value.trim().replace(/^(\d{4})(\d{2})(\d{2})$/, '$1-$2-$3');
  if (focused || !trimmed) return DATE_HINT;
  const reading = readablePartialDate(trimmed, today);
  return trimmed === localDateStr(today) ? `Today, ${reading}` : reading === value.trim() ? '' : reading;
}

/** Sort partial dates latest first, placing an unknown month or day at the end of its period. */
export function latestDateKey(value: PartialDateParts | undefined): number {
  return value?.year ? value.year * 10000 + (value.month ?? 12) * 100 + (value.day ?? 31) : 0;
}
