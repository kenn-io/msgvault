import { directoryPerson } from '../../directory/entity-fixtures.test-support';
import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../../api/client';
import type { PersonSummary } from '../../explore/models';
import type { LinkOutcome } from '../../relationships/controller.svelte';
import type { ValidatedPersonMergeRequired } from '../../directory/person-merge';
import { openTypeahead } from '../../../test/kit-ui';
import LinkIdentityDialog from './LinkIdentityDialog.svelte';

const when = '2026-07-19T10:00:00Z';

function person(id: number, label: string): PersonSummary {
  return {
    id, display_label: label, partial_label: false,
    identifiers: [{ type: 'email', value: `${label.toLowerCase()}@example.com`, participant_id: id, is_primary: true, provenance: 'participant_identifiers' }],
    activity_count: 3, meeting_count: 0, file_count: 1,
    current_relationship_temperature: 0, peak_relationship_temperature: 0, peak_relationship_year: 0,
    source_counts: [], first_at: when, last_at: when, cache_revision: 'cache-rel'
  };
}

function pathOf(request: Request): string {
  return new URL(request.url, document.baseURI).pathname;
}

function fetchHandler(overrides: Record<string, (request: Request) => Promise<Response> | Response> = {}) {
  const requests: Request[] = [];
  const fetchFn = vi.fn<typeof fetch>(async (input) => {
    const request = input instanceof Request ? input : new Request(input);
    requests.push(request);
    const handler = overrides[pathOf(request)];
    if (handler) return handler(request);
    if (pathOf(request) === '/api/v1/people/directory') return Response.json({ people: [] });
    throw new Error(`unexpected path ${pathOf(request)}`);
  });
  return { fetchFn, requests };
}

function renderDialog(overrides: Partial<{
  excludeID: number;
  onConfirm: (participantID: number) => Promise<LinkOutcome>;
  onClose: () => void;
  onMergeRequired: (conflict: ValidatedPersonMergeRequired) => void;
}> = {}, fetchOverrides: Record<string, (request: Request) => Promise<Response> | Response> = {}) {
  const { fetchFn, requests } = fetchHandler({
    '/api/v1/participants/search': async () => Response.json({ rows: [person(2, 'Bob'), person(3, 'Cara')], total_count: 2, cache_revision: 'cache-rel', search_provenance: {} }),
    ...fetchOverrides
  });
  const onClose = overrides.onClose ?? vi.fn();
  const onConfirm = overrides.onConfirm ?? vi.fn(async (): Promise<LinkOutcome> => ({ ok: true, identityRevision: 2, cacheState: 'ready' }));
  const onMergeRequired = overrides.onMergeRequired ?? vi.fn();
  const { unmount } = render(LinkIdentityDialog, {
    client: createAPIClient(fetchFn),
    excludeID: overrides.excludeID ?? 1,
    excludePersonID: null,
    onConfirmPerson: vi.fn(),
    personLabel: 'Alice Example',
    onConfirm,
    onMergeRequired,
    onClose
  });
  return { requests, onClose, onConfirm, onMergeRequired, unmount };
}

