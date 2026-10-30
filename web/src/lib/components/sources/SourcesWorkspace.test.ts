import { tick } from 'svelte';
import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { chooseSelectOption } from '../../../test/kit-ui';
import { createCaptureClient } from '../../telemetry/capture.test-support';
import SourcesWorkspace from './SourcesWorkspace.svelte';
import { syncRunStatus } from '../../jobs/fixtures.test-support';

function source(overrides: Record<string, unknown> = {}) {
  return {
    id: 1, source_type: 'gmail', identifier: 'archive@example.com', display_name: 'Archive',
    last_sync_at: '2026-07-19T10:00:00Z', updated_at: '2026-07-19T10:00:00Z',
    active_sync: null, latest_sync: null, last_successful_sync: null,
    can_sync: true, scheduled: false, next_sync_at: null, ...overrides
  };
}

function run(status: string, processed: number, overrides: Record<string, unknown> = {}) {
  return syncRunStatus({
    id: 9, started_at: '2026-07-19T10:00:00Z', completed_at: '2026-07-19T10:01:00Z', status,
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

type Handler = (url: URL, request: Request) => Response | undefined;

// Routes the sources page reads. A handler answers first; anything else is empty.
function api(sources: unknown[], handler: Handler = () => undefined, contacts: unknown[] = []) {
  return vi.fn<typeof fetch>(async (input) => {
    const request = input as Request;
    const url = new URL(request.url);
    const custom = handler(url, request);
    if (custom) return custom;
    if (url.pathname.endsWith('/sources/status')) return Response.json({ sources });
    if (url.pathname.endsWith('/carddav/connections')) return Response.json({ connections: contacts });
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
  it('lists one row per account with its status, last good sync, and Sync now', async () => {
    const fetchFn = api([
      source({ latest_sync: run('completed', 3), last_successful_sync: run('completed', 3) }),
      source({ id: 2, identifier: 'b@example.com', display_name: 'Broken', can_sync: false,
        sync_unavailable_reason: 'sync_not_configured', latest_sync: run('failed', 0, { error_message: 'boom' }) }),
      source({ id: 3, identifier: 'c@example.com', display_name: 'New', can_sync: false })
    ]);
    render(SourcesWorkspace, { client: createCaptureClient(fetchFn, []) });

    expect(await screen.findByText('Archive')).toBeDefined();
    expect(screen.getByText('Succeeded')).toBeDefined();
    expect(screen.getByText('Failed')).toBeDefined();
    expect(screen.getByText('Never synced')).toBeDefined();
    expect(screen.getByRole('button', { name: 'Sync now Archive' })).toBeDefined();
    expect(screen.getAllByText('Not scheduled')).toHaveLength(2);
    expect(screen.queryByText(/Updated/)).toBeNull();
  });

  it('starts a sync and shows Syncing until the run appears', async () => {
    let started = false;
    const fetchFn = api([source()], (url, request) => {
      if (request.method === 'POST' && url.pathname.includes('/sync/')) {
        started = true;
        return Response.json({ status: 'accepted' }, { status: 202 });
      }
      if (started && url.pathname.endsWith('/sources/status')) {
        return Response.json({ sources: [source({ active_sync: run('running', 2), latest_sync: run('running', 2) })] });
      }
      return undefined;
    });
    render(SourcesWorkspace, { client: createCaptureClient(fetchFn, []) });
    await fireEvent.click(await screen.findByRole('button', { name: 'Sync now Archive' }));
    expect((await screen.findAllByText('Syncing…')).length).toBeGreaterThan(0);
    expect(await screen.findByText('Running')).toBeDefined();
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

  it('opens a panel with the plain reason and the command that fixes it, and the row closes it again', async () => {
    const fetchFn = api([
      source({ identifier: 'archive@example.com', display_name: 'Alex', can_sync: false,
        sync_unavailable_reason: 'sync_not_configured',
        latest_sync: run('failed', 0, {
          error_message: 'oauth2: token expired and refresh token is invalid (invalid_grant)' }) })
    ]);
    render(SourcesWorkspace, { client: createCaptureClient(fetchFn, []) });

    await fireEvent.click(await screen.findByRole('button', { name: 'Details for Alex' }));
    expect(await screen.findByText('Gmail sign-in expired')).toBeDefined();
    expect(screen.getByText('msgvault add-account archive@example.com')).toBeDefined();
    expect(screen.getByText('msgvault sync archive@example.com')).toBeDefined();
    expect(screen.queryByText(/invalid_grant/)).toBeNull();

    await fireEvent.click(screen.getByRole('button', { name: 'Details for Alex' }));
    await waitFor(() => expect(screen.queryByText('Gmail sign-in expired')).toBeNull());
  });

  it('saves a schedule through Save changes, turning the source on', async () => {
    const patches: unknown[] = [];
    const settings = [
      { key: 'beeper.enabled', kind: 'boolean', value: { boolean: false }, group: 'sources', label: 'Scheduled Beeper sync' },
      { key: 'beeper.schedule', kind: 'string', value: { string: '' }, group: 'sources', label: 'Beeper schedule',
        validation: { format: 'cron' } }
    ];
    const fetchFn = api([source({ source_type: 'beeper', identifier: 'beeper', display_name: 'Beeper' })],
      (url, request) => {
        if (url.pathname.endsWith('/settings') && request.method === 'GET') {
          return Response.json({ settings, groups: [], pending_restart: false, person_enrichment_providers: [] },
            { headers: { ETag: '"1"' } });
        }
        if (url.pathname.endsWith('/settings') && request.method === 'PATCH') {
          return request.json().then((body) => {
            patches.push(body);
            return Response.json({ settings, groups: [], pending_restart: false }, { headers: { ETag: '"2"' } });
          }) as unknown as Response;
        }
        return undefined;
      });
    render(SourcesWorkspace, { client: createCaptureClient(fetchFn, []) });
    await fireEvent.click(await screen.findByRole('button', { name: 'Details for Beeper' }));
    await chooseSelectOption(await screen.findByRole('combobox', { name: 'Presets: Off' }), 'Every hour');
    await fireEvent.click(screen.getByRole('button', { name: 'Details for Beeper' }));
    expect(await screen.findByRole('dialog', { name: 'Discard changes?' })).toBeDefined();
    await fireEvent.click(screen.getByRole('button', { name: 'Keep editing' }));
    await fireEvent.click(await screen.findByRole('button', { name: 'Save changes' }));
    await waitFor(() => expect(patches).toHaveLength(1));
    expect(patches[0]).toEqual({ updates: [
      { key: 'beeper.enabled', value: { boolean: true } },
      { key: 'beeper.schedule', value: { string: '0 * * * *' } }
    ] });
  });

  it('keeps a schedule draft after a conflicting save and shows a required restart', async () => {
    const settings = [
      { key: 'beeper.enabled', kind: 'boolean', value: { boolean: true }, group: 'sources', label: 'Scheduled sync' },
      { key: 'beeper.schedule', kind: 'string', value: { string: '0 3 * * *' }, group: 'sources', label: 'Schedule', validation: { format: 'cron' } },
    ];
    const patches: unknown[] = [];
    const client = createCaptureClient(api([source({ source_type: 'beeper', identifier: 'beeper', display_name: 'Beeper' })], (url, request) => {
      if (!url.pathname.endsWith('/settings')) return undefined;
      if (request.method === 'GET') return Response.json({ settings, pending_restart: false }, { headers: { ETag: '"2"' } });
      return request.json().then(body => {
        patches.push(body);
        return patches.length === 1 ? Response.json({ message: 'Changed' }, { status: 412 }) : Response.json({ settings, pending_restart: true });
      }) as unknown as Response;
    }), []);
    render(SourcesWorkspace, { client });
    await fireEvent.click(await screen.findByRole('button', { name: 'Details for Beeper' }));
    await chooseSelectOption(await screen.findByRole('combobox', { name: 'Presets: Every day at 03:00' }), 'Every hour');
    await fireEvent.click(await screen.findByRole('button', { name: 'Save changes' }));
    await screen.findByText('The configuration changed on disk. Review the latest values and save again.');
    await fireEvent.click(screen.getByRole('button', { name: 'Save changes' }));
    await screen.findByText('Restart msgvault to apply these changes.');
    expect(patches).toEqual([
      { updates: [{ key: 'beeper.schedule', value: { string: '0 * * * *' } }] },
      { updates: [{ key: 'beeper.schedule', value: { string: '0 * * * *' } }] },
    ]);
  });

  it('shows the next sync under the schedule in the source and contact panels', async () => {
    const settings = [
      { key: 'beeper.enabled', kind: 'boolean', value: { boolean: true }, group: 'sources', label: 'Scheduled Beeper sync' },
      { key: 'beeper.schedule', kind: 'string', value: { string: '0 * * * *' }, group: 'sources', label: 'Beeper schedule',
        validation: { format: 'cron' } }
    ];
    const scheduled = { ...contact, status: { ...contact.status, enabled: true, scheduled: true, schedule: '0 * * * *',
      next_scheduled_at: '2026-07-19T12:00:00Z' } };
    const fetchFn = api([source({ source_type: 'beeper', identifier: 'beeper', display_name: 'Beeper',
      scheduled: true, schedule: '0 * * * *', next_sync_at: '2026-07-19T11:00:00Z' })],
      (url, request) => {
        if (url.pathname.endsWith('/settings') && request.method === 'GET') {
          return Response.json({ settings, groups: [], pending_restart: false, person_enrichment_providers: [] },
            { headers: { ETag: '"1"' } });
        }
        return undefined;
      }, [scheduled]);
    render(SourcesWorkspace, { client: createCaptureClient(fetchFn, []) });

    await fireEvent.click(await screen.findByRole('button', { name: 'Details for Beeper' }));
    const next = await screen.findByText(/Next sync/);
    expect(next.querySelector('time')?.getAttribute('datetime')).toBe('2026-07-19T11:00:00Z');

    await fireEvent.click(screen.getByRole('button', { name: 'Details for user-a' }));
    await waitFor(() => expect(screen.getByText(/Next sync/).querySelector('time')?.getAttribute('datetime')).toBe('2026-07-19T12:00:00Z'));
  });

  it.each(['gmail', 'imap'])('shows config-only %s schedules and next sync without editable controls', async (source_type) => {
    render(SourcesWorkspace, { client: createAPIClient(api([source({ source_type, scheduled: true, schedule: '0 * * * *', next_sync_at: '2026-07-19T11:00:00Z' })])) });
    await fireEvent.click(await screen.findByRole('button', { name: 'Details for Archive' }));
    expect(await screen.findByText('At :00 past every hour')).toBeDefined();
    expect(screen.getByText(/Next sync/).querySelector('time')?.getAttribute('datetime')).toBe('2026-07-19T11:00:00Z');
    expect(screen.queryByRole('combobox')).toBeNull();
  });

  it('groups contact connections under Contacts', async () => {
    render(SourcesWorkspace, { client: createCaptureClient(api([source()], () => undefined, [contact]), []) });
    expect(await screen.findByText('user-a')).toBeDefined();
    expect(screen.getByRole('rowheader', { name: 'Contacts' })).toBeDefined();
    expect(screen.getByRole('button', { name: 'Sync now user-a' })).toBeDefined();
  });

  it('shows the add-account command when there are no sources, and from Add source', async () => {
    render(SourcesWorkspace, { client: createCaptureClient(api([]), []) });
    expect(await screen.findByText('No sources yet')).toBeDefined();
    expect(screen.getByText('msgvault add-account you@gmail.com')).toBeDefined();
    await fireEvent.click(screen.getByRole('button', { name: 'Add source' }));
    expect(screen.getAllByText('msgvault add-account you@gmail.com')).toHaveLength(2);
  });  it('keeps polling through idle races until the accepted run completes', async () => {
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
    expect(screen.getByText('Syncing…')).toBeDefined();
    expect((screen.getByRole('button', { name: 'Sync now Archive' }) as HTMLButtonElement).disabled).toBe(true);

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
    const fetchFn = vi.fn<typeof fetch>(async () => {
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

    await screen.findByText('Syncing…');
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

    await screen.findByText('Syncing…');
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

    expect(await screen.findByText('Syncing…')).toBeDefined();
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

    expect(await screen.findByText('Syncing…')).toBeDefined();
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
