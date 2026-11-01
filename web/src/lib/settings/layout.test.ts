import { expect, it } from 'vitest';
import { normalizeCategory } from './layout';

it('opens the People catalog for the people settings alias', () => {
  expect(normalizeCategory('people')).toBe('people_catalog');
});
