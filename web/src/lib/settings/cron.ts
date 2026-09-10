/**
 * Five-field cron expressions as the daemon accepts them: minute, hour, day
 * of month, month, and day of week, with `*`, ranges, lists, steps, and
 * month or weekday names. The daemon's parser stays authoritative; this
 * module mirrors its rules closely enough to flag mistakes while typing and
 * to say in plain English when a schedule runs.
 */

export type CronFieldName = 'minute' | 'hour' | 'day' | 'month' | 'weekday';

export interface CronFieldSpec {
  name: CronFieldName;
  label: string;
  min: number;
  max: number;
  names?: Readonly<Record<string, number>>;
}

const MONTH_NAMES = ['jan', 'feb', 'mar', 'apr', 'may', 'jun', 'jul', 'aug', 'sep', 'oct', 'nov', 'dec'];
const WEEKDAY_NAMES = ['sun', 'mon', 'tue', 'wed', 'thu', 'fri', 'sat'];
const MONTHS = [
  'January', 'February', 'March', 'April', 'May', 'June',
  'July', 'August', 'September', 'October', 'November', 'December',
];
const WEEKDAYS = ['Sunday', 'Monday', 'Tuesday', 'Wednesday', 'Thursday', 'Friday', 'Saturday'];

export const CRON_FIELDS: readonly CronFieldSpec[] = [
  { name: 'minute', label: 'minute', min: 0, max: 59 },
  { name: 'hour', label: 'hour', min: 0, max: 23 },
  { name: 'day', label: 'day', min: 1, max: 31 },
  { name: 'month', label: 'month', min: 1, max: 12, names: nameTable(MONTH_NAMES, 1) },
  { name: 'weekday', label: 'weekday', min: 0, max: 6, names: nameTable(WEEKDAY_NAMES, 0) },
];

/** One whitespace-delimited piece of the expression with its position. */
export interface CronToken {
  /** Which field the token fills; `extra` when there are more than five. */
  field: CronFieldName | 'extra';
  text: string;
  start: number;
  end: number;
  error?: string;
}

/** One comma-separated term inside a field, normalised to a range and step. */
export interface CronTerm {
  start: number;
  end: number;
  step: number;
  /** True for `*` or `?`, meaning "every value". */
  all: boolean;
}

export interface CronField {
  name: CronFieldName;
  terms: CronTerm[];
  /** True when the field matches every value without a step. */
  any: boolean;
}

export interface CronParse {
  tokens: CronToken[];
  /** All five fields when the expression is valid. */
  fields?: CronField[];
  /** The IANA zone named by a `CRON_TZ=` or `TZ=` prefix, if any. */
  zone?: string;
  /** The first problem found, in plain words. */
  error?: string;
}

/** A schedule split into its optional time zone and the five fields. */
export interface CronParts {
  /** IANA zone name; empty when the daemon's local time applies. */
  zone: string;
  /** The five-field expression without the zone prefix. */
  expression: string;
}

const ZONE_PREFIX = /^\s*(?:CRON_TZ|TZ)=(\S*)\s*/;

export interface CronPreset {
  label: string;
  expression: string;
}

export const CRON_PRESETS: readonly CronPreset[] = [
  { label: 'Every 15 minutes', expression: '*/15 * * * *' },
  { label: 'Every hour', expression: '0 * * * *' },
  { label: 'Every day at 03:00', expression: '0 3 * * *' },
  { label: 'Weekdays at 09:00', expression: '0 9 * * 1-5' },
  { label: 'Sundays at 02:00', expression: '0 2 * * 0' },
  { label: 'Monthly on the 1st at 04:00', expression: '0 4 1 * *' },
];

function nameTable(names: readonly string[], offset: number): Record<string, number> {
  return Object.fromEntries(names.map((name, index) => [name, index + offset]));
}

/**
 * Separates a `CRON_TZ=<zone>` or `TZ=<zone>` prefix from the five fields.
 * The daemon reads both spellings; `joinCron` always writes `CRON_TZ=`.
 */
export function splitCron(schedule: string): CronParts {
  const match = ZONE_PREFIX.exec(schedule);
  if (!match) return { zone: '', expression: schedule };
  return { zone: match[1], expression: schedule.slice(match[0].length) };
}

/** Rebuilds a schedule; an empty expression stays empty so "off" survives. */
export function joinCron(zone: string, expression: string): string {
  const trimmed = expression.trim();
  if (zone === '' || trimmed === '') return expression;
  return `CRON_TZ=${zone} ${trimmed}`;
}

