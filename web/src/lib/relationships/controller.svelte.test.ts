import { describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../api/client';
import type { DomainSummary, PersonSummary } from '../explore/models';
import { RelationshipsController } from './controller.svelte';
import type { RelationshipRow, RelationshipTimelineRow } from './models';

const when = '2026-07-19T10:00:00Z';

function relationshipRow(id: number, label: string): RelationshipRow {
  return {
    canonical_id: id,
    display_label: label,
    last_at: when,
    member_ids: [id],
    score: 1,
    signals: {
      last_interaction_at: when,
      meeting_count: 0,
      meetings_together: 0,
      modalities: 1,
      received_from_them: 1,
      sent_count: 1,
      sent_to_them: 1
    }
  };
}

function person(id: number): PersonSummary {
  return {
    id,
    display_label: `Person ${id}`,
    partial_label: false,
    identifiers: [],
    activity_count: 1,
    file_count: 0,
    source_counts: [],
    first_at: when,
    last_at: when,
    cache_revision: 'cache-rel'
  };
}

function domainSummary(domain: string): DomainSummary {
  return {
    domain,
    activity_count: 3,
    file_count: 1,
    person_count: 2,
    first_at: when,
    last_at: when,
    source_counts: [],
    cache_revision: 'cache-rel'
  };
}

function timelineRow(key: string): RelationshipTimelineRow {
  return {
    key,
    kind: 'email',
    occurred_at: when,
    preview: 'preview',
    source_id: 1,
    title: key,
    has_attachments: false,
    message_count: 1
  };
}

function pathOf(request: Request): string {
  return new URL(request.url, document.baseURI).pathname;
}

describe('RelationshipsController.loadList', () => {
  it('ranks via /api/v1/relationships when the people facet has an empty query, preserving server order', async () => {
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      if (pathOf(request) === '/api/v1/relationships') {
        return Response.json({
          rows: [relationshipRow(2, 'Bob'), relationshipRow(1, 'Alice')],
          total_count: 2,
          cache_revision: 'cache-rel',
          identity_revision: 1
        });
      }
      throw new Error(`unexpected path ${pathOf(request)}`);
    });
    const controller = new RelationshipsController(createAPIClient(fetchFn), () => 'UTC');
    controller.showAll = true;

    await controller.loadList({ filters: [{ dimension: 'source', values: ['1'] }], presentation: 'table' });

    expect(controller.listRows).toEqual([relationshipRow(2, 'Bob'), relationshipRow(1, 'Alice')]);
    expect(controller.listLoading).toBe(false);
    expect(controller.listError).toBeNull();
    await expect(requests[0]!.clone().json()).resolves.toEqual({
      show_all: true,
      limit: 200,
      filters: [{ dimension: 'source', values: ['1'] }]
    });
  });

  it('searches via /api/v1/people/search once the query is non-empty, finding any person ranked or not', async () => {
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      if (pathOf(request) === '/api/v1/people/search') {
        return Response.json({ rows: [person(11)], total_count: 1, cache_revision: 'cache-rel', search_provenance: {} });
      }
      throw new Error(`unexpected path ${pathOf(request)}`);
    });
    const controller = new RelationshipsController(createAPIClient(fetchFn), () => 'UTC');
    controller.query = 'ali';

    await controller.loadList({ filters: [], presentation: 'table' });

    expect(controller.listRows).toEqual([person(11)]);
    await expect(requests[0]!.clone().json()).resolves.toMatchObject({ identity_query: 'ali' });
  });

  it('always searches domains via /api/v1/domains/search, even with an empty query', async () => {
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      if (pathOf(request) === '/api/v1/domains/search') {
        return Response.json({
          rows: [domainSummary('example.com')], total_count: 1, cache_revision: 'cache-rel', search_provenance: {}
        });
      }
      throw new Error(`unexpected path ${pathOf(request)}`);
    });
    const controller = new RelationshipsController(createAPIClient(fetchFn), () => 'UTC');
    controller.facet = 'domains';

    await controller.loadList({ filters: [], presentation: 'table' });

    expect(controller.listRows).toEqual([domainSummary('example.com')]);
    expect(requests).toHaveLength(1);
    await expect(requests[0]!.clone().json()).resolves.toMatchObject({ identity_query: '' });
  });

  it('names cache unavailability as a degraded state instead of throwing', async () => {
    const fetchFn = vi.fn<typeof fetch>(async () => Response.json({
      error: 'analytical_cache_unavailable', message: 'The committed analytical cache is unavailable',
      readiness: 'stale_schema', recovery_action: 'Rebuild the analytical cache'
    }, { status: 503 }));
    const controller = new RelationshipsController(createAPIClient(fetchFn), () => 'UTC');

    await expect(controller.loadList({ filters: [], presentation: 'table' })).resolves.toBeUndefined();

    expect(controller.degraded).toBe('cache_unavailable');
    expect(controller.listError).toBeNull();
    expect(controller.listLoading).toBe(false);
  });
});

