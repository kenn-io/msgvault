import { render, screen } from '@testing-library/svelte';
import { expect, it } from 'vitest';
import ConflictNotice from './ConflictNotice.svelte';

it('credits the latest editor instead of an imported source', () => {
  render(ConflictNotice, {
    noun: 'connection',
    record: {},
    saved: { updated_at: '2026-11-06T08:34:00Z', source: 'carddav_import', updated_by: 'user' },
    message: 'Changed elsewhere'
  });
  expect(screen.getByText(/^Saved on /)).toBeDefined();
  expect(screen.queryByText(/Saved from CardDAV contacts/)).toBeNull();
});
