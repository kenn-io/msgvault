import { fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';
import { createAPIClient } from '../../api/client';
import type { MeetingMetricsRequest } from '../../api/generated/models';
import type { DirectoryReadBundle } from '../../directory/models';
import { ExploreState } from '../../explore/state.svelte';
import { meetingActions, meetingMetrics } from '../../meetings/fixtures.test-support';
import { RelationshipsController } from '../../relationships/controller.svelte';
import PersonDetail from '../directory/PersonDetail.svelte';
import RelationshipsWorkspace from '../relationships/RelationshipsWorkspace.svelte';
import AppShell from '../shell/AppShell.svelte';

const person = { id: 7, revision: 1, display_name: 'Example Person', participant_ids: [3, 7], vcard_uid: 'person-7', created_at: '', updated_at: '' };
const at = '2026-01-01T00:00:00Z';
function ancillary(path: string): Response {
  if (path.endsWith('/tracking')) return Response.json({ person_id: 7, tracked: false });
  if (path.endsWith('/brief-enrollment')) return Response.json({ person_id: 7, enrolled: false });
  if (path.endsWith('/merges')) return Response.json({ merges: [], limit: 100, offset: 0 });
  return Response.json({ error: 'not_found', message: 'Synthetic unavailable resource.' }, { status: 404 });
}

describe('meeting panel surfaces', () => {
  it('refreshes same Directory person identity revisions and ignores delayed pre-merge evidence', async () => {
    const metrics: MeetingMetricsRequest[] = [];
    let resolveOld!: (response: Response) => void;
    let oldSignal!: AbortSignal;
    const client = createAPIClient(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = new URL(request.url).pathname;
      if (path.endsWith('/meetings/metrics')) {
        metrics.push(await request.clone().json());
        if (metrics.length === 1) { oldSignal = request.signal; return new Promise<Response>((resolve) => { resolveOld = resolve; }); }
        return Response.json(meetingMetrics());
      }
      if (path.endsWith('/meetings/actions')) return Response.json(meetingActions());
      return ancillary(path);
    });
    const bundle: DirectoryReadBundle = { person, etags: {}, errors: {} };
    const view = render(PersonDetail, { client, personID: 7, bundle });
    await waitFor(() => expect(resolveOld).toBeDefined());
    await view.rerender({ client, personID: 7, bundle: { ...bundle, person: { ...person, revision: 2, participant_ids: [7, 9] } } });
    expect(await screen.findByText('4 meetings')).toBeDefined();
    expect(metrics).toEqual([{ scope: { person_id: 7 } }, { scope: { person_id: 7 } }]);
    expect(oldSignal.aborted).toBe(true);
    resolveOld(Response.json(meetingMetrics({ totals: { ...meetingMetrics().totals, meeting_count: 99 } })));
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(screen.queryByText('99 meetings')).toBeNull();
  });

  it.each(['cluster:7', 'domain:exact.example'])('uses the active Relationships scope for %s', async (target) => {
    const metrics: MeetingMetricsRequest[] = [];
    const client = createAPIClient(async (input) => {
      const request = input instanceof Request ? input : new Request(input); const path = new URL(request.url).pathname;
      if (path.endsWith('/meetings/metrics')) { metrics.push(await request.clone().json()); return Response.json(meetingMetrics()); }
      if (path.endsWith('/meetings/actions')) return Response.json(meetingActions());
      if (path.endsWith('/timeline')) return Response.json({ canonical_id: 7, identity_revision: 3, cache_revision: 'rel-cache', rows: [], total_count: 0 });
      if (path.endsWith('/summary')) return Response.json({ summary: { id: 7, display_label: 'Example Person', domain: 'exact.example', activity_count: 0, file_count: 0, identifiers: [], source_counts: [], cache_revision: 'rel-cache' } });
      if (path.endsWith('/participants/7')) return Response.json({ id: 7, display_label: 'Example Person', identifiers: [], activity_count: 0, file_count: 0, source_counts: [], first_at: at, last_at: at, cache_revision: 'rel-cache' });
      if (path.endsWith('/domains/exact.example')) return Response.json({ domain: 'exact.example', activity_count: 0, file_count: 0, source_counts: [], cache_revision: 'rel-cache' });
      if (path.endsWith('/relationships')) return Response.json({ rows: [] });
      return ancillary(path);
    });
    const controller = new RelationshipsController(client, () => 'UTC');
    const predicate = { query: 'stale URL text', search_mode: 'hybrid' as const, filters: [
      { dimension: 'source' as const, values: ['2'] }, { dimension: 'after' as const, values: [at] }
    ] };
    const view = render(RelationshipsWorkspace, { props: { client, controller, target, predicate, facet: target.startsWith('domain:') ? 'domains' : 'people', showAll: false, filesOpen: false,
      onFacetChange: vi.fn(), onTargetChange: vi.fn(), onShowAllChange: vi.fn(), onFilesToggle: vi.fn() } });
    await controller.openTarget(target, predicate);
    await screen.findByText('4 meetings');
    expect(metrics.at(-1)).toEqual({ scope: { ...(target.startsWith('domain:') ? { domains: ['exact.example'] } : { participant_id: 7 }), source_ids: [2], after: at } });
    view.unmount(); controller.destroy();
  });

  it('uses separately loaded exact group authority across a workspace round-trip', async () => {
    window.history.replaceState(null, '', '/');
    const metrics: MeetingMetricsRequest[] = [];
    let groupLoads = 0;
    const client = createAPIClient(async (input) => {
      const request = input instanceof Request ? input : new Request(input); const path = new URL(request.url).pathname;
      if (path.endsWith('/meetings/metrics')) { metrics.push(await request.clone().json()); return Response.json(meetingMetrics()); }
      if (path.endsWith('/meetings/actions')) return Response.json(meetingActions());
      if (path.endsWith('/explore/groups')) {
        const body = await request.clone().json();
        if (body.group_key) {
          groupLoads += 1;
          return Response.json({ rows: [ { key: 'frequent.example', label: 'Frequent domain', count: 20, estimated_bytes: 200 }, { key: 'exact.example', label: 'Exact domain', count: 2, estimated_bytes: 20 } ], total_count: 2,
            cache_revision: 'detail-cache', search_provenance: { lexical_index_revision: 'detail-lex', vector_generation: 2 }, candidate_snapshot_id: 'detail-candidate' });
        }
      }
      if (path.endsWith('/explore/files')) return Response.json({ files: [], total_count: 0, cache_revision: 'files-cache', search_provenance: {} });
      if (path.endsWith('/explore')) return Response.json({ rows: [], total_count: 0, cache_revision: 'outer-cache', search_provenance: { lexical_index_revision: 'outer-lex', vector_generation: 1 }, candidate_snapshot_id: 'outer-candidate' });
      return ancillary(path);
    });
    const state = new ExploreState(window);
    const filters = [{ dimension: 'domain' as const, values: ['co.example'] }, { dimension: 'participant' as const, values: ['3'] }, { dimension: 'participant' as const, values: ['7'] }];
    state.commitNavigation({ workspace: 'everything', query: 'planning', searchMode: 'hybrid', filters, selectedRow: 'group:domain:exact.example' });
    const view = render(AppShell, { client, state });
    await screen.findByText('4 meetings');
    expect(metrics[0]).toMatchObject({ explore: { cache_revision: 'detail-cache', search_provenance: { lexical_index_revision: 'detail-lex', vector_generation: 2 }, candidate_snapshot_id: 'detail-candidate',
      predicate: { query: 'planning', search_mode: 'hybrid', filters: [...filters, { dimension: 'domain', values: ['exact.example'] }] } } });
    await fireEvent.click(within(screen.getByRole('navigation', { name: 'Primary' })).getByRole('button', { name: 'Settings' }));
    window.history.back();
    await new Promise((resolve) => window.addEventListener('popstate', resolve, { once: true }));
    await screen.findByText('4 meetings');
    expect(metrics.at(-1)).toEqual(metrics[0]);
    expect(groupLoads).toBe(1);
    view.unmount(); state.destroy();
  });

  it.each(['table', 'groups'])('exposes meeting-filtered Everything %s with the complete predicate and loaded authority', async (presentation) => {
    window.history.replaceState(null, '', '/');
    const metrics: MeetingMetricsRequest[] = [];
    const client = createAPIClient(async (input) => {
      const request = input instanceof Request ? input : new Request(input); const path = new URL(request.url).pathname;
      if (path.endsWith('/meetings/metrics')) { metrics.push(await request.clone().json()); return Response.json(meetingMetrics()); }
      if (path.endsWith('/meetings/actions')) return Response.json(meetingActions());
      return Response.json({ rows: [], total_count: 0, cache_revision: 'everything-cache', search_provenance: { lexical_index_revision: 'lexical-1' } });
    });
    const state = new ExploreState(window);
    const filters = [{ dimension: 'message_type' as const, values: ['meeting_transcript'] }, { dimension: 'source' as const, values: ['3'] }, { dimension: 'domain' as const, values: ['exact.example'] }];
    state.commitNavigation({ workspace: 'everything', groupingChain: presentation === 'groups' ? ['domain'] : [], filters, query: 'planning', searchMode: 'full_text' });
    const view = render(AppShell, { client, state });
    await screen.findByText('4 meetings');
    expect(metrics[0]).toMatchObject({ explore: { predicate: { ...state.predicate(), filters }, cache_revision: 'everything-cache', search_provenance: { lexical_index_revision: 'lexical-1' } } });
    view.unmount(); state.destroy();
  });
});

