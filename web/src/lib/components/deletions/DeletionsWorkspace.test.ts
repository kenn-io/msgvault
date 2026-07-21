import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { appShortcuts, initShortcuts } from '@kenn-io/kit-ui';

import { createAPIClient } from '../../api/client';
import type { components } from '../../api/generated/schema';
import DeletionsWorkspace from './DeletionsWorkspace.svelte';

type ExploreSelection = components['schemas']['ExploreSelection'];

const explicit: ExploreSelection = {
  mode: 'explicit',
  predicate: { presentation: 'table' },
  row_keys: ['source:1:message:m1'],
  cache_revision: 'cache-1',
  search_provenance: {}
};

const matching: ExploreSelection = {
  mode: 'all_matching',
  predicate: { filters: [{ dimension: 'source', values: ['1'] }], presentation: 'table' },
  exclusions: ['source:1:message:m2'],
  cache_revision: 'cache-1',
  search_provenance: {}
};

function preflight(overrides: Record<string, unknown> = {}) {
  return {
    count: 1, estimated_bytes: 120, cache_revision: 'cache-1', search_provenance: {},
    unavailable_actions: [], operation_token: 'operation-1', expires_at: '2026-07-19T10:05:00Z',
    ...overrides
  };
}

function listResponse() {
  return { manifests: [{
    id: 'batch-1', status: 'pending', created_at: '2026-07-19T10:00:00Z',
    created_by: 'api', description: 'reviewed selection', message_count: 1
  }] };
}

afterEach(() => document.body.replaceChildren());

describe('DeletionsWorkspace', () => {
  it('preflights, dry-runs, and explicitly confirms an exact selection before staging', async () => {
    const requests: Request[] = [];
    let deletionPosts = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      const path = new URL(request.url).pathname;
      if (path.endsWith('/explore/preflight')) return Response.json(preflight());
      if (request.method === 'POST') {
        deletionPosts += 1;
        return deletionPosts === 1
          ? Response.json({ dry_run: true, message_count: 1, account: 'archive@example.com', sample_gmail_ids: ['m1'] })
          : Response.json({ dry_run: false, message_count: 1, account: 'archive@example.com', id: 'batch-2', status: 'pending' }, { status: 201 });
      }
      return Response.json(listResponse());
    });
    render(DeletionsWorkspace, { client: createAPIClient(fetchFn), selection: explicit });

    await screen.findByText('batch-1');
    await fireEvent.click(screen.getByRole('button', { name: 'Review selection' }));
    expect(await screen.findByText('1 item · 120 bytes')).toBeDefined();
    await fireEvent.click(screen.getByRole('button', { name: 'Dry run' }));
    expect(await screen.findByText('Dry run: 1 item in archive@example.com')).toBeDefined();

    await fireEvent.click(screen.getByRole('button', { name: 'Stage deletion' }));
    expect(screen.getByRole('dialog', { name: 'Confirm selected deletion' })).toBeDefined();
    expect(deletionPosts).toBe(1);
    await fireEvent.click(screen.getByRole('button', { name: 'Confirm stage deletion' }));
    await waitFor(() => expect(deletionPosts).toBe(2));

    const preflightBody = await requests.find((request) => new URL(request.url).pathname.endsWith('/explore/preflight'))!.clone().json();
    expect(preflightBody).toEqual({ selection: explicit });
    const stageBody = await requests.filter((request) => request.method === 'POST').at(-1)!.clone().json();
    expect(stageBody).toMatchObject({ selection: explicit, operation_token: 'operation-1', dry_run: false });
  });

  it('uses d/D shortcuts to preflight the matching selection and never acts before confirmation', async () => {
    const detach = initShortcuts();
    let staged = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = new URL(request.url).pathname;
      if (path.endsWith('/explore/preflight')) return Response.json(preflight({ count: 8 }));
      if (request.method === 'POST') {
        staged += 1;
        return Response.json({ dry_run: false, message_count: 8, id: 'batch-2', status: 'pending' }, { status: 201 });
      }
      return Response.json({ manifests: [] });
    });
    const rendered = render(DeletionsWorkspace, { client: createAPIClient(fetchFn), selection: matching });
    try {
      await screen.findByText('No deletion manifests yet.');
      await fireEvent.keyDown(window, { key: 'D', shiftKey: true });
      expect(await screen.findByRole('dialog', { name: 'Confirm matching deletion' })).toBeDefined();
      expect(screen.getByText(/8 matching items minus 1 exclusion/)).toBeDefined();
      expect(staged).toBe(0);
      await fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));
      expect(staged).toBe(0);
    } finally {
      rendered.unmount();
      detach();
    }
  });

  it('owns deletion shortcuts instead of allowing the shell handler to consume them', async () => {
    const detach = initShortcuts();
    const shellHandler = vi.fn();
    const unregisterShell = appShortcuts.register('d', shellHandler);
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      if (new URL(request.url).pathname.endsWith('/explore/preflight')) return Response.json(preflight());
      return Response.json({ manifests: [] });
    });
    const rendered = render(DeletionsWorkspace, { client: createAPIClient(fetchFn), selection: explicit });
    try {
      await screen.findByText('No deletion manifests yet.');
      await fireEvent.keyDown(window, { key: 'd' });
      expect(await screen.findByRole('dialog', { name: 'Confirm selected deletion' })).toBeDefined();
      expect(shellHandler).not.toHaveBeenCalled();
    } finally {
      rendered.unmount();
      unregisterShell();
      detach();
    }
  });

  it('lists, inspects, and confirms cancellation while preserving lifecycle detail', async () => {
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      if (request.method === 'DELETE') return Response.json({ id: 'batch-1', status: 'cancelled' });
      if (new URL(request.url).pathname.endsWith('/batch-1')) return Response.json({
        id: 'batch-1', status: 'pending', created_at: '2026-07-19T10:00:00Z', created_by: 'api',
        description: 'reviewed selection', account: 'archive@example.com', message_count: 1,
        execution: null, summary: null
      });
      return Response.json(listResponse());
    });
    render(DeletionsWorkspace, { client: createAPIClient(fetchFn) });

    await fireEvent.click(await screen.findByRole('button', { name: 'Inspect batch-1' }));
    expect(await screen.findByText('archive@example.com')).toBeDefined();
    await fireEvent.click(screen.getByRole('button', { name: 'Cancel batch-1' }));
    expect(screen.getByRole('dialog', { name: 'Cancel deletion manifest?' })).toBeDefined();
    expect(requests.some((request) => request.method === 'DELETE')).toBe(false);
    await fireEvent.click(screen.getByRole('button', { name: 'Confirm cancel manifest' }));
    await waitFor(() => expect(requests.some((request) => request.method === 'DELETE')).toBe(true));
    expect((await screen.findAllByText('cancelled')).length).toBeGreaterThan(0);
  });

  it('shows server-supplied action reasons and disables staging', async () => {
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      if (new URL(request.url).pathname.endsWith('/explore/preflight')) return Response.json(preflight({
        unavailable_actions: [{ action: 'stage_deletion', reason: 'selection_contains_items_that_cannot_be_deleted_from_source' }]
      }));
      return Response.json({ manifests: [] });
    });
    render(DeletionsWorkspace, { client: createAPIClient(fetchFn), selection: explicit });

    await fireEvent.click(await screen.findByRole('button', { name: 'Review selection' }));
    expect(await screen.findByText(/selection_contains_items_that_cannot_be_deleted_from_source/)).toBeDefined();
    expect((screen.getByRole('button', { name: 'Stage deletion' }) as HTMLButtonElement).disabled).toBe(true);
  });
});
