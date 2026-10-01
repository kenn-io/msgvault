// Mirrors PartialDate.Validate (internal/store/partialdate.go): year 1–9999,
// month 1–12, and a day that exists in that month. A day without a year is
// checked against leap year 2000, as the store does.
const YEAR_FORMS = /^(\d{4})(?:-(\d{2})(?:-(\d{2}))?)?$|^(\d{4})(\d{2})(\d{2})$/;
const PROFILE_FORMS = /^(\d{4})(?:-(\d{2})(?:-(\d{2}))?)?$|^--(\d{2})(?:-(\d{2}))?$|^---(\d{2})$/;
const INTERVAL_MESSAGE = 'Use a year, year and month, or full date, like 2019, 2019-04, or 2019-04-12.';
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

export function intervalDateError(value: string): string | null {
  const trimmed = value.trim();
  if (!trimmed) return null;
  const match = YEAR_FORMS.exec(trimmed);
  if (!match) return INTERVAL_MESSAGE;
  const [year, month, day] = match[1] ? [match[1], match[2], match[3]] : [match[4], match[5], match[6]];
  return validParts(num(year), num(month), num(day)) ? null : INTERVAL_MESSAGE;
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
