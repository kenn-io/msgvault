import { describe, expect, it, vi } from 'vitest';

import { createAPIClient as apiClient } from '../../api/client';
import type { SavedView } from '../../api/generated/models';
import { view } from './fixtures.test-support';
import { ShellData, viewIsFiles } from './shell-data.svelte';

// Shell status polling is independent of each saved-view mutation fixture.
const createAPIClient = (fetchFn: typeof fetch) => apiClient(async (input, init) => {
  const path = new URL((input as Request).url).pathname;
  if (path === '/api/v1/operations/status') return Response.json({ lanes: [] });
  if (path === '/api/v1/sources/status') return Response.json({ sources: [] });
  return fetchFn(input, init);
});

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

  it('classifies saved views by presentation', () => {
    expect(viewIsFiles(view(1, 'a'))).toBe(false);
    expect(viewIsFiles(view(2, 'b', 'files'))).toBe(true);
  });

  describe('saved views', () => {
    it('renames with the current revision and replaces the row', async () => {
      const views = [view(1, 'Budget'), view(2, 'Invoices')];
      const { data, requests } = setup({ views });
      await data.refresh();

      expect(await data.renameView(data.views[0]!, '  Q3 budget ')).toBeUndefined();
      const patch = requests.find((request) => request.method === 'PATCH')!;
      expect(patch.headers.get('If-Match')).toBe('"saved-view-1-r1"');
      expect(data.views.map(view => view.name)).toEqual(['Invoices', 'Q3 budget']);
      expect(data.views[1]).toMatchObject({ id: 1, revision: 2 });
    });

    it('does not call the server for an unchanged or empty name', async () => {
      const { data, requests } = setup({ views: [view(1, 'Budget')] });
      await data.refresh();
      expect(await data.renameView(data.views[0]!, 'Budget')).toBeUndefined();
      expect(await data.renameView(data.views[0]!, '   ')).toBeUndefined();
      expect(requests.some((request) => request.method === 'PATCH')).toBe(false);
    });

    it('hides a deleted view at once, deletes it for real on commit, and restores it on undo', async () => {
      const views = [view(1, 'Budget'), view(2, 'Invoices')];
      const { data, requests } = setup({ views });
      await data.refresh();
      const [budget] = data.views;

      await data.deleteView(budget!);
      expect(data.views.map(({ name }) => name)).toEqual(['Invoices']);
      expect(requests.some((request) => request.method === 'DELETE')).toBe(false);

      await data.undoDelete();
      expect(data.views.map(({ name }) => name)).toEqual(['Budget', 'Invoices']);

      await data.deleteView(budget!);
      await data.expireDelete();
      const removal = requests.find((request) => request.method === 'DELETE')!;
      expect(new URL(removal.url).pathname).toBe('/api/v1/saved-views/1');
      expect(removal.headers.get('If-Match')).toBe('"saved-view-1-r1"');
      expect(removal.keepalive).toBe(true);
      expect(data.views.map(({ name }) => name)).toEqual(['Invoices']);
    });


  });
});


it('edits descriptions with the revision', async () => {
  const saved = view(1, 'Budget');
  const { data, requests } = setup({ views: [saved] });
  await data.refresh();
  expect(await data.describeView(saved, '  Quarterly review  ')).toBeUndefined();
  expect(data.views[0]?.description).toBe('Quarterly review');
  expect(requests.find(request => request.method === 'PATCH')?.headers.get('If-Match')).toBe('"saved-view-1-r1"');
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
  let finish!: (response: Response) => void;
  const pending = new Promise<Response>(resolve => { finish = resolve; });
  let reads = 0;
  let deleted = false;
  const data = new ShellData(createAPIClient(async (input, init) => {
    const request = new Request(input, init);
    if (request.method === 'PATCH') return Response.json({ ...saved, name: 'Q3 budget', revision: 2 });
    if (request.method === 'DELETE') { deleted = true; return new Response(null, { status: 204 }); }
    if (new URL(request.url).pathname === '/api/v1/saved-views') return ++reads === 1 ? pending : Response.json({ saved_views: deleted ? [] : [{ ...saved, name: 'Q3 budget', revision: 2 }] });
    return Response.json({}, { status: 404 });
  }));
  data.views = [saved];
  const reading = data.refresh();
  if (mutation === 'rename') await data.renameView(saved, 'Q3 budget');
  else { await data.deleteView(saved); await data.expireDelete(); }
  finish(Response.json({ saved_views: [saved] }));
  await reading;
  expect(data.views.map(({ name, revision }) => [name, revision])).toEqual(mutation === 'rename' ? [['Q3 budget', 2]] : []);
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



it.each(['PATCH', 'DELETE'])('refreshes a %s conflict so the next edit uses the current revision', async method => {
  const saved = view(1, 'Budget');
  const revisions: string[] = [];
  let refreshed!: () => void;
  const read = new Promise<void>(resolve => { refreshed = resolve; });
  const data = new ShellData(createAPIClient(async (input, init) => {
    const request = new Request(input, init);
    const path = new URL(request.url).pathname;
    if (request.method === 'PATCH' || request.method === 'DELETE') {
      revisions.push(request.headers.get('If-Match')!);
      return revisions.length === 1 ? Response.json({}, { status: 409 }) : Response.json({ ...saved, name: 'Q3', revision: 3 });
    }
    if (path === '/api/v1/saved-views') { refreshed(); return Response.json({ saved_views: [{ ...saved, revision: 2 }] }); }
    return Response.json({}, { status: 404 });
  }));
  data.views = [saved];
  if (method === 'PATCH') expect(await data.renameView(saved, 'Q3')).toBe('This view changed in another session. Cancel and reopen the editor to review the latest view.');
  else { await data.deleteView(saved); expect(await data.expireDelete()).toBe("Couldn't delete Budget."); }
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

it('uses server name order after a failed delete', async () => {
  const saved = view(1, 'Équipe');
  let ordered = [saved, view(2, 'école')];
  const data = new ShellData(createAPIClient(async (input, init) => {
    const request = new Request(input, init);
    if (request.method === 'DELETE') {
      ordered = [ordered[1]!, saved];
      return Response.json({}, { status: 500 });
    }
    return Response.json({ saved_views: ordered });
  }));
  await data.refresh();
  await data.deleteView(saved);
  await data.expireDelete();
  expect(data.views.map(view => view.name)).toEqual(['école', 'Équipe']);
});

it('shows a view saved while an earlier deletion commits', async () => {
  const budget = view(1, 'Budget');
  const invoices = view(2, 'Invoices');
  let views = [budget];
  let hold = false;
  let release!: () => void;
  let entered!: () => void;
  const gate = new Promise<void>(resolve => { release = resolve; });
  const started = new Promise<void>(resolve => { entered = resolve; });
  const data = new ShellData(createAPIClient(async input => {
    const request = input as Request;
    if (request.method === 'DELETE') {
      views = views.filter(saved => saved.id !== budget.id);
      return new Response(null, { status: 204 });
    }
    const snapshot = Response.json({ saved_views: views });
    if (hold) { hold = false; entered(); await gate; }
    return snapshot;
  }));
  try {
    await data.refresh();
    await data.deleteView(budget);
    views = [budget, invoices];
    hold = true;
    const reading = data.refresh();
    await started;
    await data.expireDelete();
    release();
    await reading;
    expect(data.views.map(saved => saved.name)).toEqual(['Invoices']);
  } finally {
    release();
    data.stop();
  }
});
