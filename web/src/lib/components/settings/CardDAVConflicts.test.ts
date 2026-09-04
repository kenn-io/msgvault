import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../../api/client';
import { CardDAVConflictsController } from '../../carddav/conflicts-controller.svelte';
import CardDAVConflicts from './CardDAVConflicts.svelte';
import CardDAVSettingsWorkspace from './CardDAVSettingsWorkspace.svelte';

const forbidden = {
  raw_vcard: 'BEGIN:VCARD\nFN:FORBIDDEN-VCARD\nEND:VCARD',
  url: 'https://forbidden-url.example.test/dav',
  href: '/forbidden-href/contact.vcf',
  etag: 'forbidden-etag',
  hash: 'forbidden-hash',
  uid: 'forbidden-uid',
  header: 'Authorization: forbidden-header',
  credential: 'forbidden-credential'
};

function listItem(id: number, overrides: Record<string, unknown> = {}) {
  return {
    id,
    address_book: { id: 7, name: 'Synthetic contacts', ...forbidden },
    status: 'unresolved',
    local_state: 'present',
    remote_state: 'deleted',
    allowed_resolutions: ['keep_local', 'keep_remote'],
    updated_at: '2026-08-28T10:00:00Z',
    ...forbidden,
    ...overrides
  };
}

function contactSummary(state: 'present' | 'deleted' | 'unavailable', overrides: Record<string, unknown> = {}) {
  return { state, emails: [], phones: [], ...forbidden, ...overrides };
}

function conflictDetail(id: number, overrides: Record<string, unknown> = {}) {
  return {
    id,
    address_book: { id: 7, name: 'Synthetic contacts', ...forbidden },
    status: 'unresolved',
    base: contactSummary('present', {
      display_name: 'Synthetic Contact',
      emails: ['contact@example.test'],
      phones: ['+1 555 0100'],
      truncated: true
    }),
    local: contactSummary('deleted', { display_name: 'MUST-NOT-RENDER-LOCAL' }),
    remote: contactSummary('unavailable', { emails: ['must-not-render@example.test'] }),
    allowed_resolutions: ['keep_remote'],
    created_at: '2026-08-27T10:00:00Z',
    updated_at: '2026-08-28T10:00:00Z',
    ...forbidden,
    ...overrides
  };
}

function requestOf(input: RequestInfo | URL): Request {
  return input instanceof Request ? input : new Request(input);
}

function deferredResponse() {
  let resolve!: (response: Response) => void;
  const promise = new Promise<Response>((settle) => { resolve = settle; });
  return { promise, resolve };
}

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

