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
    const panel = screen.getByRole('dialog', { name: /^Staged / });
    expect(within(panel).getByText('Cancelled')).toBeDefined();
    expect(within(panel).queryByRole('button', { name: 'Cancel batch' })).toBeNull();
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

it.each(['completed', 'failed', 'recovers'])('opens %s batch details without offering cancellation', async status => {
  const terminalStatus = status === 'recovers' ? 'completed' : status;
  let available = status !== 'recovers';
  const current = detail('terminal', terminalStatus, { description: 'Old newsletter cleanup', ...(status === 'failed' ? { execution: { failed_ids: ['message-a', 'message-b'], succeeded: 0, failed: 2 } } : {}) });
  const client = createAPIClient(async input => (input as Request).url.endsWith('/deletions')
    ? Response.json({ manifests: [batch('terminal', terminalStatus, 2)] })
    : available ? Response.json(current) : Response.json({}, { status: 503 }));
  render(DeletionsWorkspace, { controller: new DeletionsController(client) });
  await fireEvent.click(await screen.findByRole('button', { name: /^Open batch from/ }));
  if (status === 'recovers') {
    await screen.findByText("Can't open this batch.");
    available = true;
    await fireEvent.click(screen.getByRole('button', { name: /^Open batch from/ }));
  }
  await screen.findByText('Old newsletter cleanup');
  await waitFor(() => expect(screen.queryByText("Can't open this batch.")).toBeNull());
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

it.each(['failed', 'pending'])('retains the open batch through a focus refresh with %s detail reads', async failure => {
  let refreshDetail = false, status = 'in_progress';
  const controller = new DeletionsController(createAPIClient(async input => {
    if ((input as Request).url.endsWith('/deletions')) return Response.json({ manifests: [batch('active', status)] });
    if (refreshDetail) return failure === 'failed' ? Response.json({}, { status: 503 }) : new Promise<Response>(() => undefined);
    return Response.json(detail('active', 'in_progress', { execution: { succeeded: 1, failed: 0 } }));
  }));
  const view = render(DeletionsWorkspace, { controller });
  try {
    await fireEvent.click(await screen.findByRole('button', { name: /^Open batch from/ }));
    await screen.findByText('1 deleted · 0 failed');
    refreshDetail = true;
    await controller.refresh();
    await tick();
    const panel = screen.getByRole('dialog', { name: /^Staged / });
    expect(within(panel).getByText('1 deleted · 0 failed')).toBeDefined();
    expect(within(panel).getByRole('button', { name: 'Cancel batch' })).toBeDefined();
    status = 'completed';
    await controller.refresh();
    await tick();
    expect(within(panel).getByText('Deleted')).toBeDefined();
    expect(within(panel).queryByRole('button', { name: 'Cancel batch' })).toBeNull();
    expect(within(panel).getByText('1 deleted · 0 failed')).toBeDefined();
  } finally { view.unmount(); controller.destroy(); }
});

it('reads fresh progress when a batch is explicitly reopened', async () => {
  let succeeded = 0, reads = 0;
  const controller = new DeletionsController(createAPIClient(async input => {
    if ((input as Request).url.endsWith('/deletions')) return Response.json({ manifests: [batch('active', 'in_progress')] });
    reads++;
    return Response.json(detail('active', succeeded ? 'completed' : 'in_progress', { execution: { succeeded, failed: 0 } }));
  }));
  const view = render(DeletionsWorkspace, { controller });
  try {
    const row = await screen.findByRole('button', { name: /^Open batch from/ });
    await fireEvent.click(row);
    await screen.findByText('0 deleted · 0 failed');
    await fireEvent.click(row);
    expect(screen.queryByRole('dialog', { name: /^Staged / })).toBeNull();
    const beforeReopen = reads;
    succeeded = 2;
    await fireEvent.click(row);
    await screen.findByText('2 deleted · 0 failed');
    expect(reads).toBe(beforeReopen + 1);
    const panel = screen.getByRole('dialog', { name: /^Staged / });
    expect(within(panel).getByText('Deleted')).toBeDefined();
    expect(within(panel).queryByRole('button', { name: 'Cancel batch' })).toBeNull();
    expect(within(screen.getByRole('table', { name: 'Staged deletions' })).getByText('Deleted')).toBeDefined();
  } finally { view.unmount(); controller.destroy(); }
});

it('updates the open panel when a successful focus refresh loads completed results', async () => {
  let completed = false;
  const controller = new DeletionsController(createAPIClient(async input => {
    const status = completed ? 'completed' : 'in_progress';
    if ((input as Request).url.endsWith('/deletions')) return Response.json({ manifests: [batch('active', status)] });
    return Response.json(detail('active', status, { execution: completed ? { succeeded: 1, failed: 1, failed_ids: ['message-2'] } : { succeeded: 0, failed: 0 } }));
  }));
  const view = render(DeletionsWorkspace, { controller });
  try {
    await fireEvent.click(await screen.findByRole('button', { name: /^Open batch from/ }));
    await screen.findByText('0 deleted · 0 failed');
    completed = true;
    await controller.refresh();
    const panel = screen.getByRole('dialog', { name: /^Staged / });
    expect(await within(panel).findByText('1 deleted · 1 failed')).toBeDefined();
    expect(within(panel).getByText('message-2')).toBeDefined();
    expect(within(panel).getByText('Deleted')).toBeDefined();
    expect(within(panel).queryByRole('button', { name: 'Cancel batch' })).toBeNull();
  } finally { view.unmount(); controller.destroy(); }
});