it('aborts a prior group lookup immediately when the predicate changes while the new list is still loading', async () => {
  window.history.replaceState(null, '', '/');
  let oldGroupSignal!: AbortSignal;
  let groupRequests = 0;
  let resolveOldGroup!: (response: Response) => void;
  let resolveNewList!: (response: Response) => void;
  let lists = 0;
  const client = createAPIClient(async (input) => {
    const request = input instanceof Request ? input : new Request(input); const path = new URL(request.url).pathname;
    if (path.endsWith('/explore')) {
      lists += 1;
      if (lists > 1) return new Promise<Response>((resolve) => { resolveNewList = resolve; });
      return Response.json({ rows: [], total_count: 0, cache_revision: 'old-cache', search_provenance: {} });
    }
    if (path.endsWith('/explore/groups')) {
      groupRequests += 1;
      oldGroupSignal = request.signal;
      return new Promise<Response>((resolve) => { resolveOldGroup = resolve; });
    }
    if (path.endsWith('/meetings/metrics')) return Response.json(meetingMetrics());
    if (path.endsWith('/meetings/actions')) return Response.json(meetingActions());
    return ancillary(path);
  });
  const state = new ExploreState(window);
  state.commitNavigation({ workspace: 'everything', selectedRow: 'group:domain:exact.example', filters: [{ dimension: 'source', values: ['2'] }] });
  const view = render(AppShell, { client, state });
  try {
    await waitFor(() => expect(resolveOldGroup).toBeDefined());
    state.commitNavigation({ filters: [{ dimension: 'source', values: ['3'] }] });
    await waitFor(() => expect(resolveNewList).toBeDefined());
    expect(groupRequests).toBe(1);
    expect(oldGroupSignal.aborted).toBe(true);
    resolveOldGroup(Response.json({ rows: [{ key: 'exact.example', label: 'Old scope', count: 99, estimated_bytes: 0 }], total_count: 1, cache_revision: 'old-cache', search_provenance: {} }));
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(screen.queryByRole('complementary', { name: 'Reading pane: Old scope' })).toBeNull();
    expect(screen.queryByText('4 meetings')).toBeNull();
  } finally { view.unmount(); state.destroy(); }
});

