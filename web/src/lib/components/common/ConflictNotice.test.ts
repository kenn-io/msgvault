import { render, screen } from '@testing-library/svelte';
import { expect, it } from 'vitest';
import ConflictNotice from './ConflictNotice.svelte';

it.each([
  ['latest editor', 'user'],
  ['importer', 'carddav_import'],
] as const)('shows save time for a %s', (_label, updatedBy) => {
  render(ConflictNotice, {
    noun: 'connection',
    record: {},
    saved: { updated_at: new Date().toISOString(), source: 'carddav_import', ...(updatedBy ? { updated_by: updatedBy } : {}) },
    message: 'Changed elsewhere'
  });
  if (updatedBy === 'carddav_import') expect(screen.getByText(/Saved from CardDAV contacts/)).toBeDefined();
  else {
    expect(screen.getByText(/^Saved at /)).toBeDefined();
    expect(screen.queryByText(/Saved from CardDAV contacts/)).toBeNull();
  }
});
