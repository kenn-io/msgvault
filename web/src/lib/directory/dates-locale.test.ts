import { expect, it, vi } from 'vitest';

it('reads dates in the browser locale', async () => {
  const DateTimeFormat = Intl.DateTimeFormat;
  const formatter = vi.spyOn(Intl, 'DateTimeFormat').mockImplementation(function (locale, options) {
    return new DateTimeFormat(locale ?? 'fr-FR', options);
  });
  try {
    const { readablePartialDate, spokenDate } = await import('./dates');
    expect(readablePartialDate('2024-03')).toBe('mars 2024');
    expect(spokenDate('2026-03-05', new Date(2026, 10, 7))).toBe('mars 5');
  } finally { formatter.mockRestore(); }
});
