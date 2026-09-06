import { render, screen } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../../api/client';
import ProfileHistoryDialog from './ProfileHistoryDialog.svelte';

const when = '2026-08-01T00:00:00Z';

function person() {
  return { id: 7, revision: 3, display_name: 'Test User', participant_ids: [], vcard_uid: 'person-7', created_at: when, updated_at: when };
}

describe('ProfileHistoryDialog', () => {
  it('loads the generated history endpoint and renders only superseded profile rows plus observations', async () => {
    const requests: Request[] = [];
    const client = createAPIClient(vi.fn<typeof fetch>(async (input) => {
      requests.push(input instanceof Request ? input : new Request(input));
      return Response.json({
        person: person(),
        names: [
          { person_id: 7, name_kind: 'formatted', original_value: 'Current Name', is_derived: false, envelope: { id: 2, ordinal: 0, source: 'user', created_at: when, updated_at: when, vcard: {} } },
          { person_id: 7, name_kind: 'formatted', original_value: 'Previous Name', is_derived: false, envelope: { id: 1, ordinal: 0, source: 'user', created_at: when, updated_at: when, superseded_at: '2026-08-02T00:00:00Z', vcard: {} } }
        ],
        contact_points: ['workspace-a', 'workspace-b'].map((scope, index) => ({
          person_id: 7, address_kind: 'username', original_value: 'alice', normalized_value: 'alice', normalization: 'casefold', normalization_version: 1,
          service_slug: 'slack', scope_kind: 'workspace', scope_value: scope,
          envelope: { id: 20 + index, ordinal: index, source: 'user', created_at: when, updated_at: when, superseded_at: '2026-08-02T00:00:00Z', vcard: {} }
        })), addresses: [], dates: [],
        categories: [{
          person_id: 7, original_value: 'Past category', normalized_value: 'past category',
          envelope: { id: 8, ordinal: 0, source: 'user', active_until: '2026-07-31T00:00:00Z', created_at: when, updated_at: when, vcard: {} }
        }],
        media: [],
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
    expect(screen.queryByText('Current Name')).toBeNull();
    expect(screen.getByText('Past category')).toBeDefined();
    expect(screen.getByText('observed@example.test')).toBeDefined();
    expect(screen.getAllByText(/Source: user/)).toHaveLength(4);
    expect(screen.getAllByText(/Superseded: 2026-08-02T00:00:00Z/)).toHaveLength(3);
    expect(screen.getByText(/Observed: 2026-07-31T12:00:00Z/)).toBeDefined();
    expect(screen.getByText('Service: slack · Scope: workspace / workspace-a')).toBeDefined();
    expect(screen.getByText('Service: slack · Scope: workspace / workspace-b')).toBeDefined();
    expect(screen.getByText('Service: email')).toBeDefined();
    expect(new URL(requests[0]!.url).pathname).toBe('/api/v1/people/7/profile/history');
  });

  it('keeps the dialog open and reports a failed history read', async () => {
    const client = createAPIClient(vi.fn<typeof fetch>(async () => Response.json({
      error: 'unavailable', message: 'history unavailable'
    }, { status: 503 })));

    render(ProfileHistoryDialog, { client, personID: 7, onClose: vi.fn() });

    expect((await screen.findByRole('alert')).textContent).toContain('history unavailable');
    expect(screen.getByRole('dialog', { name: 'Profile history' })).toBeDefined();
  });
});