/**
 * True when the browser can resolve the zone name. "Local" and "UTC" are
 * always accepted because the daemon resolves them itself.
 */
export function isKnownTimeZone(zone: string): boolean {
  if (zone === 'UTC' || zone === 'Local') return true;
  if (zone === '') return false;
  try {
    new Intl.DateTimeFormat('en-US', { timeZone: zone });
    return true;
  } catch {
    return false;
  }
}

/** IANA zone names the browser knows, with UTC first. */
export function timeZoneNames(): string[] {
  const supported = typeof Intl.supportedValuesOf === 'function' ? Intl.supportedValuesOf('timeZone') : [];
  return ['UTC', ...supported.filter((zone) => zone !== 'UTC')];
}

/** "America/New_York" as "America/New York", for menus and summaries. */
export function timeZoneLabel(zone: string): string {
  return zone.replaceAll('_', ' ');
}

/** Splits an expression into tokens and checks each field. */
export function parseCron(schedule: string): CronParse {
  const prefix = ZONE_PREFIX.exec(schedule);
  const zone = prefix?.[1] ?? '';
  if (prefix && !isKnownTimeZone(zone)) {
    const error = zone === '' ? 'Time zone: a name is missing.' : `Time zone: "${zone}" is not a known time zone.`;
    return { tokens: [], zone, error };
  }
  const expression = prefix ? schedule.slice(prefix[0].length) : schedule;
  const tokens = tokenize(expression, prefix?.[0].length ?? 0);
  if (tokens.length === 0) {
    return { tokens, zone: zone || undefined, error: 'Enter five fields: minute, hour, day, month, and weekday.' };
  }

  const fields: CronField[] = [];
  let error: string | undefined;
  tokens.forEach((token, index) => {
    if (token.field === 'extra') {
      token.error = 'Only five fields are allowed.';
      error ??= token.error;
      return;
    }
    const spec = CRON_FIELDS[index];
    try {
      fields.push({ name: spec.name, ...parseField(token.text, spec) });
    } catch (cause) {
      token.error = cause instanceof Error ? cause.message : String(cause);
      error ??= `${capitalize(spec.label)}: ${token.error}`;
    }
  });
  if (!error && tokens.length < CRON_FIELDS.length) {
    const missing = CRON_FIELDS.slice(tokens.length).map((field) => field.label);
    error = `Missing ${joinWords(missing)}.`;
  }
  if (error) return { tokens, zone: zone || undefined, error };
  return { tokens, fields, zone: zone || undefined };
}

function tokenize(expression: string, offset: number): CronToken[] {
  const tokens: CronToken[] = [];
  const pattern = /\S+/g;
  let match: RegExpExecArray | null;
  while ((match = pattern.exec(expression)) !== null) {
    const index = tokens.length;
    tokens.push({
      field: index < CRON_FIELDS.length ? CRON_FIELDS[index].name : 'extra',
      text: match[0],
      start: offset + match.index,
      end: offset + match.index + match[0].length,
    });
  }
  return tokens;
}

function parseField(text: string, spec: CronFieldSpec): { terms: CronTerm[]; any: boolean } {
  // The daemon splits on commas and drops empty pieces, so "0," and "0,,30"
  // are lists of one and two values. A field with no values at all never
  // matches; the daemon stores it, but the browser calls it out.
  const pieces = text.split(',').filter((piece) => piece !== '');
  if (pieces.length === 0) throw new Error('a value is missing.');
  const terms = pieces.map((piece) => parseTerm(piece, spec));
  return { terms, any: terms.some((term) => term.all && term.step === 1) };
}

function parseTerm(text: string, spec: CronFieldSpec): CronTerm {
  const [rangeText, stepText, ...moreSlashes] = text.split('/');
  if (moreSlashes.length > 0) throw new Error(`too many slashes in ${text}.`);
  const [lowText, highText, ...moreHyphens] = rangeText.split('-');
  if (moreHyphens.length > 0) throw new Error(`too many hyphens in ${text}.`);

  let start: number;
  let end: number;
  let all = false;
  if (lowText === '*' || lowText === '?') {
    // The daemon reads "*-N" as "*" and ignores the range end.
    start = spec.min;
    end = spec.max;
    all = true;
  } else {
    start = parseValue(lowText, spec);
    end = highText === undefined ? start : parseValue(highText, spec);
  }

  let step = 1;
  if (stepText !== undefined) {
    step = parsePositiveInt(stepText, `step in ${text}`);
    if (highText === undefined && !all) end = spec.max;
  }

  if (start < spec.min) throw new Error(`${start} is below the minimum of ${spec.min}.`);
  if (end > spec.max) throw new Error(`${end} is above the maximum of ${spec.max}.`);
  if (start > end) throw new Error(`${start} comes after ${end}.`);
  return { start, end, step, all };
}

