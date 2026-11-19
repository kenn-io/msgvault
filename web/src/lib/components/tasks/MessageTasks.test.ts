import { taskLookup } from './fixtures.test-support';
import { render, screen, waitFor } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';
import { createAPIClient } from '../../api/client';
import { KataReadiness, kataReadinessKey } from '../../kata/kata-ready.svelte';
import MessageTasks from './MessageTasks.svelte';

describe('Message task list', () => {
  it.each(['partial', 'suppressed'])('sets Tasks group visibility for %s', async scenario => {
    const fetchFn = vi.fn<typeof fetch>(async input => {
      if ((input as Request).url.endsWith('/status')) return Response.json({ state: scenario === 'suppressed' ? 'unavailable' : 'ready', project: 'example' });
      return Response.json(taskLookup(scenario === 'partial'
        ? { state: 'partial', complete: false }
        : { state: 'unavailable', complete: false }));
    });
    render(MessageTasks, { client: createAPIClient(fetchFn), messageId: 42, title: 'Synthetic', email: true, showSetup: scenario !== 'suppressed' });
    if (scenario === 'partial') {
      expect(await screen.findByRole('region', { name: 'Tasks' })).toBeDefined();
    } else {
      await waitFor(() => expect(screen.queryByRole('status', { hidden: true })).toBeNull());
      expect(screen.queryByRole('region', { name: 'Tasks' })).toBeNull();
    }
  });

  it('reloads persisted email links and skips task lookup for non-email messages', async () => {
    const requests: Request[] = [];
    let title = 'Review notes';
    const client = createAPIClient(async (input) => {
      const request = input as Request;
      requests.push(request);
      return Response.json(request.url.endsWith('/status') ? { state: 'ready' } : taskLookup({ tasks: [{ id: 'task-1', title }] }));
    });
    const view = render(MessageTasks, { client, messageId: 42, title: 'Synthetic', email: true });
    expect(await screen.findByText('Review notes')).toBeDefined();
    title = 'Review revised notes';
    await view.rerender({ refresh: 1 });
    expect(await screen.findByText('Review revised notes')).toBeDefined();
    requests.length = 0;
    await view.rerender({ email: false, messageId: 43 });
    expect(requests).toHaveLength(0);
  });

  it.each(['refresh', 'message change'])('keeps or clears Kata rows on %s', async change => {
    let held: ((response: Response) => void) | undefined;
    let hold = false;
    const client = createAPIClient(async input => {
      const url = (input as Request).url;
      if (url.endsWith('/status')) return Response.json({ state: 'ready' });
      if (!url.includes('/kata/')) return Response.json(taskLookup({ tasks: [{ id: 'task-1', title: 'Task work' }] }, url.includes('/messages/43/') ? 43 : 42));
      if (hold) return new Promise<Response>(resolve => { held = resolve; });
      return Response.json({ truncated: false, issues: [{ uid: 'issue-1', qualified_ref: 'example#abcd', title: 'Send notes', status: 'open', web_url: 'https://example.org/tasks/abcd' }, { uid: 'issue-2', qualified_ref: 'example#closed', title: 'Closed issue', status: 'closed' }] });
    });
    const readiness = new KataReadiness(client);
    const view = render(MessageTasks, { props: { client, messageId: 42, title: 'Synthetic', email: true }, context: new Map([[kataReadinessKey, readiness]]) });
    try {
      const link = await screen.findByRole('link', { name: 'Open Send notes (example#abcd) in Kata' });
      expect(link.getAttribute('href')).toBe('https://example.org/tasks/abcd');
      await screen.findByRole('button', { name: 'Unlink Task work' });
      expect(screen.getAllByRole('heading', { name: 'Tasks' })).toHaveLength(1);
      expect(screen.getByText('Kata · example#closed · Closed')).toBeDefined();
      hold = true;
      await view.rerender(change === 'refresh' ? { refresh: 1 } : { messageId: 43 });
      await waitFor(() => expect(held).toBeDefined());
      if (change === 'refresh') expect(screen.getByText('Send notes')).toBeDefined();
      else expect(screen.queryByText('Send notes')).toBeNull();
      held!(Response.json({ truncated: false, issues: [{ uid: 'issue-1', qualified_ref: 'example#abcd', title: 'Send revised notes', status: 'open', web_url: 'https://example.org/tasks/abcd' }] }));
      expect(await screen.findByText('Send revised notes')).toBeDefined();
    } finally { view.unmount(); readiness.dispose(); }
  });

  it('updates the group for incomplete, empty, and failed Kata lookups', async () => {
    let truncated = false;
    let empty = false;
    let hold = false;
    let fail = false;
    let held: ((response: Response) => void) | undefined;
    const client = createAPIClient(async input => {
      if ((input as Request).url.endsWith('/status')) return Response.json({ state: 'ready' });
      if (hold) return new Promise<Response>(resolve => { held = resolve; });
      if (fail) return Response.json({ message: 'Kata lookup failed' }, { status: 503 });
      return Response.json({ truncated, issues: empty ? [] : [{ uid: 'issue-1', qualified_ref: 'example#abcd', title: 'Send revised notes', status: 'open', web_url: 'https://example.org/tasks/abcd' }] });
    });
    const readiness = new KataReadiness(client);
    const view = render(MessageTasks, { props: { client, messageId: 43, title: 'Synthetic' }, context: new Map([[kataReadinessKey, readiness]]) });
    try {
      await screen.findByText('Send revised notes');
      truncated = true;
      await view.rerender({ refresh: 2 });
      expect(await screen.findByText('Linked Kata issues are incomplete.')).toBeDefined();
      expect(screen.getByRole('link', { name: 'Open Send revised notes (example#abcd) in Kata' })).toBeDefined();
      empty = true;
      await view.rerender({ refresh: 3 });
      await waitFor(() => expect(screen.queryByRole('link')).toBeNull());
      expect(await screen.findByText('Linked Kata issues are incomplete.')).toBeDefined();
      expect(screen.getByRole('region', { name: 'Tasks' })).toBeDefined();
      hold = true;
      await view.rerender({ refresh: 4 });
      await waitFor(() => expect(held).toBeDefined());
      expect(screen.queryByText('Linked Kata issues are incomplete.')).toBeNull();
      held!(Response.json({ issues: [], truncated: false }));
      await waitFor(() => expect(screen.queryByRole('status', { hidden: true })).toBeNull());
      expect(screen.queryByRole('region', { name: 'Tasks' })).toBeNull();
      hold = false;
      fail = true;
      await view.rerender({ refresh: 5 });
      await screen.findByText('Kata lookup failed');
      expect(screen.getByRole('region', { name: 'Tasks' })).toBeDefined();
    } finally { view.unmount(); readiness.dispose(); }
  });
});