describe('RelationshipsController.openTarget', () => {
  it("opens a cluster target's person header and cluster timeline, storing canonical_id + identity_revision", async () => {
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      const path = pathOf(request);
      if (path === '/api/v1/people/12' && request.method === 'GET') return Response.json(person(12));
      if (path === '/api/v1/relationships/12/timeline') {
        return Response.json({
          canonical_id: 12, identity_revision: 3, rows: [timelineRow('t1')], total_count: 1, cache_revision: 'cache-rel'
        });
      }
      throw new Error(`unexpected path ${path}`);
    });
    const controller = new RelationshipsController(createAPIClient(fetchFn), () => 'America/New_York');

    await controller.openTarget('cluster:12', {
      filters: [{ dimension: 'source', values: ['1'] }], presentation: 'table'
    });

    expect(controller.detail).toEqual(person(12));
    expect(controller.timelineRows).toEqual([timelineRow('t1')]);
    expect(controller.canonicalID).toBe(12);
    expect(controller.identityRevision).toBe(3);
    expect(controller.timelineLoading).toBe(false);
    const timelineRequest = requests.find((request) => pathOf(request) === '/api/v1/relationships/12/timeline');
    await expect(timelineRequest!.clone().json()).resolves.toMatchObject({
      timezone: 'America/New_York', filters: [{ dimension: 'source', values: ['1'] }], limit: 200
    });
  });

  it('opens a domain target through the existing domain detail + domain timeline endpoints, not a cluster timeline', async () => {
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      const path = pathOf(request);
      if (path === '/api/v1/domains/example.com' && request.method === 'GET') return Response.json(domainSummary('example.com'));
      if (path === '/api/v1/domains/example.com/timeline') {
        return Response.json({ rows: [timelineRow('t1')], total_count: 1, cache_revision: 'cache-rel', search_provenance: {} });
      }
      throw new Error(`unexpected path ${path}`);
    });
    const controller = new RelationshipsController(createAPIClient(fetchFn), () => 'UTC');

    await controller.openTarget('domain:example.com', { filters: [], presentation: 'table' });

    expect(controller.detail).toEqual(domainSummary('example.com'));
    expect(controller.timelineRows).toEqual([timelineRow('t1')]);
    expect(controller.canonicalID).toBeNull();
    expect(controller.identityRevision).toBeNull();
    expect(requests.some((request) => pathOf(request).includes('/relationships/'))).toBe(false);
  });

  it('dispatches on the target prefix, not the facet, when the two disagree', async () => {
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = pathOf(request);
      if (path === '/api/v1/domains/example.com' && request.method === 'GET') return Response.json(domainSummary('example.com'));
      if (path === '/api/v1/domains/example.com/timeline') {
        return Response.json({ rows: [], total_count: 0, cache_revision: 'cache-rel', search_provenance: {} });
      }
      throw new Error(`unexpected path ${path}`);
    });
    const controller = new RelationshipsController(createAPIClient(fetchFn), () => 'UTC');
    controller.facet = 'people';

    await controller.openTarget('domain:example.com', { filters: [], presentation: 'table' });

    expect(controller.detail).toEqual(domainSummary('example.com'));
  });

  it('discards a stale openTarget response that resolves after a newer openTarget call', async () => {
    let resolveFirstPerson: ((response: Response) => void) | undefined;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = pathOf(request);
      if (path === '/api/v1/people/1' && request.method === 'GET') {
        return new Promise<Response>((resolve) => { resolveFirstPerson = resolve; });
      }
      if (path === '/api/v1/relationships/1/timeline') {
        return Response.json({ canonical_id: 1, identity_revision: 1, rows: [], total_count: 0, cache_revision: 'cache-rel' });
      }
      if (path === '/api/v1/people/2' && request.method === 'GET') return Response.json(person(2));
      if (path === '/api/v1/relationships/2/timeline') {
        return Response.json({
          canonical_id: 2, identity_revision: 5, rows: [timelineRow('t2')], total_count: 1, cache_revision: 'cache-rel'
        });
      }
      throw new Error(`unexpected path ${path}`);
    });
    const controller = new RelationshipsController(createAPIClient(fetchFn), () => 'UTC');

    const firstOpen = controller.openTarget('cluster:1', { filters: [], presentation: 'table' });
    await controller.openTarget('cluster:2', { filters: [], presentation: 'table' });

    expect(controller.detail).toEqual(person(2));
    expect(controller.canonicalID).toBe(2);

    resolveFirstPerson?.(Response.json(person(1)));
    await firstOpen;

    expect(controller.detail).toEqual(person(2));
    expect(controller.canonicalID).toBe(2);
    expect(controller.timelineRows).toEqual([timelineRow('t2')]);
  });
});

