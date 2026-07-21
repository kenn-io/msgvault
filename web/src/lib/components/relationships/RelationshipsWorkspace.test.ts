import { fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../../api/client';
import { RelationshipsController } from '../../relationships/controller.svelte';
import { computeHubLayout } from './RelationshipsWorkspace.svelte';
import RelationshipsWorkspace from './RelationshipsWorkspace.svelte';

const when = '2026-07-19T10:00:00Z';

function person(id: number, label: string) {
  return {
    id, display_label: label, partial_label: false, identifiers: [],
    activity_count: 4, file_count: 2, source_counts: [], first_at: when, last_at: when, cache_revision: 'cache-rel'
  };
}

function pathOf(request: Request): string {
  return new URL(request.url, document.baseURI).pathname;
}

/** Forces computeHubLayout into 'narrow' by making the hub root report a
 * sub-720px clientWidth on mount — jsdom's own layout engine always reports
 * 0, and the TestResizeObserver in src/test/setup.ts never fires, so this is
 * the only way to reach drawer mode from a render test. Restore in the same
 * test via the returned cleanup.
 *
 * jsdom defines `clientWidth` on Element.prototype, not HTMLElement.prototype
 * (verified: `getOwnPropertyDescriptor(HTMLElement.prototype, 'clientWidth')`
 * is `undefined`), so `original` here is always `undefined` — the override
 * always creates a new own property on HTMLElement.prototype that shadows
 * the inherited one. The restore must `delete` that own property in that
 * case; a bare `if (original)` guard never fires and leaks the 400 override
 * into every later test in this file. */
function forceNarrowContainer(): () => void {
  const proto = window.HTMLElement.prototype;
  const original = Object.getOwnPropertyDescriptor(proto, 'clientWidth');
  Object.defineProperty(proto, 'clientWidth', { configurable: true, get: () => 400 });
  return () => {
    if (original) Object.defineProperty(proto, 'clientWidth', original);
    else delete (proto as { clientWidth?: unknown }).clientWidth;
  };
}

function baseProps(fetchFn: typeof fetch) {
  return {
    client: createAPIClient(fetchFn),
    controller: new RelationshipsController(createAPIClient(fetchFn), () => 'UTC'),
    facet: 'people' as const,
    target: null,
    showAll: false,
    filesOpen: false,
    predicate: { filters: [], presentation: 'table' as const },
    onFacetChange: vi.fn(),
    onTargetChange: vi.fn(),
    onShowAllChange: vi.fn(),
    onFilesToggle: vi.fn()
  };
}

function fetchHandler(overrides: Record<string, (request: Request) => Promise<Response> | Response> = {}) {
  const requests: Request[] = [];
  const fetchFn = vi.fn<typeof fetch>(async (input) => {
    const request = input instanceof Request ? input : new Request(input);
    requests.push(request);
    const handler = overrides[pathOf(request)];
    if (handler) return handler(request);
    if (pathOf(request) === '/api/v1/relationships') {
      return Response.json({
        rows: [{
          canonical_id: 1, display_label: 'Alice Example', last_at: when, member_ids: [1], score: 2,
          signals: { last_interaction_at: when, meeting_count: 0, meetings_together: 0, modalities: 2, received_from_them: 1, sent_count: 3, sent_to_them: 1 }
        }]
      });
    }
    throw new Error(`unexpected path ${pathOf(request)}`);
  });
  return { fetchFn, requests };
}

describe('RelationshipsWorkspace', () => {
  it('renders the ranked list and the empty-state header with no reading pane open', async () => {
    const { fetchFn } = fetchHandler();
    render(RelationshipsWorkspace, { props: baseProps(fetchFn) });

    expect(await screen.findByText('Alice Example')).toBeDefined();
    expect(screen.getByText('Select a person or domain')).toBeDefined();
    expect(screen.queryByRole('complementary', { name: /Reading pane/ })).toBeNull();
  });

  it('selecting a list row calls onTargetChange and opens it through the controller', async () => {
    const { fetchFn, requests } = fetchHandler({
      '/api/v1/people/1': async () => Response.json(person(1, 'Alice Example')),
      '/api/v1/relationships/1/timeline': async () => Response.json({
        canonical_id: 1, identity_revision: 1, cache_revision: 'cache-rel',
        rows: [{ key: 'message:1', kind: 'email', occurred_at: when, preview: 'Preview', source_id: 1, title: 'Subject', has_attachments: false, message_count: 1 }],
        total_count: 1
      })
    });
    const props = baseProps(fetchFn);
    render(RelationshipsWorkspace, { props });

    await fireEvent.pointerDown((await screen.findByText('Alice Example')).closest('[role="row"]')!);
    expect(props.onTargetChange).toHaveBeenCalledWith('cluster:1');

    await waitFor(() => expect(requests.some((request) => pathOf(request) === '/api/v1/people/1')).toBe(true));
    expect(await screen.findByRole('heading', { name: 'Alice Example' })).toBeDefined();
    expect(await screen.findByText('Subject')).toBeDefined();
  });

  it('single-clicking a timeline row opens the conversation thread in the reading pane', async () => {
    const { fetchFn } = fetchHandler({
      '/api/v1/people/1': async () => Response.json(person(1, 'Alice Example')),
      '/api/v1/relationships/1/timeline': async () => Response.json({
        canonical_id: 1, identity_revision: 1, cache_revision: 'cache-rel',
        rows: [{ key: 'message:1', kind: 'email', occurred_at: when, preview: 'Preview text', source_id: 1, title: 'Subject line', has_attachments: false, message_count: 1, anchor_message_id: 9, conversation_id: 70 }],
        total_count: 1
      }),
      '/api/v1/conversations/70': async () => Response.json({
        id: 70, anchor_id: 9, has_before: false, has_after: false, total: 1,
        messages: [{
          id: 9, conversation_id: 70, subject: 'Subject line', from: 'alice@example.com',
          to: ['me@example.com'], sent_at: when, snippet: 'Preview text', body: 'Full message body text'
        }]
      })
    });
    const props = { ...baseProps(fetchFn), target: 'cluster:1' };
    render(RelationshipsWorkspace, { props });
    await props.controller.openTarget('cluster:1', props.predicate);

    await fireEvent.click((await screen.findByText('Subject line')).closest('[role="row"]')!);
    const reading = await screen.findByRole('complementary', { name: /Reading pane/ });
    // The anchor message renders expanded immediately — actual content, not metadata.
    expect(await within(reading).findByText('Full message body text')).toBeDefined();
    expect(within(reading).getByRole('button', { name: 'Collapse message 9 from alice@example.com' })).toBeDefined();
  });

  it('opens a chat_burst directly into the conversation window bounded to the local day', async () => {
    const requestedConversations: Request[] = [];
    const { fetchFn } = fetchHandler({
      '/api/v1/people/1': async () => Response.json(person(1, 'Alice Example')),
      '/api/v1/relationships/1/timeline': async () => Response.json({
        canonical_id: 1, identity_revision: 1, cache_revision: 'cache-rel',
        rows: [{
          key: 'burst:2:70:2026-07-18', kind: 'chat_burst', occurred_at: '2026-07-18T20:00:00Z',
          first_at: '2026-07-18T08:00:00Z', preview: 'Latest', source_id: 2, title: 'Team Chat',
          has_attachments: false, message_count: 6, anchor_message_id: 500, conversation_id: 70
        }],
        total_count: 1
      }),
      '/api/v1/conversations/70': async (request) => {
        requestedConversations.push(request);
        return Response.json({ id: 70, anchor_id: 500, messages: [], has_before: false, has_after: false, total: 0 });
      }
    });
    const props = { ...baseProps(fetchFn), target: 'cluster:1' };
    render(RelationshipsWorkspace, { props });
    await props.controller.openTarget('cluster:1', props.predicate);

    await fireEvent.click((await screen.findByText('6 messages in Team Chat')).closest('[role="row"]')!);
    await waitFor(() => expect(requestedConversations).toHaveLength(1));
    const url = new URL(requestedConversations[0]!.url);
    expect(url.searchParams.get('anchor')).toBe('500');
    const start = new Date(url.searchParams.get('start')!);
    const end = new Date(url.searchParams.get('end')!);
    expect(start.getHours()).toBe(0);
    expect(end.getTime() - start.getTime()).toBe(24 * 60 * 60 * 1000);
    expect(new Date('2026-07-18T08:00:00Z').getTime()).toBeGreaterThanOrEqual(start.getTime());
    expect(new Date('2026-07-18T08:00:00Z').getTime()).toBeLessThan(end.getTime());
  });

  it('swaps the center pane to FilesWorkspace when filesOpen is true', async () => {
    const { fetchFn } = fetchHandler({
      '/api/v1/people/1': async () => Response.json(person(1, 'Alice Example')),
      '/api/v1/relationships/1/timeline': async () => Response.json({
        canonical_id: 1, identity_revision: 1, cache_revision: 'cache-rel', rows: [], total_count: 0
      }),
      '/api/v1/people/1/files/search': async () => Response.json({
        files: [], total_count: 0, cache_revision: 'cache-rel', search_provenance: {}
      })
    });
    const props = { ...baseProps(fetchFn), target: 'cluster:1', filesOpen: true };
    render(RelationshipsWorkspace, { props });
    await props.controller.openTarget('cluster:1', props.predicate);

    expect(await screen.findByRole('grid', { name: 'Files results' })).toBeDefined();
    expect(screen.queryByRole('grid', { name: 'Relationship activity' })).toBeNull();
  });

  it('debounces identity search typing into one /api/v1/people/search fetch', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      const searchRequests: Request[] = [];
      const { fetchFn } = fetchHandler({
        '/api/v1/people/search': async (request) => {
          searchRequests.push(request);
          return Response.json({ rows: [], total_count: 0, cache_revision: 'cache-rel', search_provenance: {} });
        }
      });
      const props = baseProps(fetchFn);
      render(RelationshipsWorkspace, { props });
      await screen.findByText('Alice Example');

      const search = screen.getByRole('searchbox', { name: 'Search people and domains' });
      await fireEvent.input(search, { target: { value: 'a' } });
      await fireEvent.input(search, { target: { value: 'al' } });
      await fireEvent.input(search, { target: { value: 'ali' } });

      // The typed text shows immediately even though the search fetch it
      // drives is still debounced.
      expect((search as HTMLInputElement).value).toBe('ali');
      expect(searchRequests).toHaveLength(0);

      await vi.advanceTimersByTimeAsync(250);

      expect(searchRequests).toHaveLength(1);
      await expect(searchRequests[0]!.clone().json()).resolves.toMatchObject({ identity_query: 'ali' });
    } finally {
      vi.useRealTimers();
    }
  });

  it('flushes a pending debounced query write on unmount instead of dropping it', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      const searchRequests: Request[] = [];
      const { fetchFn } = fetchHandler({
        '/api/v1/people/search': async (request) => {
          searchRequests.push(request);
          return Response.json({ rows: [], total_count: 0, cache_revision: 'cache-rel', search_provenance: {} });
        }
      });
      const props = baseProps(fetchFn);
      const { unmount } = render(RelationshipsWorkspace, { props });
      await screen.findByText('Alice Example');

      const search = screen.getByRole('searchbox', { name: 'Search people and domains' });
      await fireEvent.input(search, { target: { value: 'ali' } });
      expect(props.controller.query, 'not flushed yet').toBe('');

      unmount();
      expect(props.controller.query, 'unmounting must flush, not drop, the pending write').toBe('ali');

      // The controller is owned by AppShell and outlives the round-trip:
      // remounting re-reads controller.query fresh and refetches with it.
      render(RelationshipsWorkspace, { props });
      await waitFor(() => expect(searchRequests).toHaveLength(1));
      await expect(searchRequests[0]!.clone().json()).resolves.toMatchObject({ identity_query: 'ali' });
    } finally {
      vi.useRealTimers();
    }
  });

  it('clears the reading pane when the target is cleared externally, not through this component\'s own Esc handler', async () => {
    const { fetchFn } = fetchHandler({
      '/api/v1/people/1': async () => Response.json(person(1, 'Alice Example')),
      '/api/v1/relationships/1/timeline': async () => Response.json({
        canonical_id: 1, identity_revision: 1, cache_revision: 'cache-rel',
        rows: [{ key: 'message:1', kind: 'email', occurred_at: when, preview: 'Preview', source_id: 1, title: 'Subject', has_attachments: false, message_count: 1 }],
        total_count: 1
      })
    });
    const props = { ...baseProps(fetchFn), target: 'cluster:1' };
    const { rerender } = render(RelationshipsWorkspace, { props });
    await props.controller.openTarget('cluster:1', props.predicate);

    await fireEvent.click((await screen.findByText('Subject')).closest('[role="row"]')!);
    await screen.findByRole('complementary', { name: /Reading pane/ });

    // AppShell's own hydration effect clears the target directly on a
    // Back/Forward popstate — never through this component's handleEscape.
    await rerender({ ...props, target: null });

    expect(screen.queryByRole('complementary', { name: /Reading pane/ })).toBeNull();
  });

  it('does not scope the embedded FilesWorkspace to the previous cluster while a fast target switch is still resolving', async () => {
    const { fetchFn } = fetchHandler({
      '/api/v1/people/1': async () => Response.json(person(1, 'Alice Example')),
      '/api/v1/relationships/1/timeline': async () => Response.json({
        canonical_id: 1, identity_revision: 1, cache_revision: 'cache-rel', rows: [], total_count: 0
      }),
      '/api/v1/people/1/files/search': async () => Response.json({
        files: [], total_count: 0, cache_revision: 'cache-rel', search_provenance: {}
      })
    });
    const props = { ...baseProps(fetchFn), target: 'cluster:1', filesOpen: true };
    const { rerender } = render(RelationshipsWorkspace, { props });
    await props.controller.openTarget('cluster:1', props.predicate);
    expect(await screen.findByRole('grid', { name: 'Files results' })).toBeDefined();

    // Simulate AppShell's own timing: the `target` prop (driven by the URL)
    // advances to the new cluster before the controller's own openTarget for
    // it has run — controller.canonicalID still holds cluster 1's id here.
    await rerender({ ...props, target: 'cluster:2' });

    expect(props.controller.target).toBe('cluster:1');
    expect(props.controller.canonicalID).toBe(1);
    expect(screen.queryByRole('grid', { name: 'Files results' })).toBeNull();
  });

  it('wires the embedded FilesWorkspace onOpenItem/onOpenConversation callbacks when provided', async () => {
    const fileRow = {
      id: 9, key: 'file:9', entry_key: 'message:42', message_id: 42, conversation_id: 70,
      occurred_at: when, source_id: 1, source_type: 'gmail', source_identifier: 'alice@example.com',
      containing_title: 'Subject', filename: 'report.pdf', mime_type: 'application/pdf', mime_family: 'pdf',
      size_bytes: 100, content_state: 'local_content', content_available: true
    };
    const { fetchFn } = fetchHandler({
      '/api/v1/people/1': async () => Response.json(person(1, 'Alice Example')),
      '/api/v1/relationships/1/timeline': async () => Response.json({
        canonical_id: 1, identity_revision: 1, cache_revision: 'cache-rel', rows: [], total_count: 0
      }),
      '/api/v1/people/1/files/search': async () => Response.json({
        files: [fileRow], total_count: 1, cache_revision: 'cache-rel', search_provenance: {}
      }),
      '/api/v1/files/9': async () => Response.json({
        id: 9, filename: 'report.pdf', mime_type: 'application/pdf', content_state: 'local_content',
        message_id: 42, conversation_id: 70
      })
    });
    const onOpenFileItem = vi.fn();
    const onOpenFileConversation = vi.fn();
    const props = { ...baseProps(fetchFn), target: 'cluster:1', filesOpen: true, onOpenFileItem, onOpenFileConversation };
    render(RelationshipsWorkspace, { props });
    await props.controller.openTarget('cluster:1', props.predicate);

    const row = (await screen.findByText('report.pdf')).closest('[role="row"]')!;
    await fireEvent.click(row);

    await fireEvent.click(await screen.findByRole('button', { name: 'Open containing item' }));
    expect(onOpenFileItem).toHaveBeenCalledWith('message:42');

    await fireEvent.click(await screen.findByRole('button', { name: 'Open containing conversation' }));
    expect(onOpenFileConversation).toHaveBeenCalledWith('message:42', 42, 70);
  });

  it('does not act on Esc while another scope (e.g. the Link identity dialog) is active, letting the Modal handle it', async () => {
    const { fetchFn } = fetchHandler({
      '/api/v1/people/1': async () => Response.json(person(1, 'Alice Example'))
    });
    const props = { ...baseProps(fetchFn), target: 'cluster:1' };
    render(RelationshipsWorkspace, { props });
    await props.controller.openTarget('cluster:1', props.predicate);
    await screen.findByRole('heading', { name: 'Alice Example' });

    await fireEvent.click(screen.getByRole('button', { name: 'Link identity' }));
    expect(screen.getByRole('dialog', { name: 'Link identity' })).toBeDefined();

    await fireEvent.keyDown(screen.getByRole('grid', { name: 'Relationship activity' }), { key: 'Escape' });

    // The hub's own Esc handling must not have fired: the target is still
    // open. Escape instead bubbles untouched to the Modal's own window-level
    // listener, which closes the dialog.
    expect(props.onTargetChange).not.toHaveBeenCalled();
    await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Link identity' })).toBeNull());
  });

  it('calls the domain files endpoint, not the person one, when the open target is a domain', async () => {
    const filesRequests: Request[] = [];
    const { fetchFn } = fetchHandler({
      '/api/v1/domains/example.com': async () => Response.json({
        domain: 'example.com', activity_count: 3, file_count: 1, person_count: 2,
        first_at: when, last_at: when, source_counts: [], cache_revision: 'cache-rel'
      }),
      '/api/v1/domains/example.com/timeline': async () => Response.json({ rows: [], total_count: 0 }),
      '/api/v1/domains/example.com/files/search': async (request) => {
        filesRequests.push(request);
        return Response.json({ files: [], total_count: 0, cache_revision: 'cache-rel', search_provenance: {} });
      }
    });
    const props = { ...baseProps(fetchFn), facet: 'domains' as const, target: 'domain:example.com', filesOpen: true };
    render(RelationshipsWorkspace, { props });
    await props.controller.openTarget('domain:example.com', props.predicate);

    expect(await screen.findByRole('grid', { name: 'Files results' })).toBeDefined();
    expect(filesRequests).toHaveLength(1);
  });

  it('does not mount FilesWorkspace for a cluster target until its scope resolves, then issues exactly one scoped fetch', async () => {
    let resolveTimeline: ((response: Response) => void) | undefined;
    const filesRequests: Request[] = [];
    const { fetchFn } = fetchHandler({
      '/api/v1/people/1': async () => Response.json(person(1, 'Alice Example')),
      '/api/v1/relationships/1/timeline': async () => new Promise<Response>((resolve) => { resolveTimeline = resolve; }),
      '/api/v1/people/1/files/search': async (request) => {
        filesRequests.push(request);
        return Response.json({ files: [], total_count: 0, cache_revision: 'cache-rel', search_provenance: {} });
      }
    });
    const props = { ...baseProps(fetchFn), target: 'cluster:1', filesOpen: true };
    render(RelationshipsWorkspace, { props });
    const openPromise = props.controller.openTarget('cluster:1', props.predicate);

    await screen.findByText('Loading activity…');
    expect(screen.queryByRole('grid', { name: 'Files results' })).toBeNull();
    expect(filesRequests).toHaveLength(0);

    resolveTimeline?.(Response.json({
      canonical_id: 1, identity_revision: 1, cache_revision: 'cache-rel', rows: [], total_count: 0
    }));
    await openPromise;

    expect(await screen.findByRole('grid', { name: 'Files results' })).toBeDefined();
    await waitFor(() => expect(filesRequests).toHaveLength(1));
  });

  it('treats files as closed when the target is null, even if relationshipFiles is still true', async () => {
    const { fetchFn } = fetchHandler();
    const props = { ...baseProps(fetchFn), target: null, filesOpen: true };
    render(RelationshipsWorkspace, { props });

    await screen.findByText('Alice Example');
    expect(screen.queryByRole('grid', { name: 'Files results' })).toBeNull();
    expect(screen.getByText('Select a person or domain')).toBeDefined();
  });

  it('walks Esc back one layer at a time: reading pane, then the open target', async () => {
    const { fetchFn } = fetchHandler({
      '/api/v1/people/1': async () => Response.json(person(1, 'Alice Example')),
      '/api/v1/relationships/1/timeline': async () => Response.json({
        canonical_id: 1, identity_revision: 1, cache_revision: 'cache-rel',
        rows: [{ key: 'message:1', kind: 'email', occurred_at: when, preview: 'Preview', source_id: 1, title: 'Subject', has_attachments: false, message_count: 1 }],
        total_count: 1
      })
    });
    const props = { ...baseProps(fetchFn), target: 'cluster:1' };
    render(RelationshipsWorkspace, { props });
    await props.controller.openTarget('cluster:1', props.predicate);

    await fireEvent.click((await screen.findByText('Subject')).closest('[role="row"]')!);
    const reading = await screen.findByRole('complementary', { name: /Reading pane/ });

    await fireEvent.keyDown(reading, { key: 'Escape' });
    await waitFor(() => expect(screen.queryByRole('complementary', { name: /Reading pane/ })).toBeNull());

    await fireEvent.keyDown(screen.getByRole('grid', { name: 'Relationship activity' }), { key: 'Escape' });
    expect(props.onTargetChange).toHaveBeenCalledWith(null);
  });

  it('moves focus to the timeline, then the list, as Esc walks back each layer', async () => {
    const { fetchFn } = fetchHandler({
      '/api/v1/people/1': async () => Response.json(person(1, 'Alice Example')),
      '/api/v1/relationships/1/timeline': async () => Response.json({
        canonical_id: 1, identity_revision: 1, cache_revision: 'cache-rel',
        rows: [{ key: 'message:1', kind: 'email', occurred_at: when, preview: 'Preview', source_id: 1, title: 'Subject', has_attachments: false, message_count: 1 }],
        total_count: 1
      })
    });
    const props = { ...baseProps(fetchFn), target: 'cluster:1' };
    render(RelationshipsWorkspace, { props });
    await props.controller.openTarget('cluster:1', props.predicate);

    await fireEvent.click((await screen.findByText('Subject')).closest('[role="row"]')!);
    const reading = await screen.findByRole('complementary', { name: /Reading pane/ });

    await fireEvent.keyDown(reading, { key: 'Escape' });
    const timelineGrid = screen.getByRole('grid', { name: 'Relationship activity' });
    await waitFor(() => expect(document.activeElement).toBe(timelineGrid));

    await fireEvent.keyDown(document.activeElement!, { key: 'Escape' });
    expect(props.onTargetChange).toHaveBeenCalledWith(null);
    const listGrid = screen.getByRole('grid', { name: 'Relationship results' });
    await waitFor(() => expect(document.activeElement).toBe(listGrid));
  });
});

