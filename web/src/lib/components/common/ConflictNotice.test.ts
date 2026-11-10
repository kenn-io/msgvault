import { render, screen } from '@testing-library/svelte';
import { expect, it } from 'vitest';
import ConflictNotice from './ConflictNotice.svelte';

it.each([
  ['user', /^Saved at /],
  ['carddav_import', /^Saved from CardDAV contacts at /],
] as const)('shows the source for an update by %s', (updatedBy, expected) => {
  render(ConflictNotice, {
    noun: 'connection',
    record: {},
    saved: { updated_at: new Date().toISOString(), source: 'carddav_import', updated_by: updatedBy },
    message: 'Changed elsewhere'
  });
  expect(screen.getByText(expected)).toBeDefined();
});
