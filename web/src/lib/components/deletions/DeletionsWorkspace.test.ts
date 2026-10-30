import { tick } from 'svelte';
import { fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../../api/client';
import { DeletionsController } from '../../deletions/controller.svelte';
import { batch } from '../../deletions/fixtures.test-support';
import DeletionsWorkspace from './DeletionsWorkspace.svelte';

function detail(id: string, status: string, overrides: Record<string, unknown> = {}) {
  return {
    ...batch(id, status, 2), account: 'archive@example.com',
    summary: {
      accounts: ['archive@example.com'], date_range: ['2026-07-01T00:00:00Z', '2026-07-09T00:00:00Z'],
      message_count: 2, total_size_bytes: 4096, top_senders: [{ sender: 'news@example.com', count: 2 }]
    },
    ...overrides
  };
}

interface Seen { method: string; path: string }

function api(batches: unknown[], seen: Seen[] = [], details: Record<string, unknown> = {}) {
  return createAPIClient(vi.fn<typeof fetch>(async (input) => {
    const request = input instanceof Request ? input : new Request(input);
    const path = new URL(request.url).pathname;
    seen.push({ method: request.method, path });
    if (path === '/api/v1/deletions') return Response.json({ manifests: batches });
    const id = path.split('/').at(-1)!;
    if (request.method === 'DELETE') return Response.json({ id, status: 'cancelled' });
    return Response.json(details[id] ?? detail(id, 'pending'));
  }));
}

afterEach(() => {
  vi.useRealTimers();
  document.body.replaceChildren();
});

describe('DeletionsWorkspace', () => {
  it('lists staged batches by date, count, account, and status, and ends with the delete command', async () => {
    render(DeletionsWorkspace, { controller: new DeletionsController(api([batch('b1', 'pending', 2)])) });
    expect(await screen.findByText('2 messages')).toBeDefined();
    expect(await screen.findByText('archive@example.com')).toBeDefined();
    expect(screen.getByText('Staged')).toBeDefined();
    expect(screen.queryByText('b1')).toBeNull();
    expect(screen.getByText('msgvault delete-staged')).toBeDefined();
    expect(screen.queryByText(/manifest/i)).toBeNull();
  });

  it('loads displayed accounts and waits to read cancelled rows until shown', async () => {
    const seen: Seen[] = [];
    render(DeletionsWorkspace, { controller: new DeletionsController(api([batch('visible', 'pending', 2), batch('hidden', 'cancelled', 3)], seen)) });
    await screen.findByText('archive@example.com');
    expect(screen.queryByText('3 messages')).toBeNull();
    expect(seen.filter(entry => entry.path.includes('/deletions/'))).toEqual([{ method: 'GET', path: '/api/v1/deletions/visible' }]);
    await fireEvent.click(screen.getByRole('checkbox', { name: 'Show cancelled (1)' }));
    await waitFor(() => expect(seen.filter(entry => entry.path.includes('/deletions/'))).toEqual([
      { method: 'GET', path: '/api/v1/deletions/visible' }, { method: 'GET', path: '/api/v1/deletions/hidden' }
    ]));
    expect(screen.getByText('3 messages')).toBeDefined();
    expect(screen.getByText('Cancelled')).toBeDefined();
  });

  it('keeps the batch and confirmation after failed cancellation, then retries', async () => {
    let deletes = 0, lists = 0;
    const onChanged = vi.fn();
    const client = createAPIClient(async input => {
      const request = input as Request;
      if (request.url.endsWith('/deletions')) return ++lists === 1 ? Response.json({ message: 'Offline' }, { status: 500 }) : Response.json({ manifests: [batch('b1', 'in_progress', 2)] });
      if (request.method === 'DELETE') return ++deletes === 1 ? Response.json({ message: 'Busy' }, { status: 500 }) : Response.json({ id: 'b1', status: 'cancelled' });
      return Response.json(detail('b1', 'in_progress'));
    });
    render(DeletionsWorkspace, { controller: new DeletionsController(client), onChanged });
    await fireEvent.click(await screen.findByRole('button', { name: 'Retry' }));
    await fireEvent.click(await screen.findByRole('button', { name: /^Open batch from/ }));
    await fireEvent.click(await screen.findByRole('button', { name: 'Cancel batch' }));
    await fireEvent.click(screen.getByRole('button', { name: 'Keep staged' }));
    expect(deletes).toBe(0);
    await fireEvent.click(screen.getByRole('button', { name: 'Cancel batch' }));
    const dialog = screen.getByRole('dialog', { name: 'Cancel deletion batch?' });
    await fireEvent.click(within(dialog).getByRole('button', { name: 'Cancel batch' }));
    await within(dialog).findByText('Busy');
    expect(onChanged).not.toHaveBeenCalled();
    await fireEvent.click(within(dialog).getByRole('button', { name: 'Cancel batch' }));
    await waitFor(() => expect(onChanged).toHaveBeenCalledOnce());
  });
});

it('keeps the selected batch when another account lookup finishes late', async () => {
  let finish!: (response: Response) => void;
  const client = createAPIClient(async input => {
    const request = input as Request;
    if (request.url.endsWith('/deletions')) return Response.json({ manifests: [batch('a', 'pending', 2), batch('b', 'pending', 3)] });
    return request.url.endsWith('/a') ? new Promise(resolve => { finish = resolve; }) : Response.json(detail('b', 'pending', { description: 'Batch B review' }));
  });
  const controller = new DeletionsController(client);
  render(DeletionsWorkspace, { controller });
  const rows = await screen.findAllByRole('button', { name: /^Open batch from/ });
  await fireEvent.click(rows[0]);
  await fireEvent.click(rows[1]);
  await screen.findByText('Batch B review');
  finish(Response.json({}, { status: 500 }));
  await controller.getDetail('a');
  await tick();
  expect(screen.queryByText("Can't open this batch.")).toBeNull();
  expect(screen.getByText('Batch B review')).toBeDefined();
});

it.each(['completed', 'failed'])('opens %s batch details without offering cancellation', async status => {
  render(DeletionsWorkspace, { controller: new DeletionsController(api([batch('terminal', status, 2)], [], {
    terminal: detail('terminal', status, { description: 'Old newsletter cleanup', ...(status === 'failed' ? { execution: { failed_ids: ['message-a', 'message-b'], succeeded: 0, failed: 2 } } : {}) })
  })) });
  await fireEvent.click(await screen.findByRole('button', { name: /^Open batch from/ }));
  await screen.findByText('Old newsletter cleanup');
  expect(screen.queryByRole('button', { name: 'Cancel batch' })).toBeNull();
  if (status === 'failed') {
    expect(screen.getByText('message-a')).toBeDefined();
    expect(screen.getByText('message-b')).toBeDefined();
  }
});

it('keeps the table and open detail visible during background list reads', async () => {
  let lists = 0, finish: (response: Response) => void;
  const controller = new DeletionsController(createAPIClient(async input => {
    if ((input as Request).url.endsWith('/deletions')) {
      if (++lists === 1) return Response.json({ manifests: [batch('b1', 'pending')] });
      return new Promise(resolve => finish = resolve);
    }
    return Response.json(detail('b1', 'pending'));
  }));
  const view = render(DeletionsWorkspace, { controller });
  expect(screen.getByText('Loading…')).toBeDefined();
  await fireEvent.click(await screen.findByRole('button', { name: /^Open batch from/ }));
  await screen.findByText('reviewed explicit selection');
  const table = screen.getByRole('table', { name: 'Staged deletions' });
  const refresh = controller.refresh();
  await waitFor(() => expect(lists).toBe(2));
  expect(screen.getByRole('table', { name: 'Staged deletions' })).toBe(table);
  expect(screen.queryByText('Loading…')).toBeNull();
  expect(screen.getByText('reviewed explicit selection')).toBeDefined();
  finish!(Response.json({ manifests: [batch('b1', 'pending')] }));
  await refresh;
  view.unmount(); controller.destroy();
});