it('reloads a failed meeting overview from a freshly loaded matching authority even when the cache revision is unchanged', async () => {
  window.history.replaceState(null, '', '/');
  const metrics: MeetingMetricsRequest[] = [];
  let listLoads = 0;
  let canLoad = false;
  const client = createAPIClient(async (input) => {
    const request = input instanceof Request ? input : new Request(input); const path = new URL(request.url).pathname;
    if (path.endsWith('/explore')) {
      listLoads += 1;
      if (listLoads > 1) canLoad = true;
      return Response.json({ rows: [], total_count: 0, cache_revision: 'same-cache', search_provenance: {} });
    }
    if (path.endsWith('/meetings/metrics')) {
      metrics.push(await request.clone().json());
      return canLoad ? Response.json(meetingMetrics()) : Response.json({ error: 'meeting_scope_changed', message: 'Archived scope changed.' }, { status: 409 });
    }
    if (path.endsWith('/meetings/actions')) return Response.json(meetingActions());
    return ancillary(path);
  });
  const state = new ExploreState(window);
  state.commitNavigation({ workspace: 'everything', filters: [{ dimension: 'message_type', values: ['meeting_transcript'] }] });
  const view = render(AppShell, { client, state });
  try {
    await fireEvent.click(await screen.findByRole('button', { name: 'Reload meeting activity' }));
    expect(await screen.findByText('4 meetings')).toBeDefined();
    expect(listLoads).toBe(2);
    expect(metrics).toHaveLength(2);
    expect(metrics[1]).toEqual(metrics[0]);
  } finally { view.unmount(); state.destroy(); }
});
