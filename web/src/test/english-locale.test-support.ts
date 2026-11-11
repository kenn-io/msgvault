import { afterAll, vi } from 'vitest';

const formatter = vi.hoisted(() => {
  const DateTimeFormat = Intl.DateTimeFormat;
  return vi.spyOn(Intl, 'DateTimeFormat').mockImplementation(function (locale, options) { return new DateTimeFormat(locale ?? 'en-US', options); });
});
afterAll(() => formatter.mockRestore());
