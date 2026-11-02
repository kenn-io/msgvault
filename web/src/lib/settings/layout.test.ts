import { expect, it } from 'vitest';
import { normalizeCategory } from './layout';

it('opens People lookups for the legacy people settings alias', () => {
  expect(normalizeCategory('people')).toBe('enrichment');
});
