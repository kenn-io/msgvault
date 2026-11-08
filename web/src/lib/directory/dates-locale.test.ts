import { expect, it, vi } from 'vitest';

it('reads dates in the browser locale', async () => {
  const DateTimeFormat = Intl.DateTimeFormat;
  const formatter = vi.spyOn(Intl, 'DateTimeFormat').mockImplementation(function (locale, options) {
    return new DateTimeFormat(locale ?? 'fr-FR', options);
  });
  try {
    const { readablePartialDate, dateNote } = await import('./dates');
    expect(readablePartialDate('2024-03')).toBe('mars 2024');
    expect(readablePartialDate('2024-03-05', new Date(2026, 10, 7))).toBe('5 mars 2024');
    expect(dateNote('2026-03-05', false, new Date(2026, 10, 7))).toBe('5 mars');
  } finally { formatter.mockRestore(); }
  // A fresh import loads kit-ui again, which outlasts the default 5s under a full parallel run.
}, 20_000);
