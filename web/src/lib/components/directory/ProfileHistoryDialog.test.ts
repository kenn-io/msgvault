import { render, screen } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../../api/client';
import ProfileHistoryDialog from './ProfileHistoryDialog.svelte';

const when = '2026-08-01T00:00:00Z';
const currentEnvelope = { ordinal: 0, source: 'user', active_from: when, created_at: when, updated_at: when, vcard: {} };

function person() {
  return { id: 7, revision: 3, display_name: 'Test User', participant_ids: [], vcard_uid: 'person-7', created_at: when, updated_at: when };
}

describe('ProfileHistoryDialog', () => {
  it('retains current structured provenance alongside superseded rows and observations', async () => {
    const requests: Request[] = [];
    const client = createAPIClient(vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      if (new URL(request.url).pathname.endsWith('/merges')) return Response.json({ merges: [] });
      return Response.json({
        person: person(),
        names: [
          { person_id: 7, name_kind: 'formatted', original_value: 'Current Name', is_derived: false, envelope: { id: 2, ordinal: 0, source: 'user', active_from: when, created_at: when, updated_at: when, vcard: {} } },
          { person_id: 7, name_kind: 'formatted', original_value: 'Previous Name', is_derived: false, envelope: { id: 1, ordinal: 0, source: 'user', created_at: when, updated_at: when, superseded_at: '2026-08-02T00:00:00Z', vcard: {} } }
        ],
        contact_points: [...['workspace-a', 'workspace-b'].map((scope, index) => ({
          person_id: 7, address_kind: 'username', original_value: 'alice', normalized_value: 'alice', normalization: 'casefold', normalization_version: 1,
          service_slug: 'slack', scope_kind: 'workspace', scope_value: scope,
          envelope: { id: 20 + index, ordinal: index, source: 'user', created_at: when, updated_at: when, superseded_at: '2026-08-02T00:00:00Z', vcard: {} }
        })), { person_id: 7, address_kind: 'email', original_value: 'current@example.com', normalized_value: 'current@example.com', normalization: 'email', normalization_version: 1, envelope: { ...currentEnvelope, id: 30 } }],
        addresses: [{ person_id: 7, address_kind: 'postal', original_value: 'Current address', envelope: { ...currentEnvelope, id: 31 } }],
        dates: [{ person_id: 7, date_kind: 'birthday', date: { month: 2, day: 14 }, date_text: 'February 14', original_value: '--02-14', envelope: { ...currentEnvelope, id: 32 } }],
        categories: [{ person_id: 7, normalized_value: 'current category', original_value: 'Current category', envelope: { ...currentEnvelope, id: 33 } }, {
          person_id: 7, original_value: 'Past category', normalized_value: 'past category',
          envelope: { id: 8, ordinal: 0, source: 'user', active_until: '2026-07-31T00:00:00Z', created_at: when, updated_at: when, vcard: {} }
        }],
        media: [{ person_id: 7, media_kind: 'photo', has_data: false, original_value: 'Current photo', envelope: { ...currentEnvelope, id: 34 } }],
        observations: [{
          participant_id: 70, address_kind: 'email', original_value: 'observed@example.test',
          normalized_value: 'observed@example.test', normalization: 'email', normalization_version: 1,
          service_slug: 'email', observed_at: '2026-07-31T12:00:00Z',
          envelope: { id: 9, ordinal: 0, source: 'archive_observation', created_at: when, updated_at: when, vcard: {} }
        }]
      });
    }));

    render(ProfileHistoryDialog, { client, personID: 7, onClose: vi.fn() });

    expect(await screen.findByText('Previous Name')).toBeDefined();
    for (const title of ['Names', 'Contact points', 'Addresses', 'Dates', 'Categories', 'Media', 'Seen in services']) {
      expect(screen.getByRole('heading', { name: title })).toBeDefined();
    }
    expect(screen.queryByRole('heading', { name: 'Service observations' })).toBeNull();
    expect(screen.queryByRole('heading', { name: 'Media metadata' })).toBeNull();
    const dateText = '(Jul|Aug) \\d{1,2}, 2026';
    for (const value of ['Current Name', 'current@example.com', 'Current address', 'Birthday: February 14', 'Current category', 'Current photo']) {
      const row = screen.getByText(value).closest('li')!;
      expect(row.closest('details')).toBeNull();
      expect(row.textContent).toMatch(new RegExp(`You · ${dateText}`));
      expect(row.textContent).not.toMatch(/Source:|Updated:|Valid from:|\d{4}-\d{2}-\d{2}T/);
    }
    const summaries = screen.getAllByText(/earlier values?$/).map((node) => node.textContent);
    expect(summaries).toEqual(['1 earlier value', '2 earlier values', '1 earlier value']);
    for (const value of ['Previous Name', 'Past category']) {
      const row = screen.getByText(value).closest('li')!;
      expect(row.closest('details')).not.toBeNull();
      expect(row.textContent).toMatch(new RegExp(`Removed ${dateText}`));
    }
    expect(screen.getAllByText(new RegExp(`^Removed ${dateText}`))).toHaveLength(4);
    const observed = screen.getByText('observed@example.test').closest('li')!;
    expect(observed.textContent).toMatch(/Archive observation · Jul 31, 2026/);
    expect(screen.getByText('slack · workspace / workspace-a')).toBeDefined();
    expect(screen.getByText('slack · workspace / workspace-b')).toBeDefined();
    expect(screen.getByText('email')).toBeDefined();
    expect(document.body.textContent).not.toMatch(/Service:|Scope:|Superseded:|Observed:/);
    expect(requests.map((request) => new URL(request.url).pathname)).toContain('/api/v1/people/7/profile/history');
  });

  it('keeps the dialog open and reports a failed history read', async () => {
    const client = createAPIClient(vi.fn<typeof fetch>(async input => {
      const request = input instanceof Request ? input : new Request(input);
      if (new URL(request.url).pathname.endsWith('/merges')) return Response.json({ merges: [] });
      return Response.json({ error: 'unavailable', message: 'history unavailable' }, { status: 503 });
    }));

    render(ProfileHistoryDialog, { client, personID: 7, onClose: vi.fn() });

    expect((await screen.findByRole('alert')).textContent).toBe('history unavailable');
    expect(screen.getByRole('dialog', { name: 'History' })).toBeDefined();
  });
});
