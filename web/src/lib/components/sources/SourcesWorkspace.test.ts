import { tick } from 'svelte';
import { fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { createCaptureClient } from '../../telemetry/capture.test-support';
import SourcesWorkspace from './SourcesWorkspace.svelte';
import { formatDateTime } from '../../util/format';
import { cardDAVRepairCopy } from '../../carddav/account-settings';
import { sourceStatus, syncRunStatus } from '../../jobs/fixtures.test-support';

function source(overrides: Record<string, unknown> = {}) {
  return sourceStatus({ display_name: 'Archive', ...overrides });
}

function run(status: string, processed: number, overrides: Record<string, unknown> = {}) {
  return syncRunStatus({
    id: 9, started_at: '2026-07-19T10:00:00Z', completed_at: ['running', 'paused'].includes(status) ? null : '2026-07-19T10:01:00Z', status,
    messages_processed: processed, messages_added: processed, ...overrides
  });
}

const contact = {
  connection: 'default', orphaned: false,
  status: {
    configured: true, available: true, credential_configured: true, enabled: false, scheduled: false, schedule: '',
    account: { base_url: 'https://contacts.example.com/', username: 'user-a' },
    latest: { id: 1, state: 'succeeded', started_at: '2026-07-19T10:00:00Z', finished_at: '2026-07-19T10:00:05Z',
      books: 1, created: 4, updated: 0, removed: 0, trigger: 'manual', full: false, account_id: 1 },
    latest_successful: { id: 1, state: 'succeeded', started_at: '2026-07-19T10:00:00Z',
      finished_at: '2026-07-19T10:00:05Z', books: 1, created: 4, updated: 0, removed: 0, trigger: 'manual',
      full: false, account_id: 1 }
  }
};

type Handler = (url: URL, request: Request) => Response | Promise<Response> | undefined;

// Routes the sources page reads. A handler answers first; anything else is empty.
function api(sources: unknown[], handler: Handler = () => undefined, contacts: Array<{ connection: string; status: unknown }> = []) {
  return vi.fn<typeof fetch>(async (input) => {
    const request = input as Request;
    const url = new URL(request.url);
    const custom = handler(url, request);
    if (custom) return custom;
    if (url.pathname.endsWith('/sources/status')) return Response.json({ sources });
    if (url.pathname.endsWith('/carddav/connections')) return Response.json({ connections: contacts });
    if (url.pathname.endsWith('/carddav/status')) return Response.json(contacts.find(contact => contact.connection === url.searchParams.get('connection'))?.status);
    if (url.pathname.endsWith('/carddav/books')) return Response.json({ books: [] });
    if (url.pathname.endsWith('/carddav/runs')) return Response.json({ runs: [] });
    return Response.json({}, { status: 404 });
  });
}

afterEach(() => {
  vi.useRealTimers();
  vi.restoreAllMocks();
});

const createAPIClient = (fetchFn: typeof fetch) => createCaptureClient(async (input, init) => {
  const request = input instanceof Request ? input : new Request(input, init);
  if (new URL(request.url).pathname.endsWith('/carddav/connections')) return Response.json({ connections: [] });
  return fetchFn(input, init);
}, []);
const NOT_OBSERVED = "The sync was requested, but it hasn't started yet.";

describe('SourcesWorkspace', () => {
  it('shows a status-load failure and Refresh inside the open source panel', async () => {
    let statusReads = 0;
    const fetchFn = api([source()], url => {
      if (url.pathname.endsWith('/sources/status') && ++statusReads === 2) {
        return Response.json({ message: 'Source status unavailable' }, { status: 500 });
      }
    });
    const rendered = render(SourcesWorkspace, { client: createAPIClient(fetchFn) });
    await fireEvent.click(await screen.findByRole('button', { name: 'Details for Archive' }));
    const panel = screen.getByRole('dialog', { name: 'Archive' });

    rendered.component.refresh();

    const alert = await within(panel).findByRole('alert');
    expect(alert.textContent).toContain('Source status unavailable');
    await fireEvent.click(within(alert).getByRole('button', { name: 'Refresh' }));
    await waitFor(() => expect(statusReads).toBe(3));
    await waitFor(() => expect(within(panel).queryByText('Source status unavailable')).toBeNull());
    expect(screen.getByRole('dialog', { name: 'Archive' })).toBe(panel);
  });

  it('shows a readable type and a muted reason with the raw code in its tooltip', async () => {
    const fetchFn = api([source({ id: 1, identifier: 'import@example.com', display_name: 'Imported', can_sync: false, sync_unavailable_reason: 'source_not_schedulable', source_type: 'mbox' })]);
    render(SourcesWorkspace, { client: createAPIClient(fetchFn) });

    expect(await screen.findByText('Mbox import')).toBeDefined();
    await fireEvent.click(screen.getByRole('button', { name: 'Details for Imported' }));
    expect(await screen.findByText('Mbox import · import@example.com')).toBeDefined();
    await fireEvent.click(screen.getByRole('button', { name: 'Close' }));
    expect(screen.getByText('Imported file — nothing to sync').getAttribute('title')).toBe('source_not_schedulable');
    expect(screen.queryByText('source_not_schedulable')).toBeNull();
    expect(screen.getByRole('columnheader', { name: 'Status' })).toBeDefined();
  });

  it('keeps the cron text in the schedule tooltip only', async () => {
    const fetchFn = vi.fn<typeof fetch>(async () => Response.json({ sources: [source({
      scheduled: true, schedule: '0 */6 * * *', next_sync_at: '2026-07-19T18:00:00Z'
    })] }));
    render(SourcesWorkspace, { client: createAPIClient(fetchFn) });
    await fireEvent.click(await screen.findByRole('button', { name: 'Details for Archive' }));
    const summary = await screen.findByText('At :00 past every 6th hour');
    expect(summary.getAttribute('title')).toBe('0 */6 * * *');
    expect(screen.queryByText('0 */6 * * *')).toBeNull();
  });

  it('shows status and only exposes Sync now from server capability truth', async () => {
    const fetchFn = vi.fn<typeof fetch>(async () => Response.json({ sources: [
      source(),
      source({ id: 2, source_type: 'imap', identifier: 'readonly@example.com', display_name: null,
        scheduled: false })
    ] }));
    render(SourcesWorkspace, { client: createAPIClient(fetchFn) });

    expect(await screen.findByRole('table', { name: 'Sources' })).toBeDefined();
    expect(screen.getByText('Archive')).toBeDefined();
    expect(screen.getByRole('columnheader', { name: 'Last good sync' })).toBeDefined();
    expect(screen.getByRole('button', { name: 'Sync now Archive' })).toBeDefined();
    expect(screen.queryByRole('button', { name: 'Sync now readonly@example.com' })).toBeNull();
    expect(screen.getByText('Sync not set up')).toBeDefined();
    await fireEvent.click(screen.getByRole('button', { name: 'Details for readonly@example.com' }));
    expect(screen.getByText('Not scheduled')).toBeDefined();
    expect(within(screen.getByRole('table', { name: 'Sources' })).getAllByText('Never synced')).toHaveLength(2);
  });

  it('presents generic meeting imports as on-demand API sources', async () => {
    const fetchFn = vi.fn<typeof fetch>(async () => Response.json({ sources: [source({
      source_type: 'meeting_import',
      identifier: 'local-meetings',
      display_name: 'Local Meetings',
      can_sync: false,
      scheduled: false,
      sync_unavailable_reason: 'source_not_schedulable'
    })] }));
    render(SourcesWorkspace, { client: createAPIClient(fetchFn) });

    expect(await screen.findByText('On demand')).toBeDefined();
    await fireEvent.click(screen.getByRole('button', { name: 'Details for Local Meetings' }));
    expect(screen.getByText('Meeting import · local-meetings')).toBeDefined();
    expect(screen.queryByText('Not scheduled')).toBeNull();
    expect(screen.queryByText('source_not_schedulable')).toBeNull();
    expect(screen.queryByRole('button', { name: 'Sync now Local Meetings' })).toBeNull();
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
    vi.setSystemTime(new Date('2026-07-19T12:00:00Z'));
    render(SourcesWorkspace, { client: createAPIClient(fetchFn) });

    await fireEvent.click(await screen.findByRole('button', { name: 'Details for Archive' }));
    expect(await screen.findByText('At :00 past every 6th hour')).toBeDefined();
    expect(screen.getByTitle(formatDateTime('2026-07-19T18:00:00Z', 'long'))).toBeDefined();
    expect(screen.getByText('10 processed · 10 added · 1 error')).toBeDefined();
    expect(screen.queryByTitle(/T\d\d:\d\d:\d\dZ$/)).toBeNull();
    expect(screen.queryByText('This result may be out of date.')).toBeNull();
    await fireEvent.click(screen.getByText('1 item failed'));
    expect(screen.getByText('Malformed MIME header')).toBeDefined();
  });

  it('shows the last successful sync with a readable full date in its tooltip', async () => {
    const fetchFn = vi.fn<typeof fetch>(async () => Response.json({ sources: [source({
      last_successful_sync: run('completed', 4, { completed_at: '2026-07-18T08:15:00Z' })
    })] }));
    render(SourcesWorkspace, { client: createAPIClient(fetchFn) });

    expect(await screen.findByTitle(formatDateTime('2026-07-18T08:15:00Z', 'long'))).toBeDefined();
    expect(screen.queryByTitle('2026-07-18T08:15:00Z')).toBeNull();
  });

  it('names a terminal result older than the documented threshold as stale', async () => {
    const fetchFn = vi.fn<typeof fetch>(async () => Response.json({ sources: [source({
      latest_sync: run('completed', 10, { completed_at: '2026-07-17T11:00:00Z' })
    })] }));
    vi.setSystemTime(new Date('2026-07-19T12:00:00Z'));
    render(SourcesWorkspace, { client: createAPIClient(fetchFn) });

    expect(await screen.findByText('This result may be out of date.')).toBeDefined();
  });

  it('disables the other source panel while a start is pending and posts its next enabled click', async () => {
    let finish!: (response: Response) => void;
    const posts: string[] = [];
    const fetchFn = api([
      source(), source({ id: 2, display_name: 'Other', identifier: 'other@example.com' })
    ], (url, request) => {
      if (request.method !== 'POST') return;
      posts.push(url.pathname);
      if (posts.length === 1) return new Promise<Response>(resolve => { finish = resolve; });
      return Response.json({ message: 'Other sync rejected' }, { status: 500 });
    });
    const view = render(SourcesWorkspace, { client: createCaptureClient(fetchFn, []) });
    try {
      await fireEvent.click(await screen.findByRole('button', { name: 'Sync now Archive' }));
      await waitFor(() => expect(posts).toEqual(['/api/v1/sync/archive%40example.com']));
      await fireEvent.click(screen.getByRole('button', { name: 'Details for Other' }));
      const panel = screen.getByRole('dialog', { name: 'Other' });
      const button = within(panel).getByRole('button', { name: /^Sync now$/ });
      expect((button as HTMLButtonElement).disabled).toBe(true);
      finish(Response.json({ message: 'Archive sync rejected' }, { status: 500 }));
      await waitFor(() => expect((button as HTMLButtonElement).disabled).toBe(false));
      await fireEvent.click(button);
      expect(await within(panel).findByText('Other sync rejected')).toBeDefined();
      expect(posts).toEqual(['/api/v1/sync/archive%40example.com', '/api/v1/sync/other%40example.com']);
    } finally { view.unmount(); }
  });

  it('reports each rejected click despite an unchanged stored run', async () => {
    const events: unknown[] = [];
    const fetchFn = api([source({ latest_sync: run('failed', 0) })], (_url, request) =>
      request.method === 'POST' ? Response.json({ message: 'Credentials unavailable' }, { status: 500 }) : undefined
    );
    render(SourcesWorkspace, { client: createCaptureClient(fetchFn, events) });
    await screen.findByText('Failed');
    expect(events).toEqual([]);
    for (const count of [1, 2]) {
      await fireEvent.click(screen.getByRole('button', { name: 'Sync now Archive' }));
      await screen.findByText('Credentials unavailable');
      await waitFor(() => expect(events).toHaveLength(count));
    }
    expect(events).toEqual(Array(2).fill({ event: 'error_shown', properties: { surface: 'web', kind: 'sync' } }));
  });

  it('shows panel failure repair and queued or executing sync controls', async () => {
    const fetchFn = api([
      source({ identifier: 'archive@example.com', display_name: 'Alex', can_sync: false,
        sync_unavailable_reason: 'sync_not_configured',
        latest_sync: run('failed', 0, {
          error_message: 'oauth2: token expired and refresh token is invalid (invalid_grant)' }) }),
      source({ id: 2, display_name: 'Waiting', identifier: 'waiting@example.com', can_sync: false, scheduler_queued: true, sync_unavailable_reason: 'sync_already_running', latest_sync: run('completed', 5) }), source({ id: 3, display_name: 'Executing', identifier: 'executing@example.com', scheduler_queued: true, scheduler_started_at: '2026-07-19T10:00:00Z', active_sync: null, latest_sync: run('completed', 5) })
    ]);
    render(SourcesWorkspace, { client: createCaptureClient(fetchFn, []) });

    await fireEvent.click(await screen.findByRole('button', { name: 'Details for Alex' }));
    expect(await screen.findByText('oauth2: token expired and refresh token is invalid (invalid_grant)')).toBeDefined();
    expect(screen.getByRole('button', { name: /^Copy msgvault sync/ })).toBeDefined();

    await fireEvent.click(screen.getByRole('button', { name: 'Details for Alex' }));
    await waitFor(() => expect(screen.queryByText(/invalid_grant/)).toBeNull());
    const queued = screen.getByRole('row', { name: /Waiting/ });
    expect(within(queued).getByText('Sync queued')).toBeDefined();
    const executing = screen.getByRole('row', { name: /Executing/ });
    expect(within(executing).getByText('Syncing…')).toBeDefined();
  });

  it('shows shared attachment rules and opens the existing Sources settings editor', async () => {
    const onOpenSettings = vi.fn();
    const settings = [{ key: 'beeper.media', kind: 'boolean', value: { boolean: true } }, { key: 'beeper.max_media_mb', kind: 'integer', value: { integer: 0 }, validation: { off: { value: '0', label: 'Beeper default of 250 MiB' } } }];
    const fetchFn = api([source({ source_type: 'beeper', identifier: 'beeper', display_name: 'Beeper', scheduled: true, schedule: '0 3 * * *', next_sync_at: '2026-07-19T11:00:00Z' })], url => url.pathname.endsWith('/settings') ? Response.json({ settings }) : undefined);
    render(SourcesWorkspace, { client: createCaptureClient(fetchFn, []), onOpenSettings });
    await fireEvent.click(await screen.findByRole('button', { name: 'Details for Beeper' }));
    await screen.findByText('Beeper default of 250 MiB');
    expect(screen.getByText('Defaults for Beeper accounts. Account settings can override these values.')).toBeDefined();
    expect(screen.getByText('On')).toBeDefined();
    await fireEvent.click(screen.getByRole('button', { name: 'Edit in Settings › Sources' }));
    expect(onOpenSettings).toHaveBeenCalledExactlyOnceWith('sources');
  });

  it('keeps contacts usable when mail status fails', async () => {
    const fetch = api([], url => url.pathname.endsWith('/sources/status') ? Response.json({ message: 'Mail status unavailable' }, { status: 500 }) : undefined, [contact]);
    const view = render(SourcesWorkspace, { client: createCaptureClient(fetch, []) });
    expect(await screen.findByRole('button', { name: 'Details for user-a' })).toBeDefined();
    await waitFor(() => expect((screen.getByRole('button', { name: 'Sync now user-a' }) as HTMLButtonElement).disabled).toBe(false));
    expect(screen.getByRole('alert').textContent).toContain('Mail status unavailable');
    view.unmount();
  });

  it('uses Connections for idle contacts without reading statuses or address books', async () => {
    const fetch = api([source()], () => undefined, [contact]);
    const view = render(SourcesWorkspace, { client: createCaptureClient(fetch, []) });
    expect(await screen.findByRole('button', { name: 'Details for user-a' })).toBeDefined();
    expect(screen.queryByText('No sources yet')).toBeNull();
    expect((screen.getByRole('button', { name: 'Sync now Archive' }) as HTMLButtonElement).disabled).toBe(false);
    expect((screen.getByRole('button', { name: 'Sync now user-a' }) as HTMLButtonElement).disabled).toBe(false);
    expect(fetch.mock.calls.map(([input]) => new URL((input as Request).url).pathname).sort()).toEqual(['/api/v1/carddav/connections', '/api/v1/sources/status']);
    view.unmount();
  });

  it('offers contacts sync-state retry in the row and panel', async () => {
    let statusReads = 0;
    const fetch = api([], url => {
      if (url.pathname.endsWith('/carddav/status') && ++statusReads === 1) return Response.json({}, { status: 500 });
      if (url.pathname.endsWith('/carddav/sync')) return Response.json({ status: 'succeeded' });
    }, [contact]);
    const view = render(SourcesWorkspace, { client: createCaptureClient(fetch, []) });
    await waitFor(() => expect((screen.getByRole('button', { name: 'Sync now user-a' }) as HTMLButtonElement).disabled).toBe(false));
    await fireEvent.click(screen.getByRole('button', { name: 'Sync now user-a' }));
    const table = screen.getByRole('table', { name: 'Sources' });
    await within(table).findByRole('button', { name: 'Retry CardDAV sync state' });
    expect((within(table).getByRole('button', { name: 'Sync now user-a' }) as HTMLButtonElement).disabled).toBe(true);
    await fireEvent.click(screen.getByRole('button', { name: 'Details for user-a' }));
    const panel = screen.getByRole('dialog', { name: 'user-a' });
    await fireEvent.click(within(panel).getByRole('button', { name: 'Retry CardDAV sync state' }));
    await waitFor(() => expect(screen.queryByRole('button', { name: 'Retry CardDAV sync state' })).toBeNull());
    expect((within(table).getByRole('button', { name: 'Sync now user-a' }) as HTMLButtonElement).disabled).toBe(false);
    view.unmount();
  });

  it('opens source setup and sync history from an empty workspace', async () => {
    const onOpenOperations = vi.fn();
    render(SourcesWorkspace, { client: createCaptureClient(api([]), []), onOpenOperations });
    expect(await screen.findByText('No sources yet')).toBeDefined();
    expect(screen.getByText('msgvault add-account user@example.com')).toBeDefined();
    await fireEvent.click(screen.getByRole('button', { name: 'Add source' }));
    expect(screen.getAllByText('msgvault add-account user@example.com')).toHaveLength(2);
    await fireEvent.click(screen.getByRole('button', { name: 'Sync history' }));
    expect(onOpenOperations).toHaveBeenCalledOnce();
  });

  it('keeps polling through idle races until the accepted run completes', async () => {
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
    const triggerURL = new URL(requests[1]!.url);
    expect(triggerURL.pathname).toBe('/api/v1/sync/archive%40example.com');
    expect(triggerURL.searchParams.get('source_type')).toBe('gmail');

    await vi.advanceTimersByTimeAsync(500);
    expect(screen.getByText('Awaiting accepted sync run…')).toBeDefined();
    await vi.advanceTimersByTimeAsync(1_000);
    await waitFor(() => expect(screen.getByText('5 processed')).toBeDefined());
    expect(screen.getByText('Running')).toBeDefined();
    await vi.advanceTimersByTimeAsync(500);
    await waitFor(() => expect(screen.getByText('Succeeded')).toBeDefined());

    expect(requests.every((request) => request.headers.get('Accept') !== 'text/event-stream')).toBe(true);
    expect(requests.every((request) => ['/api/v1/sources/status', '/api/v1/sync/archive%40example.com'].includes(new URL(request.url).pathname))).toBe(true);
    rendered.unmount();
  });

  it('keeps polling while the scheduler still holds the sync lock after a run completes', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    let statusReads = 0;
    const completed = () => run('completed', 8, { completed_at: '2026-07-19T10:01:00Z' });
    const fetchFn = vi.fn<typeof fetch>(async () => {
      statusReads += 1;
      if (statusReads === 1) {
        return Response.json({ sources: [source({
          can_sync: false, sync_unavailable_reason: 'sync_already_running', active_sync: run('running', 5)
        })] });
      }
      // The run has completed (no active_sync), but the scheduler still
      // reports sync_already_running while it rebuilds caches.
      if (statusReads <= 3) {
        return Response.json({ sources: [source({
          can_sync: false, sync_unavailable_reason: 'sync_already_running', latest_sync: completed()
        })] });
      }
      return Response.json({ sources: [source({ latest_sync: completed() })] });
    });
    const rendered = render(SourcesWorkspace, { client: createAPIClient(fetchFn) });

    await screen.findByText('5 processed');
    await vi.advanceTimersByTimeAsync(500);
    await waitFor(() => expect(statusReads).toBe(2));
    expect(screen.getByText('Sync in progress')).toBeDefined();
    expect(screen.queryByRole('button', { name: 'Sync now Archive' })).toBeNull();

    // Polling continues (with backoff) while the lock is held...
    await vi.advanceTimersByTimeAsync(1_000);
    await waitFor(() => expect(statusReads).toBe(3));
    await vi.advanceTimersByTimeAsync(2_000);
    await waitFor(() => expect(statusReads).toBe(4));

    // ...and stops once the lock clears and Sync now is available again.
    await waitFor(() => expect(screen.getByRole('button', { name: 'Sync now Archive' })).toBeDefined());
    await vi.advanceTimersByTimeAsync(8_000);
    expect(statusReads).toBe(4);
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

    expect(screen.getByText('Succeeded')).toBeDefined();
    await waitFor(() => expect(screen.queryByText('Awaiting accepted sync run…')).toBeNull());
    expect(screen.queryByText(NOT_OBSERVED)).toBeNull();
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

    expect(await screen.findByText(NOT_OBSERVED)).toBeDefined();
    expect(reads).toBe(4);
    await vi.advanceTimersByTimeAsync(8_000);
    expect(reads).toBe(4);
    rendered.unmount();
  });

  it('sends source_type and triggers successfully for a generic (non-account) source', async () => {
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      if (request.method === 'POST') return Response.json({ status: 'accepted', message: 'started' }, { status: 202 });
      return Response.json({ sources: [source({
        id: 3, source_type: 'granola', identifier: 'default', display_name: 'Granola'
      })] });
    });
    const rendered = render(SourcesWorkspace, { client: createAPIClient(fetchFn) });

    await fireEvent.click(await screen.findByRole('button', { name: 'Sync now Granola' }));

    const triggerRequest = requests.find((request) => request.method === 'POST');
    expect(triggerRequest).toBeDefined();
    const triggerURL = new URL(triggerRequest!.url);
    expect(triggerURL.pathname).toBe('/api/v1/sync/default');
    expect(triggerURL.searchParams.get('source_type')).toBe('granola');
    expect(screen.queryByRole('alert')).toBeNull();
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

  it('releases Sync now buttons after a timeout and refreshes status without retriggering', async () => {
    let statusReads = 0;
    let triggers = 0;
    const fetchFn = vi.fn<typeof fetch>((input) => {
      const request = input instanceof Request ? input : new Request(input);
      if (request.method === 'POST') {
        triggers += 1;
        return new Promise<Response>((_resolve, reject) => {
          request.signal.addEventListener('abort', () => reject(request.signal.reason), { once: true });
        });
      }
      statusReads += 1;
      return Promise.resolve(Response.json({ sources: [
        source(statusReads === 1 ? {} : {
          can_sync: false, sync_unavailable_reason: 'sync_already_running', active_sync: run('running', 5)
        }),
        source({ id: 2, identifier: 'other@example.com', display_name: 'Other' })
      ] }));
    });
    const rendered = render(SourcesWorkspace, {
      client: createAPIClient(fetchFn), requestTimeoutMs: 20
    });

    const archiveButton = await screen.findByRole<HTMLButtonElement>('button', { name: 'Sync now Archive' });
    const otherButton = screen.getByRole<HTMLButtonElement>('button', { name: 'Sync now Other' });
    await fireEvent.click(archiveButton);
    expect(archiveButton.disabled).toBe(true);
    expect(otherButton.disabled).toBe(true);
    expect((await screen.findByRole('alert')).textContent).toMatch(/timed out/i);
    expect(archiveButton.disabled).toBe(false);
    expect(otherButton.disabled).toBe(false);
    await fireEvent.click(screen.getByRole('button', { name: 'Refresh' }));
    expect(await screen.findByText('5 processed')).toBeDefined();
    expect((screen.getByRole('button', { name: 'Sync now Archive' }) as HTMLButtonElement).disabled).toBe(true);
    expect(triggers).toBe(1);
    rendered.unmount();
  });

  it('holds a first-load error until manual Retry', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    let reads = 0;
    const events: unknown[] = [], requests: Promise<void>[] = [];
    const fetchFn = vi.fn<typeof fetch>(async input => {
      const path = new URL((input as Request).url).pathname;
      if (path.endsWith('/carddav/connections')) return Response.json({ connections: [] });
      if (!path.endsWith('/sources/status')) return Response.json({}, { status: 404 });
      reads += 1;
      if (reads === 1) throw new Error('status endpoint unreachable');
      return Response.json({ sources: [source()] });
    });
    const rendered = render(SourcesWorkspace, { client: createCaptureClient(fetchFn, events, requests) });

    expect((await screen.findByRole('alert')).textContent).toContain('status endpoint unreachable');
    expect(reads).toBe(1);
    await tick();
    await Promise.all(requests);
    expect(events).toEqual([]);

    await vi.advanceTimersByTimeAsync(8_000);
    expect(reads).toBe(1);
    await fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
    await waitFor(() => expect(reads).toBe(2));
    expect(await screen.findByText('Archive')).toBeDefined();
    expect(screen.queryByRole('alert')).toBeNull();
    rendered.unmount();
  });

  it('holds a first-load response processing error until manual Retry', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    let reads = 0;
    const fetchFn = vi.fn<typeof fetch>(async () => {
      reads += 1;
      return Response.json(reads === 1 ? { sources: {} } : { sources: [source()] });
    });
    const rendered = render(SourcesWorkspace, { client: createAPIClient(fetchFn) });

    expect(await screen.findByRole('alert')).toBeDefined();
    await vi.advanceTimersByTimeAsync(8_000);
    expect(reads).toBe(1);
    await fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
    expect(await screen.findByText('Archive')).toBeDefined();
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

  it('aborts a stalled first status load and offers Retry', async () => {
    const signals: AbortSignal[] = [];
    const fetchFn = vi.fn<typeof fetch>((input) => {
      const request = input instanceof Request ? input : new Request(input);
      signals.push(request.signal);
      if (signals.length > 1) return Promise.resolve(Response.json({ sources: [source()] }));
      return new Promise<Response>((_resolve, reject) => {
        request.signal.addEventListener('abort', () => reject(request.signal.reason), { once: true });
      });
    });
    const rendered = render(SourcesWorkspace, {
      client: createAPIClient(fetchFn), requestTimeoutMs: 20
    });

    expect((await screen.findByRole('alert')).textContent).toMatch(/timed out/i);
    expect(signals[0]!.aborted).toBe(true);
    expect(screen.queryByText('Loading…')).toBeNull();
    await fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
    expect(await screen.findByText('Archive')).toBeDefined();
    rendered.unmount();
  });

  it('defers loading in a hidden tab and bounds the visible reload', async () => {
    const hidden = vi.spyOn(document, 'hidden', 'get').mockReturnValue(true);
    const fetchFn = vi.fn<typeof fetch>((input) => {
      const request = input instanceof Request ? input : new Request(input);
      return new Promise<Response>((_resolve, reject) => {
        request.signal.addEventListener('abort', () => reject(request.signal.reason), { once: true });
      });
    });
    const rendered = render(SourcesWorkspace, { client: createAPIClient(fetchFn), requestTimeoutMs: 20 });

    expect(screen.queryByText('Loading…')).toBeNull();
    expect(fetchFn).not.toHaveBeenCalled();
    hidden.mockReturnValue(false);
    document.dispatchEvent(new Event('visibilitychange'));
    expect((await screen.findByRole('alert')).textContent).toMatch(/timed out/i);
    expect(fetchFn).toHaveBeenCalledOnce();
    rendered.unmount();
  });

  it('bounds a later status poll after the first load succeeds', async () => {
    let reads = 0;
    const fetchFn = vi.fn<typeof fetch>((input) => {
      const request = input instanceof Request ? input : new Request(input);
      reads += 1;
      if (reads === 1) return Promise.resolve(Response.json({ sources: [source({
        can_sync: false, sync_unavailable_reason: 'sync_already_running', active_sync: run('running', 1)
      })] }));
      return new Promise<Response>((_resolve, reject) => {
        request.signal.addEventListener('abort', () => reject(request.signal.reason), { once: true });
      });
    });
    const rendered = render(SourcesWorkspace, {
      client: createAPIClient(fetchFn), requestTimeoutMs: 20
    });

    expect(await screen.findByText('1 processed')).toBeDefined();
    expect((await screen.findByRole('alert')).textContent).toMatch(/timed out/i);
    expect(reads).toBe(2);
    rendered.unmount();
  });

  it('resumes bounded polling after an active-sync status timeout', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    let reads = 0;
    let timedOutSignal: AbortSignal | undefined;
    const fetchFn = vi.fn<typeof fetch>((input) => {
      const request = input instanceof Request ? input : new Request(input);
      reads += 1;
      if (reads === 2) {
        timedOutSignal = request.signal;
        return new Promise<Response>((_resolve, reject) => {
          request.signal.addEventListener('abort', () => reject(request.signal.reason), { once: true });
        });
      }
      if (reads === 1) return Promise.resolve(Response.json({ sources: [source({
        can_sync: false, sync_unavailable_reason: 'sync_already_running', active_sync: run('running', 1)
      })] }));
      return Promise.resolve(Response.json({ sources: [source({
        latest_sync: run('completed', 3, { completed_at: '2026-07-19T10:01:00Z' })
      })] }));
    });
    const rendered = render(SourcesWorkspace, {
      client: createAPIClient(fetchFn), requestTimeoutMs: 20
    });

    expect(await screen.findByText('1 processed')).toBeDefined();
    await vi.advanceTimersByTimeAsync(10_000);
    await waitFor(() => expect(timedOutSignal?.aborted).toBe(true));
    await vi.advanceTimersByTimeAsync(10_000);
    expect(reads).toBe(3);
    expect(screen.queryByRole('alert')).toBeNull();
    rendered.unmount();
  });

  it('keeps a newer load pending when an obsolete request settles', async () => {
    let resolveOld: ((value: Response) => void) | undefined;
    let resolveNew: ((value: Response) => void) | undefined;
    const fetchFn = vi.fn<typeof fetch>(() => new Promise<Response>((resolve) => {
      if (!resolveOld) resolveOld = resolve;
      else resolveNew = resolve;
    }));
    const rendered = render(SourcesWorkspace, { client: createAPIClient(fetchFn) });
    await waitFor(() => expect(fetchFn).toHaveBeenCalledOnce());

    const hidden = vi.spyOn(document, 'hidden', 'get').mockReturnValue(true);
    document.dispatchEvent(new Event('visibilitychange'));
    hidden.mockReturnValue(false);
    document.dispatchEvent(new Event('visibilitychange'));
    await waitFor(() => expect(fetchFn).toHaveBeenCalledTimes(2));
    resolveOld!(Response.json({ sources: [source({ display_name: 'Obsolete' })] }));
    await Promise.resolve();
    expect(screen.getByText('Loading…')).toBeDefined();
    expect(screen.queryByText('Obsolete')).toBeNull();

    resolveNew!(Response.json({ sources: [source()] }));
    expect(await screen.findByText('Archive')).toBeDefined();
    rendered.unmount();
  });

  it('caps lock-hold polling and offers a manual refresh', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    let reads = 0;
    const held = source({ can_sync: false, sync_unavailable_reason: 'sync_already_running' });
    const fetchFn = vi.fn<typeof fetch>(async () => {
      reads += 1;
      return Response.json({ sources: [held] });
    });
    const rendered = render(SourcesWorkspace, {
      client: createAPIClient(fetchFn), maxLockHoldPolls: 3
    });

    await screen.findByText('Sync in progress');
    await vi.advanceTimersByTimeAsync(30_000);
    expect(reads).toBe(4);
    expect(screen.getByText(/Automatic refresh paused/)).toBeDefined();
    await vi.advanceTimersByTimeAsync(30_000);
    expect(reads).toBe(4);
    await fireEvent.click(screen.getByRole('button', { name: 'Refresh' }));
    expect(reads).toBe(5);
    rendered.unmount();
  });

  it('resets the lock-hold poll budget after a real active sync appears', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    let reads = 0;
    const held = source({ can_sync: false, sync_unavailable_reason: 'sync_already_running' });
    const fetchFn = vi.fn<typeof fetch>(async () => {
      reads += 1;
      if (reads === 3) return Response.json({ sources: [source({
        can_sync: false, sync_unavailable_reason: 'sync_already_running', active_sync: run('running', 1)
      })] });
      return Response.json({ sources: [held] });
    });
    const rendered = render(SourcesWorkspace, {
      client: createAPIClient(fetchFn), maxLockHoldPolls: 2
    });

    await screen.findByText('Sync in progress');
    await vi.advanceTimersByTimeAsync(30_000);
    expect(reads).toBe(6);
    expect(screen.getByText(/Automatic refresh paused/)).toBeDefined();
    rendered.unmount();
  });

  it('does not bypass the lock-poll cap after a status error', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    let reads = 0;
    const fetchFn = vi.fn<typeof fetch>(async () => {
      reads += 1;
      if (reads === 1) return Response.json({ sources: [source({
        can_sync: false, sync_unavailable_reason: 'sync_already_running'
      })] });
      throw new Error('status unavailable');
    });
    const rendered = render(SourcesWorkspace, {
      client: createAPIClient(fetchFn), maxLockHoldPolls: 1
    });

    expect(await screen.findByText('Sync in progress')).toBeDefined();
    await vi.advanceTimersByTimeAsync(30_000);
    expect(reads).toBe(2);
    expect(screen.getByRole('alert').textContent).toContain('status unavailable');
    rendered.unmount();
  });

  it('counts timed-out lock-hold retries toward the poll cap', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    let reads = 0;
    const timedOutSignals: AbortSignal[] = [];
    const fetchFn = vi.fn<typeof fetch>((input) => {
      const request = input instanceof Request ? input : new Request(input);
      reads += 1;
      if (reads === 1) return Promise.resolve(Response.json({ sources: [source({
        can_sync: false, sync_unavailable_reason: 'sync_already_running'
      })] }));
      timedOutSignals.push(request.signal);
      return new Promise<Response>((_resolve, reject) => {
        request.signal.addEventListener('abort', () => reject(request.signal.reason), { once: true });
      });
    });
    const rendered = render(SourcesWorkspace, {
      client: createAPIClient(fetchFn), requestTimeoutMs: 20, maxLockHoldPolls: 3
    });

    expect(await screen.findByText('Sync in progress')).toBeDefined();
    for (let expectedReads = 2; expectedReads <= 4; expectedReads += 1) {
      await vi.advanceTimersByTimeAsync(10_000);
      expect(reads).toBe(expectedReads);
      await waitFor(() => expect(timedOutSignals[expectedReads - 2]?.aborted).toBe(true));
    }
    await vi.advanceTimersByTimeAsync(10_000);
    expect(reads).toBe(4);
    expect(screen.getByText(/Automatic refresh paused/)).toBeDefined();
    rendered.unmount();
  });
});

