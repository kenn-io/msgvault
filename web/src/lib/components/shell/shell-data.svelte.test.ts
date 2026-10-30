import { describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../../api/client';
import type { SavedView } from '../../api/generated/models';
import { view } from './fixtures.test-support';
import { ShellData } from './shell-data.svelte';

interface Answers {
  views?: SavedView[];
  failing?: string[];
}

function setup(answers: Answers = {}) {
  const requests: Request[] = [];
  const fetchFn = vi.fn<typeof fetch>(async (input, init) => {
    const request = input instanceof Request ? input : new Request(input, init);
    requests.push(request);
    const path = new URL(request.url).pathname;
    if (answers.failing?.includes(path)) return Response.json({ error: 'boom' }, { status: 500 });
    if (path === '/api/v1/saved-views' && request.method === 'GET') return Response.json({ saved_views: answers.views ?? [] });
    if (path.startsWith('/api/v1/saved-views/') && request.method === 'DELETE') {
      answers.views = answers.views?.filter(view => !path.endsWith(`/${view.id}`));
      return new Response(null, { status: 204 });
    }
    if (path.startsWith('/api/v1/saved-views/') && request.method === 'PATCH') {
      const body = await request.clone().json();
      const current = (answers.views ?? []).find((candidate) => path.endsWith(`/${candidate.id}`))!;
      const updated = { ...current, ...body, revision: current.revision + 1 };
      answers.views = answers.views?.map(view => view.id === updated.id ? updated : view).sort((a, b) => a.name.localeCompare(b.name));
      return Response.json(updated);
    }
    return Response.json({}, { status: 404 });
  });
  const data = new ShellData(createAPIClient(fetchFn));
  return { data, requests };
}

describe('ShellData', () => {

  describe('saved views', () => {
    it.each([
      { field: 'name', input: '  Q3 budget ', update: { name: 'Q3 budget' }, order: ['Invoices', 'Q3 budget'] },
      { field: 'description', input: '  Quarterly review  ', update: { description: 'Quarterly review' }, order: ['Budget', 'Invoices'] },
    ])('edits $field with the current revision and replaces the row', async ({ field, input, update, order }) => {
      const views = [view(1, 'Budget'), view(2, 'Invoices')];
      const { data, requests } = setup({ views });
      await data.refresh();

      expect(await (field === 'name' ? data.renameView(data.views[0]!, input) : data.describeView(data.views[0]!, input))).toBeUndefined();
      const patch = requests.find((request) => request.method === 'PATCH')!;
      expect(patch.headers.get('If-Match')).toBe('"saved-view-1-r1"');
      expect(data.views.map(view => view.name)).toEqual(order);
      expect(data.views.find(view => view.id === 1)).toMatchObject({ id: 1, revision: 2, ...update });
    });

    it('does not call the server for an unchanged or empty name', async () => {
      const { data, requests } = setup({ views: [view(1, 'Budget')] });
      await data.refresh();
      expect(await data.renameView(data.views[0]!, 'Budget')).toBeUndefined();
      expect(await data.renameView(data.views[0]!, '   ')).toBe('Enter a name.');
      expect(requests.some((request) => request.method === 'PATCH')).toBe(false);
    });

    it('hides a deleted view at once and deletes it for real on commit', async () => {
      const views = [view(1, 'Budget'), view(2, 'Invoices')];
      const { data, requests } = setup({ views });
      await data.refresh();
      const [budget] = data.views;

      await data.deleteView(budget!);
      expect(data.views.map(({ name }) => name)).toEqual(['Invoices']);
      expect(requests.some((request) => request.method === 'DELETE')).toBe(false);

      await data.expireDelete();
      const removal = requests.find((request) => request.method === 'DELETE')!;
      expect(new URL(removal.url).pathname).toBe('/api/v1/saved-views/1');
      expect(removal.headers.get('If-Match')).toBe('"saved-view-1-r1"');
      expect(removal.keepalive).toBe(true);
      expect(data.views.map(({ name }) => name)).toEqual(['Invoices']);
    });


  });
});


it('shows a new view whose id reuses a completed deletion', async () => {
  const answers = { views: [view(1, 'Budget'), view(2, 'Invoices')] };
  const { data } = setup(answers);
  await data.refresh();
  const saved = data.views[1]!;
  await data.deleteView(saved);
  await data.expireDelete();
  answers.views = [view(1, 'Budget'), view(2, 'Receipts')];
  await data.refresh();
  expect(data.views.map(view => view.name)).toEqual(['Budget', 'Receipts']);
});


it.each(['rename', 'delete'])('keeps a completed %s when an earlier list read finishes', async mutation => {
  const saved = view(1, 'Budget');
  const invoices = view(2, 'Invoices');
  let finish!: (response: Response) => void;
  const pending = new Promise<Response>(resolve => { finish = resolve; });
  let reads = 0;
  let deleted = false;
  const data = new ShellData(createAPIClient(async (input, init) => {
    const request = new Request(input, init);
    if (request.method === 'PATCH') return Response.json({ ...saved, name: 'Q3 budget', revision: 2 });
    if (request.method === 'DELETE') { deleted = true; return new Response(null, { status: 204 }); }
    if (new URL(request.url).pathname === '/api/v1/saved-views') return ++reads === 1 ? Response.json({ saved_views: [saved] }) : reads === 2 ? pending : Response.json({ saved_views: deleted ? [invoices] : [{ ...saved, name: 'Q3 budget', revision: 2 }] });
    return Response.json({}, { status: 404 });
  }));
  await data.refresh();
  const reading = data.refresh();
  if (mutation === 'rename') await data.renameView(saved, 'Q3 budget');
  else { await data.deleteView(saved); await data.expireDelete(); }
  finish(Response.json({ saved_views: mutation === 'delete' ? [saved, invoices] : [saved] }));
  await reading;
  expect(data.views.map(({ name, revision }) => [name, revision])).toEqual(mutation === 'rename' ? [['Q3 budget', 2]] : [['Invoices', 1]]);
});

it('exposes failed reads, keeps last views, and clears the error after Retry', async () => {
  const answers: Answers = { views: [view(1, 'Budget')], failing: [] };
  const { data } = setup(answers);
  await data.refresh();
  answers.failing = ['/api/v1/saved-views'];
  await data.refresh();
  expect(data.views.map(view => view.name)).toEqual(['Budget']);
  expect(data.error).toBe("Couldn't load saved views.");
  answers.failing = [];
  await data.refresh();
  expect(data.error).toBeUndefined();
});



it('refreshes a PATCH conflict so the next edit uses the current revision', async () => {
  const saved = view(1, 'Budget');
  const revisions: string[] = [];
  let reads = 0;
  let refreshed!: () => void;
  const read = new Promise<void>(resolve => { refreshed = resolve; });
  const data = new ShellData(createAPIClient(async (input, init) => {
    const request = new Request(input, init);
    const path = new URL(request.url).pathname;
    if (request.method === 'PATCH') {
      revisions.push(request.headers.get('If-Match')!);
      return revisions.length === 1 ? Response.json({}, { status: 409 }) : Response.json({ ...saved, name: 'Q3', revision: 3 });
    }
    if (path === '/api/v1/saved-views') { if (++reads === 1) return Response.json({ saved_views: [saved] }); refreshed(); return Response.json({ saved_views: [{ ...saved, revision: 2 }] }); }
    return Response.json({}, { status: 404 });
  }));
  await data.refresh();
  expect(await data.renameView(saved, 'Q3')).toBe('This view changed in another session. Cancel this edit and try again.');
  await read;
  await vi.waitFor(() => expect(data.views[0]?.revision).toBe(2));
  await data.renameView(data.views[0]!, 'Q3');
  expect(revisions).toEqual(['"saved-view-1-r1"', '"saved-view-1-r2"']);
});

it.each(['undo', 'expire'])('keeps accepted deletions hidden while a prior DELETE waits and the newest is resolved by %s', async ending => {
  let views = [view(1, 'Alpha'), view(2, 'Beta'), view(3, 'Gamma')];
  let finish!: () => void;
  const gate = new Promise<void>(resolve => { finish = resolve; });
  const deleted: number[] = [];
  const data = new ShellData(createAPIClient(async (input, init) => {
    const request = new Request(input, init);
    const path = new URL(request.url).pathname;
    if (request.method === 'DELETE') {
      const id = Number(path.split('/').at(-1));
      if (id === 1) await gate;
      deleted.push(id);
      views = views.filter(view => view.id !== id);
      return new Response(null, { status: 204 });
    }
    return Response.json({ saved_views: views });
  }));
  await data.refresh();
  const [a, b, c] = views;
  await data.deleteView(a!);
  const second = data.deleteView(b!);
  const third = data.deleteView(c!);
  expect(data.pending?.name).toBe('Gamma');
  await data.refresh();
  expect(data.views).toEqual([]);
  if (ending === 'undo') {
    await data.undoDelete();
    expect(data.views).toEqual([c]);
  } else await data.expireDelete();
  finish();
  await Promise.all([second, third]);
  await data.expireDelete();
  await data.refresh();
  expect(deleted.sort()).toEqual(ending === 'undo' ? [1, 2] : [1, 2, 3]);
  expect(data.views).toEqual(ending === 'undo' ? [c] : []);
});

it('restores the visible view and reports a failed delete', async () => {
  const saved = view(1, 'Équipe');
  let finish!: (response: Response) => void;
  const pending = new Promise<Response>(resolve => { finish = resolve; });
  const data = new ShellData(createAPIClient(async (input, init) => {
    const request = new Request(input, init);
    if (request.method === 'DELETE') {
      return pending;
    }
    return Response.json({ saved_views: [saved] });
  }));
  await data.refresh();
  await data.deleteView(saved);
  const deleting = data.expireDelete();
  expect(data.views).toEqual([]);
  finish(Response.json({}, { status: 500 }));
  expect(await deleting).toBe("Couldn't delete Équipe.");
  expect(data.views).toContainEqual(saved);
});


it.each([
  ['pending', '2020-01-01T00:00:00Z', true],
  ['in_progress', '2020-01-01T00:00:00Z', true],
  ['completed', '2026-10-22T12:00:00Z', true],
  ['failed', '2026-10-22T11:59:59Z', false],
  ['cancelled', '2026-10-28T12:00:00Z', true],
  ['cancelled', '2026-10-01T12:00:00Z', false],
])('shows the deletion sidebar entry for %s created at %s: %s', async (status, created_at, visible) => {
  vi.spyOn(Date, 'now').mockReturnValue(Date.parse('2026-10-29T12:00:00Z'));
  const data = new ShellData(createAPIClient(async input => {
    const path = new URL(input instanceof Request ? input.url : String(input)).pathname;
    if (path.endsWith('/deletions')) return Response.json({ manifests: [{ id: 'batch-1', status, created_at }] });
    return Response.json({ saved_views: [] });
  }));
  try {
    await data.refresh();
    expect(data.deletionsStaged).toBe(visible);
  } finally { data.stop(); vi.restoreAllMocks(); }
});

it('uses cached completion time for delayed batches and keeps visibility on failed reads', async () => {
  vi.spyOn(Date, 'now').mockReturnValue(Date.parse('2026-10-29T12:00:00Z'));
  const requests: string[] = [];
  let failing = false;
  const data = new ShellData(createAPIClient(async input => {
    const path = new URL((input as Request).url).pathname;
    requests.push(path);
    if (failing) return Response.json({}, { status: 500 });
    if (path === '/api/v1/deletions') return Response.json({ manifests: [{ id: 'delayed', status: 'completed', created_at: '2026-10-01T12:00:00Z' }, { id: 'old', status: 'completed', created_at: '2026-09-01T12:00:00Z' }] });
    return Response.json({ execution: { completed_at: path.endsWith('/delayed') ? '2026-10-28T12:00:00Z' : '2026-09-02T12:00:00Z' } });
  }));
  try {
    await data.refreshDeletions();
    expect(data.deletionsStaged).toBe(true);
    await data.refreshDeletions();
    expect(requests.filter(path => path.endsWith('/delayed'))).toHaveLength(1);
    failing = true;
    await data.refreshDeletions();
    expect(data.deletionsStaged).toBe(true);
    failing = false;
    vi.mocked(Date.now).mockReturnValue(Date.parse('2026-11-05T12:00:00Z'));
    await data.refreshDeletions();
    expect(data.deletionsStaged).toBe(false);
  } finally { data.stop(); vi.restoreAllMocks(); }
});

it('refreshes deletions and jobs on the timer without rereading saved views', async () => {
  vi.useFakeTimers();
  const { data, requests } = setup();
  try {
    await data.refresh();
    data.start();
    await vi.advanceTimersByTimeAsync(30_000);
    expect(requests.filter(request => new URL(request.url).pathname === '/api/v1/saved-views')).toHaveLength(1);
    expect(requests.filter(request => new URL(request.url).pathname === '/api/v1/deletions')).toHaveLength(2);
  } finally { data.stop(); vi.useRealTimers(); }
});

it('bounds older completion reads to four while a recent batch keeps navigation visible', async () => {
  const pending: Array<(response: Response) => void> = [];
  const data = new ShellData(createAPIClient(async input => {
    if ((input as Request).url.endsWith('/saved-views')) return Response.json({ saved_views: [view(1, 'Budget')] });
    if ((input as Request).url.endsWith('/deletions')) return Response.json({ manifests: [
      { id: 'recent', status: 'pending', created_at: '2020-01-01T00:00:00Z' },
      ...Array.from({ length: 5 }, (_, i) => ({ id: `old-${i}`, status: 'completed', created_at: '2020-01-01T00:00:00Z' }))
    ] });
    return new Promise<Response>(resolve => pending.push(resolve));
  }));
  const refresh = data.refresh();
  await vi.waitFor(() => expect(pending).toHaveLength(4));
  expect(data.deletionsStaged).toBe(true);
  await vi.waitFor(() => expect(data.views.map(view => view.name)).toEqual(['Budget']));
  pending[0](Response.json({}));
  await vi.waitFor(() => expect(pending).toHaveLength(5));
  pending.slice(1).forEach(resolve => resolve(Response.json({})));
  await refresh;
  data.stop();
});

it('refreshes completion time after an old failed batch runs again', async () => {
  vi.spyOn(Date, 'now').mockReturnValue(Date.parse('2026-10-29T12:00:00Z'));
  let status = 'failed', reads = 0;
  const data = new ShellData(createAPIClient(async input => {
    if ((input as Request).url.endsWith('/deletions')) return Response.json({ manifests: [{ id: 'retry', status, created_at: '2020-01-01T00:00:00Z' }] });
    reads++;
    return Response.json({ execution: { completed_at: status === 'failed' ? '2020-01-01T00:00:00Z' : '2026-10-29T11:59:00Z' } });
  }));
  try {
    await data.refreshDeletions();
    expect(data.deletionsStaged).toBe(false);
    status = 'in_progress';
    await data.refreshDeletions();
    expect(data.deletionsStaged).toBe(true);
    status = 'completed';
    await data.refreshDeletions();
    expect(data.deletionsStaged).toBe(true);
    expect(reads).toBe(2);
  } finally { data.stop(); vi.restoreAllMocks(); }
});
