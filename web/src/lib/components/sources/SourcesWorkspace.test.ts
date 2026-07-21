import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../../api/client';
import SourcesWorkspace from './SourcesWorkspace.svelte';

function source(overrides: Record<string, unknown> = {}) {
  return {
    id: 1, source_type: 'gmail', identifier: 'archive@example.com', display_name: 'Archive',
    last_sync_at: '2026-07-19T10:00:00Z', updated_at: '2026-07-19T10:00:00Z',
    active_sync: null, latest_sync: null, last_successful_sync: null,
    can_sync: true, scheduled: false, next_sync_at: null, ...overrides
  };
}

function run(status: string, processed: number, overrides: Record<string, unknown> = {}) {
  return {
    id: 9, source_id: 1, started_at: '2026-07-19T10:00:00Z', completed_at: null,
    status, messages_processed: processed, messages_added: processed,
    messages_updated: 0, errors_count: 0, error_message: null,
    cursor_before: null, cursor_after: null, ...overrides
  };
}

afterEach(() => vi.useRealTimers());

describe('SourcesWorkspace', () => {
  it('shows status and only exposes Sync now from server capability truth', async () => {
    const fetchFn = vi.fn<typeof fetch>(async () => Response.json({ sources: [
      source(),
      source({ id: 2, source_type: 'imap', identifier: 'readonly@example.com', display_name: null,
        can_sync: false, sync_unavailable_reason: 'sync_not_configured' })
    ] }));
    render(SourcesWorkspace, { client: createAPIClient(fetchFn) });

    expect(await screen.findByRole('heading', { name: 'Archive' })).toBeDefined();
    expect(screen.getAllByText('Last successful sync')).toHaveLength(2);
    expect(screen.getByRole('button', { name: 'Sync now Archive' })).toBeDefined();
    expect(screen.queryByRole('button', { name: 'Sync now readonly@example.com' })).toBeNull();
    expect(screen.getByText('sync_not_configured')).toBeDefined();
    expect(screen.getAllByText('Not scheduled')).toHaveLength(2);
    expect(screen.getAllByText('No prior sync result')).toHaveLength(2);
    expect(screen.getAllByText(/Updated 2026-07-19T10:00:00Z/)).toHaveLength(2);
  });

  it('shows authoritative schedule, fresh result, and bounded item error details', async () => {
    const latest = run('completed', 10, {
      completed_at: '2026-07-19T11:30:00Z', errors_count: 1,
      item_errors: [{
        source_message_id: 'm-1', phase: 'ingest', error_kind: 'mime_error',
        error_message: 'Malformed MIME header', created_at: '2026-07-19T11:30:00Z'
      }]
    });
    const fetchFn = vi.fn<typeof fetch>(async () => Response.json({ sources: [source({
      scheduled: true, schedule: '0 */6 * * *', next_sync_at: '2026-07-19T18:00:00Z', latest_sync: latest
    })] }));
    render(SourcesWorkspace, {
      client: createAPIClient(fetchFn), now: () => new Date('2026-07-19T12:00:00Z')
    });

    expect(await screen.findByText('Scheduled · 0 */6 * * *')).toBeDefined();
    expect(screen.getByText('Next 2026-07-19T18:00:00Z')).toBeDefined();
    expect(screen.queryByText('stale_last_result')).toBeNull();
    expect(screen.getByText('1 item error')).toBeDefined();
    expect(screen.getByText('Malformed MIME header')).toBeDefined();
  });

  it('names a terminal result older than the documented threshold as stale', async () => {
    const fetchFn = vi.fn<typeof fetch>(async () => Response.json({ sources: [source({
      latest_sync: run('completed', 10, { completed_at: '2026-07-17T11:00:00Z' })
    })] }));
    render(SourcesWorkspace, {
      client: createAPIClient(fetchFn), now: () => new Date('2026-07-19T12:00:00Z')
    });

    expect(await screen.findByText('stale_last_result')).toBeDefined();
  });

  it('keeps polling through idle status races until the accepted run appears and completes', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    let statusReads = 0;
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      if (request.method === 'POST') return Response.json({ status: 'accepted', message: 'started' }, { status: 202 });
      statusReads += 1;
      if (statusReads === 1) return Response.json({ sources: [source()] });
      if (statusReads <= 3) return Response.json({ sources: [source()] });
      if (statusReads === 4) return Response.json({ sources: [source({ can_sync: false, sync_unavailable_reason: 'sync_already_running', active_sync: run('running', 5) })] });
      return Response.json({ sources: [source({ latest_sync: run('completed', 8, { completed_at: '2026-07-19T10:01:00Z' }) })] });
    });
    const rendered = render(SourcesWorkspace, { client: createAPIClient(fetchFn) });

    await fireEvent.click(await screen.findByRole('button', { name: 'Sync now Archive' }));
    expect(requests[1]!.method).toBe('POST');
    expect(new URL(requests[1]!.url).pathname).toBe('/api/v1/sync/archive%40example.com');

    await vi.advanceTimersByTimeAsync(500);
    expect(screen.getByText('Awaiting accepted sync run…')).toBeDefined();
    await vi.advanceTimersByTimeAsync(1_000);
    await waitFor(() => expect(screen.getByText('5 processed')).toBeDefined());
    await vi.advanceTimersByTimeAsync(500);
    await waitFor(() => expect(screen.getByText('Completed')).toBeDefined());

    expect(requests.every((request) => request.headers.get('Accept') !== 'text/event-stream')).toBe(true);
    expect(requests.every((request) => ['/api/v1/sources/status', '/api/v1/sync/archive%40example.com'].includes(new URL(request.url).pathname))).toBe(true);
    rendered.unmount();
  });

  it('observes an accepted sync that finishes between status polls', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    let statusReads = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      if (request.method === 'POST') {
        return Response.json({ status: 'accepted', message: 'started' }, { status: 202 });
      }
      statusReads += 1;
      if (statusReads === 1) {
        return Response.json({ sources: [source({
          latest_sync: run('completed', 4, { id: 9, completed_at: '2026-07-19T10:00:00Z' })
        })] });
      }
      return Response.json({ sources: [source({
        latest_sync: run('completed', 7, { id: 10, completed_at: '2026-07-19T10:01:00Z' })
      })] });
    });
    const rendered = render(SourcesWorkspace, { client: createAPIClient(fetchFn) });

    await fireEvent.click(await screen.findByRole('button', { name: 'Sync now Archive' }));
    await waitFor(() => expect(statusReads).toBe(2));

    expect(screen.getByText('Completed')).toBeDefined();
    await waitFor(() => expect(screen.queryByText('Awaiting accepted sync run…')).toBeNull());
    expect(screen.queryByText('sync_start_not_observed')).toBeNull();
    rendered.unmount();
  });

  it('stops awaiting an accepted run at the configured cap with a named state', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    let reads = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      if (request.method === 'POST') return Response.json({ status: 'accepted', message: 'started' }, { status: 202 });
      reads += 1;
      return Response.json({ sources: [source()] });
    });
    const rendered = render(SourcesWorkspace, {
      client: createAPIClient(fetchFn), maxAwaitingPolls: 3
    });

    await fireEvent.click(await screen.findByRole('button', { name: 'Sync now Archive' }));
    await vi.advanceTimersByTimeAsync(500);
    await vi.advanceTimersByTimeAsync(1_000);

    expect(await screen.findByText('sync_start_not_observed')).toBeDefined();
    expect(reads).toBe(4);
    await vi.advanceTimersByTimeAsync(8_000);
    expect(reads).toBe(4);
    rendered.unmount();
  });

  it('names conflicting runs', async () => {
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      if (request.method === 'POST') {
        return Response.json({ error: 'sync_error', message: 'sync already running for archive@example.com' }, { status: 409 });
      }
      return Response.json({ sources: [source()] });
    });
    const rendered = render(SourcesWorkspace, { client: createAPIClient(fetchFn) });

    await fireEvent.click(await screen.findByRole('button', { name: 'Sync now Archive' }));
    expect((await screen.findByRole('alert')).textContent).toContain('already running');
    rendered.unmount();
  });

  it('retries an idle status failure without an active sync or an awaited accepted run', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    let reads = 0;
    const fetchFn = vi.fn<typeof fetch>(async () => {
      reads += 1;
      if (reads === 1) throw new Error('status endpoint unreachable');
      return Response.json({ sources: [source()] });
    });
    const rendered = render(SourcesWorkspace, { client: createAPIClient(fetchFn) });

    expect((await screen.findByRole('alert')).textContent).toContain('status endpoint unreachable');
    expect(reads).toBe(1);

    // No source is syncing and no accepted run is awaited, so this retry
    // relies on the error path bypassing the active-sync gate.
    await vi.advanceTimersByTimeAsync(500);
    await waitFor(() => expect(reads).toBe(2));
    expect(await screen.findByRole('heading', { name: 'Archive' })).toBeDefined();
    expect(screen.queryByRole('alert')).toBeNull();
    rendered.unmount();
  });

  it("continues polling source A when source B's trigger is rejected", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    let statusReads = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      if (request.method === 'POST') {
        return Response.json({ error: 'sync_error', message: 'sync already running elsewhere' }, { status: 409 });
      }
      statusReads += 1;
      return Response.json({ sources: [
        source({
          id: 1, identifier: 'active@example.com', display_name: 'Active', can_sync: false,
          sync_unavailable_reason: 'sync_already_running', active_sync: run('running', statusReads)
        }),
        source({ id: 2, identifier: 'ready@example.com', display_name: 'Ready' })
      ] });
    });
    const rendered = render(SourcesWorkspace, { client: createAPIClient(fetchFn) });

    await screen.findByText('1 processed');
    await fireEvent.click(screen.getByRole('button', { name: 'Sync now Ready' }));
    expect((await screen.findByRole('alert')).textContent).toContain('already running elsewhere');
    await vi.advanceTimersByTimeAsync(500);

    await waitFor(() => expect(screen.getByText('2 processed')).toBeDefined());
    expect(screen.getByRole('alert').textContent).toContain('already running elsewhere');
    expect(statusReads).toBe(2);
    rendered.unmount();
  });

  it('aborts a stale status poll when unmounted', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const signals: AbortSignal[] = [];
    let statusReads = 0;
    const fetchFn = vi.fn<typeof fetch>((input) => {
      const request = input instanceof Request ? input : new Request(input);
      statusReads += 1;
      signals.push(request.signal);
      if (statusReads === 1) {
        return Promise.resolve(Response.json({ sources: [source({
          can_sync: false, sync_unavailable_reason: 'sync_already_running', active_sync: run('running', 5)
        })] }));
      }
      return new Promise<Response>(() => undefined);
    });
    const rendered = render(SourcesWorkspace, { client: createAPIClient(fetchFn) });
    await screen.findByText('5 processed');
    await vi.advanceTimersByTimeAsync(500);
    await waitFor(() => expect(signals).toHaveLength(2));

    rendered.unmount();

    expect(signals[1]!.aborted).toBe(true);
  });
});