describe('RelationshipsController.loadMoreTimeline', () => {
  it('sends the stored cursor and restarts from page 1 exactly once on a 409 cursor_invalidated', async () => {
    let timelineCalls = 0;
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      const path = pathOf(request);
      if (path === '/api/v1/people/1' && request.method === 'GET') return Response.json(person(1));
      if (path === '/api/v1/relationships/1/timeline') {
        timelineCalls += 1;
        const body = (await request.clone().json()) as { cursor?: string };
        if (body.cursor === undefined && timelineCalls === 1) {
          return Response.json({
            canonical_id: 1, identity_revision: 1, rows: [timelineRow('t1')], total_count: 3,
            cache_revision: 'cache-rel', next_cursor: 'page-2'
          });
        }
        if (body.cursor === 'page-2') {
          return Response.json({ error: 'cursor_invalidated', message: 'The timeline context changed; restart pagination' }, { status: 409 });
        }
        if (body.cursor === undefined && timelineCalls === 3) {
          return Response.json({
            canonical_id: 1, identity_revision: 2, rows: [timelineRow('t1-restart')], total_count: 1, cache_revision: 'cache-rel-2'
          });
        }
        throw new Error(`unexpected timeline call ${timelineCalls} body ${JSON.stringify(body)}`);
      }
      throw new Error(`unexpected path ${path}`);
    });
    const controller = new RelationshipsController(createAPIClient(fetchFn), () => 'UTC');
    await controller.openTarget('cluster:1', { filters: [], presentation: 'table' });
    expect(controller.timelineCursor).toBe('page-2');

    await controller.loadMoreTimeline();

    expect(timelineCalls).toBe(3);
    expect(controller.timelineRows).toEqual([timelineRow('t1-restart')]);
    expect(controller.timelineCursor).toBeNull();
    expect(controller.timelineLoadingMore).toBe(false);
    expect(controller.identityRevision).toBe(2);
    expect(controller.timelineRestartNotice).toBe('Timeline restarted: the archive changed.');
  });

  it('clears a stale restart notice when a fresh openTarget navigates elsewhere', async () => {
    let timelineCalls = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = pathOf(request);
      if (path === '/api/v1/people/1' && request.method === 'GET') return Response.json(person(1));
      if (path === '/api/v1/people/2' && request.method === 'GET') return Response.json(person(2));
      if (path === '/api/v1/relationships/1/timeline') {
        timelineCalls += 1;
        const body = (await request.clone().json()) as { cursor?: string };
        if (body.cursor === undefined && timelineCalls === 1) {
          return Response.json({
            canonical_id: 1, identity_revision: 1, rows: [timelineRow('t1')], total_count: 2,
            cache_revision: 'cache-rel', next_cursor: 'page-2'
          });
        }
        if (body.cursor === 'page-2') {
          return Response.json({ error: 'cursor_invalidated', message: 'restart pagination' }, { status: 409 });
        }
        return Response.json({
          canonical_id: 1, identity_revision: 2, rows: [timelineRow('t1-restart')], total_count: 1, cache_revision: 'cache-rel-2'
        });
      }
      if (path === '/api/v1/relationships/2/timeline') {
        return Response.json({ canonical_id: 2, identity_revision: 1, rows: [], total_count: 0, cache_revision: 'cache-rel' });
      }
      throw new Error(`unexpected path ${path}`);
    });
    const controller = new RelationshipsController(createAPIClient(fetchFn), () => 'UTC');
    await controller.openTarget('cluster:1', { filters: [], presentation: 'table' });
    await controller.loadMoreTimeline();
    expect(controller.timelineRestartNotice).toBe('Timeline restarted: the archive changed.');

    await controller.openTarget('cluster:2', { filters: [], presentation: 'table' });
    expect(controller.timelineRestartNotice).toBeNull();
  });

  it('resets timelineLoadingMore on every early return, so a failed page never wedges pagination', async () => {
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = pathOf(request);
      if (path === '/api/v1/people/1' && request.method === 'GET') return Response.json(person(1));
      if (path === '/api/v1/relationships/1/timeline') {
        const body = (await request.clone().json()) as { cursor?: string };
        if (body.cursor === undefined) {
          return Response.json({
            canonical_id: 1, identity_revision: 1, rows: [timelineRow('t1')], total_count: 2,
            cache_revision: 'cache-rel', next_cursor: 'page-2'
          });
        }
        return Response.json({ error: 'internal_error', message: 'boom' }, { status: 500 });
      }
      throw new Error(`unexpected path ${path}`);
    });
    const controller = new RelationshipsController(createAPIClient(fetchFn), () => 'UTC');
    await controller.openTarget('cluster:1', { filters: [], presentation: 'table' });
    expect(controller.timelineLoadingMore).toBe(false);

    // A second concurrent call while a page is in flight must return immediately without
    // ever setting the flag itself (the guard at the top of loadMoreTimeline).
    const first = controller.loadMoreTimeline();
    const second = controller.loadMoreTimeline();
    await Promise.all([first, second]);

    expect(controller.timelineLoadingMore).toBe(false);
    expect(controller.timelineError).toBe('boom');
    expect(controller.timelineCursor).toBeNull();

    // Pagination stopped (cursor cleared on failure): calling again is a guarded no-op,
    // not a second in-flight request left stuck at loadingMore = true forever.
    await controller.loadMoreTimeline();
    expect(controller.timelineLoadingMore).toBe(false);
  });
});

