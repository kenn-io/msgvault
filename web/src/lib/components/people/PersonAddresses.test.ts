import { fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import { tick } from 'svelte';
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
  it('shows an unlink × only on chips for other cluster members, never on the open cluster\'s own identifiers', () => {
    render(PersonAddresses, baseProps({ detail: clusteredPerson() }));

    expect(screen.queryByRole('button', { name: /Unlink alice@example.com|Unlink Alice/ })).toBeNull();
    expect(screen.getByRole('button', { name: 'Unlink +15550100002' })).toBeDefined();
    expect(screen.getByRole('button', { name: 'Unlink carol@example.com' })).toBeDefined();
  });

  it('does not show unlink controls when the person has no cluster', () => {
    render(PersonAddresses, baseProps());
    expect(screen.queryByRole('button', { name: /^Unlink / })).toBeNull();
  });

  it('renders a fallback chip with its own detach control for a cluster member with no identifier rows', async () => {
    const onUnlinkParticipants = vi.fn(async (): Promise<LinkOutcome> => ({ ok: true, identityRevision: 4, cacheState: 'ready' }));
    render(PersonAddresses, baseProps({ detail: clusteredPersonWithBareMember(), onUnlinkParticipants }));

    expect(screen.getByLabelText('Linked profile 78').textContent).toContain('no stored address');
    const detachButton = screen.getByRole('button', { name: 'Unlink profile 78' });
    await fireEvent.click(detachButton);
    await fireEvent.click(screen.getByRole('button', { name: 'Unlink' }));

    await waitFor(() => expect(onUnlinkParticipants).toHaveBeenCalledWith(12, 78));
  });

  it('names a linked member chip and its controls by its visible address', async () => {
    const base = clusteredPersonWithBareMember();
    render(PersonAddresses, baseProps({ detail: {
      ...base,
      cluster: { ...base.cluster!, members: [{ participant_id: 78, email: 'bare@example.com' }] }
    } }));

    const chip = screen.getByLabelText('Linked profile bare@example.com');
    expect(chip.textContent).toContain('bare@example.com');
    expect(chip.textContent).not.toContain('no stored address');
    await fireEvent.click(screen.getByRole('button', { name: 'Unlink profile bare@example.com' }));
    expect(screen.getByRole('group', { name: 'Confirm unlinking profile bare@example.com' })).toBeDefined();
  });

  it('distinguishes member-only controls with identical display names', () => {
    const base = clusteredPersonWithBareMember();
    render(PersonAddresses, baseProps({ detail: {
      ...base,
      identifiers: base.identifiers.filter((identifier) => identifier.participant_id !== 56),
      cluster: { ...base.cluster!, members: [
        { participant_id: 56, display_name: 'Shared Example' },
        { participant_id: 78, display_name: 'Shared Example' }
      ] }
    } }));

    expect(screen.getByRole('button', { name: 'Unlink profile Shared Example (56)' })).toBeDefined();
    expect(screen.getByRole('button', { name: 'Unlink profile Shared Example (78)' })).toBeDefined();
  });

  it('hides an opaque key on the chip and copies the full value', async () => {
    let copied = '';
    const originalClipboard = navigator.clipboard;
    const originalExecCommand = document.execCommand;
    Object.defineProperty(navigator, 'clipboard', { configurable: true, value: undefined });
    Object.defineProperty(document, 'execCommand', { configurable: true, value: vi.fn(() => {
      copied = document.querySelector('textarea')?.value ?? '';
      return true;
    }) });
    try {
      const key = 'beeper:8:whatsapp:9:@user:x.y';
      const base = person();
      render(PersonAddresses, baseProps({ detail: {
        ...base,
        identifiers: [...base.identifiers, {
          type: 'beeper', value: key, display_value: key, is_primary: false,
          participant_id: 12, provenance: 'participant_identifiers', service_label: 'WhatsApp',
          participant_display_name: 'Alias Example', scope_kind: 'account', scope_value: 'local-whatsapp_ba_example'
        }]
      } }));

      const copy = screen.getByRole('button', { name: 'Copy WhatsApp identifier for Alias Example (profile 12)' });
      const chip = copy.closest('.detail-row');
      expect(chip?.textContent).toContain('WhatsApp');
      expect(chip?.textContent).toContain('local-whatsapp_ba_example');



      await fireEvent.click(copy);
      await waitFor(() => expect(copied).toBe(key));
    } finally {
      Object.defineProperty(navigator, 'clipboard', { configurable: true, value: originalClipboard });
      Object.defineProperty(document, 'execCommand', { configurable: true, value: originalExecCommand });
    }
  });

  it('shows every distinct origin incident to a linked member', () => {
    const base = clusteredPerson();
    render(PersonAddresses, baseProps({ detail: {
      ...base,
      cluster: { ...base.cluster!, edges: [
        { participant_a: 12, participant_b: 34, link_origin: { kind: 'manual' } },
        { participant_a: 34, participant_b: 56,
          link_origin: { kind: 'candidate', source: 'carddav_import', basis: 'email' } }
      ] }
    } }));

    const chip = screen.getByLabelText('Identity +15550100002');
    expect(chip.textContent).toContain('linked manually');
    expect(chip.textContent).toContain('matched from CardDAV (email)');
  });

  it('does not describe other links on the viewed profile’s own identifier', () => {
    const base = clusteredPerson();
    render(PersonAddresses, baseProps({ detail: {
      ...base,
      cluster: { ...base.cluster!, edges: [
        { participant_a: 12, participant_b: 34, link_origin: { kind: 'manual' } }
      ] }
    } }));

    expect(screen.getByLabelText('Identity Alice').textContent).toContain('this profile');
    expect(screen.getByLabelText('Identity Alice').textContent).not.toContain('linked manually');
    expect(screen.getByLabelText('Identity +15550100002').textContent).toContain('linked manually');
  });

  it('distinguishes opaque identity controls even when service and member names match', async () => {
    const base = clusteredPerson();
    render(PersonAddresses, baseProps({ detail: {
      ...base,
      identifiers: [34, 56].map((id) => ({
        type: 'beeper', value: `beeper-user-${id}`, participant_id: id,
        is_primary: true, provenance: 'participant_identifiers',
        service_label: 'WhatsApp', participant_display_name: 'Alias Example'
      }))
    } }));

    expect(screen.getByRole('button', { name: 'Copy WhatsApp identifier for Alias Example (profile 34)' })).toBeDefined();
    expect(screen.getByRole('button', { name: 'Copy WhatsApp identifier for Alias Example (profile 56)' })).toBeDefined();
    expect(screen.getByRole('button', { name: 'Unlink WhatsApp identifier for Alias Example (profile 34)' })).toBeDefined();
    await fireEvent.click(screen.getByRole('button', { name: 'Unlink WhatsApp identifier for Alias Example (profile 56)' }));
    expect(screen.getByRole('group', { name: 'Confirm unlinking WhatsApp identifier for Alias Example (profile 56)' })).toBeDefined();
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
    expect(onUnlinkParticipants).toHaveBeenCalledTimes(1);
  });

  it('an unlink outcome for a later edge that resolves after navigating away stops touching this component\'s state', async () => {
    let resolveSecondEdge: ((outcome: LinkOutcome) => void) | undefined;
    let callCount = 0;
    const onUnlinkParticipants = vi.fn((): Promise<LinkOutcome> => {
      callCount += 1;
      if (callCount === 1) return Promise.resolve({ ok: true, identityRevision: 3, cacheState: 'ready' });
      return new Promise<LinkOutcome>((resolve) => { resolveSecondEdge = resolve; });
    });
    const { rerender } = render(PersonAddresses, baseProps({ detail: clusteredPerson(), onUnlinkParticipants }));

    await fireEvent.click(screen.getByRole('button', { name: 'Unlink +15550100002' }));
    await fireEvent.click(screen.getByRole('button', { name: 'Unlink' }));
    await waitFor(() => expect(onUnlinkParticipants).toHaveBeenCalledWith(12, 34));
    await waitFor(() => expect(onUnlinkParticipants).toHaveBeenCalledWith(34, 56));

    // Navigate away while the second edge's unlink call is still pending.
    const otherPerson = { ...clusteredPerson(), id: 200 };
    await rerender(baseProps({ detail: otherPerson, onUnlinkParticipants }));

    resolveSecondEdge?.({ ok: false, code: 'error', message: 'Request failed (500)' });
    // A macrotask, not a fixed count of microtask flushes: drains the
    // continuation after the awaited unlink call regardless of how many
    // .then hops it takes to reach the (skipped, once fixed) unlinkError
    // write and the finally block's `unlinking = false`.
    await new Promise((resolve) => setTimeout(resolve, 0));

    expect(screen.queryByRole('alert'), 'a stale failure must not surface on the wrong person').toBeNull();
  });

  it('finishes unlinking every captured edge after navigating away mid-sequence, without stale UI on the new view', async () => {
    let resolveFirstEdge: ((outcome: LinkOutcome) => void) | undefined;
    let callCount = 0;
    const onUnlinkParticipants = vi.fn((): Promise<LinkOutcome> => {
      callCount += 1;
      if (callCount === 1) return new Promise<LinkOutcome>((resolve) => { resolveFirstEdge = resolve; });
      // ok/stale would raise the cache banner if a stale outcome were still
      // applied to whichever person is showing now.
      return Promise.resolve({ ok: true, identityRevision: 4, cacheState: 'stale' });
    });
    const { rerender } = render(PersonAddresses, baseProps({ detail: clusteredPerson(), onUnlinkParticipants }));

    await fireEvent.click(screen.getByRole('button', { name: 'Unlink +15550100002' }));
    await fireEvent.click(screen.getByRole('button', { name: 'Unlink' }));
    await waitFor(() => expect(onUnlinkParticipants).toHaveBeenCalledWith(12, 34));
    expect(onUnlinkParticipants).toHaveBeenCalledTimes(1);

    // Navigate to a different person while the first edge's unlink call is
    // still in flight.
    const otherPerson = { ...clusteredPerson(), id: 200 };
    await rerender(baseProps({ detail: otherPerson, onUnlinkParticipants }));

    resolveFirstEdge?.({ ok: true, identityRevision: 3, cacheState: 'ready' });
    // Every captured edge must still be unlinked: stopping after the current
    // edge would persist a half-split cluster (some aliases detached, others
    // still merged) with no error anywhere.
    await waitFor(() => expect(onUnlinkParticipants).toHaveBeenCalledWith(34, 56));
    expect(onUnlinkParticipants).toHaveBeenCalledTimes(2);

    // Drain the loop's continuation, then confirm the second edge's stale
    // ok/stale outcome was not applied to person 200's header.
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(screen.queryByRole('alert'), 'stale outcome must not raise the banner for person 200').toBeNull();
  });
});
