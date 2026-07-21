import { describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../api/client';
import type { FileMetadata, FileSearchRow } from './models';
import { createExploreAPI } from './api';

type RequiredNonNullableString<T, K extends keyof T> =
  {} extends Pick<T, K> ? false : null extends T[K] ? false : T[K] extends string ? true : false;
const generatedFileStringContract: [
  RequiredNonNullableString<FileMetadata, 'filename'>,
  RequiredNonNullableString<FileMetadata, 'mime_type'>,
  RequiredNonNullableString<FileSearchRow, 'filename'>,
  RequiredNonNullableString<FileSearchRow, 'mime_type'>
] = [true, true, true, true];

describe('ExploreAPI routing', () => {
  it('keeps public TypeScript file metadata strings required and non-nullable', () => {
    expect(generatedFileStringContract).toEqual([true, true, true, true]);
  });
  it('loads filtered semantic coverage and exact visible lexical counts', async () => {
    const paths: string[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      paths.push(new URL(request.url).pathname);
      if (new URL(request.url).pathname.endsWith('/coverage')) {
        return Response.json({
          status: 'incomplete', eligible_count: 2, embedded_count: 1, percentage: 50,
          vector_generation: 7, cache_revision: 'cache-2', actions: []
        });
      }
      return Response.json({
        counts: [{ row_key: 'conversation:1', count: 3 }], cache_revision: 'cache-2',
        lexical_index_revision: 'fts-2', canonical_query_hash: 'query-2'
      });
    });
    const api = createExploreAPI(createAPIClient(fetchFn));

    const coverage = await api.coverage([{ dimension: 'source', values: ['1'] }]);
    const counts = await api.matchCounts(
      { query: 'alpha', search_mode: 'hybrid', filters: [], presentation: 'table' },
      ['conversation:1']
    );

    expect(coverage).toMatchObject({ status: 'incomplete', eligible_count: 2, embedded_count: 1 });
    expect(counts.counts).toEqual({ 'conversation:1': 3 });
    expect(counts.canonicalQueryHash).toBe('query-2');
    expect(paths).toEqual(['/api/v1/search/coverage', '/api/v1/explore/match-counts']);
  });

  it('passes cursors and cancellation through the session-aware entry request', async () => {
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      return Response.json({
        rows: [],
        total_count: 50_000,
        cache_revision: 'cache-1',
        search_provenance: {},
        next_cursor: 'page-2'
      });
    });
    const controller = new AbortController();
    const api = createExploreAPI(createAPIClient(fetchFn));

    await api.explore({ filters: [], presentation: 'table', limit: 500, cursor: 'page-1' }, controller.signal);

    expect(requests).toHaveLength(1);
    expect(new URL(requests[0]!.url).pathname).toBe('/api/v1/explore');
    controller.abort();
    expect(requests[0]!.signal.aborted).toBe(true);
    await expect(requests[0]!.json()).resolves.toMatchObject({ cursor: 'page-1', limit: 500 });
  });

  it('routes grouped predicates to the groups endpoint with one active dimension', async () => {
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      return Response.json({
        rows: [{ key: '7', label: 'Example source', count: 12, estimated_bytes: 42, latest_at: '2026-07-18T12:00:00Z' }],
        total_count: 1,
        cache_revision: 'cache-1',
        search_provenance: {}
      });
    });
    const api = createExploreAPI(createAPIClient(fetchFn));

    const loaded = await api.groups(
      {
        filters: [{ dimension: 'source', values: ['2'] }],
        grouping: ['source', 'year'],
        presentation: 'table',
        sort: [{ field: 'occurred_at', direction: 'desc' }],
        limit: 500,
        cursor: 'groups-2'
      },
      'source'
    );

    expect(loaded.status).toBe('ready');
    expect(new URL(requests[0]!.url).pathname).toBe('/api/v1/explore/groups');
    await expect(requests[0]!.json()).resolves.toEqual({
      filters: [{ dimension: 'source', values: ['2'] }],
      grouping: ['source'],
      presentation: 'table',
      limit: 500,
      cursor: 'groups-2'
    });
  });

  it('routes grouped Files exclusively through the file population with file constraints', async () => {
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      return Response.json({
        rows: [{ key: '7', label: 'Example source', count: 2, estimated_bytes: 300, latest_at: '2026-07-18T12:00:00Z' }],
        total_count: 1, cache_revision: 'cache-files', search_provenance: {}
      });
    });
    const api = createExploreAPI(createAPIClient(fetchFn));

    const loaded = await api.fileGroups(
      { filters: [{ dimension: 'source', values: ['7'] }], presentation: 'table', limit: 500 },
      'invoice', ['pdf'], 'participant'
    );

    expect(loaded.status).toBe('ready');
    expect(requests).toHaveLength(1);
    expect(new URL(requests[0]!.url).pathname).toBe('/api/v1/files/groups');
    await expect(requests[0]!.json()).resolves.toEqual({
      predicate: { filters: [{ dimension: 'source', values: ['7'] }], presentation: 'table', limit: 500 },
      filename_query: 'invoice', mime_families: ['pdf'], grouping: ['participant'], limit: 500
    });
  });

  it('loads one bounded attachment-fact page with cancellation and canonical context', async () => {
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      return Response.json({
        files: [{
          key: 'message:4:file:8', entry_key: 'message:4', message_id: 4,
          occurred_at: '2026-07-18T12:00:00Z', source_id: 1,
          source_identifier: 'archive@example.com', title: 'Synthetic entry',
          filename: 'notes.txt', size: 42
        }],
        total_count: 230,
        cache_revision: 'cache-2',
        search_provenance: { lexical_index_revision: 'fts-2' },
        next_cursor: 'files-2'
      });
    });
    const controller = new AbortController();
    const api = createExploreAPI(createAPIClient(fetchFn));

    const loaded = await api.files({
      query: 'pasta', search_mode: 'full_text',
      filters: [{ dimension: 'participant', values: ['42'] }],
      presentation: 'files', limit: 500, cursor: 'files-1'
    }, controller.signal);

    expect(loaded.status).toBe('ready');
    expect(loaded).toMatchObject({
      result: { totalCount: 230, nextCursor: 'files-2', files: [{ filename: 'notes.txt' }] }
    });
    expect(new URL(requests[0]!.url).pathname).toBe('/api/v1/explore/files');
    await expect(requests[0]!.clone().json()).resolves.toEqual({
      predicate: {
        query: 'pasta', search_mode: 'full_text',
        filters: [{ dimension: 'participant', values: ['42'] }],
        presentation: 'files', limit: 500
      },
      cursor: 'files-1', limit: 100
    });
    controller.abort();
    expect(requests[0]!.signal.aborted).toBe(true);
  });

  it('uses a non-interactive full rebuild and accepts only a completed NDJSON stream', async () => {
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      return new Response([
        JSON.stringify({ type: 'stdout', data: 'Building generation 2\n' }),
        JSON.stringify({ type: 'complete' })
      ].join('\n') + '\n', { headers: { 'Content-Type': 'application/x-ndjson' } });
    });
    const api = createExploreAPI(createAPIClient(fetchFn));

    await api.runCoverageAction('build_index', 'stale');

    expect(requests).toHaveLength(1);
    await expect(requests[0]!.json()).resolves.toEqual({
      args: ['embeddings', 'build', '--full-rebuild', '--yes']
    });
  });

  it.each([
    ['error event', [
      { type: 'stdout', data: 'started\n' },
      { type: 'error', error: 'embedding endpoint failed' }
    ]],
    ['failed completion', [
      { type: 'complete', error: 'generation activation failed' }
    ]]
  ])('rejects a successful HTTP response containing a streamed %s', async (_name, events) => {
    const fetchFn = vi.fn<typeof fetch>(async () => new Response(
      events.map((event) => JSON.stringify(event)).join('\n') + '\n',
      { headers: { 'Content-Type': 'application/x-ndjson' } }
    ));
    const api = createExploreAPI(createAPIClient(fetchFn));

    await expect(api.runCoverageAction('build_index', 'incomplete'))
      .rejects.toThrow(/failed|activation/i);
  });
});