describe('RelationshipsController.linkParticipants / unlinkParticipants', () => {
  it('maps 200 to ok with cacheState, and re-opens the target once identityRevision actually changed', async () => {
    let personCalls = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = pathOf(request);
      if (path === '/api/v1/people/1' && request.method === 'GET') {
        personCalls += 1;
        return Response.json(person(1));
      }
      if (path === '/api/v1/relationships/1/timeline') {
        return Response.json({
          canonical_id: 1, identity_revision: personCalls, rows: [], total_count: 0, cache_revision: 'cache-rel'
        });
      }
      if (path === '/api/v1/identity/links') return Response.json({ identity_revision: 2, cache_state: 'ready' });
      throw new Error(`unexpected path ${path}`);
    });
    const controller = new RelationshipsController(createAPIClient(fetchFn), () => 'UTC');
    await controller.openTarget('cluster:1', { filters: [], presentation: 'table' });
    expect(controller.identityRevision).toBe(1);

    const outcome = await controller.linkParticipants(1, 2);

    expect(outcome).toEqual({ ok: true, identityRevision: 2, cacheState: 'ready' });
    expect(personCalls).toBe(2);
    expect(controller.identityRevision).toBe(2);
  });

  it('does not re-open the target when identityRevision is unchanged', async () => {
    let personCalls = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = pathOf(request);
      if (path === '/api/v1/people/1' && request.method === 'GET') {
        personCalls += 1;
        return Response.json(person(1));
      }
      if (path === '/api/v1/relationships/1/timeline') {
        return Response.json({ canonical_id: 1, identity_revision: 1, rows: [], total_count: 0, cache_revision: 'cache-rel' });
      }
      if (path === '/api/v1/identity/unlinks') return Response.json({ identity_revision: 1, cache_state: 'stale' });
      throw new Error(`unexpected path ${path}`);
    });
    const controller = new RelationshipsController(createAPIClient(fetchFn), () => 'UTC');
    await controller.openTarget('cluster:1', { filters: [], presentation: 'table' });
    expect(personCalls).toBe(1);

    const outcome = await controller.unlinkParticipants(1, 3);

    expect(outcome).toEqual({ ok: true, identityRevision: 1, cacheState: 'stale' });
    expect(personCalls).toBe(1);
  });

  it('also reloads the ranked list after a successful link, using the last-loaded predicate', async () => {
    let personCalls = 0;
    let relationshipsCalls = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = pathOf(request);
      if (path === '/api/v1/relationships') {
        relationshipsCalls += 1;
        return Response.json({
          rows: [relationshipRow(1, 'Alice')], total_count: 1, cache_revision: 'cache-rel', identity_revision: 1
        });
      }
      if (path === '/api/v1/people/1' && request.method === 'GET') {
        personCalls += 1;
        return Response.json(person(1));
      }
      if (path === '/api/v1/relationships/1/timeline') {
        return Response.json({
          canonical_id: 1, identity_revision: personCalls, rows: [], total_count: 0, cache_revision: 'cache-rel'
        });
      }
      if (path === '/api/v1/identity/links') return Response.json({ identity_revision: 2, cache_state: 'ready' });
      throw new Error(`unexpected path ${path}`);
    });
    const controller = new RelationshipsController(createAPIClient(fetchFn), () => 'UTC');
    await controller.loadList({ filters: [], presentation: 'table' });
    await controller.openTarget('cluster:1', { filters: [], presentation: 'table' });
    expect(relationshipsCalls).toBe(1);

    const outcome = await controller.linkParticipants(1, 2);

    expect(outcome).toEqual({ ok: true, identityRevision: 2, cacheState: 'ready' });
    expect(relationshipsCalls).toBe(2);
  });

  it('does not reload the list when none was ever loaded (no stray fetch)', async () => {
    const fetchFn = vi.fn<typeof fetch>(async () => Response.json({ identity_revision: 1, cache_state: 'ready' }));
    const controller = new RelationshipsController(createAPIClient(fetchFn), () => 'UTC');

    const outcome = await controller.linkParticipants(1, 2);

    expect(outcome).toEqual({ ok: true, identityRevision: 1, cacheState: 'ready' });
    expect(fetchFn).toHaveBeenCalledTimes(1);
  });

  it('refreshes detail unconditionally when identityRevision was null after a failed timeline load', async () => {
    let personCalls = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = pathOf(request);
      if (path === '/api/v1/people/1' && request.method === 'GET') {
        personCalls += 1;
        return Response.json(person(1));
      }
      if (path === '/api/v1/relationships/1/timeline') {
        return personCalls === 1
          ? Response.json({ error: 'internal_error', message: 'boom' }, { status: 500 })
          : Response.json({
              canonical_id: 1, identity_revision: 3, rows: [], total_count: 0, cache_revision: 'cache-rel'
            });
      }
      if (path === '/api/v1/identity/links') return Response.json({ identity_revision: 3, cache_state: 'ready' });
      throw new Error(`unexpected path ${path}`);
    });
    const controller = new RelationshipsController(createAPIClient(fetchFn), () => 'UTC');
    await controller.openTarget('cluster:1', { filters: [], presentation: 'table' });
    expect(controller.identityRevision).toBeNull();
    expect(personCalls).toBe(1);

    const outcome = await controller.linkParticipants(1, 2);

    expect(outcome).toEqual({ ok: true, identityRevision: 3, cacheState: 'ready' });
    expect(personCalls).toBe(2);
    expect(controller.identityRevision).toBe(3);
  });

  it('maps a 409 already_linked body to code already_linked', async () => {
    const fetchFn = vi.fn<typeof fetch>(async () => Response.json(
      { error: 'already_linked', message: 'these participants are already connected through other links' },
      { status: 409 }
    ));
    const controller = new RelationshipsController(createAPIClient(fetchFn), () => 'UTC');

    const outcome = await controller.linkParticipants(1, 2);

    expect(outcome).toEqual({
      ok: false, code: 'already_linked', message: 'these participants are already connected through other links'
    });
  });

  it('maps a 400 body to code invalid', async () => {
    const fetchFn = vi.fn<typeof fetch>(async () => Response.json(
      { error: 'invalid_participant_id', message: 'participant_a and participant_b must be distinct positive participant IDs' },
      { status: 400 }
    ));
    const controller = new RelationshipsController(createAPIClient(fetchFn), () => 'UTC');

    const outcome = await controller.unlinkParticipants(-1, 2);

    expect(outcome).toEqual({
      ok: false, code: 'invalid', message: 'participant_a and participant_b must be distinct positive participant IDs'
    });
  });

  it('maps any other failure (500, network error) to code error', async () => {
    const fetchFn = vi.fn<typeof fetch>(async () => Response.json(
      { error: 'internal_error', message: 'failed to update participant links' }, { status: 500 }
    ));
    const controller = new RelationshipsController(createAPIClient(fetchFn), () => 'UTC');

    const outcome = await controller.linkParticipants(1, 2);

    expect(outcome).toEqual({ ok: false, code: 'error', message: 'failed to update participant links' });
  });
});

