import { fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';

import type { PersonSummary } from '../../explore/models';
import type { LinkOutcome } from '../../relationships/controller.svelte';
import PersonAddresses from './PersonAddresses.svelte';

const when = '2026-07-19T10:00:00Z';

function person(): PersonSummary {
  return {
    id: 12, display_label: 'Alice Example', partial_label: false,
    identifiers: [
      { type: 'email', value: 'alice@example.com', display_value: 'Alice', participant_id: 12, is_primary: true, provenance: 'participant_identifiers' },
      { type: 'phone', value: '+15550100001', participant_id: 12, is_primary: false, provenance: 'participant_identifiers' }
    ],
    activity_count: 42, meeting_count: 0, file_count: 3,
    current_relationship_temperature: 62, peak_relationship_temperature: 87, peak_relationship_year: 2018,
    source_counts: [{ source_type: 'gmail', count: 42 }],
    first_at: when, last_at: when, cache_revision: 'cache-rel'
  };
}

/** A linked cluster spanning three participants in a chain (12–34–56, not a
 * star): 34 is a cut vertex, so detaching it must remove both incident
 * edges, not just one. */
function clusteredPerson(): PersonSummary {
  return {
    id: 12, display_label: 'Alice Example', partial_label: false,
    identifiers: [
      { type: 'email', value: 'alice@example.com', display_value: 'Alice', participant_id: 12, is_primary: true, provenance: 'participant_identifiers' },
      { type: 'phone', value: '+15550100002', participant_id: 34, is_primary: true, provenance: 'participant_identifiers' },
      { type: 'email', value: 'carol@example.com', participant_id: 56, is_primary: true, provenance: 'participant_identifiers' }
    ],
    activity_count: 42, meeting_count: 0, file_count: 3,
    current_relationship_temperature: 62, peak_relationship_temperature: 87, peak_relationship_year: 2018,
    source_counts: [{ source_type: 'gmail', count: 42 }],
    first_at: when, last_at: when, cache_revision: 'cache-rel',
    cluster: {
      canonical_id: 12, member_ids: [12, 34, 56],
      edges: [{ participant_a: 12, participant_b: 34 }, { participant_a: 34, participant_b: 56 }]
    }
  };
}

/** A cluster where member 78 was joined by a manual participant link with
 * no stored identifier evidence at all (78 has no row in `identifiers`),
 * unlike 34/56 above which each have their own identifier row. */
function clusteredPersonWithBareMember(): PersonSummary {
  const base = clusteredPerson();
  return {
    ...base,
    cluster: {
      canonical_id: 12, member_ids: [12, 34, 56, 78],
      edges: [...(base.cluster?.edges ?? []), { participant_a: 12, participant_b: 78 }]
    }
  };
}

function baseProps(overrides: Record<string, unknown> = {}) {
  return {
    detail: person(),
    onUnlinkParticipants: vi.fn(async (): Promise<LinkOutcome> => ({ ok: true, identityRevision: 3, cacheState: 'ready' })),
    ...overrides
  };
}

describe('PersonAddresses', () => {
  it('offers unlink only for another clustered member', async () => {
    const view = render(PersonAddresses, baseProps({ detail: clusteredPerson() }));
    expect(screen.getByLabelText('Identity Alice').textContent).toContain('this profile');
    expect(screen.queryByRole('button', { name: /Unlink alice@example.com|Unlink Alice/ })).toBeNull();
    expect(screen.getByRole('button', { name: 'Unlink +15550100002' })).toBeDefined();
    expect(screen.getByRole('button', { name: 'Unlink carol@example.com' })).toBeDefined();
    await view.rerender(baseProps());
    expect(screen.queryByRole('button', { name: /Unlink/ })).toBeNull();
  });

  it.each(['none', 'email', 'duplicate names'])('names member-only controls with %s evidence', async evidence => {
    const base = clusteredPersonWithBareMember();
    const onUnlinkParticipants = vi.fn(async (): Promise<LinkOutcome> => ({ ok: true, identityRevision: 4, cacheState: 'ready' }));
    const members = evidence === 'email' ? [{ participant_id: 78, email: 'bare@example.com' }] : evidence === 'duplicate names' ? [{ participant_id: 56, display_name: 'Shared Example' }, { participant_id: 78, display_name: 'Shared Example' }] : [];
    render(PersonAddresses, baseProps({ detail: { ...base, identifiers: evidence === 'duplicate names' ? base.identifiers.filter(identifier => identifier.participant_id !== 56) : base.identifiers, cluster: { ...base.cluster!, members } }, onUnlinkParticipants }));
    if (evidence === 'duplicate names') {
      expect(screen.getByRole('button', { name: 'Unlink profile Shared Example (56)' })).toBeDefined();
      expect(screen.getByRole('button', { name: 'Unlink profile Shared Example (78)' })).toBeDefined();
    } else {
      const name = evidence === 'email' ? 'bare@example.com' : '78';
      await fireEvent.click(screen.getByRole('button', { name: `Unlink profile ${name}` }));
      expect(screen.getByRole('group', { name: `Confirm unlinking profile ${name}` })).toBeDefined();
      if (evidence === 'none') {
        await fireEvent.click(screen.getByRole('button', { name: 'Unlink' }));
        await waitFor(() => expect(onUnlinkParticipants).toHaveBeenCalledWith(12, 78));
      }
    }
  });

  it('distinguishes opaque identity controls even when service and member names match', async () => {
    const writeText = vi.fn(async () => undefined);
    const originalClipboard = navigator.clipboard;
    Object.defineProperty(navigator, 'clipboard', { configurable: true, value: { writeText } });
    try {
      const base = clusteredPerson();
      render(PersonAddresses, baseProps({ detail: {
        ...base,
        identifiers: [34, 56].map((id) => ({
          type: 'beeper', value: `beeper-user-${id}`, participant_id: id,
          is_primary: true, provenance: 'participant_identifiers',
          service_label: 'WhatsApp', participant_display_name: 'Alias Example', scope_kind: 'account', scope_value: 'local-whatsapp_ba_example'
        }))
      } }));

      const copy = screen.getByRole('button', { name: 'Copy WhatsApp identifier for Alias Example (profile 34)' });
      const chip = copy.closest('.detail-row');
      expect(chip?.textContent).toContain('WhatsApp');
      expect(chip?.textContent).toContain('local-whatsapp_ba_example');
      expect(chip?.querySelector('summary')?.textContent).not.toContain('beeper-user-34');
      await fireEvent.click(copy);
      await waitFor(() => expect(writeText).toHaveBeenCalledWith('beeper-user-34'));
      expect(screen.getByRole('button', { name: 'Copy WhatsApp identifier for Alias Example (profile 56)' })).toBeDefined();
      expect(screen.getByRole('button', { name: 'Unlink WhatsApp identifier for Alias Example (profile 34)' })).toBeDefined();
      await fireEvent.click(screen.getByRole('button', { name: 'Unlink WhatsApp identifier for Alias Example (profile 56)' }));
      expect(screen.getByRole('group', { name: 'Confirm unlinking WhatsApp identifier for Alias Example (profile 56)' })).toBeDefined();
    } finally { Object.defineProperty(navigator, 'clipboard', { configurable: true, value: originalClipboard }); }
  });

  it('confirming a cut-vertex member\'s unlink removes every edge incident to it, not just one', async () => {
    const onUnlinkParticipants = vi.fn(async (): Promise<LinkOutcome> => ({ ok: true, identityRevision: 4, cacheState: 'ready' }));
    render(PersonAddresses, baseProps({ detail: clusteredPerson(), onUnlinkParticipants }));

    await fireEvent.click(screen.getByRole('button', { name: 'Unlink +15550100002' }));
    expect(screen.getByRole('group', { name: 'Confirm unlinking +15550100002' })).toBeDefined();
    await fireEvent.click(screen.getByRole('button', { name: 'Unlink' }));

    await waitFor(() => expect(onUnlinkParticipants).toHaveBeenCalledTimes(2));
    expect(onUnlinkParticipants).toHaveBeenCalledWith(12, 34);
    expect(onUnlinkParticipants).toHaveBeenCalledWith(34, 56);
    expect(screen.queryByRole('group', { name: 'Confirm unlinking +15550100002' })).toBeNull();
    expect(screen.queryByRole('alert')).toBeNull();
  });

  it('cancelling an unlink confirm leaves the chip untouched and calls nothing', async () => {
    const onUnlinkParticipants = vi.fn();
    render(PersonAddresses, baseProps({ detail: clusteredPerson(), onUnlinkParticipants }));

    await fireEvent.click(screen.getByRole('button', { name: 'Unlink carol@example.com' }));
    await fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));

    expect(screen.queryByRole('group', { name: 'Confirm unlinking carol@example.com' })).toBeNull();
    expect(screen.getByRole('button', { name: 'Unlink carol@example.com' })).toBeDefined();
    expect(onUnlinkParticipants).not.toHaveBeenCalled();
  });

  it('shows an inline error and keeps the confirm state on an unlink failure', async () => {
    const onUnlinkParticipants = vi.fn(async (): Promise<LinkOutcome> => ({ ok: false, code: 'error', message: 'Request failed (500)' }));
    render(PersonAddresses, baseProps({ detail: clusteredPerson(), onUnlinkParticipants }));

    await fireEvent.click(screen.getByRole('button', { name: 'Unlink +15550100002' }));
    await fireEvent.click(screen.getByRole('button', { name: 'Unlink' }));

    expect((await screen.findByRole('alert')).textContent).toContain('Request failed (500)');
    expect(screen.getByRole('group', { name: 'Confirm unlinking +15550100002' })).toBeDefined();
    expect(onUnlinkParticipants).toHaveBeenCalledTimes(1);
  });

  it.each(['failure', 'stale-cache banner'])('finishes captured unlinks without showing a late %s on the next person', async outcome => {
    let release!: (result: LinkOutcome) => void;
    let calls = 0;
    const onUnlinkParticipants = vi.fn((): Promise<LinkOutcome> => {
      calls++;
      if (calls === (outcome === 'failure' ? 2 : 1)) return new Promise(resolve => { release = resolve; });
      return Promise.resolve({ ok: true, identityRevision: 4, cacheState: outcome === 'failure' ? 'ready' : 'stale' });
    });
    const { rerender } = render(PersonAddresses, baseProps({ detail: clusteredPerson(), onUnlinkParticipants }));
    await fireEvent.click(screen.getByRole('button', { name: 'Unlink +15550100002' }));
    await fireEvent.click(screen.getByRole('button', { name: 'Unlink' }));
    await waitFor(() => expect(onUnlinkParticipants).toHaveBeenCalledTimes(outcome === 'failure' ? 2 : 1));
    await rerender(baseProps({ detail: { ...person(), id: 200, identifiers: [{ type: 'email', value: 'next@example.com', participant_id: 200, is_primary: true, provenance: 'participant_identifiers' }], cluster: { canonical_id: 200, member_ids: [200, 201], edges: [{ participant_a: 200, participant_b: 201 }] } }, onUnlinkParticipants }));
    expect(screen.queryByRole('group', { name: /Confirm unlinking/ })).toBeNull();
    release(outcome === 'failure' ? { ok: false, code: 'error', message: 'Request failed (500)' } : { ok: true, identityRevision: 3, cacheState: 'ready' });
    await waitFor(() => expect(onUnlinkParticipants).toHaveBeenCalledWith(34, 56));
    await new Promise(resolve => setTimeout(resolve, 0));
    expect(screen.queryByRole('alert')).toBeNull();
  });
});
