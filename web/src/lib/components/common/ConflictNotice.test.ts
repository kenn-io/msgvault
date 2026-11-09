import { render, screen } from '@testing-library/svelte';
import { expect, it } from 'vitest';
import ConflictNotice from './ConflictNotice.svelte';

it('credits the latest editor instead of an imported source', () => {
  render(ConflictNotice, {
    noun: 'connection',
    record: {},
    saved: { updated_at: new Date().toISOString(), source: 'carddav_import', updated_by: 'user' },
    message: 'Changed elsewhere'
  });
  expect(screen.getByText(/^Saved at /)).toBeDefined();
  expect(screen.queryByText(/Saved from CardDAV contacts/)).toBeNull();
});

it('shows only the save time when the record does not identify its last writer', () => {
  render(ConflictNotice, {
    noun: 'job',
    record: {},
    saved: { updated_at: new Date().toISOString(), source: 'carddav_import' },
    message: 'Changed elsewhere'
  });
  expect(screen.getByText(/^Saved at /)).toBeDefined();
  expect(screen.queryByText(/Saved from CardDAV contacts/)).toBeNull();
});
