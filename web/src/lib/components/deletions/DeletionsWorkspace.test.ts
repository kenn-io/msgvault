import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { appShortcuts, initShortcuts } from '@kenn-io/kit-ui';

import { createAPIClient } from '../../api/client';
import type { ExploreSelection as GeneratedExploreSelection } from '../../api/generated/models';
import DeletionsWorkspace from './DeletionsWorkspace.svelte';

type ExploreSelection = GeneratedExploreSelection;

const explicit: ExploreSelection = {
  mode: 'explicit',
  predicate: { presentation: 'table' },
  row_keys: ['source:1:message:m1'],
  cache_revision: 'cache-1',
  search_provenance: {},
};

const matching: ExploreSelection = {
  mode: 'all_matching',
  predicate: { filters: [{ dimension: 'source', values: ['1'] }], presentation: 'table' },
  exclusions: ['source:1:message:m2'],
  cache_revision: 'cache-1',
  search_provenance: {},
};

function preflight(overrides: Record<string, unknown> = {}) {
  return {
    count: 1,
    estimated_bytes: 120,
    cache_revision: 'cache-1',
    search_provenance: {},
    unavailable_actions: [],
    operation_token: 'operation-1',
    expires_at: '2026-07-19T10:05:00Z',
    ...overrides,
  };
}