describe('RelationshipsWorkspace drawer (narrow layout)', () => {
  it('is inert while closed; opening focuses the search input and traps Tab; Esc returns focus to the toggle', async () => {
    const restoreContainer = forceNarrowContainer();
    try {
      const { fetchFn } = fetchHandler();
      render(RelationshipsWorkspace, { props: baseProps(fetchFn) });
      await screen.findByText('Alice Example');

      const listPane = document.querySelector<HTMLElement & { inert: boolean }>('.pane-list')!;
      expect(listPane.inert).toBe(true);

      const toggle = screen.getByRole('button', { name: 'Show relationship list' });
      await fireEvent.click(toggle);
      expect(listPane.inert).toBe(false);

      const searchInput = screen.getByRole('searchbox', { name: 'Search people and domains' });
      await waitFor(() => expect(document.activeElement).toBe(searchInput));

      const resultsGrid = screen.getByRole('grid', { name: 'Relationship results' });
      resultsGrid.focus();
      await fireEvent.keyDown(resultsGrid, { key: 'Tab' });
      expect(document.activeElement).toBe(searchInput);

      await fireEvent.keyDown(searchInput, { key: 'Tab', shiftKey: true });
      expect(document.activeElement).toBe(resultsGrid);

      await fireEvent.keyDown(document.activeElement!, { key: 'Escape' });
      expect(listPane.inert).toBe(true);
      expect(document.activeElement).toBe(toggle);
    } finally {
      restoreContainer();
    }
  });

  it('Esc clearing the open target focuses the drawer toggle, not the still-inert list grid', async () => {
    const restoreContainer = forceNarrowContainer();
    try {
      const { fetchFn } = fetchHandler({
        '/api/v1/people/1': async () => Response.json(person(1, 'Alice Example')),
        '/api/v1/relationships/1/timeline': async () => Response.json({
          canonical_id: 1, identity_revision: 1, cache_revision: 'cache-rel', rows: [], total_count: 0
        })
      });
      const props = { ...baseProps(fetchFn), target: 'cluster:1' };
      render(RelationshipsWorkspace, { props });
      await props.controller.openTarget('cluster:1', props.predicate);
      await screen.findByRole('heading', { name: 'Alice Example' });

      // The drawer starts closed (selecting a row closes it), so the list
      // pane is inert; without the fix, Esc would call .focus() on it and
      // land nowhere real.
      const listPane = document.querySelector<HTMLElement & { inert: boolean }>('.pane-list')!;
      expect(listPane.inert).toBe(true);

      await fireEvent.keyDown(screen.getByRole('grid', { name: 'Relationship activity' }), { key: 'Escape' });
      expect(props.onTargetChange).toHaveBeenCalledWith(null);

      const toggle = screen.getByRole('button', { name: 'Show relationship list' });
      await waitFor(() => expect(document.activeElement).toBe(toggle));
    } finally {
      restoreContainer();
    }
  });
});

describe('forceNarrowContainer test helper', () => {
  it('fully restores clientWidth on cleanup instead of leaking the 400px override', () => {
    const probe = document.createElement('div');
    document.body.appendChild(probe);
    const baseline = probe.clientWidth;

    const restore = forceNarrowContainer();
    expect(probe.clientWidth).toBe(400);
    restore();

    expect(probe.clientWidth).toBe(baseline);
    probe.remove();
  });
});

describe('computeHubLayout', () => {
  it('classifies wide and narrow by container width', () => {
    expect(computeHubLayout(1300)).toBe('wide');
    expect(computeHubLayout(1100)).toBe('wide');
    expect(computeHubLayout(720)).toBe('wide');
    expect(computeHubLayout(719)).toBe('narrow');
    expect(computeHubLayout(400)).toBe('narrow');
  });
});