describe('LinkIdentityDialog', () => {
  it('debounces search input into one participants/search call carrying identity_query', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      const { requests } = renderDialog();
      const input = await openTypeahead('Search people');

      await fireEvent.input(input, { target: { value: 'B' } });
      await fireEvent.input(input, { target: { value: 'Bo' } });
      await fireEvent.input(input, { target: { value: 'Bob' } });
      expect(requests).toHaveLength(0);

      await vi.advanceTimersByTimeAsync(250);
      const observedRequests = requests.filter(request => pathOf(request) === '/api/v1/participants/search');
      expect(observedRequests).toHaveLength(1);
      await expect(observedRequests[0]!.clone().json()).resolves.toMatchObject({ identity_query: 'Bob' });

      expect(await screen.findByText('Bob')).toBeDefined();
      expect(screen.getByText('Cara')).toBeDefined();
    } finally {
      vi.useRealTimers();
    }
  });

  it('excludes the currently open cluster member from search results', async () => {
    const { fetchFn } = fetchHandler({
      '/api/v1/participants/search': async () => Response.json({
        rows: [person(1, 'Self'), person(2, 'Bob')], total_count: 2, cache_revision: 'cache-rel', search_provenance: {}
      })
    });
    render(LinkIdentityDialog, { client: createAPIClient(fetchFn), excludeID: 1, excludePersonID: null, personLabel: 'Alice Example', onConfirm: vi.fn(), onConfirmPerson: vi.fn(), onMergeRequired: vi.fn(), onClose: vi.fn() });
    await fireEvent.input(await openTypeahead('Search people'), { target: { value: 'e' } });

    expect(await screen.findByText('Bob')).toBeDefined();
    expect(screen.queryByText('Self')).toBeNull();
  });

  it.each([
    ['click', 'Bob', 2],
    ['Enter', 'Cara', 3]
  ] as const)('confirms a result selected by %s after focus leaves the picker', async (how, label, id) => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      const { onConfirm } = renderDialog();
      const input = await openTypeahead('Search people');
      await fireEvent.input(input, { target: { value: label[0] } });
      const option = await screen.findByRole('option', { name: new RegExp(label) });
      if (how === 'click') {
        await fireEvent.mouseDown(option);
      } else {
        await fireEvent.keyDown(input, { key: 'ArrowDown' });
        await fireEvent.keyDown(input, { key: 'Enter' });
      }
      const trigger = await screen.findByRole('button', { name: `Search people: ${label}` });
      await waitFor(() => expect(document.activeElement).toBe(trigger));

      // Leaving the closed picker makes Typeahead send another empty query, which also clears the results.
      const confirm = screen.getByRole('button', { name: 'Merge' });
      confirm.focus();
      await vi.advanceTimersByTimeAsync(250);

      expect(screen.getByRole('button', { name: `Search people: ${label}` })).toBeDefined();
      expect(confirm).toHaveProperty('disabled', false);
      await fireEvent.click(confirm);
      await waitFor(() => expect(onConfirm).toHaveBeenCalledWith(id));
    } finally {
      vi.useRealTimers();
    }
  });

  it('drops the prior selection when the picker reopens', async () => {
    const { onConfirm } = renderDialog();
    expect(screen.getByRole('button', { name: 'Merge' })).toHaveProperty('disabled', true);
    await fireEvent.input(await openTypeahead('Search people'), { target: { value: 'B' } });
    await fireEvent.mouseDown(await screen.findByRole('option', { name: /Bob/ }));
    expect(screen.getByRole('button', { name: 'Merge' })).toHaveProperty('disabled', false);

    const input = await openTypeahead('Search people');

    expect(input.value).toBe('');
    expect(screen.getByRole('button', { name: 'Merge' })).toHaveProperty('disabled', true);
    await fireEvent.click(screen.getByRole('button', { name: 'Merge' }));
    expect(onConfirm).not.toHaveBeenCalled();
  });

  it('shows an already-linked outcome inline and stays open', async () => {
    const onClose = vi.fn();
    const onConfirm = vi.fn(async (): Promise<LinkOutcome> => ({ ok: false, code: 'already_linked', message: 'Target unavailable' }));
    renderDialog({ onClose, onConfirm });
    await fireEvent.input(await openTypeahead('Search people'), { target: { value: 'B' } });
    await fireEvent.mouseDown(await screen.findByRole('option', { name: /Bob/ }));
    await fireEvent.click(screen.getByRole('button', { name: 'Merge' }));

    expect((await screen.findByRole('alert')).textContent).toContain('Already linked — these two are treated as the same person.');
    expect(onClose).not.toHaveBeenCalled();
  });

  it('hands a merge-required outcome to its parent without closing or retrying', async () => {
    const conflict = {
      error: 'person_merge_required', message: 'Choose a survivor', profiles: [
        { etag: '"person-7-r4"', person: { id: 7, revision: 4 } },
        { etag: '"person-9-r2"', person: { id: 9, revision: 2 } }
      ]
    } as unknown as ValidatedPersonMergeRequired;
    const onConfirm = vi.fn(async (): Promise<LinkOutcome> => ({
      ok: false, code: 'merge_required', message: conflict.message, conflict
    }));
    const onClose = vi.fn();
    const { onMergeRequired } = renderDialog({ onConfirm, onClose });
    await fireEvent.input(await openTypeahead('Search people'), { target: { value: 'B' } });
    await fireEvent.mouseDown(await screen.findByRole('option', { name: /Bob/ }));

    await fireEvent.click(screen.getByRole('button', { name: 'Merge' }));

    await waitFor(() => expect(onMergeRequired).toHaveBeenCalledWith(conflict));
    expect(onConfirm).toHaveBeenCalledOnce();
    expect(onClose).not.toHaveBeenCalled();
  });

  it('cancels via the Cancel button without confirming', async () => {
    const onClose = vi.fn();
    const onConfirm = vi.fn();
    renderDialog({ onClose, onConfirm });
    await fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));
    expect(onClose).toHaveBeenCalled();
    expect(onConfirm).not.toHaveBeenCalled();
  });

  it('blocks Cancel, Escape, and the backdrop while a confirm is pending', async () => {
    let resolveConfirm: ((outcome: LinkOutcome) => void) | undefined;
    const onConfirm = vi.fn(() => new Promise<LinkOutcome>((resolve) => { resolveConfirm = resolve; }));
    const onClose = vi.fn();
    renderDialog({ onClose, onConfirm });
    await fireEvent.input(await openTypeahead('Search people'), { target: { value: 'B' } });
    await fireEvent.mouseDown(await screen.findByRole('option', { name: /Bob/ }));
    await fireEvent.click(screen.getByRole('button', { name: 'Merge' }));
    await waitFor(() => expect(onConfirm).toHaveBeenCalledWith(2));

    expect(screen.getByRole('button', { name: 'Cancel' })).toHaveProperty('disabled', true);
    await fireEvent.keyDown(window, { key: 'Escape' });
    await fireEvent.pointerDown(document.querySelector('.kit-modal-overlay')!);
    expect(onClose, 'no dismissal path may hide a pending confirm').not.toHaveBeenCalled();

    // Success still closes the dialog itself once the outcome is known.
    resolveConfirm?.({ ok: true, identityRevision: 4, cacheState: 'ready' });
    await waitFor(() => expect(onClose).toHaveBeenCalledOnce());
    expect(screen.queryByRole('alert')).toBeNull();
  });

  it('stays open with the error and re-enabled dismissal after a failed confirm', async () => {
    let resolveConfirm: ((outcome: LinkOutcome) => void) | undefined;
    const onConfirm = vi.fn(() => new Promise<LinkOutcome>((resolve) => { resolveConfirm = resolve; }));
    const onClose = vi.fn();
    renderDialog({ onClose, onConfirm });
    await fireEvent.input(await openTypeahead('Search people'), { target: { value: 'B' } });
    await fireEvent.mouseDown(await screen.findByRole('option', { name: /Bob/ }));
    await fireEvent.click(screen.getByRole('button', { name: 'Merge' }));
    await waitFor(() => expect(onConfirm).toHaveBeenCalledWith(2));

    resolveConfirm?.({ ok: false, code: 'error', message: 'Request failed (500)' });
    expect((await screen.findByRole('alert')).textContent).toContain('Request failed (500)');
    expect(screen.getByRole('button', { name: 'Cancel' })).toHaveProperty('disabled', false);

    await fireEvent.keyDown(window, { key: 'Escape' });
    expect(onClose).toHaveBeenCalledOnce();
  });

  it('cancels a pending debounced search when the dialog unmounts', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      const { requests, unmount } = renderDialog();
      const input = await openTypeahead('Search people');

      await fireEvent.input(input, { target: { value: 'Bob' } });
      expect(requests, 'debounce has not fired yet').toHaveLength(0);

      unmount();
      await vi.advanceTimersByTimeAsync(250);
      expect(requests, 'unmounting must cancel the pending debounced search').toHaveLength(0);
    } finally {
      vi.useRealTimers();
    }
  });

  it('aborts an in-flight search request when the dialog unmounts mid-request', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      let capturedSignal: AbortSignal | undefined;
      const { unmount } = renderDialog({}, {
        '/api/v1/participants/search': (request) => {
          capturedSignal = request.signal;
          return new Promise<Response>(() => {
            /* never resolves: the assertion is on abort, not on a response */
          });
        }
      });
      await fireEvent.input(await openTypeahead('Search people'), { target: { value: 'Bob' } });
      await vi.advanceTimersByTimeAsync(250);
      // the debounced search must have started the request before unmount
      expect(capturedSignal).toBeDefined();
      expect(capturedSignal?.aborted).toBe(false);

      unmount();
      expect(capturedSignal?.aborted).toBe(true);
    } finally {
      vi.useRealTimers();
    }
  });
});

