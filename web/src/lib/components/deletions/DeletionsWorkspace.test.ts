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

function api(batches: unknown[], seen: Seen[] = []) {
  return createAPIClient(vi.fn<typeof fetch>(async (input) => {
    const request = input instanceof Request ? input : new Request(input);
    const path = new URL(request.url).pathname;
    seen.push({ method: request.method, path });
    if (path === '/api/v1/deletions') return Response.json({ manifests: batches });
    const id = path.split('/').at(-1)!;
    return Response.json(detail(id, 'pending'));
  }));
}

afterEach(() => {
  vi.useRealTimers();
  document.body.replaceChildren();
});

describe('DeletionsWorkspace', () => {
  it('shows an alert and retry without an empty state when the first list load fails', async () => {
    const client = createAPIClient(async () => Response.json({ message: 'Offline' }, { status: 500 }));
    const controller = new DeletionsController(client);
    const view = render(DeletionsWorkspace, { client, controller });
    try {
      const alert = await screen.findByRole('alert');
      expect(within(alert).getByText("Can't load staged deletions.")).toBeDefined();
      expect(within(alert).getByRole('button', { name: 'Retry' })).toBeDefined();
      expect(screen.queryByText('Nothing staged')).toBeNull();
    } finally { view.unmount(); controller.destroy(); }
  });

  it('lists staged batches and shows cancelled history without detail reads', async () => {
    const seen: Seen[] = [];
    const client = api([
      { ...batch('b1', 'pending', 2), account: 'work@example.com' },
      { ...batch('hidden', 'cancelled', 3), account: 'personal@example.com' },
    ], seen);
    render(DeletionsWorkspace, { client, controller: new DeletionsController(client) });
    expect(await screen.findByRole('cell', { name: '2' })).toBeDefined();
    expect(screen.getByText('Staged')).toBeDefined();
    expect(screen.queryByText('b1')).toBeNull();
    expect(screen.getByText('msgvault delete-staged')).toBeDefined();
    expect(screen.queryByText(/manifest/i)).toBeNull();
    expect(screen.getAllByRole('button', { name: /^Open batch from/ })).toHaveLength(1);
    expect(screen.queryByRole('cell', { name: '3' })).toBeNull();
    await fireEvent.click(screen.getByRole('checkbox', { name: 'Show cancelled (1)' }));
    expect(screen.getByRole('cell', { name: '3' })).toBeDefined();
    expect(screen.getByText('Cancelled')).toBeDefined();
    expect(screen.getAllByRole('button', { name: /^Open batch from/ })).toHaveLength(2);
    const table = screen.getByRole('table', { name: 'Staged deletions' });
    const rows = within(table).getAllByRole('row').slice(1);
    expect(within(rows[0]).getByText('work@example.com')).toBeDefined();
    expect(within(rows[1]).getByText('personal@example.com')).toBeDefined();
    expect(seen).toEqual([{ method: 'GET', path: '/api/v1/deletions' }]);
  });

  it('shows a detail error in the panel and retries to show the account and counts', async () => {
    let reads = 0;
    const client = createAPIClient(async input => {
      const request = input as Request;
      if (request.url.endsWith('/deletions')) return Response.json({ manifests: [batch('b1', 'pending', 2)] });
      if (request.url.endsWith('/deletions/b1')) return ++reads === 1
        ? Response.json({}, { status: 503 })
        : Response.json(detail('b1', 'failed', { execution: { succeeded: 1, failed: 1, failed_ids: ['message-a'] } }));
      throw new Error(`Unexpected request: ${request.url}`);
    });
    const controller = new DeletionsController(client);
    const view = render(DeletionsWorkspace, { client, controller });
    try {
      await fireEvent.click(await screen.findByRole('button', { name: /^Open batch from/ }));
      const panel = screen.getByRole('dialog', { name: /^Staged / });
      const alert = await within(panel).findByRole('alert');
      expect(within(alert).getByText("Can't load batch details.")).toBeDefined();
      expect(within(panel).queryByText('Account unavailable')).toBeNull();
      expect(within(panel).getByText('Staged')).toBeDefined();
      expect(within(panel).getByRole('button', { name: 'Cancel batch' })).toBeDefined();
      await fireEvent.click(within(alert).getByRole('button', { name: 'Retry' }));
      expect(await within(panel).findByText('archive@example.com')).toBeDefined();
      expect(within(panel).getByText('1 deleted · 1 failed')).toBeDefined();
      expect(within(panel).getByText('message-a')).toBeDefined();
      expect(within(panel).queryByRole('alert')).toBeNull();
      expect(reads).toBe(2);
    } finally { view.unmount(); controller.destroy(); }
  });

  it('shows Account unavailable after loading batch details without an account', async () => {
    const seen: Seen[] = [];
    let finish!: (response: Response) => void;
    const client = createAPIClient(async input => {
      const request = input as Request;
      const path = new URL(request.url).pathname;
      seen.push({ method: request.method, path });
      if (path.endsWith('/deletions')) return Response.json({ manifests: [batch('b1', 'pending', 2)] });
      return new Promise(resolve => { finish = resolve; });
    });
    render(DeletionsWorkspace, { client, controller: new DeletionsController(client) });
    await fireEvent.click(await screen.findByRole('button', { name: /^Open batch from/ }));
    const panel = screen.getByRole('dialog', { name: /^Staged / });
    expect(within(panel).queryByText('Account unavailable')).toBeNull();
    expect(within(panel).getByText('Loading…')).toBeDefined();
    const { account, ...withoutAccount } = detail('b1', 'completed');
    finish(Response.json(withoutAccount));
    expect(await within(panel).findByText('Account unavailable')).toBeDefined();
    expect(within(panel).queryByText('Loading…')).toBeNull();
    expect(seen).toEqual([{ method: 'GET', path: '/api/v1/deletions' }, { method: 'GET', path: '/api/v1/deletions/b1' }]);
  });

  it('keeps the batch and confirmation after failed cancellation, then retries', async () => {
    let deletes = 0;
    const onChanged = vi.fn();
    const client = createAPIClient(async input => {
      const request = input as Request;
      if (request.url.endsWith('/deletions')) return Response.json({ manifests: [batch('b1', 'in_progress', 2)] });
      if (request.method === 'DELETE') return ++deletes === 1 ? Response.json({ message: 'Busy' }, { status: 500 }) : Response.json({ id: 'b1', status: 'cancelled' });
      return Response.json(detail('b1', deletes >= 2 ? 'cancelled' : 'in_progress'));
    });
    render(DeletionsWorkspace, { client, controller: new DeletionsController(client), onChanged });
    await fireEvent.click(await screen.findByRole('button', { name: /^Open batch from/ }));
    await fireEvent.click(await screen.findByRole('button', { name: 'Cancel batch' }));
    await fireEvent.click(within(screen.getByRole('dialog', { name: 'Cancel deletion batch?' })).getByRole('button', { name: 'Cancel' }));
    expect(screen.queryByRole('dialog', { name: 'Cancel deletion batch?' })).toBeNull();
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

it('keeps the selected batch when another batch-detail lookup finishes late', async () => {
  let finish!: (response: Response) => void;
  const client = createAPIClient(async input => {
    const request = input as Request;
    if (request.url.endsWith('/deletions')) return Response.json({ manifests: [batch('a', 'pending', 2), batch('b', 'pending', 3)] });
    return request.url.endsWith('/a') ? new Promise(resolve => { finish = resolve; }) : Response.json(detail('b', 'pending', { description: 'Batch B review' }));
  });
  const controller = new DeletionsController(client);
  render(DeletionsWorkspace, { client, controller });
  const rows = await screen.findAllByRole('button', { name: /^Open batch from/ });
  await fireEvent.click(rows[0]);
  await fireEvent.click(rows[1]);
  await screen.findByText('Batch B review');
  const response = Response.json({}, { status: 500 });
  const readBody = vi.spyOn(response, 'text');
  finish(response);
  await waitFor(() => expect(readBody).toHaveResolvedWith('{}'));
  await tick();
  expect(screen.getByText('Batch B review')).toBeDefined();
});

it('opens failed batch details without offering cancellation', async () => {
  const current = detail('terminal', 'failed', { description: 'Old newsletter cleanup', execution: { failed_ids: ['message-a', 'message-b'], succeeded: 0, failed: 2 } });
  const client = createAPIClient(async input => (input as Request).url.endsWith('/deletions')
    ? Response.json({ manifests: [batch('terminal', 'failed', 2)] })
    : Response.json(current));
  render(DeletionsWorkspace, { client, controller: new DeletionsController(client) });
  await fireEvent.click(await screen.findByRole('button', { name: /^Open batch from/ }));
  await screen.findByText('Old newsletter cleanup');
  expect(screen.queryByRole('button', { name: 'Cancel batch' })).toBeNull();
  expect(screen.getByText('message-a')).toBeDefined();
  expect(screen.getByText('message-b')).toBeDefined();
});

it('keeps the table and open detail visible during background list reads', async () => {
  let lists = 0, finish: (response: Response) => void;
  const client = createAPIClient(async input => {
    if ((input as Request).url.endsWith('/deletions')) {
      if (++lists === 1) return Response.json({ manifests: [batch('b1', 'pending')] });
      return new Promise(resolve => finish = resolve);
    }
    return Response.json(detail('b1', 'pending'));
  });
  const controller = new DeletionsController(client);
  const view = render(DeletionsWorkspace, { client, controller });
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

it('refreshes execution counts when the open batch completes', async () => {
  let reads = 0, status = 'in_progress';
  const client = createAPIClient(async input => {
    if ((input as Request).url.endsWith('/deletions')) return Response.json({ manifests: [batch('active', status)] });
    reads++;
    return Response.json(detail('active', status, { execution: { succeeded: status === 'completed' ? 2 : 1, failed: 0 } }));
  });
  const controller = new DeletionsController(client);
  const view = render(DeletionsWorkspace, { client, controller });
  try {
    await fireEvent.click(await screen.findByRole('button', { name: /^Open batch from/ }));
    await screen.findByText('1 deleted · 0 failed');
    await controller.refresh();
    await tick();
    const panel = screen.getByRole('dialog', { name: /^Staged / });
    expect(within(panel).getByText('1 deleted · 0 failed')).toBeDefined();
    expect(within(panel).getByRole('button', { name: 'Cancel batch' })).toBeDefined();
    expect(reads).toBe(1);
    status = 'completed';
    await controller.refresh();
    await tick();
    expect(within(panel).getByText('Deleted')).toBeDefined();
    expect(within(panel).queryByRole('button', { name: 'Cancel batch' })).toBeNull();
    expect(await within(panel).findByText('2 deleted · 0 failed')).toBeDefined();
    expect(reads).toBe(2);
  } finally { view.unmount(); controller.destroy(); }
});

it('reads fresh progress when a batch is explicitly reopened', async () => {
  let succeeded = 0, reads = 0;
  const client = createAPIClient(async input => {
    if ((input as Request).url.endsWith('/deletions')) return Response.json({ manifests: [batch('active', 'in_progress')] });
    reads++;
    return Response.json(detail('active', succeeded ? 'completed' : 'in_progress', { execution: { succeeded, failed: 0 } }));
  });
  const controller = new DeletionsController(client);
  const view = render(DeletionsWorkspace, { client, controller });
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
  } finally { view.unmount(); controller.destroy(); }
});
