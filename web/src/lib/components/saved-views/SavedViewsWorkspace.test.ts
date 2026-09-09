import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../../api/client';
import { ExploreGroupDimension } from '../../api/generated/models';
import { defaultExploreURLState, parseExploreURLState, serializeExploreURLState } from '../../explore/state.svelte';
import type { ExploreURLState } from '../../explore/models';
import SavedViewsWorkspace from './SavedViewsWorkspace.svelte';

const currentState: ExploreURLState = {
  schemaVersion: 2,
  workspace: 'everything',
  directoryQuery: '', directoryContactState: '', directoryCategory: '', directoryOrganization: '',
  directoryPrimaryChannel: '', directoryLastContactAfter: '', directoryLastContactBefore: '', directorySort: 'name', directoryPersonID: null,
  reviewKind: 'identity', identityState: 'candidate', relationshipReviewState: 'pending',
  query: 'invoice',
  searchMode: 'full_text',
  filters: [{ dimension: 'source', values: ['1'] }],
  groupingChain: ['domain'],
  presentation: 'table',
  sort: [{ field: 'occurred_at', direction: 'desc' }],
  fileFilenameQuery: '', fileMIMEFamilies: [], personFilePresentation: 'files',
  personFileDirections: ['from_person'], columns: ['kind', 'title'], columnWidths: {},
  activeRow: null, selectedRow: null, inspectorPinned: true, inspectorWidth: 380,
  conversationAnchor: null, scrollAnchor: null,
  relationshipFacet: 'people', relationshipTarget: null,
  relationshipShowAll: false, relationshipFiles: false,
  operationLane: '', operationKind: '', operationState: '',
  operationStartedFrom: '', operationStartedBefore: '', operationRunID: null, operationStatus: '',
  settingsAuthority: ''
};

function savedView(overrides: Record<string, unknown> = {}) {
  return {
    id: 7, name: 'Invoices', description: 'Quarterly review',
    canonical_state: {
      query: 'invoice', search_mode: 'full_text',
      filters: [{ field: 'source', operator: 'in', values: ['1'] }],
      grouping: ['domain'], presentation: 'table',
      sort: [{ field: 'occurred_at', direction: 'desc' }],
      columns: ['kind', 'title'], inspector_pinned: true
    },
    schema_version: 1, revision: 3,
    created_at: '2026-07-19T10:00:00Z', updated_at: '2026-07-19T11:00:00Z',
    ...overrides
  };
}

