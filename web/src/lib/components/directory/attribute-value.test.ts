import { expect, it } from 'vitest';
import { displayClaimValue } from './attribute-value';

it('describes a null claim value', () => {
  expect(displayClaimValue(null, 'attribute')).toBe('No value');
});

it.each([
  [{ title: 'Engineer' }, 'Engineer'],
  [{ organization: 'Acme' }, 'Acme'],
  [{ organization: 'Acme', role: 'Engineer' }, 'Engineer at Acme'],
  [{ organization: { name: 'Acme' }, role: 'Engineer' }, 'Engineer at Acme'],
])('formats employment claims with either organization shape', (value, expected) => {
  expect(displayClaimValue(value, 'employment')).toBe(expected);
});
