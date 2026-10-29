import { describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../../api/client';
import type { SavedView } from '../../api/generated/models';
import { ShellData, viewIsFiles, viewState } from './shell-data.svelte';

const NOW = Date.parse('2026-10-27T12:00:00Z');
const DAY = 24 * 60 * 60 * 1000;
const iso = (ago: number) => new Date(NOW - ago).toISOString();

interface Answers {
  views?: SavedView[];
  failing?: string[];
}

function view(id: number, name: string, presentation = 'table'): SavedView {
  return {
    id, name, revision: 1, schema_version: 2,
    created_at: iso(DAY), updated_at: iso(DAY),
    canonical_state: { presentation, filters: [], grouping: [], sort: [], columns: [] },
  };
}

function setup(answers: Answers = {}) {
  const requests: Request[] = [];
  const fetchFn = vi.fn<typeof fetch>(async (input, init) => {
    const request = input instanceof Request ? input : new Request(input, init);
    requests.push(request);
    const path = new URL(request.url).pathname;
    if (answers.failing?.includes(path)) return Response.json({ error: 'boom' }, { status: 500 });
    if (path === '/api/v1/saved-views' && request.method === 'GET') return Response.json({ saved_views: answers.views ?? [] });
    if (path.startsWith('/api/v1/saved-views/') && request.method === 'DELETE') return new Response(null, { status: 204 });
    if (path.startsWith('/api/v1/saved-views/') && request.method === 'PATCH') {
      const body = await request.clone().json();
      const current = (answers.views ?? []).find((candidate) => path.endsWith(`/${candidate.id}`))!;
      return Response.json({ ...current, ...body, revision: current.revision + 1 });
    }
    return Response.json({}, { status: 404 });
  });
  const data = new ShellData(createAPIClient(fetchFn));
  return { data, requests };
}

describe('ShellData', () => {
  it('reads saved views', async () => {
    const { data } = setup({
      views: [view(1, 'Budget'), view(2, 'Invoices', 'files')],
    });
    await data.refresh();

    expect(data.views.map(({ name }) => name)).toEqual(['Budget', 'Invoices']);
  });





  it('classifies saved views by presentation and restores their state', () => {
    expect(viewIsFiles(view(1, 'a'))).toBe(false);
    expect(viewIsFiles(view(2, 'b', 'files'))).toBe(true);
    expect(viewState(view(2, 'b', 'files')).workspace).toBe('files');
    expect(viewState(view(1, 'a')).workspace).toBe('everything');
  });

  describe('saved views', () => {
    it('renames with the current revision and replaces the row', async () => {
      const views = [view(1, 'Budget')];
      const { data, requests } = setup({ views });
      await data.refresh();

      expect(await data.renameView(data.views[0]!, '  Q3 budget ')).toBeUndefined();
      const patch = requests.find((request) => request.method === 'PATCH')!;
      expect(patch.headers.get('If-Match')).toBe('"saved-view-1-r1"');
      expect(data.views[0]).toMatchObject({ name: 'Q3 budget', revision: 2 });
    });

    it('does not call the server for an unchanged or empty name', async () => {
      const { data, requests } = setup({ views: [view(1, 'Budget')] });
      await data.refresh();
      expect(await data.renameView(data.views[0]!, 'Budget')).toBeUndefined();
      expect(await data.renameView(data.views[0]!, '   ')).toBeUndefined();
      expect(requests.some((request) => request.method === 'PATCH')).toBe(false);
    });

    it('reports a conflict from another session', async () => {
      const fetchFn = vi.fn<typeof fetch>(async () => Response.json({}, { status: 409 }));
      const data = new ShellData(createAPIClient(fetchFn));
      expect(await data.renameView(view(1, 'Budget'), 'Q3')).toBe('This view changed in another session.');
    });

    it('hides a deleted view at once, deletes it for real on commit, and restores it on undo', async () => {
      const views = [view(1, 'Budget'), view(2, 'Invoices')];
      const { data, requests } = setup({ views });
      await data.refresh();
      const [budget] = data.views;

      data.hideView(budget!);
      expect(data.views.map(({ name }) => name)).toEqual(['Invoices']);
      expect(requests.some((request) => request.method === 'DELETE')).toBe(false);

      data.restoreView(budget!);
      expect(data.views.map(({ name }) => name)).toEqual(['Budget', 'Invoices']);

      data.hideView(budget!);
      await data.commitDelete(budget!);
      const removal = requests.find((request) => request.method === 'DELETE')!;
      expect(new URL(removal.url).pathname).toBe('/api/v1/saved-views/1');
      expect(removal.headers.get('If-Match')).toBe('"saved-view-1-r1"');
      expect(data.views.map(({ name }) => name)).toEqual(['Invoices']);
    });

    it('puts a view back when the delete fails', async () => {
      const fetchFn = vi.fn<typeof fetch>(async () => Response.json({ error: 'boom' }, { status: 500 }));
      const data = new ShellData(createAPIClient(fetchFn));
      const saved = view(1, 'Budget');
      data.hideView(saved);
      expect(await data.commitDelete(saved)).toBe("Couldn't delete Budget.");
      expect(data.views).toEqual([saved]);
    });
  });
});

it('keeps a pending deletion hidden when the shell refreshes', async () => {
  const saved = view(1, 'Budget');
  const { data } = setup({ views: [saved] });
  await data.refresh(); data.hideView(saved); await data.refresh();
  expect(data.views).toEqual([]);
  data.restoreView(saved);
  expect(data.views).toEqual([saved]);
});
it('edits descriptions with the revision and reports a conflict', async () => {
  const saved = view(1, 'Budget');
  const { data, requests } = setup({ views: [saved] });
  await data.refresh();
  expect(await data.describeView(saved, '  Quarterly review  ')).toBeUndefined();
  expect(data.views[0]?.description).toBe('Quarterly review');
  expect(requests.find(request => request.method === 'PATCH')?.headers.get('If-Match')).toBe('"saved-view-1-r1"');
  const conflicted = new ShellData(createAPIClient(async () => Response.json({}, { status: 409 })));
  expect(await conflicted.describeView(saved, 'Review')).toBe('This view changed in another session.');
});

it('shows a new view whose id reuses a completed deletion', async () => {
  const answers = { views: [view(1, 'Budget'), view(2, 'Invoices')] };
  const { data } = setup(answers);
  await data.refresh();
  const saved = data.views[1]!;
  data.hideView(saved);
  await data.commitDelete(saved);
  answers.views = [view(1, 'Budget'), view(2, 'Receipts')];
  await data.refresh();
  expect(data.views.map(view => view.name)).toEqual(['Budget', 'Receipts']);
});
it('restores an undone view in server name order', async () => {
  const { data } = setup({ views: [view(2, 'Alpha'), view(1, 'Zeta')] });
  await data.refresh();
  const saved = data.views[0]!;
  data.hideView(saved); data.restoreView(saved);
  expect(data.views.map(view => view.name)).toEqual(['Alpha', 'Zeta']);
});
it('uses a keepalive request when committing a pending deletion', async () => {
  const { data, requests } = setup();
  await data.commitDelete(view(1, 'Budget'));
  expect(requests.find(request => request.method === 'DELETE')?.keepalive).toBe(true);
});