describe('RelationshipsController.clearTarget', () => {
  it('resets target/detail/timeline state and discards a late in-flight detail response', async () => {
    let resolveTimeline: ((response: Response) => void) | undefined;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = pathOf(request);
      if (path === '/api/v1/people/1' && request.method === 'GET') return Response.json(person(1));
      if (path === '/api/v1/relationships/1/timeline') {
        return new Promise<Response>((resolve) => { resolveTimeline = resolve; });
      }
      throw new Error(`unexpected path ${path}`);
    });
    const controller = new RelationshipsController(createAPIClient(fetchFn), () => 'UTC');
    const openPromise = controller.openTarget('cluster:1', { filters: [], presentation: 'table' });
    await Promise.resolve();

    controller.clearTarget();

    expect(controller.target).toBeNull();
    expect(controller.detail).toBeNull();
    expect(controller.canonicalID).toBeNull();
    expect(controller.identityRevision).toBeNull();

    resolveTimeline?.(Response.json({
      canonical_id: 1, identity_revision: 1, rows: [timelineRow('t1')], total_count: 1, cache_revision: 'cache-rel'
    }));
    await openPromise;

    expect(controller.target).toBeNull();
    expect(controller.detail).toBeNull();
    expect(controller.timelineRows).toEqual([]);
  });
});

