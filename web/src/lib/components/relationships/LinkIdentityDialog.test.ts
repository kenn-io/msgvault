import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../../api/client';
import type { PersonSummary } from '../../explore/models';
import type { LinkOutcome } from '../../relationships/controller.svelte';
import LinkIdentityDialog from './LinkIdentityDialog.svelte';

const when = '2026-07-19T10:00:00Z';

function person(id: number, label: string): PersonSummary {
  return {
    id, display_label: label, partial_label: false,
    identifiers: [{ type: 'email', value: `${label.toLowerCase()}@example.com`, participant_id: id, is_primary: true, provenance: 'participant_identifiers' }],
    activity_count: 3, file_count: 1, source_counts: [], first_at: when, last_at: when, cache_revision: 'cache-rel'
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
    throw new Error(`unexpected path ${pathOf(request)}`);
  });
  return { fetchFn, requests };
}

function renderDialog(overrides: Partial<{
  excludeID: number;
  onConfirm: (participantID: number) => Promise<LinkOutcome>;
  onClose: () => void;
}> = {}, fetchOverrides: Record<string, (request: Request) => Promise<Response> | Response> = {}) {
  const { fetchFn, requests } = fetchHandler({
    '/api/v1/people/search': async () => Response.json({ rows: [person(2, 'Bob'), person(3, 'Cara')], total_count: 2, cache_revision: 'cache-rel', search_provenance: {} }),
    ...fetchOverrides
  });
  const onClose = overrides.onClose ?? vi.fn();
  const onConfirm = overrides.onConfirm ?? vi.fn(async (): Promise<LinkOutcome> => ({ ok: true, identityRevision: 2, cacheState: 'ready' }));
  const { unmount } = render(LinkIdentityDialog, {
    client: createAPIClient(fetchFn),
    excludeID: overrides.excludeID ?? 1,
    onConfirm,
    onClose
  });
  return { requests, onClose, onConfirm, unmount };
}

