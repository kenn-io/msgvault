import { taskLookup } from './fixtures.test-support';
import { render, screen, waitFor } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';
import { createAPIClient } from '../../api/client';
import { KataReadiness, kataReadinessKey } from '../../kata/kata-ready.svelte';
import MessageTasks from './MessageTasks.svelte';

describe('Message task list', () => {
  it.each(['partial', 'suppressed', 'linked', 'error'])('sets Tasks group visibility for %s', async scenario => {
    const client = createAPIClient(async input => {
      const url = (input as Request).url;
      if (url.endsWith('/status')) return Response.json({ state: scenario === 'suppressed' ? 'unavailable' : 'ready', project: 'example' });
      if (url.includes('/kata/')) return scenario === 'error'
        ? Response.json({ message: 'Kata lookup failed' }, { status: 503 })
        : Response.json({ issues: [{ uid: 'issue-1', qualified_ref: 'example#abcd', title: 'Kata work', status: 'open', web_url: 'https://example.org/issue' }], truncated: false });
      return Response.json(taskLookup(scenario === 'partial'
        ? { state: 'partial', complete: false }
        : scenario === 'suppressed'
          ? { state: 'unavailable', complete: false }
          : { tasks: [{ id: 'task-1', title: 'Task work' }] }));
    });
    const readiness = scenario === 'linked' || scenario === 'error' ? new KataReadiness(client) : undefined;
    const view = render(MessageTasks, {
      props: { client, messageId: 42, title: 'Synthetic', email: scenario !== 'error', showSetup: scenario !== 'suppressed' },
      context: readiness ? new Map([[kataReadinessKey, readiness]]) : new Map(),
    });
    try {
      if (scenario === 'partial') {
        expect(await screen.findByRole('region', { name: 'Tasks' })).toBeDefined();
      } else if (scenario === 'suppressed') {
        await waitFor(() => expect(screen.queryByRole('status')).toBeNull());
        expect(screen.queryByRole('region', { name: 'Tasks' })).toBeNull();
      } else if (scenario === 'linked') {
        await screen.findByText('Task work');
        await screen.findByText('Kata work');
        expect(screen.getAllByRole('heading', { name: 'Tasks' })).toHaveLength(1);
      } else {
        await screen.findByText('Kata lookup failed');
        expect(screen.getByRole('heading', { name: 'Tasks' })).toBeDefined();
      }
    } finally { view.unmount(); readiness?.dispose(); }
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

  it('shows linked Kata issues and refreshes them after the task dialog closes', async () => {
    let title = 'Send notes';
    let truncated = false;
    let empty = false;
    let held: ((response: Response) => void) | undefined;
    let hold = false;
    const client = createAPIClient(async (input) => {
      if ((input as Request).url.endsWith('/status')) return Response.json({ state: 'ready' });
      if (hold) return new Promise<Response>(resolve => { held = resolve; });
      return Response.json({ truncated, issues: empty ? [] : [{ uid: 'issue-1', qualified_ref: 'example#abcd', title, status: 'open', web_url: 'https://example.org/tasks/abcd' }, { uid: 'issue-2', qualified_ref: 'example#closed', title: 'Closed issue', status: 'closed' }] });
    });
    const readiness = new KataReadiness(client);
    const view = render(MessageTasks, { props: { client, messageId: 42, title: 'Synthetic' }, context: new Map([[kataReadinessKey, readiness]]) });
    const link = await screen.findByRole('link', { name: 'Open Send notes (example#abcd) in Kata' });
    expect(link.getAttribute('href')).toBe('https://example.org/tasks/abcd');
    expect(screen.getByText('Kata · example#closed · Closed')).toBeDefined();
    title = 'Send revised notes';
    hold = true;
    await view.rerender({ refresh: 1 });
    await waitFor(() => expect(held).toBeDefined());
    expect(screen.getByText('Send notes')).toBeDefined();
    hold = false;
    held!(Response.json({ truncated: false, issues: [{ uid: 'issue-1', qualified_ref: 'example#abcd', title, status: 'open', web_url: 'https://example.org/tasks/abcd' }] }));
    held = undefined;
    expect(await screen.findByText('Send revised notes')).toBeDefined();
    hold = true;
    await view.rerender({ messageId: 43 });
    await waitFor(() => expect(held).toBeDefined());
    expect(screen.queryByText('Send revised notes')).toBeNull();
    hold = false;
    held!(Response.json({ truncated: false, issues: [{ uid: 'issue-1', qualified_ref: 'example#abcd', title, status: 'open', web_url: 'https://example.org/tasks/abcd' }] }));
    held = undefined;
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
    await waitFor(() => expect(screen.queryByRole('status')).toBeNull());
    expect(screen.queryByText('Linked Kata issues are incomplete.')).toBeNull();
    view.unmount(); readiness.dispose();
  });
});