describe('RelationshipsController abort/destroy', () => {
  it('destroy() aborts the in-flight list load and detail load AbortControllers', async () => {
    const signals: AbortSignal[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      signals.push(request.signal);
      return new Promise<Response>(() => {
        // Never resolves; destroy() must abort it rather than leave it hanging forever.
      });
    });
    const controller = new RelationshipsController(createAPIClient(fetchFn), () => 'UTC');

    void controller.loadList({ filters: [], presentation: 'table' });
    void controller.openTarget('cluster:1', { filters: [], presentation: 'table' });
    await Promise.resolve();
    await Promise.resolve();

    expect(signals.length).toBeGreaterThanOrEqual(2);
    expect(signals.every((signal) => !signal.aborted)).toBe(true);

    controller.destroy();

    expect(signals.every((signal) => signal.aborted)).toBe(true);
  });

  it('aborts the previous list load when a new one starts', async () => {
    const signals: AbortSignal[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      signals.push(request.signal);
      if (signals.length === 1) return new Promise<Response>(() => {});
      return Response.json({ rows: [], total_count: 0, cache_revision: 'cache-rel', identity_revision: 1 });
    });
    const controller = new RelationshipsController(createAPIClient(fetchFn), () => 'UTC');

    void controller.loadList({ filters: [], presentation: 'table' });
    await Promise.resolve();
    await controller.loadList({ filters: [], presentation: 'table' });

    expect(signals[0]!.aborted).toBe(true);
    expect(signals[1]!.aborted).toBe(false);
  });
});
