import { render, screen, waitFor } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';
import { createAPIClient } from '../../api/client';
import { KataReadiness, kataReadinessKey } from '../../kata/kata-ready.svelte';
import MessageTasks from './MessageTasks.svelte';

describe('Message task list', () => {
  it('hides an unavailable empty group when setup is suppressed', async () => {
    const client = createAPIClient(async input => Response.json((input as Request).url.endsWith('/status')
      ? { state: 'unavailable' }
      : { state: 'unavailable', complete: false, tasks: [], outbound_metadata: {} }));
    render(MessageTasks, { client, messageId: 42, title: 'Synthetic', email: true, showSetup: false });
    await waitFor(() => expect(screen.queryByRole('status', { hidden: true })).toBeNull());
    expect(screen.queryByRole('heading', { name: 'Tasks' })).toBeNull();
  });

  it('shows an outage notice when setup is enabled and retains safe cached rows', async () => {
    const client = createAPIClient(async input => Response.json((input as Request).url.endsWith('/status')
      ? { state: 'unavailable' }
      : { state: 'unavailable', complete: false, tasks: [{ id: 'one', title: 'Cached task' }], outbound_metadata: {} }));
    render(MessageTasks, { client, messageId: 42, title: 'Synthetic', email: true });
    expect(await screen.findByText('Cached task')).toBeDefined();
    expect(screen.getByRole('heading', { name: 'Tasks' })).toBeDefined();
    expect(screen.getByRole('button', { name: 'Open Settings' })).toBeDefined();
  });

  it('keeps the partial-index notice visible without rows', async () => {
    const client = createAPIClient(async input => Response.json((input as Request).url.endsWith('/status')
      ? { state: 'ready' }
      : { state: 'partial', complete: false, tasks: [], outbound_metadata: {} }));
    render(MessageTasks, { client, messageId: 42, title: 'Synthetic', email: true });
    expect(await screen.findByText('Some linked tasks may be missing.')).toBeDefined();
    expect(screen.getByRole('heading', { name: 'Tasks' })).toBeDefined();
  });

  it('keeps a lookup error visible without rows', async () => {
    const client = createAPIClient(async input => (input as Request).url.endsWith('/status')
      ? Response.json({ state: 'ready' }) : Response.json({ message: 'Lookup failed' }, { status: 503 }));
    render(MessageTasks, { client, messageId: 42, title: 'Synthetic', email: true });
    expect(await screen.findByText('Lookup failed')).toBeDefined();
    expect(screen.getByRole('heading', { name: 'Tasks' })).toBeDefined();
  });

  it('shows the session-only cache notice when no tasks are linked', async () => {
    const client = createAPIClient(async input => Response.json((input as Request).url.endsWith('/status')
      ? { state: 'ready', project: 'example' }
      : { state: 'ready', complete: true, reason: 'cache_persistence_unsupported', tasks: [], outbound_metadata: {} }));
    render(MessageTasks, { client, messageId: 42, title: 'Synthetic', email: true });
    expect((await screen.findByText('Linked-task index updates are available for this session but are not persisted to disk on this platform.')).closest('section[hidden]')).toBeNull();
  });

  it('keeps an empty group hidden while task and issue lookups are pending', async () => {
    let resolveTasks!: (response: Response) => void;
    let resolveIssues!: (response: Response) => void;
    const tasks = new Promise<Response>(resolve => { resolveTasks = resolve; });
    const issues = new Promise<Response>(resolve => { resolveIssues = resolve; });
    let tasksStarted = false;
    let issuesStarted = false;
    const client = createAPIClient(async input => {
      const url = (input as Request).url;
      if (url.endsWith('/status')) return Response.json({ state: 'ready' });
      if (url.includes('/kata/')) { issuesStarted = true; return issues; }
      tasksStarted = true; return tasks;
    });
    const readiness = new KataReadiness(client);
    await waitFor(() => expect(readiness.ready).toBe(true));
    const view = render(MessageTasks, { props: { client, messageId: 42, title: 'Synthetic', email: true }, context: new Map([[kataReadinessKey, readiness]]) });
    try {
      await waitFor(() => expect(tasksStarted && issuesStarted).toBe(true));
      expect(screen.queryByRole('heading', { name: 'Tasks' })).toBeNull();
      resolveTasks(Response.json({ state: 'ready', complete: true, tasks: [], outbound_metadata: {} }));
      resolveIssues(Response.json({ issues: [], truncated: false }));
      await waitFor(() => expect(screen.queryByRole('status', { hidden: true })).toBeNull());
      expect(screen.queryByRole('heading', { name: 'Tasks' })).toBeNull();
    } finally { view.unmount(); readiness.dispose(); }
  });
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
