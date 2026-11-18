import { taskOutboundMetadata } from './fixtures.test-support';
import { render, screen, waitFor } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';
import { createAPIClient } from '../../api/client';
import { KataReadiness, kataReadinessKey } from '../../kata/kata-ready.svelte';
import MessageTasks from './MessageTasks.svelte';

describe('Message task list', () => {
  it('clears Kata rows for another message', async () => {
    let hold = false;
    let respond: ((response: Response) => void) | undefined;
    const client = createAPIClient(async input => {
      if ((input as Request).url.endsWith('/status')) return Response.json({ state: 'ready', project: 'example' });
      if (hold) return new Promise<Response>(resolve => { respond = resolve; });
      return Response.json({ issues: [{ uid: 'issue-1', qualified_ref: 'example#abcd', title: 'Linked work', status: 'open' }], truncated: false });
    });
    const readiness = new KataReadiness(client);
    const view = render(MessageTasks, { props: { client, messageId: 42, title: 'Synthetic' }, context: new Map([[kataReadinessKey, readiness]]) });
    try {
      await screen.findByText('Linked work');
      hold = true;
      await view.rerender({ messageId: 43 });
      await waitFor(() => expect(respond).toBeDefined());
      expect(screen.queryByRole('heading', { name: 'Tasks' })).toBeNull();
      expect(screen.queryByText('Linked work')).toBeNull();
      respond!(Response.json({ issues: [], truncated: false }));
    } finally { view.unmount(); readiness.dispose(); }
  });

  it('keeps the partial index notice inside the Tasks group', async () => {
    const client = createAPIClient(async input => Response.json((input as Request).url.endsWith('/status')
      ? { state: 'ready', project: 'example' }
      : { state: 'partial', complete: false, tasks: [], outbound_metadata: taskOutboundMetadata }));
    render(MessageTasks, { client, messageId: 42, title: 'Synthetic', email: true });
    expect(await screen.findByText('Some linked tasks may be missing.')).toBeDefined();
    expect(screen.getByRole('region', { name: 'Tasks' }).textContent).toContain('Some linked tasks may be missing.');
  });

  it.each([
    { name: 'session-only index', status: 'ready', lookup: { state: 'ready', reason: 'cache_persistence_unsupported', tasks: [] }, notice: 'Linked-task index updates are available for this session' },
    { name: 'lookup error', status: 'ready', lookup: null, notice: 'Lookup failed' },
    { name: 'unavailable service', status: 'unavailable', lookup: { state: 'unavailable', tasks: [] }, notice: 'Task integration unavailable' },
  ])('shows the $name notice inside the Tasks group', async ({ status, lookup, notice }) => {
    const client = createAPIClient(async input => {
      if ((input as Request).url.endsWith('/status')) return Response.json({ state: status, project: 'example' });
      return lookup ? Response.json({ ...lookup, outbound_metadata: taskOutboundMetadata }) : Response.json({ message: 'Lookup failed' }, { status: 503 });
    });
    render(MessageTasks, { client, messageId: 42, title: 'Synthetic', email: true });
    await waitFor(() => expect(screen.getByRole('region', { name: 'Tasks' }).textContent).toContain(notice));
  });

  it('hides an unavailable service when setup is suppressed', async () => {
    const client = createAPIClient(async input => Response.json((input as Request).url.endsWith('/status')
      ? { state: 'unavailable' }
      : { state: 'unavailable', complete: false, tasks: [], outbound_metadata: taskOutboundMetadata }));
    render(MessageTasks, { client, messageId: 42, title: 'Synthetic', email: true, showSetup: false });
    await waitFor(() => expect(screen.queryByRole('status')).toBeNull());
    expect(screen.queryByRole('region', { name: 'Tasks' })).toBeNull();
  });

  it('groups task and Kata links under one Tasks heading', async () => {
    const client = createAPIClient(async input => {
      const url = (input as Request).url;
      if (url.endsWith('/status')) return Response.json({ state: 'ready', project: 'example' });
      if (url.includes('/kata/')) return Response.json({ issues: [{ uid: 'issue-1', qualified_ref: 'example#abcd', title: 'Kata work', status: 'open', web_url: 'https://example.org/issue' }, { uid: 'issue-2', qualified_ref: 'example#closed', title: 'Closed issue', status: 'closed' }], truncated: false });
      return Response.json({ state: 'ready', complete: true, tasks: [{ id: 'task-1', title: 'Task work' }], outbound_metadata: taskOutboundMetadata });
    });
    const readiness = new KataReadiness(client);
    const view = render(MessageTasks, { props: { client, messageId: 42, title: 'Synthetic', email: true }, context: new Map([[kataReadinessKey, readiness]]) });
    try {
      await screen.findByText('Task work');
      await screen.findByText('Kata work');
      expect(screen.getAllByRole('heading', { name: 'Tasks' })).toHaveLength(1);
      expect(screen.getByText('Kata · example#closed · Closed')).toBeDefined();
    } finally { view.unmount(); readiness.dispose(); }
  });
  it.each(['error', 'incomplete'])('shows the Tasks group for a Kata %s notice without rows', async result => {
    const client = createAPIClient(async input => (input as Request).url.endsWith('/status') ? Response.json({ state: 'ready' }) : result === 'error'
      ? Response.json({ message: 'Kata lookup failed' }, { status: 503 })
      : Response.json({ issues: [], truncated: true }));
    const readiness = new KataReadiness(client);
    const view = render(MessageTasks, { props: { client, messageId: 42, title: 'Synthetic' }, context: new Map([[kataReadinessKey, readiness]]) });
    try {
      await screen.findByText(result === 'error' ? 'Kata lookup failed' : 'Linked Kata issues are incomplete.');
      expect(screen.getByRole('heading', { name: 'Tasks' })).toBeDefined();
    }
    finally { view.unmount(); readiness.dispose(); }
  });

  it('reloads persisted email links and skips task lookup for non-email messages', async () => {
    const requests: Request[] = [];
    let title = 'Review notes';
    const client = createAPIClient(async (input) => {
      const request = input as Request;
      requests.push(request);
      return Response.json(request.url.endsWith('/status') ? { state: 'ready' } : { state: 'ready', complete: true, tasks: [{ id: 'task-1', title }], outbound_metadata: taskOutboundMetadata });
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
      return Response.json({ truncated, issues: empty ? [] : [{ uid: 'issue-1', qualified_ref: 'example#abcd', title, status: 'open', web_url: 'https://example.org/tasks/abcd' }] });
    });
    const readiness = new KataReadiness(client);
    const view = render(MessageTasks, { props: { client, messageId: 42, title: 'Synthetic' }, context: new Map([[kataReadinessKey, readiness]]) });
    const link = await screen.findByRole('link', { name: 'Open Send notes (example#abcd) in Kata' });
    expect(link.getAttribute('href')).toBe('https://example.org/tasks/abcd');
    title = 'Send revised notes';
    hold = true;
    await view.rerender({ refresh: 1 });
    await waitFor(() => expect(held).toBeDefined());
    expect(screen.getByText('Send notes')).toBeDefined();
    hold = false;
    held!(Response.json({ truncated: false, issues: [{ uid: 'issue-1', qualified_ref: 'example#abcd', title, status: 'open', web_url: 'https://example.org/tasks/abcd' }] }));
    held = undefined;
    expect(await screen.findByText('Send revised notes')).toBeDefined();
    truncated = true;
    await view.rerender({ refresh: 2 });
    expect(await screen.findByText('Linked Kata issues are incomplete.')).toBeDefined();
    expect(screen.getByRole('link', { name: 'Open Send revised notes (example#abcd) in Kata' })).toBeDefined();
    empty = true;
    await view.rerender({ refresh: 3 });
    await waitFor(() => expect(screen.queryByRole('link')).toBeNull());
    expect(await screen.findByText('Linked Kata issues are incomplete.')).toBeDefined();
    hold = true;
    await view.rerender({ refresh: 4 });
    await waitFor(() => expect(held).toBeDefined());
    expect(screen.queryByRole('heading', { name: 'Tasks' })).toBeNull();
    held!(Response.json({ issues: [], truncated: false }));
    await waitFor(() => expect(screen.queryByRole('status')).toBeNull());
    expect(screen.queryByText('Linked Kata issues are incomplete.')).toBeNull();
    view.unmount(); readiness.dispose();
  });
});
