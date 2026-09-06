import { fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import { appShortcuts } from '@kenn-io/kit-ui';
import { describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../../api/client';
import { LOAD_THROUGH_END_MAX_PAGES } from '../../explore/paging';
import { ExploreState, parseExploreURLState, serializeExploreURLState } from '../../explore/state.svelte';
import { chooseSelectOption } from '../../../test/kit-ui';
import AppShell from './AppShell.svelte';

function exploreResponse(overrides: Record<string, unknown> = {}) {
  return {
    rows: [],
    total_count: 0,
    cache_revision: 'cache-1',
    search_provenance: {},
    ...overrides
  };
}

describe('AppShell', () => {
  function entry(index: number) {
    return {
      key: `message:${index}`,
      kind: 'message',
      message_type: 'email',
      conversation_type: 'email',
      title: `Synthetic subject ${index}`,
      preview: `Synthetic excerpt ${index}`,
      occurred_at: '2026-07-18T12:00:00Z',
      source_id: 1,
      source_identifier: 'archive@example.com',
      source_type: 'synthetic',
      participant_labels: ['Example Person'],
      participant_ids: [1],
      attachment_count: 0,
      attachment_size: 0,
      has_attachments: false,
      deleted_from_source: false,
      message_count: 1,
      match: {}
    };
  }

  it('focuses search with slash and leaves Escape to the search input', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
    const state = new ExploreState(window);
    render(AppShell, { client: createAPIClient(vi.fn()), state, enabled: false });

    await fireEvent.keyDown(window, { key: '/' });
    const search = screen.getByRole('searchbox', { name: 'Search everything' });
    expect(document.activeElement).toBe(search);
    await fireEvent.keyDown(search, { key: 'Escape' });
    expect(document.activeElement).toBe(search);
    state.destroy();
  });


  it('suspends the shortcut registry without preventing defaults in every editable control', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
    const state = new ExploreState(window);
    const rendered = render(AppShell, { client: createAPIClient(vi.fn()), state, enabled: false });
    const handleKeydown = vi.spyOn(appShortcuts, 'handleKeydown');
    const search = screen.getByRole('searchbox', { name: 'Search everything' });
    const textarea = document.createElement('textarea');
    const select = document.createElement('select');
    const editable = document.createElement('div');
    editable.setAttribute('contenteditable', 'plaintext-only');
    editable.tabIndex = 0;
    const iframe = document.createElement('iframe');
    document.body.append(textarea, select, editable, iframe);

    for (const control of [search, textarea, select, editable, iframe]) {
      control.focus();
      await waitFor(() => expect(appShortcuts.activeScope()).toBe('everything-editable'));
      for (const init of [
        { key: 'k', ctrlKey: true },
        { key: 'k', metaKey: true },
        { key: 'd' },
        { key: 'D', shiftKey: true },
        { key: 'Escape' }
      ]) {
        const event = new KeyboardEvent('keydown', { ...init, bubbles: true, cancelable: true });
        control.dispatchEvent(event);
        expect(handleKeydown.mock.results.at(-1)?.value).toBe(false);
        expect(event.defaultPrevented).toBe(false);
      }
    }

    textarea.focus();
    textarea.remove();
    await waitFor(() => expect(appShortcuts.activeScope()).toBe('root'));

    const outside = document.createElement('button');
    document.body.append(outside);
    outside.focus();
    const slash = new KeyboardEvent('keydown', { key: '/', bubbles: true, cancelable: true });
    outside.dispatchEvent(slash);
    expect(handleKeydown.mock.results.at(-1)?.value).toBe(true);
    expect(slash.defaultPrevented).toBe(true);
    expect(document.activeElement).toBe(search);

    outside.focus();
    await fireEvent.keyDown(outside, { key: 'd' });
    expect(state.current.workspace).toBe('everything');

    select.remove();
    editable.remove();
    iframe.remove();
    outside.remove();
    rendered.unmount();
    expect(appShortcuts.activeScope()).toBe('root');
    handleKeydown.mockRestore();
    state.destroy();
  });


  it('does not reinstall an editable shortcut scope from a queued callback after unmount', async () => {
    const state = new ExploreState(window);
    const rendered = render(AppShell, { client: createAPIClient(vi.fn()), state, enabled: false });
    const textarea = document.createElement('textarea');
    document.body.append(textarea);
    textarea.focus();
    await waitFor(() => expect(appShortcuts.activeScope()).toBe('everything-editable'));
    textarea.dispatchEvent(new FocusEvent('focusout', { bubbles: true }));
    rendered.unmount();
    await Promise.resolve();
    expect(appShortcuts.activeScope()).toBe('root');
    textarea.remove();
    state.destroy();
  });


  it('guards slash for modifiers and every editable contenteditable value except false', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
    const state = new ExploreState(window);
    render(AppShell, { client: createAPIClient(vi.fn()), state, enabled: false });
    const search = screen.getByRole('searchbox', { name: 'Search everything' });
    const host = document.createElement('div');
    document.body.append(host);

    for (const value of ['', 'true', 'plaintext-only']) {
      host.setAttribute('contenteditable', value);
      host.focus();
      await fireEvent.keyDown(host, { key: '/' });
      expect(document.activeElement).toBe(host);
    }
    host.setAttribute('contenteditable', 'false');
    host.focus();
    await waitFor(() => expect(appShortcuts.activeScope()).toBe('root'));
    await fireEvent.keyDown(host, { key: '/' });
    expect(document.activeElement).toBe(search);

    for (const modifier of ['ctrlKey', 'metaKey', 'altKey'] as const) {
      host.focus();
      await fireEvent.keyDown(host, { key: '/', [modifier]: true });
      expect(document.activeElement).toBe(host);
    }
    await fireEvent.keyDown(host, { key: '?', shiftKey: true });
    expect(screen.getByRole('dialog', { name: 'Keyboard shortcuts' })).toBeDefined();
    await fireEvent.click(screen.getByRole('button', { name: 'Close' }));
    host.remove();
    state.destroy();
  });


  it('does not steal table shortcuts from editable controls', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
    const state = new ExploreState(window);
    render(AppShell, { client: createAPIClient(vi.fn()), state, enabled: false });
    const search = screen.getByRole('searchbox', { name: 'Search everything' });
    search.focus();

    await fireEvent.keyDown(search, { key: 'j' });

    expect(document.activeElement).toBe(search);
    state.destroy();
  });


  it('commits workspace navigation to URL history', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
    const state = new ExploreState(window);
    const push = vi.spyOn(window.history, 'pushState');
    render(AppShell, { client: createAPIClient(vi.fn()), state, enabled: false });

    await fireEvent.click(screen.getByRole('button', { name: 'Settings' }));

    expect(state.current.workspace).toBe('settings');
    expect(push).toHaveBeenCalledOnce();
    state.destroy();
  });


  it('acknowledges history restoration immediately in a workspace without a result grid', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
    const state = new ExploreState(window);
    state.replaceTransient({
      workspace: 'settings', activeRow: 'message:stale',
      scrollAnchor: { key: 'message:stale', offset: 12 }
    });
    const rendered = render(AppShell, { client: createAPIClient(vi.fn()), state, enabled: false });

    await waitFor(() => expect(state.peekRestorationEpoch()).toBeUndefined());
    const theme = screen.getByRole('button', { name: 'Change theme (current: System)' });
    theme.focus();
    await Promise.resolve();
    expect(document.activeElement).toBe(theme);

    rendered.unmount();
    state.destroy();
  });


  it('keeps the Kit theme toggle in sync with the session appearance override', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
    const state = new ExploreState(window);
    const rendered = render(AppShell, {
      client: createAPIClient(vi.fn()), state, enabled: false,
      appearanceDefaults: { theme: 'system', density: 'compact' }
    });

    await fireEvent.click(screen.getByRole('button', { name: 'Change theme (current: System)' }));

    expect(screen.getByRole('button', { name: 'Change theme (current: Light)' })).toBeDefined();
    expect(JSON.parse(sessionStorage.getItem('msgvault.appearance.override') ?? '{}')).toEqual({
      theme: 'light'
    });

    rendered.unmount();
    state.destroy();
  });


  it('restores the daemon theme after a temporary Kit theme selection', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
    const state = new ExploreState(window);
    const rendered = render(AppShell, {
      client: createAPIClient(vi.fn()), state, enabled: false,
      appearanceDefaults: { theme: 'dark', density: 'compact' }
    });

    await fireEvent.click(screen.getByRole('button', { name: 'Change theme (current: Dark)' }));
    await fireEvent.click(screen.getByRole('button', { name: 'Use daemon theme' }));

    expect(screen.getByRole('button', { name: 'Change theme (current: Dark)' })).toBeDefined();
    expect(screen.queryByRole('button', { name: 'Use daemon theme' })).toBeNull();
    expect(sessionStorage.getItem('msgvault.appearance.override')).toBeNull();

    rendered.unmount();
    state.destroy();
  });


  it('renders every archive management destination from primary navigation', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const path = new URL(input instanceof Request ? input.url : String(input)).pathname;
      if (path.endsWith('/saved-views')) return Response.json({ saved_views: [] });
      if (path.endsWith('/sources/status')) return Response.json({ sources: [] });
      if (path.endsWith('/deletions')) return Response.json({ manifests: [] });
      return Response.json(exploreResponse());
    });
    const state = new ExploreState(window);
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state, enabled: false });

    for (const [tab, label, workspace] of [
      ['Saved Views', 'Saved Views', 'saved_views'],
      ['Sources', 'Sources', 'sources'],
      ['Deletions', 'Deletions', 'deletions']
    ] as const) {
      await fireEvent.click(screen.getByRole('button', { name: tab }));
      expect(await screen.findByRole('main', { name: label })).toBeDefined();
      expect(state.current.workspace).toBe(workspace);
    }

    rendered.unmount();
    state.destroy();
  });


  it('presents the primary navigation tabs with Relationships first and People/Domains retired', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
    const state = new ExploreState(window);
    const rendered = render(AppShell, {
      client: createAPIClient(vi.fn<typeof fetch>(async () => Response.json(exploreResponse()))),
      state, enabled: false
    });

    const nav = screen.getByRole('navigation', { name: 'Primary' });
    expect(within(nav).getAllByRole('button').map((button) => button.textContent?.trim())).toEqual([
      'Relationships', 'Directory', 'Reviews', 'Everything', 'Files', 'Saved Views', 'Sources', 'Deletions', 'Settings'
    ]);
    expect(screen.queryByRole('button', { name: 'People' })).toBeNull();
    expect(screen.queryByRole('button', { name: 'Domains' })).toBeNull();

    rendered.unmount();
    state.destroy();
  });

  it('restores the Directory review queue and exposes Reviews in the command surface', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({
      workspace: 'directory_review', reviewKind: 'identity', identityState: 'conflict'
    }))}`);
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      const path = new URL(request.url).pathname;
      if (path === '/api/v1/identity/match-candidates') {
        return Response.json({ candidates: [], limit: 100, offset: 0 });
      }
      return Response.json(exploreResponse());
    });
    const state = new ExploreState(window);
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state, enabled: false });

    expect(await screen.findByRole('main', { name: 'Reviews' })).toBeDefined();
    expect(screen.getByRole('heading', { name: 'Reviews' })).toBeDefined();
    expect(state.current).toMatchObject({
      workspace: 'directory_review', reviewKind: 'identity', identityState: 'conflict'
    });
    await vi.waitFor(() => expect(requests.some((request) => {
      const url = new URL(request.url);
      return url.pathname === '/api/v1/identity/match-candidates' &&
        url.searchParams.get('state') === 'conflict' &&
        url.searchParams.get('limit') === '100' &&
        url.searchParams.get('offset') === '0';
    })).toBe(true));

    state.commitWorkspace('everything');
    await fireEvent.keyDown(window, { key: 'k', ctrlKey: true });
    const palette = screen.getByRole('dialog', { name: 'Everything commands' });
    const input = within(palette).getByRole('combobox');
    await fireEvent.input(input, { target: { value: 'reviews' } });
    await fireEvent.click(within(palette).getByRole('option', { name: 'Open Reviews' }));
    expect(state.current.workspace).toBe('directory_review');
    expect(requests.filter((request) => new URL(request.url).pathname === '/api/v1/identity/match-candidates')).toHaveLength(1);

    rendered.unmount();
    state.destroy();
  });

  it('owns imported relationship URL state and reloads it on restoration', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({
      workspace: 'directory_review', reviewKind: 'relationship', relationshipReviewState: 'accepted'
    }))}`);
    const calls: Array<{ method: string; path: string; status: string | null }> = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const url = new URL(request.url);
      if (url.pathname === '/api/v1/person-relationship-reviews') {
        calls.push({ method: request.method, path: url.pathname, status: url.searchParams.get('status') });
        return Response.json({ reviews: [] });
      }
      return Response.json(exploreResponse());
    });
    const state = new ExploreState(window);
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state, enabled: false });

    expect(await screen.findByRole('heading', { name: 'Imported relationships' })).toBeDefined();
    await waitFor(() => expect(calls).toEqual([
      { method: 'GET', path: '/api/v1/person-relationship-reviews', status: 'accepted' }
    ]));

    const accepted = screen.getByRole('radio', { name: 'Accepted' });
    accepted.focus();
    await fireEvent.keyDown(accepted, { key: 'ArrowRight' });
    await waitFor(() => expect(state.current.relationshipReviewState).toBe('rejected'));
    expect(calls.at(-1)).toEqual({ method: 'GET', path: '/api/v1/person-relationship-reviews', status: 'rejected' });

    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({
      workspace: 'directory_review', reviewKind: 'relationship', relationshipReviewState: 'accepted'
    }))}`);
    window.dispatchEvent(new PopStateEvent('popstate'));
    await waitFor(() => expect(calls).toHaveLength(3));
    expect(calls.at(-1)).toEqual({ method: 'GET', path: '/api/v1/person-relationship-reviews', status: 'accepted' });
    expect(state.current.relationshipReviewState).toBe('accepted');

    rendered.unmount();
    state.destroy();
  });

  it('owns the selected-person fact ledger and reloads the same person on history restoration', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({
      workspace: 'directory_review', reviewKind: 'fact', identityState: 'candidate', directoryPersonID: 42
    }))}`);
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      const path = new URL(request.url).pathname;
      if (path === '/api/v1/person-fact-targets') return Response.json({ fingerprint: 'safe', version: 'safe', targets: [] });
      if (path.endsWith('/fact-evidence')) return Response.json({ evidence: [] });
      if (path.endsWith('/fact-claims')) return Response.json({ claims: [] });
      if (path.endsWith('/fact-decisions')) return Response.json({ decisions: [] });
      if (path.endsWith('/fact-pins')) return Response.json({ pins: [] });
      return Response.json(exploreResponse());
    });
    const state = new ExploreState(window);
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state, enabled: false });

    expect(await screen.findByText('Person ID 42')).toBeDefined();
    await vi.waitFor(() => expect(requests.filter((request) => new URL(request.url).pathname.includes('fact'))).toHaveLength(5));
    window.dispatchEvent(new PopStateEvent('popstate'));
    await vi.waitFor(() => expect(requests.filter((request) => new URL(request.url).pathname.includes('fact'))).toHaveLength(10));

    await fireEvent.click(screen.getByRole('button', { name: 'Open person profile' }));
    expect(state.current).toMatchObject({ workspace: 'directory', directoryPersonID: 42 });
    rendered.unmount();
    state.destroy();
  });

  it('navigates a completed review merge through the durable-person route and keeps its announcement mounted', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({
      workspace: 'directory_review', reviewKind: 'identity', identityState: 'candidate'
    }))}`);
    const candidate = {
      id: 17, left_id: 170, left_kind: 'beeper_user', right_id: 171, right_kind: 'participant',
      basis: 'stable_provider_id', source: 'synthetic', state: 'candidate', evidence: [],
      created_at: '2026-08-01T00:00:00Z', updated_at: '2026-08-01T00:00:00Z'
    };
    const person = (id: number, revision: number, name: string) => ({
      id, revision, display_name: name, participant_ids: [id * 10], vcard_uid: `synthetic-${id}`,
      created_at: '2026-08-01T00:00:00Z', updated_at: '2026-08-02T00:00:00Z'
    });
    const conflict = {
      error: 'person_merge_required', message: 'Choose a survivor', profiles: [
        { person: person(7, 4, 'Synthetic One'), etag: '"person-7-r4"' },
        { person: person(9, 2, 'Synthetic Two'), etag: '"person-9-r2"' }
      ]
    };
    const survivor = person(7, 5, 'Synthetic One');
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      const path = new URL(request.url).pathname;
      if (path === '/api/v1/identity/match-candidates' && request.method === 'GET') {
        return Response.json({ candidates: [candidate], limit: 100, offset: 0 });
      }
      if (path.endsWith('/accept')) return Response.json(conflict, { status: 409 });
      if (path === '/api/v1/people/7/merge' && request.method === 'POST') return Response.json({
        cache_state: 'stale', identity_revision: 8, person: survivor, review_candidates: [],
        merge: {
          id: 41, survivor_person_id: 7, absorbed_person_id: 9, current_person_id: 7,
          survivor_vcard_uid: 'synthetic-7', absorbed_vcard_uid: 'synthetic-9',
          survivor_revision_before: 4, absorbed_revision_before: 2, survivor_revision_after: 5,
          actor: 'web', snapshot_version: 1, snapshot_sha256: 'synthetic-digest',
          created_at: '2026-08-03T00:00:00Z'
        }
      }, { headers: { ETag: '"person-7-r4"' } });
      if (path === '/api/v1/people/directory') return Response.json({ people: [] });
      if (path === '/api/v1/people/7') return Response.json(survivor, { headers: { ETag: '"person-7-r5"' } });
      if (path === '/api/v1/people/7/profile') return Response.json({
        person: survivor, names: [], contact_points: [], addresses: [], dates: [], categories: [], media: []
      });
      if (path === '/api/v1/people/7/attributes') return Response.json({ person_id: 7, attributes: [] });
      if (path === '/api/v1/people/7/contact-state') return Response.json({
        person_id: 7, cadence_status: 'current', computed_at: '2026-08-03T00:00:00Z', interaction_count: 0, stale: false
      });
      if (path === '/api/v1/people/7/employments') return Response.json({ employments: [] });
      if (path === '/api/v1/people/7/relationships') return Response.json({ relationships: [] });
      if (path === '/api/v1/people/7/days') return Response.json({ person_id: 7, days: [], total_count: 0 });
      if (path === '/api/v1/people/7/files/search') return Response.json({ files: [], total_count: 0, cache_revision: 'cache-person', search_provenance: {} });
      return Response.json(exploreResponse());
    });
    const state = new ExploreState(window);
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state, enabled: false });

    await fireEvent.click(await screen.findByRole('button', { name: 'Link identities' }));
    await fireEvent.click(screen.getByRole('dialog', { name: 'Link identities' }).querySelector('button.kit-button--solid')!);
    await fireEvent.click(await screen.findByRole('button', { name: 'Resolve merge' }));
    await fireEvent.click(screen.getByRole('radio', { name: 'Synthetic One (Person 7)' }));
    await fireEvent.click(screen.getByRole('checkbox', { name: /I understand this consolidates both profiles/i }));
    await fireEvent.click(screen.getByRole('button', { name: 'Merge into selected survivor' }));

    await waitFor(() => expect(state.current).toMatchObject({ workspace: 'directory', directoryPersonID: 7 }));
    expect(screen.getByRole('status', { name: 'Operation status' }).textContent)
      .toContain('People merged into Synthetic One. Identity cache stale.');
    expect(requests.some((request) => request.method === 'GET' && new URL(request.url).pathname === '/api/v1/people/7')).toBe(true);
    expect(requests.filter((request) => new URL(request.url).pathname.endsWith('/accept'))).toHaveLength(1);

    rendered.unmount();
    state.destroy();
  });

  it('restarts the review queue at page zero after same-filter popstate restoration', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({
      workspace: 'directory_review', reviewKind: 'identity', identityState: 'candidate'
    }))}`);
    const restored = (() => {
      let resolve!: (response: Response) => void;
      const promise = new Promise<Response>((next) => { resolve = next; });
      return { promise, resolve };
    })();
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      if (requests.length === 1) return Response.json({
        candidates: [{
          id: 17, left_id: 170, left_kind: 'beeper_user', right_id: 171, right_kind: 'participant',
          basis: 'stable_provider_id', source: 'synthetic', state: 'candidate', evidence: [],
          created_at: '2026-08-01T00:00:00Z', updated_at: '2026-08-01T00:00:00Z'
        }],
        limit: 100,
        offset: 0
      });
      return restored.promise;
    });
    const state = new ExploreState(window);
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state, enabled: false });

    expect(await screen.findByRole('heading', { name: 'Identity match 17' })).toBeDefined();
    window.history.replaceState(null, '', serializeExploreURLState(state.current));
    window.dispatchEvent(new PopStateEvent('popstate'));

    await waitFor(() => expect(requests).toHaveLength(2));
    expect(screen.getByText('Loading identity matches…')).toBeDefined();
    expect(screen.queryByRole('heading', { name: 'Identity match 17' })).toBeNull();
    expect(new URL(requests[1]!.url).searchParams.get('offset')).toBe('0');
    restored.resolve(Response.json({ error: 'unavailable', message: 'Restored queue unavailable' }, { status: 503 }));
    expect((await screen.findByRole('alert')).textContent).toContain('Restored queue unavailable');

    rendered.unmount();
    state.destroy();
  });

  it('navigates to the distinct Directory workspace and loads URL-restored state through its controller', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({
      workspace: 'directory', directoryQuery: 'synthetic', directoryPersonID: null
    }))}`);
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      if (new URL(request.url).pathname === '/api/v1/people/directory') return Response.json({
        people: [{ id: 7, revision: 1, display_name: 'Synthetic Person', contact_state: 'active', categories: [], organizations: [] }]
      });
      return Response.json(exploreResponse());
    });
    const state = new ExploreState(window);
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state, enabled: false });

    expect(await screen.findByRole('heading', { name: 'Directory' })).toBeDefined();
    expect(await screen.findByText('Synthetic Person')).toBeDefined();
    expect(state.current.workspace).toBe('directory');
    expect(new URL(requests[0]!.url).searchParams.get('q')).toBe('synthetic');

    rendered.unmount();
    state.destroy();
  });

  it('owns an ephemeral CardDAV conflict handoff and Browser Back restores the prior Directory person', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({
      workspace: 'directory', directoryPersonID: 7
    }))}`);
    let publicationReads = 0;
    let restoredPublicationSignal: AbortSignal | undefined;
    let resolveRestoredPublication!: (response: Response) => void;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = new URL(request.url).pathname;
      if (path === '/api/v1/people/directory') return Response.json({ people: [{
        id: 7, revision: 1, display_name: 'Synthetic Person', contact_state: 'active',
        categories: [], organizations: []
      }] });
      if (path === '/api/v1/carddav/publications/7') {
        publicationReads += 1;
        if (publicationReads === 1) return Response.json({
          person_id: 7, state: 'conflict', desired: true, conflict_id: 41,
          address_book: { id: 5, name: 'Synthetic contacts' }
        });
        restoredPublicationSignal = request.signal;
        return new Promise<Response>((resolve) => { resolveRestoredPublication = resolve; });
      }
      if (path === '/api/v1/people/7') return Response.json({
        id: 7, revision: 1, display_name: 'Synthetic Person', participant_ids: [], vcard_uid: '',
        created_at: '2026-08-01T00:00:00Z', updated_at: '2026-08-01T00:00:00Z'
      });
      if (path === '/api/v1/people/7/profile') return Response.json({
        person: { id: 7, revision: 1, display_name: 'Synthetic Person' },
        names: [], contact_points: [], addresses: [], dates: [], categories: [], media: []
      });
      if (path === '/api/v1/people/7/attributes') return Response.json({ person_id: 7, attributes: [] });
      if (path === '/api/v1/people/7/contact-state') return Response.json({
        person_id: 7, cadence_status: 'current', interaction_count: 0,
        computed_at: '2026-08-28T10:00:00Z', stale: false
      });
      if (path === '/api/v1/people/7/employments') return Response.json({ employments: [] });
      if (path === '/api/v1/people/7/relationships') return Response.json({ relationships: [] });
      if (path === '/api/v1/people/7/days') return Response.json({ person_id: 7, days: [], total_count: 0 });
      if (path === '/api/v1/people/7/files/search') return Response.json({
        files: [], total_count: 0, cache_revision: 'synthetic', search_provenance: {}
      });
      if (path === '/api/v1/people/7/merges') return Response.json({ merges: [], limit: 100, offset: 0 });
      return Response.json(exploreResponse());
    });
    const state = new ExploreState(window);
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state, enabled: false });

    await fireEvent.click(await screen.findByRole('button', { name: 'Review CardDAV conflict 41' }));
    expect(state.current.workspace).toBe('settings');
    expect(screen.getByRole('status', { name: 'Operation status' }).textContent)
      .toBe('Opening CardDAV conflict 41 in Settings.');

    window.history.back();
    await new Promise((resolve) => window.addEventListener('popstate', resolve, { once: true }));
    await waitFor(() => expect(state.current).toMatchObject({ workspace: 'directory', directoryPersonID: 7 }));
    expect(await screen.findByRole('heading', { name: 'Synthetic Person' })).toBeDefined();
    expect(screen.getAllByRole('status', { name: 'Operation status' })).toHaveLength(1);
    await waitFor(() => expect(restoredPublicationSignal).toBeDefined());

    await fireEvent.click(screen.getByRole('button', { name: 'Settings' }));
    expect(restoredPublicationSignal?.aborted).toBe(true);
    resolveRestoredPublication(Response.json({
      person_id: 7, state: 'published', desired: true,
      address_book: { id: 6, name: 'Old contacts' }
    }));
    await Promise.resolve();
    expect(screen.queryByText('Old contacts')).toBeNull();

    rendered.unmount();
    state.destroy();
  });

  it('does not reset Directory page one for ordinary selection and filter commits', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({
      workspace: 'directory', directoryQuery: '', directoryPersonID: null
    }))}`);
    const directoryRequests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = new URL(request.url).pathname;
      if (path === '/api/v1/people/directory') {
        directoryRequests.push(request);
        const query = new URL(request.url).searchParams.get('q');
        return Response.json({ people: [{
          id: 7, revision: 1, display_name: query ? 'Refined Person' : 'Synthetic Person',
          contact_state: 'active', categories: [], organizations: []
        }] });
      }
      if (path.endsWith('/files/search')) return Response.json({ files: [], total_count: 0, cache_revision: 'synthetic', search_provenance: {} });
      if (path.startsWith('/api/v1/people/7')) return Response.json({ person_id: 7, employments: [], relationships: [], days: [], attributes: [], categories: [], names: [], contact_points: [], addresses: [], dates: [] });
      return Response.json(exploreResponse());
    });
    const state = new ExploreState(window);
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state, enabled: false });

    await screen.findByText('Synthetic Person');
    await fireEvent.click(screen.getByRole('row', { name: /Synthetic Person/ }));
    await Promise.resolve();
    expect(directoryRequests).toHaveLength(1);

    await fireEvent.input(screen.getByRole('searchbox', { name: 'Search directory' }), {
      target: { value: 'refined' }
    });
    await waitFor(() => expect(screen.getByText('Refined Person')).toBeDefined());
    await new Promise<void>((resolve) => setTimeout(resolve, 0));
    expect(directoryRequests).toHaveLength(2);

    rendered.unmount();
    state.destroy();
  });

  it('restarts Directory page one after a popstate restoration', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({
      workspace: 'directory', directoryQuery: 'synthetic', directoryPersonID: null
    }))}`);
    const directoryRequests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = new URL(request.url).pathname;
      if (path === '/api/v1/people/directory') {
        directoryRequests.push(request);
        const cursor = new URL(request.url).searchParams.get('cursor');
        return Response.json(cursor
          ? { people: [{ id: 8, revision: 1, display_name: 'Second Page', contact_state: 'active', categories: [], organizations: [] }] }
          : { people: [{ id: 7, revision: 1, display_name: 'First Page', contact_state: 'active', categories: [], organizations: [] }], next_cursor: 'next-page' });
      }
      return Response.json(exploreResponse());
    });
    const state = new ExploreState(window);
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state, enabled: false });

    await screen.findByText('First Page');
    await fireEvent.click(screen.getByRole('button', { name: 'Load more people' }));
    await screen.findByText('Second Page');
    expect(directoryRequests).toHaveLength(2);

    window.history.replaceState(null, '', serializeExploreURLState(state.current));
    window.dispatchEvent(new PopStateEvent('popstate'));

    await waitFor(() => expect(directoryRequests).toHaveLength(3));
    expect(new URL(directoryRequests[2]!.url).searchParams.get('cursor')).toBeNull();

    rendered.unmount();
    state.destroy();
  });

  it('keeps an AppShell-owned Directory request alive across a workspace round-trip', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({
      workspace: 'directory', directoryQuery: '', directoryPersonID: null
    }))}`);
    const directoryRequests: Request[] = [];
    let resolveDirectory: ((response: Response) => void) | undefined;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      if (new URL(request.url).pathname === '/api/v1/people/directory') {
        directoryRequests.push(request);
        return new Promise<Response>((resolve) => { resolveDirectory = resolve; });
      }
      return Response.json(exploreResponse());
    });
    const state = new ExploreState(window);
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state, enabled: false });

    await waitFor(() => expect(resolveDirectory).toBeDefined());
    await fireEvent.click(screen.getByRole('button', { name: 'Everything' }));
    await fireEvent.click(screen.getByRole('button', { name: 'Directory' }));
    resolveDirectory?.(Response.json({ people: [{
      id: 7, revision: 1, display_name: 'Synthetic Person', contact_state: 'active', categories: [], organizations: []
    }] }));

    expect(await screen.findByText('Synthetic Person')).toBeDefined();
    expect(directoryRequests).toHaveLength(1);

    rendered.unmount();
    state.destroy();
  });

  it('aborts an AppShell-owned Directory request when the shell is finally destroyed', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({
      workspace: 'directory', directoryQuery: '', directoryPersonID: null
    }))}`);
    let request: Request | undefined;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      request = input instanceof Request ? input : new Request(input);
      return new Promise<Response>(() => undefined);
    });
    const state = new ExploreState(window);
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state, enabled: false });

    await waitFor(() => expect(request).toBeDefined());
    rendered.unmount();

    expect(request?.signal.aborted).toBe(true);
    state.destroy();
  });


  it('renders the Relationships hub for the default landing workspace', async () => {
    window.history.replaceState(null, '', '/');
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const path = new URL(input instanceof Request ? input.url : String(input)).pathname;
      if (path === '/api/v1/relationships') return Response.json({
        rows: [{
          canonical_id: 1, display_label: 'Alice Example', last_at: '2026-07-19T10:00:00Z', member_ids: [1], score: 1,
          signals: {
            last_interaction_at: '2026-07-19T10:00:00Z', meeting_count: 0, meetings_together: 0, modalities: 1,
            received_from_them: 1, sent_count: 1, sent_to_them: 1
          }
        }]
      });
      return Response.json(exploreResponse());
    });
    const state = new ExploreState(window);
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state });

    expect(await screen.findByRole('main', { name: 'Relationships' })).toBeDefined();
    expect(await screen.findByText('Alice Example')).toBeDefined();
    expect(state.current.workspace).toBe('relationships');

    rendered.unmount();
    state.destroy();
  });


  it('restores a legacy workspace=people URL into the hub with the facet set', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({
      workspace: 'people', analysisTarget: 'person:42'
    }))}`);
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const path = new URL(input instanceof Request ? input.url : String(input)).pathname;
      if (path === '/api/v1/participants/42') return Response.json({
        id: 42, display_label: 'Legacy Person', partial_label: false, identifiers: [],
        activity_count: 1, file_count: 0, source_counts: [], first_at: '2026-07-19T10:00:00Z',
        last_at: '2026-07-19T10:00:00Z', cache_revision: 'cache-legacy'
      });
      if (path === '/api/v1/relationships/42/timeline') return Response.json({
        canonical_id: 42, identity_revision: 1, cache_revision: 'cache-legacy', rows: [], total_count: 0
      });
      return Response.json(exploreResponse());
    });
    const state = new ExploreState(window);
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state });

    expect(await screen.findByRole('main', { name: 'Relationships' })).toBeDefined();
    expect(state.current.workspace).toBe('relationships');
    expect(state.current.relationshipFacet).toBe('people');
    expect(state.current.relationshipTarget).toBe('cluster:42');
    expect(screen.getByRole('radio', { name: 'People' }).getAttribute('aria-checked')).toBe('true');
    expect(await screen.findByRole('heading', { name: 'Legacy Person' })).toBeDefined();

    rendered.unmount();
    state.destroy();
  });


  it('keeps Relationships and shows its degraded state when the URL explicitly names it', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'relationships' }))}`);
    const fetchFn = vi.fn<typeof fetch>(async () => Response.json({
      error: 'analytical_cache_unavailable', message: 'The committed analytical cache is unavailable',
      readiness: 'stale_schema', recovery_action: 'Rebuild the analytical cache'
    }, { status: 503 }));
    const state = new ExploreState(window);
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state });

    expect(await screen.findByText('Relationship ranking needs the analytical cache/engine')).toBeDefined();
    expect(screen.getByRole('main', { name: 'Relationships' })).toBeDefined();
    expect(state.current.workspace).toBe('relationships');
    expect(screen.queryByRole('main', { name: 'Everything' })).toBeNull();

    rendered.unmount();
    state.destroy();
  });


  it('falls back to Everything from the default Relationships landing when the archive engine is unavailable', async () => {
    window.history.replaceState(null, '', '/');
    const fetchFn = vi.fn<typeof fetch>(async () => Response.json({
      error: 'analytical_cache_unavailable', message: 'The committed analytical cache is unavailable',
      readiness: 'stale_schema', recovery_action: 'Rebuild the analytical cache'
    }, { status: 503 }));
    const state = new ExploreState(window);
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state });

    await waitFor(() => expect(state.current.workspace).toBe('everything'));
    expect(await screen.findByRole('main', { name: 'Everything' })).toBeDefined();
    expect(screen.queryByRole('main', { name: 'Relationships' })).toBeNull();

    rendered.unmount();
    state.destroy();
  });

  it('keeps the default Relationships landing while the analytical cache is building', async () => {
    window.history.replaceState(null, '', '/');
    const fetchFn = vi.fn<typeof fetch>(async () => Response.json({
      error: 'analytical_cache_unavailable', message: 'The analytical cache is being prepared',
      readiness: 'building', recovery_action: ''
    }, { status: 503 }));
    const state = new ExploreState(window);
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state });

    expect(await screen.findByText('Preparing relationship ranking…')).toBeDefined();
    expect(state.current.workspace).toBe('relationships');
    expect(screen.queryByRole('main', { name: 'Everything' })).toBeNull();

    rendered.unmount();
    state.destroy();
  });


  it('a committed replace for the landing fallback keeps Back from resurrecting the degraded hub', async () => {
    window.history.replaceState(null, '', '/');
    const fetchFn = vi.fn<typeof fetch>(async () => Response.json({
      error: 'analytical_cache_unavailable', message: 'The committed analytical cache is unavailable',
      readiness: 'stale_schema', recovery_action: 'Rebuild the analytical cache'
    }, { status: 503 }));
    const state = new ExploreState(window);
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state });

    await waitFor(() => expect(state.current.workspace).toBe('everything'));
    expect(await screen.findByRole('main', { name: 'Everything' })).toBeDefined();

    // Any later push navigation (state.svelte.ts's `navigate()`, 'push'
    // branch) rewrites the CURRENT history entry from `committed` before
    // pushing the new one — a transient replace never updates `committed`,
    // so this push would otherwise rewrite entry #1 back to the degraded
    // 'relationships' landing the user never actually saw past.
    state.commitSearch('synthetic', 'full_text');
    await waitFor(() => expect(state.current.query).toBe('synthetic'));

    window.history.back();
    await waitFor(() => expect(state.current.query).toBe(''));
    expect(state.current.workspace).toBe('everything');
    expect(screen.queryByRole('main', { name: 'Relationships' })).toBeNull();

    rendered.unmount();
    state.destroy();
  });


  it('keeps a later explicit Relationships visit degraded instead of bouncing, after the landing fallback allowance was already spent', async () => {
    window.history.replaceState(null, '', '/');
    let relationshipsDegraded = false;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const path = new URL(input instanceof Request ? input.url : String(input)).pathname;
      if (path === '/api/v1/relationships') {
        if (relationshipsDegraded) return Response.json({
          error: 'analytical_cache_unavailable', message: 'The committed analytical cache is unavailable',
          readiness: 'stale_schema', recovery_action: 'Rebuild the analytical cache'
        }, { status: 503 });
        return Response.json({
          rows: [{
            canonical_id: 1, display_label: 'Alice Example', last_at: '2026-07-19T10:00:00Z', member_ids: [1], score: 1,
            signals: {
              last_interaction_at: '2026-07-19T10:00:00Z', meeting_count: 0, meetings_together: 0, modalities: 1,
              received_from_them: 1, sent_count: 1, sent_to_them: 1
            }
          }]
        });
      }
      return Response.json(exploreResponse());
    });
    const state = new ExploreState(window);
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state });

    // The default landing is healthy, so the one-shot landing fallback never
    // fires: the hub stays on Relationships and the allowance is still
    // unspent going into the next step.
    expect(await screen.findByRole('main', { name: 'Relationships' })).toBeDefined();
    expect(await screen.findByText('Alice Example')).toBeDefined();
    expect(state.current.workspace).toBe('relationships');

    // The user explicitly navigates to Everything via the tab. That
    // user-initiated navigation spends the one-shot landing-fallback
    // allowance, even though it never fired.
    const nav = screen.getByRole('navigation', { name: 'Primary' });
    await fireEvent.click(within(nav).getByRole('button', { name: 'Everything' }));
    expect(await screen.findByRole('main', { name: 'Everything' })).toBeDefined();

    // Now the relationships list starts reporting the cache as unavailable,
    // and the user explicitly returns to Relationships via the tab.
    relationshipsDegraded = true;
    await fireEvent.click(within(nav).getByRole('button', { name: 'Relationships' }));

    // It degrades, but this is no longer the initial landing, so it must
    // show its own degraded state rather than bounce back to Everything.
    expect(await screen.findByText('Relationship ranking needs the analytical cache/engine')).toBeDefined();
    expect(screen.getByRole('main', { name: 'Relationships' })).toBeDefined();
    expect(state.current.workspace).toBe('relationships');
    expect(screen.queryByRole('main', { name: 'Everything' })).toBeNull();

    rendered.unmount();
    state.destroy();
  });


  it('clears the hub detail pane when the URL target becomes null (Esc / Back)', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({
      workspace: 'relationships', relationshipTarget: 'cluster:1'
    }))}`);
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const path = new URL(input instanceof Request ? input.url : String(input)).pathname;
      if (path === '/api/v1/participants/1') return Response.json({
        id: 1, display_label: 'Alice Example', partial_label: false, identifiers: [],
        activity_count: 1, file_count: 0, source_counts: [], first_at: '2026-07-19T10:00:00Z',
        last_at: '2026-07-19T10:00:00Z', cache_revision: 'cache-rel'
      });
      if (path === '/api/v1/relationships/1/timeline') return Response.json({
        canonical_id: 1, identity_revision: 1, cache_revision: 'cache-rel', rows: [], total_count: 0
      });
      if (path === '/api/v1/relationships') return Response.json({ rows: [] });
      return Response.json(exploreResponse());
    });
    const state = new ExploreState(window);
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state });

    expect(await screen.findByRole('heading', { name: 'Alice Example' })).toBeDefined();

    state.commitNavigation({ relationshipTarget: null });

    await waitFor(() => expect(screen.queryByRole('heading', { name: 'Alice Example' })).toBeNull());
    expect(screen.getByText('Select a person or domain')).toBeDefined();
    expect(state.current.workspace).toBe('relationships');

    rendered.unmount();
    state.destroy();
  });

  it.each([200, 201])('hands a selected relationship participant to Directory promotion on %i', async (status) => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({
      workspace: 'relationships', relationshipTarget: 'cluster:11'
    }))}`);
    const requests: Request[] = [];
    let promoted = false;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      const path = new URL(request.url).pathname;
      if (path === '/api/v1/relationships') return Response.json({ rows: [] });
      if (path === '/api/v1/participants/11') return Response.json({
        id: 11, display_label: 'Synthetic Candidate', partial_label: false, identifiers: [],
        activity_count: 1, file_count: 0, source_counts: [], first_at: '2026-07-19T10:00:00Z',
        last_at: '2026-07-19T10:00:00Z', cache_revision: 'cache-rel'
      });
      if (path === '/api/v1/relationships/11/timeline') return Response.json({
        canonical_id: 11, identity_revision: 1, cache_revision: 'cache-rel', rows: [], total_count: 0
      });
      if (path === '/api/v1/people/directory') return Response.json({
        people: promoted ? [{
          id: 42, display_name: 'Synthetic Candidate', revision: 1, primary_channel: 'email',
          contact_state: 'active', categories: [], organizations: []
        }] : []
      });
      if (path === '/api/v1/people' && request.method === 'POST') {
        promoted = true;
        return Response.json({ id: 42, revision: 1 }, { status });
      }
      if (path === '/api/v1/people/42') return Response.json({ id: 42, revision: 1, display_name: 'Synthetic Candidate' });
      if (path === '/api/v1/people/42/profile') return Response.json({
        person: { id: 42, revision: 1, display_name: 'Synthetic Candidate' }, names: [], contact_points: [],
        addresses: [], dates: [], categories: [], media: []
      });
      if (path === '/api/v1/people/42/attributes') return Response.json({ person_id: 42, attributes: [] });
      if (path === '/api/v1/people/42/contact-state') return Response.json({
        person_id: 42, cadence_status: 'current', computed_at: '2026-07-19T10:00:00Z',
        interaction_count: 1, stale: false
      });
      if (path === '/api/v1/people/42/employments') return Response.json({ employments: [] });
      if (path === '/api/v1/people/42/relationships') return Response.json({ relationships: [] });
      if (path === '/api/v1/people/42/days') return Response.json({ person_id: 42, days: [], total_count: 0 });
      if (path === '/api/v1/people/42/files/search') return Response.json({
        files: [], total_count: 0, cache_revision: 'cache-person', search_provenance: {}
      });
      return Response.json(exploreResponse());
    });
    const state = new ExploreState(window);
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state });

    expect(await screen.findByRole('heading', { name: 'Synthetic Candidate' })).toBeDefined();
    await fireEvent.click(screen.getByRole('button', { name: 'Open in Directory' }));
    expect(await screen.findByRole('main', { name: 'Directory' })).toBeDefined();
    await fireEvent.click(screen.getByRole('button', { name: 'Promote to person' }));

    await waitFor(() => expect(state.current.directoryPersonID).toBe(42));
    expect(state.current.workspace).toBe('directory');
    expect(new URL(window.location.href).searchParams.get('explore')).toContain('directoryPersonID');
    const promotion = requests.find((request) =>
      new URL(request.url).pathname === '/api/v1/people' && request.method === 'POST'
    );
    expect(promotion).toBeDefined();
    await expect(promotion!.clone().json()).resolves.toEqual({ participant_id: 11 });
    expect(requests.filter((request) => new URL(request.url).pathname === '/api/v1/people/directory').length)
      .toBeGreaterThanOrEqual(2);

    rendered.unmount();
    state.destroy();
  });

  it('renders actionable Directory guidance for a relationship promotion conflict', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({
      workspace: 'relationships', relationshipTarget: 'cluster:11'
    }))}`);
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = new URL(request.url).pathname;
      if (path === '/api/v1/relationships') return Response.json({ rows: [] });
      if (path === '/api/v1/participants/11') return Response.json({
        id: 11, display_label: 'Synthetic Candidate', partial_label: false, identifiers: [],
        activity_count: 1, file_count: 0, source_counts: [], first_at: '2026-07-19T10:00:00Z',
        last_at: '2026-07-19T10:00:00Z', cache_revision: 'cache-rel'
      });
      if (path === '/api/v1/relationships/11/timeline') return Response.json({
        canonical_id: 11, identity_revision: 1, cache_revision: 'cache-rel', rows: [], total_count: 0
      });
      if (path === '/api/v1/people/directory') return Response.json({ people: [] });
      if (path === '/api/v1/people' && request.method === 'POST') return Response.json({
        error: 'person_binding_conflict', message: 'This identity is already bound to another person'
      }, { status: 409 });
      return Response.json(exploreResponse());
    });
    const state = new ExploreState(window);
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state });

    expect(await screen.findByRole('heading', { name: 'Synthetic Candidate' })).toBeDefined();
    await fireEvent.click(screen.getByRole('button', { name: 'Open in Directory' }));
    await fireEvent.click(await screen.findByRole('button', { name: 'Promote to person' }));

    const alert = await screen.findByRole('alert');
    expect(alert.textContent).toContain('already bound to another person');
    expect(alert.textContent).toContain('resolve that binding before promoting');
    expect(state.current.directoryPersonID).toBeNull();

    rendered.unmount();
    state.destroy();
  });

  it('clears prior Directory selection and promotion state before opening a new relationship candidate', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({
      workspace: 'directory', directoryPersonID: 7
    }))}`);
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = new URL(request.url).pathname;
      if (path === '/api/v1/people/directory') return Response.json({ people: [{
        id: 7, display_name: 'Prior Directory Person', revision: 3, contact_state: 'active', categories: [], organizations: []
      }] });
      if (path === '/api/v1/people/7') return Response.json({ id: 7, revision: 3, display_name: 'Prior Directory Person' });
      if (path === '/api/v1/people/7/profile') return Response.json({
        person: { id: 7, revision: 3, display_name: 'Prior Directory Person' }, names: [], contact_points: [], addresses: [], dates: [], categories: [], media: []
      });
      if (path === '/api/v1/people/7/attributes') return Response.json({ person_id: 7, attributes: [] });
      if (path === '/api/v1/people/7/contact-state') return Response.json({ person_id: 7, state: 'active' });
      if (path === '/api/v1/people/7/employments') return Response.json({ employments: [] });
      if (path === '/api/v1/people/7/relationships') return Response.json({ relationships: [] });
      if (path === '/api/v1/people/7/days') return Response.json({ person_id: 7, days: [], total_count: 0 });
      if (path === '/api/v1/people/7/files/search') return Response.json({ files: [], total_count: 0, cache_revision: 'cache-person', search_provenance: {} });
      if (path === '/api/v1/relationships') return Response.json({ rows: [] });
      if (path === '/api/v1/participants/11' || path === '/api/v1/participants/12') {
        const id = Number(path.split('/').at(-1));
        return Response.json({ id, display_label: `Candidate ${id}`, partial_label: false, identifiers: [], activity_count: 1, file_count: 0, source_counts: [], first_at: '2026-07-19T10:00:00Z', last_at: '2026-07-19T10:00:00Z', cache_revision: 'cache-rel' });
      }
      if (path === '/api/v1/relationships/11/timeline' || path === '/api/v1/relationships/12/timeline') {
        const canonicalID = Number(path.split('/')[3]);
        return Response.json({ canonical_id: canonicalID, identity_revision: 1, cache_revision: 'cache-rel', rows: [], total_count: 0 });
      }
      if (path === '/api/v1/people' && request.method === 'POST') return Response.json({
        error: 'person_binding_conflict', message: 'This identity is already bound to another person'
      }, { status: 409 });
      return Response.json(exploreResponse());
    });
    const state = new ExploreState(window);
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state });

    expect(await screen.findByRole('heading', { name: 'Prior Directory Person' })).toBeDefined();
    state.commitNavigation({ workspace: 'relationships', relationshipTarget: 'cluster:11' });
    expect(await screen.findByRole('heading', { name: 'Candidate 11' })).toBeDefined();
    await fireEvent.click(screen.getByRole('button', { name: 'Open in Directory' }));
    await fireEvent.click(await screen.findByRole('button', { name: 'Promote to person' }));
    expect((await screen.findByRole('alert')).textContent).toContain('already bound to another person');

    state.commitNavigation({ workspace: 'relationships', relationshipTarget: 'cluster:12' });
    expect(await screen.findByRole('heading', { name: 'Candidate 12' })).toBeDefined();
    await fireEvent.click(screen.getByRole('button', { name: 'Open in Directory' }));

    expect(await screen.findByRole('main', { name: 'Directory' })).toBeDefined();
    expect(screen.queryByRole('heading', { name: 'Prior Directory Person' })).toBeNull();
    expect(screen.queryByRole('alert')).toBeNull();
    expect(state.current.directoryPersonID).toBeNull();

    rendered.unmount();
    state.destroy();
  });


  it('re-opens the hub target when the predicate changes but the target string does not', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({
      workspace: 'relationships', relationshipTarget: 'cluster:1'
    }))}`);
    const timelineBodies: unknown[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = new URL(request.url).pathname;
      if (path === '/api/v1/participants/1') return Response.json({
        id: 1, display_label: 'Alice Example', partial_label: false, identifiers: [],
        activity_count: 1, file_count: 0, source_counts: [], first_at: '2026-07-19T10:00:00Z',
        last_at: '2026-07-19T10:00:00Z', cache_revision: 'cache-rel'
      });
      if (path === '/api/v1/relationships/1/timeline') {
        timelineBodies.push(await request.clone().json());
        return Response.json({
          canonical_id: 1, identity_revision: 1, cache_revision: 'cache-rel', rows: [], total_count: 0
        });
      }
      if (path === '/api/v1/relationships') return Response.json({ rows: [] });
      return Response.json(exploreResponse());
    });
    const state = new ExploreState(window);
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state });

    await waitFor(() => expect(timelineBodies).toHaveLength(1));
    expect((timelineBodies[0] as { filters?: unknown[] }).filters).toEqual([]);

    // The target string is unchanged; only the predicate (a filter picked
    // up from elsewhere in the app) changes.
    state.commitNavigation({ filters: [{ dimension: 'source', values: ['1'] }] });

    await waitFor(() => expect(timelineBodies).toHaveLength(2));
    expect((timelineBodies[1] as { filters?: unknown[] }).filters).toEqual([{ dimension: 'source', values: ['1'] }]);

    rendered.unmount();
    state.destroy();
  });


  it('opening a file from the hub\'s embedded Files pane navigates to Everything with the item selected', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({
      workspace: 'relationships', relationshipTarget: 'cluster:1', relationshipFiles: true
    }))}`);
    const fileRow = {
      id: 9, key: 'file:9', entry_key: 'message:42', message_id: 42, conversation_id: 70,
      occurred_at: '2026-07-19T10:00:00Z', source_id: 1, source_type: 'gmail',
      source_identifier: 'alice@example.com', containing_title: 'Subject', filename: 'report.pdf',
      mime_type: 'application/pdf', mime_family: 'pdf', size_bytes: 100,
      content_state: 'local_content', content_available: true
    };
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const path = new URL(input instanceof Request ? input.url : String(input)).pathname;
      if (path === '/api/v1/participants/1') return Response.json({
        id: 1, display_label: 'Alice Example', partial_label: false, identifiers: [],
        activity_count: 1, file_count: 1, source_counts: [], first_at: '2026-07-19T10:00:00Z',
        last_at: '2026-07-19T10:00:00Z', cache_revision: 'cache-rel'
      });
      if (path === '/api/v1/relationships/1/timeline') return Response.json({
        canonical_id: 1, identity_revision: 1, cache_revision: 'cache-rel', rows: [], total_count: 0
      });
      if (path === '/api/v1/participants/1/files/search') return Response.json({
        files: [fileRow], total_count: 1, cache_revision: 'cache-rel', search_provenance: {}
      });
      if (path === '/api/v1/files/9') return Response.json({
        id: 9, filename: 'report.pdf', mime_type: 'application/pdf', content_state: 'local_content',
        message_id: 42, conversation_id: 70
      });
      return Response.json(exploreResponse());
    });
    const state = new ExploreState(window);
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state });

    const row = (await screen.findByText('report.pdf')).closest('[role="row"]')!;
    await fireEvent.click(row);
    await fireEvent.click(await screen.findByRole('button', { name: 'Open containing item' }));

    await waitFor(() => expect(state.current.workspace).toBe('everything'));
    expect(state.current.selectedRow).toBe('message:42');

    rendered.unmount();
    state.destroy();
  });


  it('does not mutate Everything state when Escape bubbles up from an empty Relationships hub', async () => {
    window.history.replaceState(null, '', '/');
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const path = new URL(input instanceof Request ? input.url : String(input)).pathname;
      if (path === '/api/v1/relationships') return Response.json({ rows: [] });
      return Response.json(exploreResponse());
    });
    const state = new ExploreState(window);
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state });

    await screen.findByRole('main', { name: 'Relationships' });
    expect(state.current.workspace).toBe('relationships');

    // A groupingChain left behind by a prior Everything session: commitWorkspace
    // does not reset it, and Relationships never uses it — a bubbled Escape
    // from the hub must not silently pop it.
    state.replaceTransient({ groupingChain: ['source'], selectedRow: 'message:1' });

    const grid = screen.getByRole('grid', { name: 'Relationship results' });
    grid.focus();
    await fireEvent.keyDown(grid, { key: 'Escape' });

    expect(state.current.groupingChain).toEqual(['source']);
    expect(state.current.selectedRow).toBe('message:1');
    expect(state.current.workspace).toBe('relationships');

    rendered.unmount();
    state.destroy();
  });


  it('debounces filename search typing into one committed state write', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
    const searchRequests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      if (new URL(request.url).pathname === '/api/v1/files/search') {
        searchRequests.push(request);
        return Response.json({ files: [], total_count: 0, cache_revision: 'cache-1', search_provenance: {} });
      }
      return Response.json(exploreResponse());
    });
    const state = new ExploreState(window);
    state.replaceTransient({ workspace: 'files' });
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state });
    try {
      const input = await screen.findByLabelText('Filter filename');
      await screen.findByRole('grid', { name: 'Files results' });
      const initialRequestCount = searchRequests.length;
      await fireEvent.input(input, { target: { value: 'in' } });
      await fireEvent.input(input, { target: { value: 'invoice' } });
      expect(state.current.fileFilenameQuery).toBe('');
      expect(searchRequests.length).toBe(initialRequestCount);
      await vi.advanceTimersByTimeAsync(250);
      expect(state.current.fileFilenameQuery).toBe('invoice');
      expect(searchRequests.length).toBe(initialRequestCount + 1);
      const lastRequest = searchRequests[searchRequests.length - 1]!;
      await expect(lastRequest.clone().json()).resolves.toMatchObject({ filename_query: 'invoice' });
    } finally {
      rendered.unmount();
      state.destroy();
      vi.useRealTimers();
    }
  });


  it('flushes a pending debounced filename patch before a MIME-filter navigation commits', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
    const searchRequests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      if (new URL(request.url).pathname === '/api/v1/files/search') {
        searchRequests.push(request);
        return Response.json({ files: [], total_count: 0, cache_revision: 'cache-1', search_provenance: {} });
      }
      return Response.json(exploreResponse());
    });
    const state = new ExploreState(window);
    state.replaceTransient({ workspace: 'files' });
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state });
    try {
      const input = await screen.findByLabelText('Filter filename');
      const pdfCheckbox = await screen.findByRole('checkbox', { name: 'pdf' });
      await screen.findByRole('grid', { name: 'Files results' });
      const initialRequestCount = searchRequests.length;
      // Start a debounced filename-search patch (queued for 250ms) and then,
      // still inside that window, commit a navigation.
      await fireEvent.input(input, { target: { value: 'invoice' } });
      await fireEvent.click(pdfCheckbox);
      // The pending patch flushes immediately so the typed text is not lost;
      // the navigation commit applies on top and wins for fileMIMEFamilies.
      expect(state.current.fileFilenameQuery).toBe('invoice');
      expect(state.current.fileMIMEFamilies).toEqual(['pdf']);
      expect(searchRequests.length).toBe(initialRequestCount + 1);
      const lastRequest = searchRequests[searchRequests.length - 1]!;
      await expect(lastRequest.clone().json()).resolves.toMatchObject({
        filename_query: 'invoice', mime_families: ['pdf']
      });

      await vi.advanceTimersByTimeAsync(250);
      // Flushing cleared the pending timer, so nothing fires again later.
      expect(state.current.fileFilenameQuery).toBe('invoice');
      expect(searchRequests.length).toBe(initialRequestCount + 1);
    } finally {
      rendered.unmount();
      state.destroy();
      vi.useRealTimers();
    }
  });


  it('discards a pending debounced filename patch on Back instead of letting it clobber restored state', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
    const searchRequests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      if (new URL(request.url).pathname === '/api/v1/files/search') {
        searchRequests.push(request);
        return Response.json({ files: [], total_count: 0, cache_revision: 'cache-1', search_provenance: {} });
      }
      return Response.json(exploreResponse());
    });
    const state = new ExploreState(window);
    state.commitNavigation({ workspace: 'files' });
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state });
    try {
      const input = await screen.findByLabelText('Filter filename');
      await screen.findByRole('grid', { name: 'Files results' });
      const initialRequestCount = searchRequests.length;
      // Queue a debounced filename-search patch, then navigate back before
      // its timer fires. The restored state must not be clobbered later.
      await fireEvent.input(input, { target: { value: 'invoice' } });
      window.history.back();
      await new Promise((resolve) => window.addEventListener('popstate', resolve, { once: true }));

      expect(state.current.fileFilenameQuery).toBe('');
      await vi.advanceTimersByTimeAsync(250);
      expect(state.current.fileFilenameQuery).toBe('');
      expect(searchRequests.length).toBe(initialRequestCount);
    } finally {
      rendered.unmount();
      state.destroy();
      vi.useRealTimers();
    }
  });


  it('drills a Files group into a canonical filter without opening a stale group selection', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      const path = new URL(request.url).pathname;
      if (path.endsWith('/groups')) return Response.json({
        rows: [{ key: '7', label: 'Example source', count: 12, estimated_bytes: 42, latest_at: '2026-07-18T12:00:00Z' }],
        total_count: 1, cache_revision: 'cache-1', search_provenance: {}
      });
      if (path.endsWith('/files/search')) return Response.json({
        files: [], total_count: 0, cache_revision: 'cache-1', search_provenance: {}
      });
      return Response.json(exploreResponse());
    });
    const state = new ExploreState(window);
    state.replaceTransient({
      workspace: 'files', groupingChain: ['source'], fileFilenameQuery: 'invoice', fileMIMEFamilies: ['pdf']
    });
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state });

    await screen.findByRole('grid', { name: 'Files grouped by source' });
    await screen.findByText('Example source');
    const groupRequest = requests.find((request) => new URL(request.url).pathname.endsWith('/groups'))!;
    expect(new URL(groupRequest.url).pathname).toBe('/api/v1/files/groups');
    await expect(groupRequest.clone().json()).resolves.toMatchObject({
      filename_query: 'invoice', mime_families: ['pdf'], grouping: ['source']
    });
    await fireEvent.click(screen.getByRole('button', { name: 'Drill into Example source' }));
    await screen.findByRole('grid', { name: 'Files results' });

    expect(state.current.workspace).toBe('files');
    expect(state.current.filters).toEqual([{ dimension: 'source', values: ['7'] }]);
    expect(state.current.groupingChain).toEqual([]);
    expect(state.current.selectedRow).toBeNull();
    rendered.unmount();
    state.destroy();
  });


  it('clears a stale Everything sortNotice when the workspace changes', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = new URL(request.url).pathname;
      if (path === '/api/v1/files/search') return Response.json({
        files: [], total_count: 0, cache_revision: 'cache-1', search_provenance: {}
      });
      return Response.json(exploreResponse());
    });
    const state = new ExploreState(window);
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state });
    await screen.findByRole('grid', { name: 'Everything results' });

    await fireEvent.keyDown(window, { key: 'r' });
    expect(screen.getByRole('status', { name: 'Sort status' }).textContent)
      .toContain('reverse order is not supported');

    await fireEvent.click(screen.getByRole('button', { name: 'Files' }));
    expect(screen.getByRole('status', { name: 'Sort status' }).textContent)
      .toBe('Newest first is the canonical Everything order.');
    rendered.unmount();
    state.destroy();
  });


  it('announces the End cap outside Everything in the files-shell grouped workspace', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
    let groupPostCount = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = new URL(request.url).pathname;
      if (path !== '/api/v1/files/groups') return Response.json(exploreResponse());
      groupPostCount += 1;
      const page = groupPostCount;
      return Response.json({
        rows: [{ key: String(page), label: `Source ${page}`, count: 1, estimated_bytes: 42, latest_at: '2026-07-18T12:00:00Z' }],
        total_count: 10_000, cache_revision: 'cache-1', search_provenance: {}, next_cursor: `cursor-${page}`
      });
    });
    const state = new ExploreState(window);
    state.replaceTransient({ workspace: 'files', groupingChain: ['source'] });
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state });
    const grid = await screen.findByRole('grid', { name: 'Files grouped by source' });
    await screen.findByText('Source 1');

    grid.focus();
    await fireEvent.keyDown(grid, { key: 'End' });

    await waitFor(() => expect(groupPostCount).toBe(1 + LOAD_THROUGH_END_MAX_PAGES));
    await waitFor(() =>
      expect(screen.getByRole('status', { name: 'Sort status' }).textContent).toMatch(/press End again to continue/)
    );
    rendered.unmount();
    state.destroy();
  });


  it('clears a stale Files End-cap notice when switching presentation to Everything, but not on mere paging', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
    let groupPostCount = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = new URL(request.url).pathname;
      if (path === '/api/v1/files/groups') {
        groupPostCount += 1;
        const page = groupPostCount;
        return Response.json({
          rows: [
            { key: `${page}a`, label: `Source ${page}a`, count: 1, estimated_bytes: 42, latest_at: '2026-07-18T12:00:00Z' },
            { key: `${page}b`, label: `Source ${page}b`, count: 1, estimated_bytes: 42, latest_at: '2026-07-18T12:00:00Z' }
          ],
          total_count: 10_000, cache_revision: 'cache-1', search_provenance: {}, next_cursor: `cursor-${page}`
        });
      }
      return Response.json(exploreResponse());
    });
    const state = new ExploreState(window);
    state.replaceTransient({ workspace: 'files', groupingChain: ['source'] });
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state });
    const grid = await screen.findByRole('grid', { name: 'Files grouped by source' });
    await screen.findByText('Source 1a');

    grid.focus();
    await fireEvent.keyDown(grid, { key: 'End' });
    await waitFor(() => expect(groupPostCount).toBe(1 + LOAD_THROUGH_END_MAX_PAGES));
    await waitFor(() =>
      expect(screen.getByRole('status', { name: 'Sort status' }).textContent).toMatch(/press End again to continue/)
    );

    // Paging/navigating within the same workspace must not clear the notice.
    await fireEvent.keyDown(grid, { key: 'ArrowDown' });
    expect(screen.getByRole('status', { name: 'Sort status' }).textContent).toMatch(/press End again to continue/);

    await chooseSelectOption(screen.getByRole('combobox', { name: /^Show as:/ }), 'Table');
    await waitFor(() => expect(state.current.workspace).toBe('everything'));
    expect(screen.getByRole('status', { name: 'Sort status' }).textContent)
      .toBe('Newest first is the canonical Everything order.');
    rendered.unmount();
    state.destroy();
  });


  it('shares nested grouping between the context picker and command palette', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
    const state = new ExploreState(window);
    const rendered = render(AppShell, {
      client: createAPIClient(vi.fn()), state, enabled: false
    });

    await fireEvent.keyDown(window, { key: 'g' });
    const picker = screen.getByRole('combobox', { name: /Group by/ });
    expect(document.activeElement).toBe(picker);
    expect(picker.getAttribute('aria-expanded')).toBe('true');
    await fireEvent.click(screen.getByRole('option', { name: 'People' }));
    expect(state.current.groupingChain).toEqual(['participant']);

    await fireEvent.keyDown(window, { key: 'g' });
    await fireEvent.click(screen.getByRole('option', { name: 'Year' }));
    expect(state.current.groupingChain).toEqual(['participant', 'year']);
    expect(screen.getByLabelText('Active analytical context').textContent).toContain('Group People');
    expect(screen.getByLabelText('Active analytical context').textContent).toContain('Year');

    await fireEvent.keyDown(window, { key: 'k', ctrlKey: true });
    const palette = screen.getByRole('dialog', { name: 'Everything commands' });
    expect(palette).toBeDefined();
    expect(within(palette).getByRole('option', { name: /Labels — unavailable/ }).getAttribute('aria-disabled'))
      .toBe('true');
    const paletteInput = within(palette).getByRole('combobox');
    paletteInput.focus();
    await waitFor(() => expect(appShortcuts.activeScope()).toBe('everything-editable'));
    await fireEvent.input(paletteInput, { target: { value: 'group' } });
    await fireEvent.keyDown(paletteInput, { key: 'Escape' });
    expect(screen.getByRole('dialog', { name: 'Everything commands' })).toBeDefined();
    expect((paletteInput as HTMLInputElement).value).toBe('');
    expect(appShortcuts.activeScope()).toBe('everything-editable');
    await fireEvent.keyDown(paletteInput, { key: 'Escape' });
    await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Everything commands' })).toBeNull());
    await waitFor(() => expect(appShortcuts.activeScope()).toBe('root'));

    await fireEvent.keyDown(window, { key: 'f' });
    expect(screen.getByRole('button', { name: 'Filters' }).getAttribute('aria-expanded')).toBe('true');
    await fireEvent.keyDown(window, { key: 'r' });
    expect(screen.getByRole('status', { name: 'Sort status' }).textContent).toContain('newest first');
    rendered.unmount();
    state.destroy();
  });
});