it('publishes source rows while the optional contacts request waits', async () => {
  vi.useFakeTimers();
  let reads = 0;
  const client = createCaptureClient(async input => (input as Request).url.endsWith('/sources/status')
    ? Response.json({ sources: [++reads === 1 ? source({ active_sync: run('running', 5, { messages_added: 3, errors_count: 2 }) }) : source({ latest_sync: run('completed', 8) })] })
    : new Promise<Response>(() => undefined), []);
  const view = render(SourcesWorkspace, { client });
  try {
    await vi.advanceTimersByTimeAsync(0);
    expect(screen.getByText('5 processed')).toBeDefined();
    expect(screen.getByText('3 added · 2 errors')).toBeDefined();
    await vi.advanceTimersByTimeAsync(1000);
    expect(screen.getByText('Succeeded')).toBeDefined();
    expect(reads).toBe(2);
  } finally { view.unmount(); }
});

it.each(['failed', 'partial'])('shows the latest contacts %s error in the account panel', async state => {
  const connection = { ...contact, status: { ...contact.status, latest: { ...contact.status.latest, state, error_code: 'upstream_failed', error_message: 'CardDAV server request failed.' } } };
  const view = render(SourcesWorkspace, { client: createCaptureClient(api([], () => undefined, [connection]), []) });
  try {
    await fireEvent.click(await screen.findByRole('button', { name: 'Details for user-a' }));
    expect(within(screen.getByRole('region', { name: 'Failure' })).getByText('CardDAV server request failed.')).toBeDefined();
  } finally { view.unmount(); }
});

