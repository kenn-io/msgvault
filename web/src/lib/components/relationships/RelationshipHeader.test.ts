import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../../api/client';
import type { DomainSummary, PersonSummary } from '../../explore/models';
import type { LinkOutcome } from '../../relationships/controller.svelte';
import RelationshipHeader from './RelationshipHeader.svelte';

const when = '2026-07-19T10:00:00Z';

function person(): PersonSummary {
  return {
    id: 12, display_label: 'Alice Example', partial_label: false,
    identifiers: [
      { type: 'email', value: 'alice@example.com', display_value: 'Alice', participant_id: 12, is_primary: true, provenance: 'participant_identifiers' },
      { type: 'phone', value: '+15550100001', participant_id: 12, is_primary: false, provenance: 'participant_identifiers' }
    ],
    activity_count: 42, file_count: 3, source_counts: [{ source_type: 'gmail', count: 42 }],
    first_at: when, last_at: when, cache_revision: 'cache-rel'
  };
}

function domain(): DomainSummary {
  return {
    domain: 'example.com', activity_count: 100, file_count: 5, person_count: 8,
    first_at: when, last_at: when, source_counts: [], cache_revision: 'cache-rel'
  };
}

function searchResult(): PersonSummary {
  return {
    id: 99, display_label: 'Bob Example', partial_label: false,
    identifiers: [{ type: 'email', value: 'bob@example.com', participant_id: 99, is_primary: true, provenance: 'participant_identifiers' }],
    activity_count: 7, file_count: 0, source_counts: [], first_at: when, last_at: when, cache_revision: 'cache-rel'
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
    activity_count: 42, file_count: 3, source_counts: [{ source_type: 'gmail', count: 42 }],
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

function searchClient(): ReturnType<typeof createAPIClient> {
  const fetchFn = vi.fn<typeof fetch>(async (input) => {
    const request = input instanceof Request ? input : new Request(input);
    if (new URL(request.url).pathname === '/api/v1/people/search') {
      return Response.json({ rows: [searchResult()], total_count: 1, cache_revision: 'cache-rel', search_provenance: {} });
    }
    throw new Error(`unexpected fetch to ${request.url}`);
  });
  return createAPIClient(fetchFn);
}

function baseProps(overrides: Record<string, unknown> = {}) {
  return {
    detail: person(),
    filesOpen: false,
    onFilesToggle: vi.fn(),
    client: searchClient(),
    onLinkParticipants: vi.fn(async (): Promise<LinkOutcome> => ({ ok: true, identityRevision: 2, cacheState: 'ready' })),
    onUnlinkParticipants: vi.fn(async (): Promise<LinkOutcome> => ({ ok: true, identityRevision: 3, cacheState: 'ready' })),
    ...overrides
  };
}

/** Opens the Link identity dialog, searches for "Bob", selects the stubbed
 * search result (participant #99), and clicks Link to confirm. Exercises the
 * real LinkIdentityDialog end to end; its own search/select mechanics are
 * covered directly in LinkIdentityDialog.test.ts. */
async function linkToSearchResult(): Promise<void> {
  await fireEvent.click(screen.getByRole('button', { name: 'Link identity' }));
  await fireEvent.input(screen.getByRole('searchbox', { name: 'Search people to link' }), { target: { value: 'Bob' } });
  await fireEvent.click(await screen.findByRole('option', { name: /Bob Example/ }));
  await fireEvent.click(screen.getByRole('button', { name: 'Link' }));
}

describe('RelationshipHeader', () => {
  it('shows a placeholder status when nothing is selected', () => {
    render(RelationshipHeader, baseProps({ detail: null }));
    expect(screen.getByRole('status').textContent).toContain('Select a person or domain');
  });

  it('renders a person: display name, identity chips, item counts, and the Files toggle', async () => {
    const onFilesToggle = vi.fn();
    render(RelationshipHeader, baseProps({ onFilesToggle }));

    expect(screen.getByRole('heading', { name: 'Alice Example' })).toBeDefined();
    expect(screen.getByText(/42 items/)).toBeDefined();
    expect(screen.getByText(/3 files/)).toBeDefined();
    expect(screen.getByText('Alice')).toBeDefined();
    expect(screen.getByText('alice@example.com')).toBeDefined();
    expect(screen.getByText(/email · Primary · participant_identifiers/)).toBeDefined();
    expect(screen.getByText('+15550100001')).toBeDefined();
    expect(screen.getByText(/phone · Secondary · participant_identifiers/)).toBeDefined();

    await fireEvent.click(screen.getByRole('button', { name: 'Files 3' }));
    expect(onFilesToggle).toHaveBeenCalledWith(true);
  });

  it('renders a domain by domain name and person count, without identity chips or a Link identity button', () => {
    render(RelationshipHeader, baseProps({ detail: domain() }));

    expect(screen.getByRole('heading', { name: 'example.com' })).toBeDefined();
    expect(screen.getByText(/100 items/)).toBeDefined();
    expect(screen.getByText(/8 people/)).toBeDefined();
    expect(screen.queryByLabelText('Archive-wide identity evidence')).toBeNull();
    expect(screen.queryByRole('button', { name: 'Link identity' })).toBeNull();
  });

  it('toggles Files off when already open', async () => {
    const onFilesToggle = vi.fn();
    render(RelationshipHeader, baseProps({ filesOpen: true, onFilesToggle }));

    await fireEvent.click(screen.getByRole('button', { name: 'Files 3' }));
    expect(onFilesToggle).toHaveBeenCalledWith(false);
  });

  it('opens the Link identity dialog for a person, and Esc closes it', async () => {
    render(RelationshipHeader, baseProps());

    await fireEvent.click(screen.getByRole('button', { name: 'Link identity' }));
    expect(screen.getByRole('dialog', { name: 'Link identity' })).toBeDefined();

    await fireEvent.keyDown(window, { key: 'Escape' });
    expect(screen.queryByRole('dialog', { name: 'Link identity' })).toBeNull();
  });

  it('links against the open cluster member ID; an ok/ready outcome closes the dialog silently', async () => {
    const onLinkParticipants = vi.fn(async (): Promise<LinkOutcome> => ({ ok: true, identityRevision: 2, cacheState: 'ready' }));
    render(RelationshipHeader, baseProps({ onLinkParticipants }));

    await linkToSearchResult();

    await waitFor(() => expect(onLinkParticipants).toHaveBeenCalledWith(12, 99));
    expect(screen.queryByRole('dialog', { name: 'Link identity' })).toBeNull();
    expect(screen.queryByRole('alert')).toBeNull();
  });

  it('shows already_linked as an inline dialog error and keeps the dialog open', async () => {
    const onLinkParticipants = vi.fn(async (): Promise<LinkOutcome> => (
      { ok: false, code: 'already_linked', message: 'these participants are already connected through other links' }
    ));
    render(RelationshipHeader, baseProps({ onLinkParticipants }));

    await linkToSearchResult();

    expect((await screen.findByRole('alert')).textContent).toContain(
      'These identities are already linked through another identifier.'
    );
    expect(screen.getByRole('dialog', { name: 'Link identity' })).toBeDefined();
  });

  it('raises the identity_cache_stale banner on an ok/stale outcome, and Retry re-invokes the same pair', async () => {
    const onLinkParticipants = vi.fn(async (): Promise<LinkOutcome> => ({ ok: true, identityRevision: 2, cacheState: 'stale' }));
    render(RelationshipHeader, baseProps({ onLinkParticipants }));

    await linkToSearchResult();

    expect((await screen.findByRole('alert')).textContent).toContain(
      'Identity saved; the cache refresh failed — groupings may be stale until a rebuild. Retrying is safe.'
    );
    expect(onLinkParticipants).toHaveBeenCalledWith(12, 99);
    expect(screen.queryByRole('dialog', { name: 'Link identity' })).toBeNull();

    await fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
    await waitFor(() => expect(onLinkParticipants).toHaveBeenCalledTimes(2));
    expect(onLinkParticipants).toHaveBeenLastCalledWith(12, 99);
  });

  it('clears the stale banner after a later ready outcome, and it persists across a dialog close in the meantime', async () => {
    const onLinkParticipants = vi.fn(async (): Promise<LinkOutcome> => ({ ok: true, identityRevision: 2, cacheState: 'stale' }));
    render(RelationshipHeader, baseProps({ onLinkParticipants }));

    await linkToSearchResult();
    await screen.findByRole('alert');

    // The dialog closed itself on the ok outcome; the banner must still show.
    expect(screen.queryByRole('dialog', { name: 'Link identity' })).toBeNull();
    expect(screen.getByRole('alert')).toBeDefined();

    // Reopening and closing the dialog without linking again must not
    // disturb the still-pending stale banner.
    await fireEvent.click(screen.getByRole('button', { name: 'Link identity' }));
    await fireEvent.keyDown(window, { key: 'Escape' });
    expect(screen.getByRole('alert')).toBeDefined();

    onLinkParticipants.mockResolvedValueOnce({ ok: true, identityRevision: 3, cacheState: 'ready' });
    await fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
    await waitFor(() => expect(screen.queryByRole('alert')).toBeNull());
  });

  it('shows an unlink × only on chips for other cluster members, never on the open cluster\'s own identifiers', () => {
    render(RelationshipHeader, baseProps({ detail: clusteredPerson() }));

    expect(screen.queryByRole('button', { name: /Detach identity #12/ })).toBeNull();
    expect(screen.getByRole('button', { name: 'Detach identity #34 from this cluster' })).toBeDefined();
    expect(screen.getByRole('button', { name: 'Detach identity #56 from this cluster' })).toBeDefined();
  });

  it('does not show unlink controls when the person has no cluster', () => {
    render(RelationshipHeader, baseProps());
    expect(screen.queryByRole('button', { name: /Detach identity/ })).toBeNull();
  });

  it('renders a fallback chip with its own detach control for a cluster member with no identifier rows', async () => {
    const onUnlinkParticipants = vi.fn(async (): Promise<LinkOutcome> => ({ ok: true, identityRevision: 4, cacheState: 'ready' }));
    render(RelationshipHeader, baseProps({ detail: clusteredPersonWithBareMember(), onUnlinkParticipants }));

    expect(screen.getByText(/identity #78/)).toBeDefined();
    const detachButton = screen.getByRole('button', { name: 'Detach identity #78 from this cluster' });
    await fireEvent.click(detachButton);
    await fireEvent.click(screen.getByRole('button', { name: 'Detach' }));

    await waitFor(() => expect(onUnlinkParticipants).toHaveBeenCalledWith(12, 78));
  });

  it('confirming a cut-vertex member\'s unlink removes every edge incident to it, not just one', async () => {
    const onUnlinkParticipants = vi.fn(async (): Promise<LinkOutcome> => ({ ok: true, identityRevision: 4, cacheState: 'ready' }));
    render(RelationshipHeader, baseProps({ detail: clusteredPerson(), onUnlinkParticipants }));

    await fireEvent.click(screen.getByRole('button', { name: 'Detach identity #34 from this cluster' }));
    expect(screen.getByRole('group', { name: 'Confirm detaching identity #34' })).toBeDefined();
    await fireEvent.click(screen.getByRole('button', { name: 'Detach' }));

    await waitFor(() => expect(onUnlinkParticipants).toHaveBeenCalledTimes(2));
    expect(onUnlinkParticipants).toHaveBeenCalledWith(12, 34);
    expect(onUnlinkParticipants).toHaveBeenCalledWith(34, 56);
    expect(screen.queryByRole('group', { name: 'Confirm detaching identity #34' })).toBeNull();
    expect(screen.queryByRole('alert')).toBeNull();
  });

  it('cancelling an unlink confirm leaves the chip untouched and calls nothing', async () => {
    const onUnlinkParticipants = vi.fn();
    render(RelationshipHeader, baseProps({ detail: clusteredPerson(), onUnlinkParticipants }));

    await fireEvent.click(screen.getByRole('button', { name: 'Detach identity #56 from this cluster' }));
    await fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));

    expect(screen.queryByRole('group', { name: 'Confirm detaching identity #56' })).toBeNull();
    expect(screen.getByRole('button', { name: 'Detach identity #56 from this cluster' })).toBeDefined();
    expect(onUnlinkParticipants).not.toHaveBeenCalled();
  });

  it('treats an already-clean edge (idempotent 200) as success and closes the confirm', async () => {
    // Simulates confirming after the edge was already removed elsewhere:
    // UnlinkParticipants is idempotent, so the store 200s without error.
    const onUnlinkParticipants = vi.fn(async (): Promise<LinkOutcome> => ({ ok: true, identityRevision: 5, cacheState: 'ready' }));
    render(RelationshipHeader, baseProps({ detail: clusteredPerson(), onUnlinkParticipants }));

    await fireEvent.click(screen.getByRole('button', { name: 'Detach identity #34 from this cluster' }));
    await fireEvent.click(screen.getByRole('button', { name: 'Detach' }));

    await waitFor(() => expect(onUnlinkParticipants).toHaveBeenCalledTimes(2));
    expect(screen.queryByRole('alert')).toBeNull();
    expect(screen.queryByRole('group', { name: 'Confirm detaching identity #34' })).toBeNull();
  });

  it('shows an inline error and keeps the confirm state on an unlink failure', async () => {
    const onUnlinkParticipants = vi.fn(async (): Promise<LinkOutcome> => ({ ok: false, code: 'error', message: 'Request failed (500)' }));
    render(RelationshipHeader, baseProps({ detail: clusteredPerson(), onUnlinkParticipants }));

    await fireEvent.click(screen.getByRole('button', { name: 'Detach identity #34 from this cluster' }));
    await fireEvent.click(screen.getByRole('button', { name: 'Detach' }));

    expect((await screen.findByRole('alert')).textContent).toContain('Request failed (500)');
    expect(onUnlinkParticipants).toHaveBeenCalledTimes(1);
  });

  it('clears the stale banner and any pending unlink confirm when navigating to a different person', async () => {
    const onLinkParticipants = vi.fn(async (): Promise<LinkOutcome> => ({ ok: true, identityRevision: 2, cacheState: 'stale' }));
    const { rerender } = render(RelationshipHeader, baseProps({ onLinkParticipants }));

    await linkToSearchResult();
    await screen.findByRole('alert');
    expect(screen.getByRole('alert')).toBeDefined();

    const otherPerson = { ...clusteredPerson(), id: 200 };
    await rerender(baseProps({ onLinkParticipants, detail: otherPerson }));
    expect(screen.queryByRole('alert')).toBeNull();
  });

  it('a link outcome that resolves after navigating away does not repopulate the banner for the new person', async () => {
    let resolveLink: ((outcome: LinkOutcome) => void) | undefined;
    const onLinkParticipants = vi.fn(
      () => new Promise<LinkOutcome>((resolve) => { resolveLink = resolve; })
    );
    const { rerender } = render(RelationshipHeader, baseProps({ onLinkParticipants }));

    await fireEvent.click(screen.getByRole('button', { name: 'Link identity' }));
    await fireEvent.input(screen.getByRole('searchbox', { name: 'Search people to link' }), { target: { value: 'Bob' } });
    await fireEvent.click(await screen.findByRole('option', { name: /Bob Example/ }));
    await fireEvent.click(screen.getByRole('button', { name: 'Link' }));
    await waitFor(() => expect(onLinkParticipants).toHaveBeenCalledWith(12, 99));

    // Navigate to a different person while the link call for Alice (id 12)
    // is still in flight.
    const otherPerson = { ...clusteredPerson(), id: 200 };
    await rerender(baseProps({ onLinkParticipants, detail: otherPerson }));
    expect(screen.queryByRole('alert')).toBeNull();

    // The stale outcome resolves for the person that's no longer open.
    // LinkIdentityDialog's own confirmLink closes the dialog once
    // RelationshipHeader's confirmLink (which this awaits) returns — waiting
    // for that close is what actually proves the full continuation,
    // including any (skipped) applyOutcome call, ran to completion.
    resolveLink?.({ ok: true, identityRevision: 9, cacheState: 'stale' });
    await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Link identity' })).toBeNull());

    expect(screen.queryByRole('alert'), 'must not resurrect the banner for person 200').toBeNull();
  });

  it('an unlink outcome for a later edge that resolves after navigating away stops touching this component\'s state', async () => {
    let resolveSecondEdge: ((outcome: LinkOutcome) => void) | undefined;
    let callCount = 0;
    const onUnlinkParticipants = vi.fn((): Promise<LinkOutcome> => {
      callCount += 1;
      if (callCount === 1) return Promise.resolve({ ok: true, identityRevision: 3, cacheState: 'ready' });
      return new Promise<LinkOutcome>((resolve) => { resolveSecondEdge = resolve; });
    });
    const { rerender } = render(RelationshipHeader, baseProps({ detail: clusteredPerson(), onUnlinkParticipants }));

    await fireEvent.click(screen.getByRole('button', { name: 'Detach identity #34 from this cluster' }));
    await fireEvent.click(screen.getByRole('button', { name: 'Detach' }));
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
});