it.each((['saved', 'observed'] as const).flatMap(source => (['HTTP 503', 'transport failure'] as const).map(failure => ({ source, failure }))))('keeps $source search choices usable when the other source has $failure', async ({ source, failure }) => {
  const onConfirmPerson = vi.fn(async (): Promise<LinkOutcome> => ({ ok: true, identityRevision: 2, cacheState: 'ready' }));
  const onConfirm = vi.fn(async (): Promise<LinkOutcome> => ({ ok: true, identityRevision: 2, cacheState: 'ready' }));
  const client = createAPIClient(async input => {
    const path = new URL(input instanceof Request ? input.url : String(input)).pathname;
    if (path.endsWith('/people/directory') && source === 'saved') return Response.json({ people: [directoryPerson(9, { revision: 1, display_name: 'Synthetic Saved' })] });
    if (path.endsWith('/participants/search') && source === 'observed') return Response.json({ rows: [person(2, 'Synthetic Observed')], total_count: 1, cache_revision: 'cache-rel', search_provenance: {} });
    if (failure === 'transport failure') throw new TypeError('Source unavailable');
    return Response.json({ error: 'unavailable', message: 'Source unavailable' }, { status: 503 });
  });
  const view = render(LinkIdentityDialog, { client, excludeID: 1, excludePersonID: null, personLabel: 'Alice Example', onConfirm, onConfirmPerson, onMergeRequired: vi.fn(), onClose: vi.fn() });
  try {
    await fireEvent.input(await openTypeahead('Search people'), { target: { value: 'Synthetic' } });
    await screen.findByText('Source unavailable');
    await fireEvent.mouseDown(await screen.findByRole('option', { name: source === 'saved' ? /Synthetic Saved/ : /Synthetic Observed/ }));
    await fireEvent.click(screen.getByRole('button', { name: 'Merge' }));
    await waitFor(() => expect(source === 'saved' ? onConfirmPerson : onConfirm).toHaveBeenCalledWith(source === 'saved' ? 9 : 2));
  } finally { view.unmount(); }
});
