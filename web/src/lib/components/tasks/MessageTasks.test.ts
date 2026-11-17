import { render, screen, waitFor } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';
import { createAPIClient } from '../../api/client';
import { KataReadiness, kataReadinessKey } from '../../kata/kata-ready.svelte';
import MessageTasks from './MessageTasks.svelte';

describe('Message task list', () => {
  it('groups task and Kata links under one Tasks heading', async () => {
    const client = createAPIClient(async input => {
      const url = (input as Request).url;
      if (url.endsWith('/status')) return Response.json({ state: 'ready', project: 'example' });
      if (url.includes('/kata/')) return Response.json({ issues: [{ uid: 'issue-1', qualified_ref: 'example#abcd', title: 'Kata work', status: 'open', web_url: 'https://example.org/issue' }], truncated: false });
      return Response.json({ state: 'ready', complete: true, tasks: [{ id: 'task-1', title: 'Task work' }], outbound_metadata: {} });
    });
    const readiness = new KataReadiness(client);
    const view = render(MessageTasks, { props: { client, messageId: 42, title: 'Synthetic', email: true }, context: new Map([[kataReadinessKey, readiness]]) });
    try {
      await screen.findByText('Task work');
      await screen.findByText('Kata work');
      expect(screen.getAllByRole('heading', { name: 'Tasks' })).toHaveLength(1);
      expect(screen.getByText('Tasks · example')).toBeDefined();
      expect(screen.getByText('Kata · example#abcd · Open')).toBeDefined();
    } finally { view.unmount(); readiness.dispose(); }
  });
  it('shows a closed issue state beside its reference', async () => {
    const client = createAPIClient(async input => Response.json((input as Request).url.endsWith('/status') ? { state: 'ready' } : { issues: [{ uid: 'issue-1', qualified_ref: 'example#abcd', title: 'Closed issue', status: 'closed' }], truncated: false }));
    const readiness = new KataReadiness(client);
    const view = render(MessageTasks, { props: { client, messageId: 42, title: 'Synthetic' }, context: new Map([[kataReadinessKey, readiness]]) });
    try { expect(await screen.findByText('Kata · example#abcd · Closed')).toBeDefined(); }
    finally { view.unmount(); readiness.dispose(); }
  });

  it('reports a failed Kata lookup in the Tasks group', async () => {
    const client = createAPIClient(async input => (input as Request).url.endsWith('/status') ? Response.json({ state: 'ready' }) : Response.json({ message: 'Kata lookup failed' }, { status: 503 }));
    const readiness = new KataReadiness(client);
    const view = render(MessageTasks, { props: { client, messageId: 42, title: 'Synthetic' }, context: new Map([[kataReadinessKey, readiness]]) });
    try { expect((await screen.findByRole('alert')).textContent).toContain('Kata lookup failed'); }
    finally { view.unmount(); readiness.dispose(); }
  });

  it('offers setup when the task integration is disabled', async () => {
    const client = createAPIClient(async input => Response.json((input as Request).url.endsWith('/status') ? { state: 'disabled' } : { state: 'disabled', complete: false, tasks: [], outbound_metadata: {} }));
    render(MessageTasks, { client, messageId: 42, title: 'Synthetic', email: true });
    expect(await screen.findByRole('button', { name: 'Set up' })).toBeDefined();
  });

  it('reloads persisted email links and skips task lookup for non-email messages', async () => {
    const requests: Request[] = [];
    let title = 'Review notes';
    const client = createAPIClient(async (input) => {
      const request = input as Request;
      requests.push(request);
      return Response.json(request.url.endsWith('/status') ? { state: 'ready' } : { state: 'ready', complete: true, tasks: [{ id: 'task-1', title }], outbound_metadata: {} });
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
    const client = createAPIClient(async (input) => Response.json((input as Request).url.endsWith('/status') ? { state: 'ready' } : { truncated, issues: empty ? [] : [{ uid: 'issue-1', qualified_ref: 'example#abcd', title, status: 'open', web_url: 'https://example.org/tasks/abcd' }] }));
    const readiness = new KataReadiness(client);
    const view = render(MessageTasks, { props: { client, messageId: 42, title: 'Synthetic' }, context: new Map([[kataReadinessKey, readiness]]) });
    const link = await screen.findByRole('link', { name: 'Open in Kata' });
    expect(link.getAttribute('href')).toBe('https://example.org/tasks/abcd');
    title = 'Send revised notes';
    await view.rerender({ refresh: 1 });
    await waitFor(() => expect(screen.getByRole('link', { name: 'Open in Kata' })).toBeDefined());
    truncated = true;
    await view.rerender({ refresh: 2 });
    expect(await screen.findByText('Linked Kata issues are incomplete.')).toBeDefined();
    expect(screen.getByRole('link', { name: 'Open in Kata' })).toBeDefined();
    empty = true;
    await view.rerender({ refresh: 3 });
    await waitFor(() => expect(screen.queryByRole('link')).toBeNull());
    expect(await screen.findByText('Linked Kata issues are incomplete.')).toBeDefined();
    expect(screen.queryByText('No linked Kata issues.')).toBeNull();
    truncated = false;
    await view.rerender({ refresh: 4 });
    await waitFor(() => expect(screen.queryByRole('heading', { name: 'Tasks' })).toBeNull());
    expect(screen.queryByText('Linked Kata issues are incomplete.')).toBeNull();
    view.unmount(); readiness.dispose();
  });
});