describe('CardDAVConflicts', () => {
  it('renders only safe comparison summaries with explicit present, deleted, unavailable, and truncated text', async () => {
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const path = new URL(requestOf(input).url).pathname;
      if (path.endsWith('/41')) return Response.json(conflictDetail(41));
      return Response.json({ conflicts: [listItem(41)] });
    });
    const controller = new CardDAVConflictsController(createAPIClient(fetchFn));
    await controller.load();
    const rendered = render(CardDAVConflicts, { controller });

    await fireEvent.click(screen.getByRole('button', { name: 'Review conflict 41 in Synthetic contacts' }));
    const detail = await screen.findByRole('region', { name: 'CardDAV conflict 41 comparison' });

    expect(within(detail).getByRole('heading', { name: 'Base' })).toBeDefined();
    expect(within(detail).getByRole('heading', { name: 'Local' })).toBeDefined();
    expect(within(detail).getByRole('heading', { name: 'Remote' })).toBeDefined();
    expect(within(detail).getByText('Present')).toBeDefined();
    expect(within(detail).getByText('Synthetic Contact')).toBeDefined();
    expect(within(detail).getByText('contact@example.test')).toBeDefined();
    expect(within(detail).getByText('+1 555 0100')).toBeDefined();
    expect(within(detail).getByText('Additional name, email, or phone values are not shown.')).toBeDefined();
    expect(within(detail).getByText('Deleted. This side is a deletion tombstone.')).toBeDefined();
    expect(within(detail).getByText('Unavailable. No safe comparison summary is available.')).toBeDefined();
    expect(screen.getByText('Only display name, email addresses, and phone numbers are shown. Your choice applies to the whole card.')).toBeDefined();
    expect(screen.getByRole('button', { name: 'Keep remote card' })).toBeDefined();
    expect(screen.queryByRole('button', { name: 'Keep local card' })).toBeNull();
    expect(rendered.container.textContent).not.toMatch(/FORBIDDEN-VCARD|MUST-NOT-RENDER|must-not-render/i);
    expect(rendered.container.innerHTML).not.toMatch(/forbidden-(?:url|href|etag|hash|uid|header|credential)/i);
    controller.destroy();
  });

  it('shows honest empty safe fields and no actions for a resolved detail', async () => {
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const path = new URL(requestOf(input).url).pathname;
      if (path.endsWith('/52')) return Response.json(conflictDetail(52, {
        status: 'resolved',
        resolution: 'keep_local',
        resolved_at: '2026-08-28T11:00:00Z',
        base: contactSummary('present'),
        local: contactSummary('present'),
        remote: contactSummary('present'),
        allowed_resolutions: []
      }));
      return Response.json({ conflicts: [listItem(52, { status: 'resolved', allowed_resolutions: [] })] });
    });
    const controller = new CardDAVConflictsController(createAPIClient(fetchFn));
    await controller.load();
    render(CardDAVConflicts, { controller });

    await fireEvent.click(screen.getByRole('button', { name: 'Review conflict 52 in Synthetic contacts' }));
    await screen.findByRole('region', { name: 'CardDAV conflict 52 comparison' });

    expect(screen.getAllByText('No display name')).toHaveLength(3);
    expect(screen.getAllByText('No email addresses')).toHaveLength(3);
    expect(screen.getAllByText('No phone numbers')).toHaveLength(3);
    expect(screen.getByText('Resolved by keeping the local card.')).toBeDefined();
    expect(screen.queryByRole('button', { name: /Keep (?:local|remote) card/ })).toBeNull();
    controller.destroy();
  });

  it('renders accessible loading, fixed error, GET-only retry, and empty queue states', async () => {
    const first = deferredResponse();
    let listReads = 0;
    const fetchFn = vi.fn<typeof fetch>(async () => {
      listReads += 1;
      if (listReads === 1) return first.promise;
      return Response.json({ conflicts: [] });
    });
    const controller = new CardDAVConflictsController(createAPIClient(fetchFn));
    const rendered = render(CardDAVConflicts, { controller });

    expect(screen.getByLabelText('CardDAV conflict queue').getAttribute('aria-busy')).toBe('true');
    expect(screen.getByText('Loading CardDAV conflicts…')).toBeDefined();
    const load = controller.load();
    first.resolve(Response.json({ error: 'unavailable', message: forbidden.credential }, { status: 503 }));
    await load;
    expect((await screen.findByRole('alert')).textContent).toContain('Unable to load CardDAV conflicts.');
    expect(rendered.container.textContent).not.toContain(forbidden.credential);

    await fireEvent.click(screen.getByRole('button', { name: 'Retry CardDAV conflicts' }));
    expect(await screen.findByText('No unresolved CardDAV conflicts.')).toBeDefined();
    expect(listReads).toBe(2);
    controller.destroy();
  });

  it('renders typed unavailable as optional setup without detail or mutation and recovers on load', async () => {
    let configured = false;
    const requests: Array<{ method: string; path: string }> = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      const path = new URL(request.url).pathname;
      requests.push({ method: request.method, path });
      if (path === '/api/v1/carddav/conflicts') {
        if (!configured) {
          return Response.json({ error: 'carddav_unavailable', message: forbidden.credential }, { status: 503 });
        }
        return Response.json({ conflicts: [listItem(41)] });
      }
      if (path === '/api/v1/carddav/conflicts/41') return Response.json(conflictDetail(41));
      throw new Error(`Unexpected ${request.method} ${path}`);
    });
    const controller = new CardDAVConflictsController(createAPIClient(fetchFn));
    await controller.load();
    const rendered = render(CardDAVConflicts, { controller });

    expect(screen.getByText('CardDAV conflict review is unavailable.')).toBeDefined();
    expect(screen.getByText('Configure or repair CardDAV in Settings before reviewing conflicts.')).toBeDefined();
    expect(screen.queryByRole('alert')).toBeNull();
    expect(screen.queryByRole('button', { name: 'Retry CardDAV conflicts' })).toBeNull();
    expect(rendered.container.textContent).not.toContain(forbidden.credential);
    expect(requests).toEqual([{ method: 'GET', path: '/api/v1/carddav/conflicts' }]);

    configured = true;
    await controller.load();
    await fireEvent.click(await screen.findByRole('button', { name: 'Review conflict 41 in Synthetic contacts' }));
    expect(await screen.findByRole('region', { name: 'CardDAV conflict 41 comparison' })).toBeDefined();
    expect(requests).toEqual([
      { method: 'GET', path: '/api/v1/carddav/conflicts' },
      { method: 'GET', path: '/api/v1/carddav/conflicts' },
      { method: 'GET', path: '/api/v1/carddav/conflicts/41' }
    ]);
    controller.destroy();
  });

  it('moves focus to the unavailable status when an in-queue request replaces the focused surface', async () => {
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const path = new URL(requestOf(input).url).pathname;
      if (path === '/api/v1/carddav/conflicts') {
        return Response.json({ conflicts: [listItem(41)] });
      }
      if (path === '/api/v1/carddav/conflicts/41') {
        return Response.json({ error: 'carddav_unavailable' }, { status: 503 });
      }
      throw new Error(`Unexpected ${path}`);
    });
    const controller = new CardDAVConflictsController(createAPIClient(fetchFn));
    await controller.load();
    render(CardDAVConflicts, { controller });

    const row = screen.getByRole('button', { name: 'Review conflict 41 in Synthetic contacts' });
    row.focus();
    expect(document.activeElement).toBe(row);
    await fireEvent.click(row);

    const status = await screen.findByRole('status', { name: 'CardDAV conflict review is unavailable.' });
    await waitFor(() => expect(document.activeElement).toBe(status));
    expect(document.activeElement?.isConnected).toBe(true);
    expect(screen.getAllByRole('status', { name: 'CardDAV conflict review is unavailable.' })).toHaveLength(1);
    controller.destroy();
  });

  it('does not steal focus when unavailable is the initial surface state', async () => {
    const fetchFn = vi.fn<typeof fetch>(async () => (
      Response.json({ error: 'carddav_unavailable' }, { status: 503 })
    ));
    const controller = new CardDAVConflictsController(createAPIClient(fetchFn));
    await controller.load();
    render(CardDAVConflicts, { controller });

    const status = screen.getByRole('status', { name: 'CardDAV conflict review is unavailable.' });
    expect(document.activeElement).not.toBe(status);
    expect(screen.getAllByRole('status', { name: 'CardDAV conflict review is unavailable.' })).toHaveLength(1);
    controller.destroy();
  });

  it('does not reclaim focus after an ordinary null-target departure from the available surface', async () => {
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const path = new URL(requestOf(input).url).pathname;
      if (path === '/api/v1/carddav/conflicts') return Response.json({ conflicts: [listItem(41)] });
      if (path === '/api/v1/carddav/conflicts/41') {
        return Response.json({ error: 'carddav_unavailable' }, { status: 503 });
      }
      throw new Error(`Unexpected ${path}`);
    });
    const controller = new CardDAVConflictsController(createAPIClient(fetchFn));
    await controller.load();
    render(CardDAVConflicts, { controller });

    const row = screen.getByRole('button', { name: 'Review conflict 41 in Synthetic contacts' });
    row.focus();
    row.blur();
    expect(document.activeElement).not.toBe(row);
    await controller.select(41);

    const status = await screen.findByRole('status', { name: 'CardDAV conflict review is unavailable.' });
    expect(document.activeElement).not.toBe(status);
    controller.destroy();
  });

  it('does not overwrite focus moved outside while an unavailable response is pending', async () => {
    const detail = deferredResponse();
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const path = new URL(requestOf(input).url).pathname;
      if (path === '/api/v1/carddav/conflicts') return Response.json({ conflicts: [listItem(41)] });
      if (path === '/api/v1/carddav/conflicts/41') return detail.promise;
      throw new Error(`Unexpected ${path}`);
    });
    const controller = new CardDAVConflictsController(createAPIClient(fetchFn));
    await controller.load();
    render(CardDAVConflicts, { controller });
    const outside = document.createElement('button');
    outside.textContent = 'Outside conflict surface';
    document.body.append(outside);

    const row = screen.getByRole('button', { name: 'Review conflict 41 in Synthetic contacts' });
    row.focus();
    const selection = controller.select(41);
    outside.focus();
    detail.resolve(Response.json({ error: 'carddav_unavailable' }, { status: 503 }));
    await selection;

    await screen.findByRole('status', { name: 'CardDAV conflict review is unavailable.' });
    await waitFor(() => expect(document.activeElement).toBe(outside));
    outside.remove();
    controller.destroy();
  });

  it('does not overwrite connected external focus moved after unavailable replacement renders', async () => {
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const path = new URL(requestOf(input).url).pathname;
      if (path === '/api/v1/carddav/conflicts') return Response.json({ conflicts: [listItem(41)] });
      if (path === '/api/v1/carddav/conflicts/41') {
        return Response.json({ error: 'carddav_unavailable' }, { status: 503 });
      }
      throw new Error(`Unexpected ${path}`);
    });
    const controller = new CardDAVConflictsController(createAPIClient(fetchFn));
    await controller.load();
    render(CardDAVConflicts, { controller });
    const outside = document.createElement('button');
    outside.textContent = 'External race target';
    document.body.append(outside);
    let unavailableFocusEvents = 0;
    const recordUnavailableFocus = (event: FocusEvent) => {
      if (event.target instanceof Element && event.target.matches(
        '[role="status"][aria-label="CardDAV conflict review is unavailable."]'
      )) unavailableFocusEvents += 1;
    };
    document.addEventListener('focusin', recordUnavailableFocus);
    const observer = new MutationObserver(() => {
      const renderedStatus = document.querySelector(
        '[role="status"][aria-label="CardDAV conflict review is unavailable."]'
      );
      if (renderedStatus) outside.focus();
    });
    observer.observe(document.body, { childList: true, subtree: true });

    const row = screen.getByRole('button', { name: 'Review conflict 41 in Synthetic contacts' });
    row.focus();
    await fireEvent.click(row);

    await screen.findByRole('status', { name: 'CardDAV conflict review is unavailable.' });
    await waitFor(() => expect(document.activeElement).toBe(outside));
    expect(unavailableFocusEvents).toBe(0);
    observer.disconnect();
    document.removeEventListener('focusin', recordUnavailableFocus);
    outside.remove();
    controller.destroy();
  });

  it('moves modal-owned focus to unavailable status when ambiguous resolution reconciliation loses CardDAV', async () => {
    let resolving = false;
    let posts = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      const path = new URL(request.url).pathname;
      if (request.method === 'POST') {
        posts += 1;
        resolving = true;
        return Response.json({ error: 'carddav_write_failed' }, { status: 503 });
      }
      if (resolving) return Response.json({ error: 'carddav_unavailable' }, { status: 503 });
      if (path === '/api/v1/carddav/conflicts') return Response.json({ conflicts: [listItem(41)] });
      if (path === '/api/v1/carddav/conflicts/41') return Response.json(conflictDetail(41));
      throw new Error(`Unexpected ${request.method} ${path}`);
    });
    const controller = new CardDAVConflictsController(createAPIClient(fetchFn));
    await controller.load();
    render(CardDAVConflicts, { controller });

    await fireEvent.click(screen.getByRole('button', { name: 'Review conflict 41 in Synthetic contacts' }));
    await fireEvent.click(await screen.findByRole('button', { name: 'Keep remote card' }));
    const dialog = screen.getByRole('dialog', { name: 'Keep remote CardDAV card' });
    const confirm = within(dialog).getByRole('button', { name: 'Keep remote card' });
    confirm.focus();
    expect(document.activeElement).toBe(confirm);
    await fireEvent.click(confirm);

    const status = await screen.findByRole('status', { name: 'CardDAV conflict review is unavailable.' });
    await waitFor(() => expect(document.activeElement).toBe(status));
    expect(posts).toBe(1);
    controller.destroy();
  });

  it('clears an open decision intent across unavailable and requires a fresh choice after recovery', async () => {
    let configured = true;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      const path = new URL(request.url).pathname;
      if (path === '/api/v1/carddav/conflicts') {
        return configured
          ? Response.json({ conflicts: [listItem(41)] })
          : Response.json({ error: 'carddav_unavailable' }, { status: 503 });
      }
      if (path === '/api/v1/carddav/conflicts/41') return Response.json(conflictDetail(41));
      throw new Error(`Unexpected ${request.method} ${path}`);
    });
    const controller = new CardDAVConflictsController(createAPIClient(fetchFn));
    await controller.load();
    render(CardDAVConflicts, { controller });
    await fireEvent.click(screen.getByRole('button', { name: 'Review conflict 41 in Synthetic contacts' }));
    await fireEvent.click(await screen.findByRole('button', { name: 'Keep remote card' }));
    expect(screen.getByRole('dialog')).toBeDefined();

    configured = false;
    await controller.retryList();
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
    expect(screen.getByText('CardDAV conflict review is unavailable.')).toBeDefined();

    configured = true;
    await controller.load();
    await fireEvent.click(await screen.findByRole('button', { name: 'Review conflict 41 in Synthetic contacts' }));
    expect(await screen.findByRole('button', { name: 'Keep remote card' })).toBeDefined();
    expect(screen.queryByRole('dialog')).toBeNull();

    await fireEvent.click(screen.getByRole('button', { name: 'Keep remote card' }));
    expect(screen.getByRole('dialog')).toBeDefined();
    controller.destroy();
  });

  it('keeps a transport list failure actionable with GET-only retry', async () => {
    let reads = 0;
    const fetchFn = vi.fn<typeof fetch>(async () => {
      reads += 1;
      if (reads === 1) throw new TypeError('synthetic connection reset');
      return Response.json({ conflicts: [] });
    });
    const controller = new CardDAVConflictsController(createAPIClient(fetchFn));
    await controller.load();
    render(CardDAVConflicts, { controller });

    expect(screen.getByRole('alert').textContent).toContain('Unable to load CardDAV conflicts.');
    await fireEvent.click(screen.getByRole('button', { name: 'Retry CardDAV conflicts' }));
    expect(await screen.findByText('No unresolved CardDAV conflicts.')).toBeDefined();
    expect(reads).toBe(2);
    controller.destroy();
  });

  it('removes a resolved row, announces the exact receipt once, and focuses the next connected row', async () => {
    let posts = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      const path = new URL(request.url).pathname;
      if (request.method === 'POST') {
        posts += 1;
        return Response.json({ id: 41, status: 'resolved', resolution: 'keep_remote' });
      }
      if (path.endsWith('/41')) return Response.json(conflictDetail(41));
      return Response.json({ conflicts: [listItem(41), listItem(42)] });
    });
    const controller = new CardDAVConflictsController(createAPIClient(fetchFn));
    await controller.load();
    render(CardDAVConflicts, { controller });

    await fireEvent.click(screen.getByRole('button', { name: 'Review conflict 41 in Synthetic contacts' }));
    await screen.findByRole('button', { name: 'Keep remote card' });
    await fireEvent.click(screen.getByRole('button', { name: 'Keep remote card' }));
    await fireEvent.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Keep remote card' }));

    await waitFor(() => expect(screen.queryByRole('button', { name: 'Review conflict 41 in Synthetic contacts' })).toBeNull());
    const next = screen.getByRole('button', { name: 'Review conflict 42 in Synthetic contacts' });
    await waitFor(() => expect(document.activeElement).toBe(next));
    expect(screen.getAllByRole('status')).toHaveLength(1);
    expect(screen.getByRole('status').textContent).toBe('CardDAV conflict 41 resolved by keeping the remote card.');
    expect(posts).toBe(1);
    controller.destroy();
  });

  it('focuses the stable queue heading when the resolved row has no connected neighbor', async () => {
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      const path = new URL(request.url).pathname;
      if (request.method === 'POST') {
        return Response.json({ id: 41, status: 'resolved', resolution: 'keep_remote' });
      }
      if (path.endsWith('/41')) return Response.json(conflictDetail(41));
      return Response.json({ conflicts: [listItem(41)] });
    });
    const controller = new CardDAVConflictsController(createAPIClient(fetchFn));
    await controller.load();
    render(CardDAVConflicts, { controller });

    await fireEvent.click(screen.getByRole('button', { name: 'Review conflict 41 in Synthetic contacts' }));
    await fireEvent.click(await screen.findByRole('button', { name: 'Keep remote card' }));
    await fireEvent.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Keep remote card' }));

    const heading = screen.getByRole('heading', { name: 'Unresolved conflicts' });
    await waitFor(() => expect(document.activeElement).toBe(heading));
    expect(screen.getByText('No unresolved CardDAV conflicts.')).toBeDefined();
    controller.destroy();
  });

  it('closes stale intent, locks actions after failed reconciliation, and retries GETs only', async () => {
    let posts = 0;
    let listReads = 0;
    let detailReads = 0;
    let failReads = true;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      const path = new URL(request.url).pathname;
      if (request.method === 'POST') {
        posts += 1;
        return Response.json({ error: 'carddav_conflict_stale' }, { status: 409 });
      }
      if (path === '/api/v1/carddav/conflicts') {
        listReads += 1;
        if (listReads > 1 && failReads) return Response.json({ error: 'unavailable' }, { status: 503 });
        return Response.json({ conflicts: [listItem(41)] });
      }
      detailReads += 1;
      if (detailReads > 1 && failReads) return Response.json({ error: 'unavailable' }, { status: 503 });
      return Response.json(conflictDetail(41, { allowed_resolutions: ['keep_local'] }));
    });
    const controller = new CardDAVConflictsController(createAPIClient(fetchFn));
    await controller.load();
    render(CardDAVConflicts, { controller });
    await fireEvent.click(screen.getByRole('button', { name: 'Review conflict 41 in Synthetic contacts' }));
    await screen.findByRole('button', { name: 'Keep local card' });
    await fireEvent.click(screen.getByRole('button', { name: 'Keep local card' }));
    await fireEvent.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Keep local card' }));

    expect(await screen.findByText('Current CardDAV conflict state is unknown. Retry state before resolving it.')).toBeDefined();
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
    expect(screen.queryByRole('button', { name: 'Keep local card' })).toBeNull();
    expect(posts).toBe(1);

    failReads = false;
    await fireEvent.click(screen.getByRole('button', { name: 'Retry conflict state' }));
    expect(await screen.findByRole('button', { name: 'Keep local card' })).toBeDefined();
    expect([posts, listReads, detailReads]).toEqual([1, 3, 3]);
    controller.destroy();
  });

  it('aborts a pending resolution and prevents late focus after the Settings CardDAV context is destroyed', async () => {
    const mutation = deferredResponse();
    let mutationSignal: AbortSignal | undefined;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      const path = new URL(request.url).pathname;
      if (request.method === 'POST') {
        mutationSignal = request.signal;
        return mutation.promise;
      }
      if (path === '/api/v1/carddav/status') return Response.json({ configured: false, available: false, credential_configured: false, enabled: false, scheduled: false, schedule: '' });
      if (path === '/api/v1/carddav/books') return Response.json({ books: [] });
      if (path === '/api/v1/carddav/runs') return Response.json({ runs: [] });
      if (path.endsWith('/41')) return Response.json(conflictDetail(41));
      if (path === '/api/v1/carddav/conflicts') return Response.json({ conflicts: [listItem(41)] });
      throw new Error(`Unexpected ${request.method} ${path}`);
    });
    const focus = vi.spyOn(HTMLElement.prototype, 'focus');
    const rendered = render(CardDAVSettingsWorkspace, { client: createAPIClient(fetchFn), settings: [] });
    await fireEvent.click(await screen.findByRole('button', { name: 'Review conflict 41 in Synthetic contacts' }));
    await fireEvent.click(await screen.findByRole('button', { name: 'Keep remote card' }));
    await fireEvent.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Keep remote card' }));
    await waitFor(() => expect(mutationSignal).toBeDefined());

    rendered.unmount();
    const focusCallsAfterDestroy = focus.mock.calls.length;
    expect(mutationSignal?.aborted).toBe(true);
    mutation.resolve(Response.json({ id: 41, status: 'resolved', resolution: 'keep_remote' }));
    await Promise.resolve();

    expect(focus).toHaveBeenCalledTimes(focusCallsAfterDestroy);
    expect(document.body.textContent).not.toContain('resolved by keeping');
  });
});
