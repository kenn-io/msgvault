export function isLocalBound(value: string | undefined): boolean {
  return Boolean(value && /[+-]\d\d:\d\d$/.test(value));
}

export function localBoundary(date: Date): string {
  const pad = (value: number) => String(value).padStart(2, '0');
  const offset = -date.getTimezoneOffset();
  const sign = offset < 0 ? '-' : '+';
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}T${pad(date.getHours())}:${pad(date.getMinutes())}:${pad(date.getSeconds())}${sign}${pad(Math.floor(Math.abs(offset) / 60))}:${pad(Math.abs(offset) % 60)}`;
}

export function boundaryDate(value: string, before: boolean): string {
  const date = isLocalBound(value) ? new Date(value.replace(/[+-]\d\d:\d\d$/, 'Z')) : new Date(value);
  if (Number.isNaN(date.getTime())) return '';
  if (before) date.setUTCMilliseconds(date.getUTCMilliseconds() - 1);
  return date.toISOString().slice(0, 10);
}

export function editedBoundary(value: string, before: boolean, local: boolean): string {
  const [year, month, day] = value.split('-').map(Number) as [number, number, number];
  const date = local ? new Date(year, month - 1, day + Number(before)) : new Date(Date.UTC(year, month - 1, day + Number(before)));
  return local ? localBoundary(date) : date.toISOString();
}
