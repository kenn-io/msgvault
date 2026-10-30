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

  it('says nothing is staged in one line and still shows the command', async () => {
    render(DeletionsWorkspace, { client: api([]) });
    expect(await screen.findByText('Nothing staged')).toBeDefined();
    expect(screen.getByText('msgvault delete-staged')).toBeDefined();
  });

  it('keeps cancelled batches behind a filter', async () => {
    render(DeletionsWorkspace, { client: api([batch('b1', 'pending', 2), batch('b2', 'cancelled', 5)]) });
    await screen.findByText('2 messages');
    expect(screen.queryByText('5 messages')).toBeNull();
    await fireEvent.click(screen.getByRole('checkbox', { name: 'Show cancelled (1)' }));
    expect(screen.getByText('5 messages')).toBeDefined();
    expect(screen.getByText('Cancelled')).toBeDefined();
  });

  it('opens a panel with the summary and a copyable ID, and the row closes it again', async () => {
    render(DeletionsWorkspace, { client: api([batch('batch-123', 'pending', 2)]) });
    const row = await screen.findByRole('button', { name: /^Open batch from/ });
    await fireEvent.click(row);
    expect(await screen.findByText('news@example.com')).toBeDefined();
    expect(screen.getByText('batch-123')).toBeDefined();
    expect(screen.getByRole('button', { name: 'Copy batch ID' })).toBeDefined();
    expect(screen.getByRole('button', { name: 'Close' })).toBeDefined();
    await fireEvent.click(row);
    await waitFor(() => expect(screen.queryByText('news@example.com')).toBeNull());
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

  it('loads accounts beyond the first twenty batches with four requests at a time', async () => {
    const batches = Array.from({ length: 25 }, (_, i) => batch(`batch-${i}`, 'pending', i + 1));
    let active = 0, peak = 0;
    const client = createAPIClient(async input => {
      const path = new URL(input instanceof Request ? input.url : String(input)).pathname;
      if (path === '/api/v1/deletions') return Response.json({ manifests: batches });
      active++;
      peak = Math.max(peak, active);
      await new Promise<void>(resolve => queueMicrotask(resolve));
      active--;
      const id = path.split('/').at(-1)!;
      return Response.json(detail(id, 'pending', { account: `${id}@example.com` }));
    });
    render(DeletionsWorkspace, { client });
    expect(await screen.findByText('batch-24@example.com')).toBeDefined();
    expect(peak).toBeLessThanOrEqual(4);
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