it('shows OAuth app repair instructions for contacts rather than assuming expiry', async () => {
  const onOpenSettings = vi.fn();
  render(SourcesWorkspace, { client: createCaptureClient(api([], () => undefined, [{ ...contact, status: { ...contact.status, repair_reason: 'google_authorization_required', latest: { ...contact.status.latest, state: 'partial', error_code: 'upstream_failed', error_message: 'CardDAV server request failed.' } } }]), []), onOpenSettings });
  await fireEvent.click(await screen.findByRole('button', { name: 'Details for user-a' }));
  expect(within(screen.getByRole('region', { name: 'Failure' })).getByText(cardDAVRepairCopy.google_authorization_required)).toBeDefined();
  expect(screen.queryByText('CardDAV server request failed.')).toBeNull();
  await fireEvent.click(screen.getByRole('button', { name: 'Open Settings › Contacts' }));
  expect(onOpenSettings).toHaveBeenCalledExactlyOnceWith('carddav', 'default');
});

it.each(['error', 'timeout'])('recovers a contacts-only load %s without claiming the archive has no sources', async failure => {
  let contactsReads = 0;
  const fetch = vi.fn<typeof globalThis.fetch>(async input => {
    const request = input as Request;
    if (request.url.endsWith('/sources/status')) return Response.json({ sources: [] });
    if (request.url.endsWith('/carddav/connections')) {
      if (++contactsReads > 1) return Response.json({ connections: [contact] });
      if (failure === 'error') return Response.json({}, { status: 500 });
      return new Promise<Response>((_resolve, reject) => request.signal.addEventListener('abort', () => reject(request.signal.reason), { once: true }));
    }
    if (request.url.includes('/carddav/status')) return Response.json(contact.status);
    if (request.url.includes('/carddav/books')) return Response.json({ books: [] });
    return Response.json({}, { status: 404 });
  });
  const view = render(SourcesWorkspace, { client: createCaptureClient(fetch, []), requestTimeoutMs: 20 });
  try {
    expect((await screen.findByRole('alert')).textContent).toMatch(/contacts/i);
    expect(screen.queryByText('No sources yet')).toBeNull();
    await fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
    expect(await screen.findByRole('button', { name: 'Details for user-a' })).toBeDefined();
    expect(screen.queryByRole('alert')).toBeNull();
  } finally { view.unmount(); }
});