// The daemon reads numbers with Atoi, so a leading plus sign is allowed.
const WHOLE_NUMBER = /^\+?\d+$/;

function parseValue(text: string, spec: CronFieldSpec): number {
  if (text === '') throw new Error('a value is missing.');
  if (WHOLE_NUMBER.test(text)) return Number(text);
  const named = spec.names?.[text.toLowerCase()];
  if (named !== undefined) return named;
  throw new Error(`"${text}" is not a ${spec.label}.`);
}

function parsePositiveInt(text: string, what: string): number {
  if (!WHOLE_NUMBER.test(text)) throw new Error(`${what} must be a whole number.`);
  const value = Number(text);
  if (value === 0) throw new Error(`${what} must be at least 1.`);
  return value;
}

/**
 * Text for showing a stored schedule outside an editor: the English
 * description when the expression parses, otherwise the expression itself.
 */
export function scheduleSummary(expression: string | null | undefined): string {
  if (!expression) return '';
  const parsed = parseCron(expression);
  return parsed.fields ? describeFields(parsed.fields, parsed.zone) : expression;
}

/** Plain-English summary of a parsed schedule, such as "At 03:00 every day". */
export function describeCron(expression: string): string {
  const parsed = parseCron(expression);
  if (!parsed.fields) return parsed.error ?? '';
  return describeFields(parsed.fields, parsed.zone);
}

/** Describes parsed fields; a zone adds ", Europe/Berlin time" at the end. */
export function describeFields(fields: CronField[], zone?: string): string {
  const [minute, hour, day, month, weekday] = fields;
  const time = describeTime(minute, hour);
  const days = describeDays(day, month, weekday);
  // "Every 15 minutes every day" says nothing extra; a clock time does.
  const clockTime = time.startsWith('at ') && !time.startsWith('at :');
  const text = days === 'every day' && !clockTime ? capitalize(time) : capitalize(`${time} ${days}`);
  return zone ? `${text}, ${timeZoneLabel(zone)} time` : text;
}

// A "*\/N" term only means "every N" when N divides the field evenly. "*\/40"
// in the minute field fires at :00 and :40, with gaps of 40 and 20 minutes,
// so it is described by its positions instead.
function evenStep(field: CronField): number | undefined {
  if (field.terms.length !== 1) return undefined;
  const term = field.terms[0];
  if (!term.all || term.step <= 1) return undefined;
  const span = CRON_FIELDS.find((spec) => spec.name === field.name);
  if (!span || (span.max - span.min + 1) % term.step !== 0) return undefined;
  return term.step;
}

function describeTime(minute: CronField, hour: CronField): string {
  const minuteValues = values(minute);
  const hourValues = values(hour);
  const minuteStep = evenStep(minute);
  const hourStep = evenStep(hour);

  if (minute.any && hour.any) return 'every minute';
  if (minuteStep && hour.any) return `every ${minuteStep} minutes`;
  if (minuteStep) return `every ${minuteStep} minutes ${describeHourWindow(hour)}`;
  if (minute.any) return `every minute ${describeHourWindow(hour)}`;

  if (minuteValues.length === 1) {
    const mm = pad(minuteValues[0]);
    if (hour.any) return `at :${mm} past every hour`;
    if (hourStep) return `at :${mm} past every ${ordinalStep(hourStep)} hour`;
    if (hourValues.length <= 6) return `at ${joinWords(hourValues.map((h) => `${pad(h)}:${mm}`))}`;
    return `at :${mm} past ${describeHourWindow(hour)}`;
  }

  if (hour.any && minuteValues.length <= 4) {
    return `at ${joinWords(minuteValues.map((m) => `:${pad(m)}`))} past every hour`;
  }
  if (hourValues.length === 1 && minuteValues.length <= 4) {
    return `at ${joinWords(minuteValues.map((m) => `${pad(hourValues[0])}:${pad(m)}`))}`;
  }
  return `at minute ${describeGeneric(minute)} of hour ${describeGeneric(hour)}`;
}