function listResponse() {
  return {
    manifests: [
      {
        id: 'batch-1',
        status: 'pending',
        created_at: '2026-07-19T10:00:00Z',
        created_by: 'api',
        description: 'reviewed selection',
        message_count: 1,
      },
    ],
  };
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
          ? Response.json({ dry_run: true, matched_count: 1, message_count: 1, skipped_count: 0, account: 'archive@example.com', sample_gmail_ids: ['m1'] })
          : Response.json(
              { dry_run: false, matched_count: 1, message_count: 1, skipped_count: 0, account: 'archive@example.com', id: 'batch-2', status: 'pending' },
              { status: 201 },
            );
      }
      return Response.json(listResponse());
    });
    render(DeletionsWorkspace, { client: createAPIClient(fetchFn), selection: explicit });

    await screen.findByText('batch-1');
    await fireEvent.click(screen.getByRole('button', { name: 'Review selection' }));
    expect(await screen.findByText('1 item · 120 bytes')).toBeDefined();
    await fireEvent.click(screen.getByRole('button', { name: 'Dry run' }));
    expect(await screen.findByText(/Dry run: Matched: 1 · Staged: 1 · Skipped: 0 in archive@example.com/)).toBeDefined();
    expect(screen.queryByRole('alert')).toBeNull();

    await fireEvent.click(screen.getByRole('button', { name: 'Stage deletion' }));
    expect(screen.getByRole('dialog', { name: 'Confirm selected deletion' })).toBeDefined();
    expect(deletionPosts).toBe(1);
    await fireEvent.click(screen.getByRole('button', { name: 'Confirm stage deletion' }));
    await waitFor(() => expect(deletionPosts).toBe(2));
    expect(await screen.findByText(/Batch ID: batch-2/)).toBeDefined();

    const preflightBody = await requests
      .find((request) => new URL(request.url).pathname.endsWith('/explore/preflight'))!
      .clone()
      .json();
    expect(preflightBody).toEqual({ selection: explicit });
    const stageBody = await requests
      .filter((request) => request.method === 'POST')
      .at(-1)!
      .clone()
      .json();
    expect(stageBody).toMatchObject({ selection: explicit, operation_token: 'operation-1', dry_run: false });
  });

  it('shows mixed dry-run and staged counts with a partial-staging warning', async () => {
    const requests: Request[] = [];
    let deletionPosts = 0;
    const mixed = { dry_run: true, matched_count: 3, message_count: 2, skipped_count: 1, account: 'archive@example.com' };
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      const path = new URL(request.url).pathname;
      if (path.endsWith('/explore/preflight')) return Response.json(preflight({ count: 3 }));
      if (request.method === 'POST') {
        deletionPosts += 1;
        return deletionPosts === 1
          ? Response.json(mixed)
          : Response.json({ ...mixed, dry_run: false, id: 'batch-2', status: 'pending' }, { status: 201 });
      }
      return Response.json({ manifests: [] });
    });
    render(DeletionsWorkspace, { client: createAPIClient(fetchFn), selection: explicit });

    await screen.findByText('No deletion manifests yet.');
    await fireEvent.click(screen.getByRole('button', { name: 'Review selection' }));
    await screen.findByText('3 items · 120 bytes');
    await fireEvent.click(screen.getByRole('button', { name: 'Dry run' }));
    expect(await screen.findByText(/Dry run: Matched: 3 · Staged: 2 · Skipped: 1 in archive@example.com/)).toBeDefined();
    expect(screen.getByRole('alert').textContent).toMatch(/Partial staging.*deletable Gmail subset.*unsupported match may be skipped/);

    await fireEvent.click(screen.getByRole('button', { name: 'Stage deletion' }));
    const dialog = screen.getByRole('dialog', { name: 'Confirm selected deletion' });
    expect(dialog.textContent).toMatch(/Dry run: Matched: 3 · Staged: 2 · Skipped: 1/);
    expect(dialog.textContent).toMatch(/deletable Gmail subset/);
    expect(deletionPosts).toBe(1);
    await fireEvent.click(screen.getByRole('button', { name: 'Confirm stage deletion' }));
    await waitFor(() => expect(deletionPosts).toBe(2));
    expect(await screen.findByText(/Staged: Matched: 3 · Staged: 2 · Skipped: 1 in archive@example.com/)).toBeDefined();
    expect(screen.getByRole('alert').textContent).toMatch(/was staged.*unsupported match was skipped/);
    expect(requests.filter((request) => request.method === 'POST').length).toBeGreaterThanOrEqual(2);
  });

  it('derives optional counts when the response omits one field at a time', async () => {
    let deletionPosts = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = new URL(request.url).pathname;
      if (path.endsWith('/explore/preflight')) return Response.json(preflight({ count: 3 }));
      if (request.method === 'POST') {
        deletionPosts += 1;
        return deletionPosts === 1
          ? Response.json({ dry_run: true, message_count: 2, skipped_count: 1 })
          : Response.json({ dry_run: true, matched_count: 3, message_count: 2 });
      }
      return Response.json({ manifests: [] });
    });
    render(DeletionsWorkspace, { client: createAPIClient(fetchFn), selection: explicit });

    await screen.findByText('No deletion manifests yet.');
    await fireEvent.click(screen.getByRole('button', { name: 'Review selection' }));
    await screen.findByText('3 items · 120 bytes');
    await fireEvent.click(screen.getByRole('button', { name: 'Dry run' }));
    expect(await screen.findByText(/Dry run: Matched: 3 · Staged: 2 · Skipped: 1/)).toBeDefined();
    await fireEvent.click(screen.getByRole('button', { name: 'Dry run' }));
    expect(await screen.findByText(/Dry run: Matched: 3 · Staged: 2 · Skipped: 1/)).toBeDefined();
    expect(screen.getByRole('alert')).toBeDefined();
  });

  it('discards a late dry-run response after the selection changes', async () => {
    let resolveDryRun!: (response: Response) => void;
    const pendingDryRun = new Promise<Response>((resolve) => {
      resolveDryRun = resolve;
    });
    let posts = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = new URL(request.url).pathname;
      if (path.endsWith('/explore/preflight')) return Response.json(preflight());
      if (request.method === 'POST') {
        posts += 1;
        return pendingDryRun;
      }
      return Response.json({ manifests: [] });
    });
    const rendered = render(DeletionsWorkspace, { client: createAPIClient(fetchFn), selection: explicit });

    await screen.findByText('No deletion manifests yet.');
    await fireEvent.click(screen.getByRole('button', { name: 'Review selection' }));
    await screen.findByText('1 item · 120 bytes');
    await fireEvent.click(screen.getByRole('button', { name: 'Dry run' }));
    await waitFor(() => expect(posts).toBe(1));
    await rendered.rerender({ client: createAPIClient(fetchFn), selection: matching });
    resolveDryRun(Response.json({ dry_run: true, message_count: 1 }));

    expect(await screen.findByText('The selection changed while it was being reviewed. Review it again.')).toBeDefined();
    expect(screen.queryByText(/Dry run: Matched/)).toBeNull();
    rendered.unmount();
  });

  it('keeps a committed stage result and refreshes manifests after the selection changes', async () => {
    let resolveStage!: (response: Response) => void;
    const pendingStage = new Promise<Response>((resolve) => {
      resolveStage = resolve;
    });
    let listCalls = 0;
    let stagePosts = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = new URL(request.url).pathname;
      if (path.endsWith('/explore/preflight')) return Response.json(preflight());
      if (request.method === 'POST') {
        stagePosts += 1;
        return pendingStage;
      }
      listCalls += 1;
      return listCalls === 1
        ? Response.json({ manifests: [] })
        : Response.json({
            manifests: [
              {
                id: 'batch-committed',
                status: 'pending',
                created_at: '2026-07-19T10:00:00Z',
                created_by: 'api',
                description: 'reviewed selection',
                message_count: 1,
              },
            ],
          });
    });
    const rendered = render(DeletionsWorkspace, { client: createAPIClient(fetchFn), selection: explicit });

    await screen.findByText('No deletion manifests yet.');
    await fireEvent.click(screen.getByRole('button', { name: 'Review selection' }));
    await screen.findByText('1 item · 120 bytes');
    await fireEvent.click(screen.getByRole('button', { name: 'Stage deletion' }));
    await fireEvent.click(screen.getByRole('button', { name: 'Confirm stage deletion' }));
    await waitFor(() => expect(stagePosts).toBe(1));
    await rendered.rerender({ client: createAPIClient(fetchFn), selection: matching });

    resolveStage(
      Response.json(
        {
          dry_run: false,
          matched_count: 1,
          message_count: 1,
          skipped_count: 0,
          account: 'archive@example.com',
          id: 'batch-committed',
          status: 'pending',
        },
        { status: 201 },
      ),
    );

    expect(await screen.findByText(/Staged: Matched: 1 · Staged: 1 · Skipped: 0 in archive@example.com · Batch ID: batch-committed/)).toBeDefined();
    expect(await screen.findByText('batch-committed')).toBeDefined();
    expect(listCalls).toBe(2);
    rendered.unmount();
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
      expect(screen.getByText(/server stages only deletable Gmail matches and may skip unsupported matches/)).toBeDefined();
      expect(screen.queryByText(/8 matching items minus 1 exclusion/)).toBeNull();
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
      if (new URL(request.url).pathname.endsWith('/batch-1'))
        return Response.json({
          id: 'batch-1',
          status: 'pending',
          created_at: '2026-07-19T10:00:00Z',
          created_by: 'api',
          description: 'reviewed selection',
          account: 'archive@example.com',
          message_count: 1,
          execution: null,
          summary: null,
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

  it('discloses the active-only deletion scope reported by the preflight review', async () => {
    let scoped = false;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      if (new URL(request.url).pathname.endsWith('/explore/preflight')) {
        return Response.json(preflight(scoped ? { search_deletion_scope: 'active' } : {}));
      }
      return Response.json({ manifests: [] });
    });
    render(DeletionsWorkspace, { client: createAPIClient(fetchFn), selection: explicit });

    await fireEvent.click(await screen.findByRole('button', { name: 'Review selection' }));
    await screen.findByText('1 item · 120 bytes');
    expect(screen.queryByText(/active messages only/)).toBeNull();

    scoped = true;
    await fireEvent.click(screen.getByRole('button', { name: 'Review selection' }));
    expect(await screen.findByText('Semantic search covers active messages only.')).toBeDefined();
  });

  it('shows server-supplied action reasons and disables staging', async () => {
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      if (new URL(request.url).pathname.endsWith('/explore/preflight'))
        return Response.json(
          preflight({
            unavailable_actions: [
              { action: 'stage_deletion', reason: 'selection_contains_items_that_cannot_be_deleted_from_source' },
            ],
          }),
        );
      return Response.json({ manifests: [] });
    });
    render(DeletionsWorkspace, { client: createAPIClient(fetchFn), selection: explicit });

    await fireEvent.click(await screen.findByRole('button', { name: 'Review selection' }));
    expect(await screen.findByText(/selection_contains_items_that_cannot_be_deleted_from_source/)).toBeDefined();
    expect((screen.getByRole('button', { name: 'Stage deletion' }) as HTMLButtonElement).disabled).toBe(true);
  });

  it('clears stale result counts when dry-run and create requests fail', async () => {
    let deletionPosts = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      if (new URL(request.url).pathname.endsWith('/explore/preflight')) return Response.json(preflight());
      if (request.method === 'POST') {
        deletionPosts += 1;
        if (deletionPosts === 1 || deletionPosts === 3)
          return Response.json({ dry_run: true, message_count: 1 });
        return Response.json({ message: deletionPosts === 2 ? 'dry run failed' : 'create failed' }, { status: 500 });
      }
      return Response.json({ manifests: [] });
    });
    render(DeletionsWorkspace, { client: createAPIClient(fetchFn), selection: explicit });

    await screen.findByText('No deletion manifests yet.');
    await fireEvent.click(screen.getByRole('button', { name: 'Review selection' }));
    await screen.findByText('1 item · 120 bytes');
    await fireEvent.click(screen.getByRole('button', { name: 'Dry run' }));
    expect(await screen.findByText(/Dry run: Matched: 1/)).toBeDefined();
    await fireEvent.click(screen.getByRole('button', { name: 'Dry run' }));
    expect(await screen.findByText('dry run failed')).toBeDefined();
    expect(screen.queryByText(/Dry run: Matched: 1/)).toBeNull();

    await fireEvent.click(screen.getByRole('button', { name: 'Dry run' }));
    expect(await screen.findByText(/Dry run: Matched: 1/)).toBeDefined();
    await fireEvent.click(screen.getByRole('button', { name: 'Stage deletion' }));
    await fireEvent.click(screen.getByRole('button', { name: 'Confirm stage deletion' }));
    expect(await screen.findByText('create failed')).toBeDefined();
    expect(screen.queryByText(/Dry run: Matched: 1/)).toBeNull();
  });
});