it('publishes usable contacts when mail status pends', async () => {
  vi.useFakeTimers();
  let reads = 0;
  const client = createCaptureClient(api([], url => {
    if (url.pathname.endsWith('/sources/status')) return new Promise<Response>(() => undefined);
    if (url.pathname.endsWith('/carddav/status') && url.searchParams.get('connection') === 'slow') return new Promise<Response>(() => undefined);
    if (url.pathname.endsWith('/carddav/status')) return Response.json(++reads === 1
      ? { ...contact.status, active: { ...contact.status.latest, state: 'running', finished_at: null } }
      : contact.status);
  }, [{ ...contact, status: { ...contact.status, active: { ...contact.status.latest, state: 'running', finished_at: null } } }, { ...contact, connection: 'slow', status: { ...contact.status, active: { ...contact.status.latest, state: 'running', finished_at: null }, account: { ...contact.status.account, username: 'user-b' } } }]), []);
  const view = render(SourcesWorkspace, { client });
  try {
    await vi.advanceTimersByTimeAsync(0);
    expect(within(screen.getByRole('row', { name: /user-a/ })).getByText('Running')).toBeDefined();
    await vi.advanceTimersByTimeAsync(1000);
    expect(within(screen.getByRole('row', { name: /user-a/ })).getByText('Succeeded')).toBeDefined();
    expect((screen.getByRole('button', { name: 'Sync now user-a' }) as HTMLButtonElement).disabled).toBe(false);
    expect(within(screen.getByRole('row', { name: /user-b/ })).getByText('Sync in progress')).toBeDefined();
  } finally { view.unmount(); }
});