describe('LinkIdentityDialog', () => {
  it('debounces search input into one people/search call carrying identity_query', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      const { requests } = renderDialog();
      const input = screen.getByRole('searchbox', { name: 'Search people to link' });

      await fireEvent.input(input, { target: { value: 'B' } });
      await fireEvent.input(input, { target: { value: 'Bo' } });
      await fireEvent.input(input, { target: { value: 'Bob' } });
      expect(requests).toHaveLength(0);

      await vi.advanceTimersByTimeAsync(250);
      expect(requests).toHaveLength(1);
      await expect(requests[0]!.clone().json()).resolves.toMatchObject({ identity_query: 'Bob' });

      expect(await screen.findByText('Bob')).toBeDefined();
      expect(screen.getByText('Cara')).toBeDefined();
    } finally {
      vi.useRealTimers();
    }
  });

  it('excludes the currently open cluster member from search results', async () => {
    const { fetchFn } = fetchHandler({
      '/api/v1/people/search': async () => Response.json({
        rows: [person(1, 'Self'), person(2, 'Bob')], total_count: 2, cache_revision: 'cache-rel', search_provenance: {}
      })
    });
    render(LinkIdentityDialog, { client: createAPIClient(fetchFn), excludeID: 1, onConfirm: vi.fn(), onClose: vi.fn() });
    await fireEvent.input(screen.getByRole('searchbox', { name: 'Search people to link' }), { target: { value: 'e' } });

    expect(await screen.findByText('Bob')).toBeDefined();
    expect(screen.queryByText('Self')).toBeNull();
  });

  it('selects a result by click or Enter, then confirms with that participant ID', async () => {
    const { onConfirm } = renderDialog();
    await fireEvent.input(screen.getByRole('searchbox', { name: 'Search people to link' }), { target: { value: 'B' } });
    const bobOption = await screen.findByRole('option', { name: /Bob/ });

    await fireEvent.click(bobOption);
    expect(bobOption.getAttribute('aria-selected')).toBe('true');

    await fireEvent.click(screen.getByRole('button', { name: 'Link' }));
    await waitFor(() => expect(onConfirm).toHaveBeenCalledWith(2));
  });

  it('selects a result via Enter without requiring a click', async () => {
    const { onConfirm } = renderDialog();
    await fireEvent.input(screen.getByRole('searchbox', { name: 'Search people to link' }), { target: { value: 'C' } });
    const caraOption = await screen.findByRole('option', { name: /Cara/ });

    await fireEvent.keyDown(caraOption, { key: 'Enter' });
    expect(caraOption.getAttribute('aria-selected')).toBe('true');

    await fireEvent.click(screen.getByRole('button', { name: 'Link' }));
    await waitFor(() => expect(onConfirm).toHaveBeenCalledWith(3));
  });

  it('disables Link until a result is selected', async () => {
    renderDialog();
    expect(screen.getByRole('button', { name: 'Link' })).toHaveProperty('disabled', true);
  });

  it('closes silently on an ok/ready outcome', async () => {
    const onClose = vi.fn();
    const onConfirm = vi.fn(async (): Promise<LinkOutcome> => ({ ok: true, identityRevision: 4, cacheState: 'ready' }));
    renderDialog({ onClose, onConfirm });
    await fireEvent.input(screen.getByRole('searchbox', { name: 'Search people to link' }), { target: { value: 'B' } });
    await fireEvent.click(await screen.findByRole('option', { name: /Bob/ }));
    await fireEvent.click(screen.getByRole('button', { name: 'Link' }));

    await waitFor(() => expect(onClose).toHaveBeenCalled());
    expect(screen.queryByRole('alert')).toBeNull();
  });

  it('closes on an ok/stale outcome too, leaving the banner to the header', async () => {
    const onClose = vi.fn();
    const onConfirm = vi.fn(async (): Promise<LinkOutcome> => ({ ok: true, identityRevision: 4, cacheState: 'stale' }));
    renderDialog({ onClose, onConfirm });
    await fireEvent.input(screen.getByRole('searchbox', { name: 'Search people to link' }), { target: { value: 'B' } });
    await fireEvent.click(await screen.findByRole('option', { name: /Bob/ }));
    await fireEvent.click(screen.getByRole('button', { name: 'Link' }));

    await waitFor(() => expect(onClose).toHaveBeenCalled());
  });

  it('shows an inline already_linked error and stays open', async () => {
    const onClose = vi.fn();
    const onConfirm = vi.fn(async (): Promise<LinkOutcome> => ({ ok: false, code: 'already_linked', message: 'nope' }));
    renderDialog({ onClose, onConfirm });
    await fireEvent.input(screen.getByRole('searchbox', { name: 'Search people to link' }), { target: { value: 'B' } });
    await fireEvent.click(await screen.findByRole('option', { name: /Bob/ }));
    await fireEvent.click(screen.getByRole('button', { name: 'Link' }));

    expect((await screen.findByRole('alert')).textContent).toContain('These identities are already linked through another identifier.');
    expect(onClose).not.toHaveBeenCalled();
  });

  it('shows the outcome message inline for invalid/error outcomes and stays open', async () => {
    const onClose = vi.fn();
    const onConfirm = vi.fn(async (): Promise<LinkOutcome> => ({ ok: false, code: 'error', message: 'Request failed (500)' }));
    renderDialog({ onClose, onConfirm });
    await fireEvent.input(screen.getByRole('searchbox', { name: 'Search people to link' }), { target: { value: 'B' } });
    await fireEvent.click(await screen.findByRole('option', { name: /Bob/ }));
    await fireEvent.click(screen.getByRole('button', { name: 'Link' }));

    expect((await screen.findByRole('alert')).textContent).toContain('Request failed (500)');
    expect(onClose).not.toHaveBeenCalled();
  });

  it('closes on Esc', async () => {
    const onClose = vi.fn();
    renderDialog({ onClose });
    await fireEvent.keyDown(window, { key: 'Escape' });
    expect(onClose).toHaveBeenCalled();
  });

  it('cancels via the Cancel button without confirming', async () => {
    const onClose = vi.fn();
    const onConfirm = vi.fn();
    renderDialog({ onClose, onConfirm });
    await fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));
    expect(onClose).toHaveBeenCalled();
    expect(onConfirm).not.toHaveBeenCalled();
  });

  it('cancels a pending debounced search and aborts an in-flight request when the dialog unmounts', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      const { requests, unmount } = renderDialog();
      const input = screen.getByRole('searchbox', { name: 'Search people to link' });

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
        '/api/v1/people/search': (request) => {
          capturedSignal = request.signal;
          return new Promise<Response>(() => {
            /* never resolves: the assertion is on abort, not on a response */
          });
        }
      });
      await fireEvent.input(screen.getByRole('searchbox', { name: 'Search people to link' }), { target: { value: 'Bob' } });
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
