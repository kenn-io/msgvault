import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
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
  it('shows each address and its source once when expanded', async () => {
    render(PersonAddresses, baseProps());
    const email = screen.getByLabelText('Identity Alice');
    await fireEvent.click(within(email).getByText('Alice'));
    expect(email.querySelector('p')?.textContent).toBe('this profile · alice@example.com · email · primary · stored identifier');
    const phone = screen.getByLabelText('Identity +15550100001');
    await fireEvent.click(within(phone).getByText('+15550100001'));
    expect(phone.querySelector('p')?.textContent).toBe('this profile · phone · secondary · stored identifier');
  });

  it('offers unlink only for another clustered member', async () => {
    const view = render(PersonAddresses, baseProps({ detail: clusteredPerson() }));
    expect(screen.getByLabelText('Identity Alice').textContent).toContain('this profile');
    expect(screen.queryByRole('button', { name: /Unlink alice@example.com|Unlink Alice/ })).toBeNull();
    expect(screen.getByRole('button', { name: 'Unlink +15550100002' })).toBeDefined();
    expect(screen.getByRole('button', { name: 'Unlink carol@example.com' })).toBeDefined();
    await view.rerender(baseProps());
    expect(screen.queryByRole('button', { name: /Unlink/ })).toBeNull();
  });

  it.each(['own addresses', 'linked address', 'all addresses', 'normalized phone'])('filters addresses already listed in Details: %s', scenario => {
    const own = person();
    const listed = own.identifiers.map(identifier => ({ address_kind: identifier.type, original_value: identifier.value, normalized_value: identifier.value }));
    let detail = scenario === 'all addresses' ? own : clusteredPerson();
    if (scenario === 'linked address') listed.push({ address_kind: 'phone', original_value: '+1 (555) 010-0002', normalized_value: '+15550100002' });
    if (scenario === 'normalized phone') {
      detail = { ...own, identifiers: [{ type: 'phone', value: '+12025550123', participant_id: 12, is_primary: true, provenance: 'participant_identifiers' }] };
      listed.splice(0, listed.length, { address_kind: 'phone', original_value: '(202) 555-0123', normalized_value: '+12025550123' });
    }
    render(PersonAddresses, baseProps({ detail, listedInDetails: listed }));
    if (scenario === 'own addresses' || scenario === 'linked address') {
      expect(screen.queryByLabelText('Identity Alice')).toBeNull();
      expect(screen.getByRole('button', { name: 'Unlink +15550100002' })).toBeDefined();
      expect(screen.getByRole('button', { name: 'Unlink carol@example.com' })).toBeDefined();
    } else expect(screen.queryByRole('heading', { name: /addresses/i })).toBeNull();
  });

  it.each(['none', 'email', 'duplicate names'])('names member-only controls with %s evidence', evidence => {
    const base = clusteredPersonWithBareMember();
    const members = evidence === 'email' ? [{ participant_id: 78, email: 'bare@example.com' }] : evidence === 'duplicate names' ? [{ participant_id: 56, display_name: 'Shared Example' }, { participant_id: 78, display_name: 'Shared Example' }] : [];
    render(PersonAddresses, baseProps({ detail: { ...base, identifiers: evidence === 'duplicate names' ? base.identifiers.filter(identifier => identifier.participant_id !== 56) : base.identifiers, cluster: { ...base.cluster!, members } } }));
    if (evidence === 'duplicate names') {
      expect(screen.getByRole('button', { name: 'Unlink profile Shared Example (56)' })).toBeDefined();
      expect(screen.getByRole('button', { name: 'Unlink profile Shared Example (78)' })).toBeDefined();
    } else {
      const name = evidence === 'email' ? 'bare@example.com' : '78';
      expect(screen.getByRole('button', { name: `Unlink profile ${name}` })).toBeDefined();
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
      expect(within(chip as HTMLElement).getAllByText('WhatsApp')).toHaveLength(1);
      await fireEvent.click(within(chip as HTMLElement).getByText('WhatsApp'));
      expect(chip?.querySelector('p')?.textContent).toBe('Alias Example · linked · beeper-user-34 · account: local-whatsapp_ba_example · beeper · primary · stored identifier');
      expect(chip?.querySelector('summary')?.textContent).not.toContain('beeper-user-34');
      await fireEvent.click(copy);
      await waitFor(() => expect(writeText).toHaveBeenCalledWith('beeper-user-34'));
      expect(screen.getByRole('button', { name: 'Copy WhatsApp identifier for Alias Example (profile 56)' })).toBeDefined();
      expect(screen.getByRole('button', { name: 'Unlink WhatsApp identifier for Alias Example (profile 34)' })).toBeDefined();
      await fireEvent.click(screen.getByRole('button', { name: 'Unlink WhatsApp identifier for Alias Example (profile 56)' }));
      expect(screen.getByRole('group', { name: 'Confirm unlinking WhatsApp identifier for Alias Example (profile 56)' })).toBeDefined();
    } finally { Object.defineProperty(navigator, 'clipboard', { configurable: true, value: originalClipboard }); }
  });

  it.each(['success on cut vertex', 'cancel', 'failure then stale and retry'])('handles unlink confirmation: %s', async outcome => {
    const onUnlinkParticipants = vi.fn<(...ids: number[]) => Promise<LinkOutcome>>()
      .mockResolvedValue({ ok: true, identityRevision: 4, cacheState: 'ready' });
    if (outcome === 'failure then stale and retry') onUnlinkParticipants
      .mockResolvedValueOnce({ ok: false, code: 'error', message: 'Request failed (500)' })
      .mockResolvedValueOnce({ ok: true, identityRevision: 4, cacheState: 'stale' });
    render(PersonAddresses, baseProps({ detail: clusteredPerson(), onUnlinkParticipants }));
    const name = outcome === 'success on cut vertex' ? '+15550100002' : 'carol@example.com';
    await fireEvent.click(screen.getByRole('button', { name: `Unlink ${name}` }));
    expect(screen.getByRole('group', { name: `Confirm unlinking ${name}` })).toBeDefined();
    await fireEvent.click(screen.getByRole('button', { name: outcome === 'cancel' ? 'Cancel' : 'Unlink' }));

    if (outcome === 'cancel') {
      expect(screen.queryByRole('group', { name: `Confirm unlinking ${name}` })).toBeNull();
      expect(screen.getByRole('button', { name: `Unlink ${name}` })).toBeDefined();
      expect(onUnlinkParticipants).not.toHaveBeenCalled();
    } else if (outcome === 'success on cut vertex') {
      await waitFor(() => expect(onUnlinkParticipants).toHaveBeenCalledTimes(2));
      expect(onUnlinkParticipants).toHaveBeenCalledWith(12, 34);
      expect(onUnlinkParticipants).toHaveBeenCalledWith(34, 56);
      expect(screen.queryByRole('group', { name: `Confirm unlinking ${name}` })).toBeNull();
      expect(screen.queryByRole('alert')).toBeNull();
    } else {
      expect((await screen.findByRole('alert')).textContent).toContain('Request failed (500)');
      expect(screen.getByRole('group', { name: `Confirm unlinking ${name}` })).toBeDefined();
      expect(onUnlinkParticipants).toHaveBeenCalledTimes(1);
      await fireEvent.click(screen.getByRole('button', { name: 'Unlink' }));
      expect((await screen.findByRole('alert')).textContent).toContain('Identity saved; the cache refresh failed');
      await fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
      await waitFor(() => expect(screen.queryByRole('alert')).toBeNull());
      expect(onUnlinkParticipants.mock.calls).toEqual([[34, 56], [34, 56], [34, 56]]);
    }
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
    await act(async () => { await onUnlinkParticipants.mock.results[1]!.value; });
    expect(screen.queryByRole('alert')).toBeNull();
  });
});
