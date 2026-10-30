import { fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../../api/client';
import DeletionsWorkspace from './DeletionsWorkspace.svelte';

function batch(id: string, status: string, count: number, created = '2026-07-19T10:00:00Z') {
  return { id, status, created_at: created, created_by: 'api', description: 'reviewed explicit selection', message_count: count };
}

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
    render(DeletionsWorkspace, { client: api([batch('b1', 'pending', 2)]) });
    expect(await screen.findByText('2 messages')).toBeDefined();
    expect(await screen.findByText('archive@example.com')).toBeDefined();
    expect(screen.getByText('Staged')).toBeDefined();
    expect(screen.queryByText('b1')).toBeNull();
    expect(screen.getByText('msgvault delete-staged')).toBeDefined();
    expect(screen.queryByText(/manifest/i)).toBeNull();
  });

  it('keeps cancelled batches behind a filter', async () => {
    render(DeletionsWorkspace, { client: api([batch('b1', 'pending', 2), batch('b2', 'cancelled', 5)]) });
    await screen.findByText('2 messages');
    expect(screen.queryByText('5 messages')).toBeNull();
    await fireEvent.click(screen.getByRole('checkbox', { name: 'Show cancelled (1)' }));
    expect(screen.getByText('5 messages')).toBeDefined();
    expect(screen.getByText('Cancelled')).toBeDefined();
  });

  it('waits for confirmation and immediately sends cancellation', async () => {
    const seen: Seen[] = [], onChanged = vi.fn();
    render(DeletionsWorkspace, { client: api([batch('b1', 'pending', 2)], seen), onChanged });
    await fireEvent.click(await screen.findByRole('button', { name: /^Open batch from/ }));
    await fireEvent.click(await screen.findByRole('button', { name: 'Cancel batch' }));
    expect(seen.filter(entry => entry.method === 'DELETE')).toEqual([]);
    const confirm = await screen.findByRole('dialog', { name: 'Cancel deletion batch?' });
    await fireEvent.click(within(confirm).getByRole('button', { name: 'Cancel batch' }));
    await waitFor(() => expect(seen.filter(entry => entry.method === 'DELETE')).toEqual([{ method: 'DELETE', path: '/api/v1/deletions/b1' }]));
    await waitFor(() => expect(onChanged).toHaveBeenCalledOnce());
    await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Cancel deletion batch?' })).toBeNull());
    expect(screen.queryByRole('button', { name: /^Open batch from/ })).toBeNull();
  });

  it('holds four account reads, then loads later displayed accounts', async () => {
    const batches = Array.from({ length: 6 }, (_, i) => batch(`batch-${i}`, i === 5 ? 'cancelled' : 'pending', i + 1));
    const pending: Array<(response: Response) => void> = [];
    const ids: string[] = [];
    const client = createAPIClient(async input => {
      const path = new URL((input as Request).url).pathname;
      if (path === '/api/v1/deletions') return Response.json({ manifests: batches });
      ids.push(path.split('/').at(-1)!);
      return new Promise<Response>(resolve => pending.push(resolve));
    });
    render(DeletionsWorkspace, { client });
    await waitFor(() => expect(ids).toEqual(['batch-0', 'batch-1', 'batch-2', 'batch-3']));
    pending[0](Response.json({}, { status: 500 }));
    await waitFor(() => expect(ids).toHaveLength(5));
    for (let i = 1; i < 5; i++) pending[i](Response.json(detail(ids[i], 'pending', { account: `${ids[i]}@example.com` })));
    await screen.findByText('batch-4@example.com');
    expect(ids).not.toContain('batch-5');
    await fireEvent.click(screen.getByRole('checkbox', { name: 'Show cancelled (1)' }));
    await waitFor(() => expect(ids).toHaveLength(9));
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
    render(DeletionsWorkspace, { client, onChanged });
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

  it('offers no cancel once a batch is deleted', async () => {
    render(DeletionsWorkspace, {
      client: api([batch('b1', 'completed', 2)], [], { b1: detail('b1', 'completed') })
    });
    await fireEvent.click(await screen.findByRole('button', { name: /^Open batch from/ }));
    await screen.findByText('news@example.com');
    expect(screen.queryByRole('button', { name: 'Cancel batch' })).toBeNull();
  });
});

it('ignores a closed detail response after another batch opens', async () => {
  const waiting: Array<(response: Response) => void> = [];
  let reads = 0;
  const client = createAPIClient(async input => {
    const request = input as Request;
    if (request.url.endsWith('/deletions')) return Response.json({ manifests: [batch('a', 'pending', 2), batch('b', 'pending', 3, '2026-07-20T10:00:00Z')] });
    const id = request.url.split('/').at(-1)!;
    if (++reads <= 2) return reads === 1 ? Response.json({}, { status: 500 }) : Response.json(detail(id, 'pending'));
    return new Promise<Response>(resolve => waiting.push(resolve));
  });
  render(DeletionsWorkspace, { client });
  await waitFor(() => expect(reads).toBe(2));
  const rows = screen.getAllByRole('button', { name: /^Open batch from/ });
  await fireEvent.click(rows[0]);
  await waitFor(() => expect(waiting).toHaveLength(1));
  await fireEvent.click(rows[1]);
  await fireEvent.click(rows[0]);
  waiting[2](Response.json(detail('a', 'pending', { account: 'new@example.com' })));
  await screen.findByText('new@example.com');
  waiting[0](Response.json(detail('a', 'pending', { account: 'stale@example.com' })));
  waiting[1](Response.json(detail('b', 'pending')));
  await new Promise(resolve => setTimeout(resolve, 0));
  expect(screen.queryByText('stale@example.com')).toBeNull();
});