function describeHourWindow(hour: CronField): string {
  if (hour.any) return 'every hour';
  const step = evenStep(hour);
  if (step) return `every ${ordinalStep(step)} hour`;
  if (hour.terms.length === 1) {
    const term = hour.terms[0];
    if (term.step === 1 && term.start !== term.end) return `from ${pad(term.start)}:00 to ${pad(term.end)}:59`;
    if (term.step === 1) return `during the ${pad(term.start)}:00 hour`;
  }
  return `during hours ${describeGeneric(hour)}`;
}

function describeDays(day: CronField, month: CronField, weekday: CronField): string {
  const dayPart = day.any ? '' : describeDayOfMonth(day);
  const weekdayPart = weekday.any ? '' : describeWeekday(weekday);
  const monthPart = month.any ? '' : describeMonth(month);

  let result = 'every day';
  if (dayPart && weekdayPart) result = `${dayPart} or ${weekdayPart}`;
  else if (dayPart) result = dayPart;
  else if (weekdayPart) result = weekdayPart;
  return monthPart ? `${result} ${monthPart}` : result;
}

function describeDayOfMonth(day: CronField): string {
  const step = evenStep(day);
  if (step) return `every ${ordinalStep(step)} day of the month`;
  // 31 days never divide evenly, but "*\/2" is exactly the odd days.
  if (day.terms.length === 1 && day.terms[0].all && day.terms[0].step === 2) return 'on odd days of the month';
  const list = values(day);
  if (list.length <= 4) return `on the ${joinWords(list.map(ordinal))} of the month`;
  return `on days ${describeGeneric(day)} of the month`;
}

function describeWeekday(weekday: CronField): string {
  const step = evenStep(weekday);
  if (step) return `every ${ordinalStep(step)} day of the week`;
  if (weekday.terms.length === 1) {
    const term = weekday.terms[0];
    if (term.step === 1 && term.start === 1 && term.end === 5) return 'on weekdays';
    if (term.step === 1 && term.start !== term.end) return `${WEEKDAYS[term.start]} to ${WEEKDAYS[term.end]}`;
  }
  const list = values(weekday);
  if (list.length === 1) return `on ${WEEKDAYS[list[0]]}s`;
  if (list.length === 2 && list[0] === 0 && list[1] === 6) return 'on weekends';
  if (list.length <= 4) return `on ${joinWords(list.map((d) => WEEKDAYS[d]))}`;
  return `on weekdays ${describeGeneric(weekday)}`;
}

function describeMonth(month: CronField): string {
  const step = evenStep(month);
  if (step) return `every ${ordinalStep(step)} month`;
  if (month.terms.length === 1) {
    const term = month.terms[0];
    if (term.step === 1 && term.start !== term.end) return `${MONTHS[term.start - 1]} to ${MONTHS[term.end - 1]}`;
  }
  const list = values(month);
  if (list.length <= 4) return `in ${joinWords(list.map((m) => MONTHS[m - 1]))}`;
  return `in months ${describeGeneric(month)}`;
}

function describeGeneric(field: CronField): string {
  return field.terms
    .map((term) => {
      const base = term.all ? '*' : term.start === term.end ? String(term.start) : `${term.start}-${term.end}`;
      return term.step > 1 ? `${base}/${term.step}` : base;
    })
    .join(', ');
}

function values(field: CronField): number[] {
  const set = new Set<number>();
  for (const term of field.terms) {
    for (let value = term.start; value <= term.end; value += term.step) set.add(value);
  }
  return [...set].sort((a, b) => a - b);
}

function ordinalStep(step: number): string {
  return step === 2 ? 'other' : ordinal(step);
}

function ordinal(value: number): string {
  const tens = value % 100;
  if (tens >= 11 && tens <= 13) return `${value}th`;
  switch (value % 10) {
    case 1:
      return `${value}st`;
    case 2:
      return `${value}nd`;
    case 3:
      return `${value}rd`;
    default:
      return `${value}th`;
  }
}

function pad(value: number): string {
  return String(value).padStart(2, '0');
}

function joinWords(items: string[]): string {
  if (items.length <= 1) return items.join('');
  if (items.length === 2) return `${items[0]} and ${items[1]}`;
  return `${items.slice(0, -1).join(', ')}, and ${items.at(-1)}`;
}

function capitalize(text: string): string {
  return text.charAt(0).toUpperCase() + text.slice(1);
}