describe('SavedViewsWorkspace', () => {
  it.each(['semantic', 'hybrid'] as const)('saves filter-only views without a %s search mode', async (searchMode) => {
    for (const query of ['', ' \t\n ']) {
      const requests: Request[] = [];
      const fetchFn = vi.fn<typeof fetch>(async (input) => {
        const request = input instanceof Request ? input : new Request(input);
        requests.push(request);
        return Response.json(request.method === 'GET' ? { saved_views: [] } : savedView());
      });
      const component = render(SavedViewsWorkspace, {
        client: createAPIClient(fetchFn), currentState: { ...currentState, query, searchMode }
      });
      await screen.findByText('No Saved Views yet');
      await fireEvent.input(screen.getByLabelText('Name'), { target: { value: 'Invoices' } });
      await fireEvent.click(screen.getByRole('button', { name: 'Save' }));
      await screen.findByRole('heading', { name: 'Invoices' });
      const { canonical_state: saved } = await requests[1]!.clone().json();
      expect(saved).not.toHaveProperty('query');
      expect(saved).not.toHaveProperty('search_mode');
      expect(saved.filters).toEqual([{ field: 'source', operator: 'in', values: ['1'] }]);
      component.unmount();
    }
  });

  it('blocks opening a definition the server marks incompatible', async () => {
    const onOpen = vi.fn();
    render(SavedViewsWorkspace, {
      client: createAPIClient(vi.fn<typeof fetch>(async () => Response.json({ saved_views: [savedView({
        canonical_state: { search_mode: 'semantic' },
        incompatibility_reason: 'Semantic and hybrid exploration require free text'
      })] }))), currentState, onOpen
    });
    expect((await screen.findByRole('alert')).textContent).toContain('require free text');
    const open = screen.getByRole('button', { name: 'Open Invoices' }) as HTMLButtonElement;
    expect(open.disabled).toBe(true);
    await fireEvent.click(open);
    expect(onOpen).not.toHaveBeenCalled();
  });

  it.each(['full_text', 'semantic', 'hybrid'] as const)('creates a %s query without persisting selection tokens', async (searchMode) => {
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      if (request.method === 'GET') return Response.json({ saved_views: [] });
      return Response.json(savedView(), { status: 201 });
    });
    render(SavedViewsWorkspace, {
      client: createAPIClient(fetchFn), currentState: { ...currentState, query: ' invoice ', searchMode },
      selection: { mode: 'all_matching', operationToken: 'session-secret' }
    });

    await screen.findByText('No Saved Views yet');
    await fireEvent.input(screen.getByLabelText('Name'), { target: { value: 'Invoices' } });
    await fireEvent.input(screen.getByLabelText('Description'), { target: { value: 'Quarterly review' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Save' }));

    await screen.findByRole('heading', { name: 'Invoices' });
    const body = await requests[1]!.clone().json();
    expect(body).toEqual({
      name: 'Invoices', description: 'Quarterly review', schema_version: 1,
      canonical_state: {
        query: 'invoice', search_mode: searchMode,
        filters: [{ field: 'source', operator: 'in', values: ['1'] }],
        grouping: ['domain'], presentation: 'table',
        sort: [{ field: 'occurred_at', direction: 'desc' }],
        columns: ['kind', 'title']
      }
    });
    expect(JSON.stringify(body)).not.toContain('session-secret');
    expect(JSON.stringify(body)).not.toContain('selection');
    expect(JSON.stringify(body)).not.toContain('inspector_pinned');
  });

  it.each(Object.values(ExploreGroupDimension))('opens %s grouping and preserves it in the analytical URL state', async (dimension) => {
    const onOpen = vi.fn();
    render(SavedViewsWorkspace, {
      client: createAPIClient(vi.fn<typeof fetch>(async () => Response.json({ saved_views: [savedView({
        canonical_state: { ...savedView().canonical_state, grouping: [dimension] }
      })] }))),
      currentState: { ...currentState, activeRow: 'message:9', selectedRow: 'message:9' }, onOpen
    });

    await fireEvent.click(await screen.findByRole('button', { name: 'Open Invoices' }));

    expect(onOpen).toHaveBeenCalledWith(expect.objectContaining({
      workspace: 'everything', query: 'invoice', searchMode: 'full_text',
      filters: [{ dimension: 'source', values: ['1'] }], groupingChain: [dimension],
      activeRow: null, selectedRow: null, scrollAnchor: null
    }));
    expect(onOpen.mock.calls[0]![0]).not.toHaveProperty('selection');
    expect(onOpen.mock.calls[0]![0]).not.toHaveProperty('inspectorPinned');
    const restored = parseExploreURLState(serializeExploreURLState({
      ...defaultExploreURLState, ...onOpen.mock.calls[0]![0]
    }));
    expect(restored.groupingChain).toEqual([dimension]);
  });

  it('translates persisted v1 source identifiers and equality operators into current filters', async () => {
    const onOpen = vi.fn();
    const legacy = savedView({
      canonical_state: {
        query: 'invoice', search_mode: 'full_text',
        filters: [{ field: 'source_id', operator: 'eq', values: ['1'] }],
        grouping: [], presentation: 'table',
        sort: [{ field: 'occurred_at', direction: 'desc' }],
        columns: ['kind', 'title'], inspector_pinned: false
      }
    });
    render(SavedViewsWorkspace, {
      client: createAPIClient(vi.fn<typeof fetch>(async () => Response.json({ saved_views: [legacy] }))),
      currentState, onOpen
    });

    await fireEvent.click(await screen.findByRole('button', { name: 'Open Invoices' }));

    expect(onOpen).toHaveBeenCalledWith(expect.objectContaining({
      filters: [{ dimension: 'source', values: ['1'] }]
    }));
  });

  it('opens views filtered by every Explore dimension the daemon executes', async () => {
    const onOpen = vi.fn();
    const view = savedView({
      canonical_state: {
        filters: [
          { field: 'identity', operator: 'in', values: ['1:me@example.com:sent'] },
          { field: 'mailing_list', operator: 'eq', values: ['<dev@example.test>'] },
          { field: 'participant_id', operator: 'in', values: ['42'] }
        ],
        presentation: 'table'
      }
    });
    render(SavedViewsWorkspace, {
      client: createAPIClient(vi.fn<typeof fetch>(async () => Response.json({ saved_views: [view] }))),
      currentState, onOpen
    });

    await fireEvent.click(await screen.findByRole('button', { name: 'Open Invoices' }));

    expect(onOpen).toHaveBeenCalledWith(expect.objectContaining({
      filters: [
        { dimension: 'identity', values: ['1:me@example.com:sent'] },
        { dimension: 'mailing_list', values: ['<dev@example.test>'] },
        { dimension: 'participant', values: ['42'] }
      ]
    }));
  });

  it('updates with optimistic revision truth and explicitly confirms delete', async () => {
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      if (request.method === 'GET') return Response.json({ saved_views: [savedView()] });
      if (request.method === 'PATCH') return Response.json(savedView({ name: 'Invoices 2026', revision: 4 }));
      return new Response(null, { status: 204 });
    });
    render(SavedViewsWorkspace, { client: createAPIClient(fetchFn), currentState });

    await fireEvent.click(await screen.findByRole('button', { name: 'Edit Invoices' }));
    await fireEvent.input(screen.getByLabelText('Edit name'), { target: { value: 'Invoices 2026' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Save changes' }));
    await screen.findByRole('heading', { name: 'Invoices 2026' });
    expect(requests[1]!.headers.get('If-Match')).toBe('"saved-view-7-r3"');

    await fireEvent.click(screen.getByRole('button', { name: 'Delete Invoices 2026' }));
    expect(screen.getByRole('dialog', { name: 'Delete Saved View?' })).toBeDefined();
    expect(requests).toHaveLength(2);
    await fireEvent.click(screen.getByRole('button', { name: 'Confirm delete' }));
    await waitFor(() => expect(requests).toHaveLength(3));
    expect(requests[2]!.headers.get('If-Match')).toBe('"saved-view-7-r4"');
    expect(await screen.findByText('No Saved Views yet')).toBeDefined();
  });

  it('keeps incompatible schema records visible and offers confirmed removal, not migration', async () => {
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      if (request.method === 'DELETE') return new Response(null, { status: 204 });
      return Response.json({ saved_views: [savedView({ schema_version: 99, incompatibility_reason: "unsupported schema", canonical_state: { query: { text: "future" } } })] });
    });
    render(SavedViewsWorkspace, {
      client: createAPIClient(fetchFn),
      currentState
    });

    expect((await screen.findByRole('alert')).textContent).toContain('schema version 99');
    expect(screen.getByRole('alert').textContent).toContain('Automatic migration is not supported');
    expect((screen.getByRole('button', { name: 'Open Invoices' }) as HTMLButtonElement).disabled).toBe(true);
    await fireEvent.click(screen.getByRole('button', { name: 'Remove incompatible Invoices' }));
    expect(screen.getByRole('dialog', { name: 'Delete Saved View?' })).toBeDefined();
    expect(requests).toHaveLength(1);
    await fireEvent.click(screen.getByRole('button', { name: 'Confirm delete' }));
    await waitFor(() => expect(requests).toHaveLength(2));
    expect(requests[1]!.method).toBe('DELETE');
  });
});
