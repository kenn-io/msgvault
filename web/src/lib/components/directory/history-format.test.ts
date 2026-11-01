import { expect, it } from 'vitest';
import { historySource } from './history-format';

it('labels CardDAV imports as Address book', () => {
  expect(historySource('carddav_import')).toBe('Address book');
});
