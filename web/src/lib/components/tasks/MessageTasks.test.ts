import { taskLookup } from './fixtures.test-support';
import { render, screen, waitFor } from '@testing-library/svelte';
import { describe, expect, it } from 'vitest';
import { createAPIClient } from '../../api/client';
import { KataReadiness, kataReadinessKey } from '../../kata/kata-ready.svelte';
import MessageTasks from './MessageTasks.svelte';

describe('Message task list', () => {
  it('shows the Tasks group for a partial index notice', async () => {
    const client = createAPIClient(async input => Response.json((input as Request).url.endsWith('/status')
      ? { state: 'ready', project: 'example' } : taskLookup({ state: 'partial', complete: false })));
    render(MessageTasks, { client, messageId: 42, title: 'Synthetic', email: true });
    expect(await screen.findByRole('region', { name: 'Tasks' })).toBeDefined();
  });

  it('hides the Tasks group when setup is suppressed', async () => {
    const client = createAPIClient(async input => Response.json((input as Request).url.endsWith('/status')
      ? { state: 'unreachable', project: 'example' } : taskLookup({ state: 'unavailable', complete: false })));
    render(MessageTasks, { client, messageId: 42, title: 'Synthetic', email: true, showSetup: false });
    await waitFor(() => expect(screen.queryByRole('status', { hidden: true })).toBeNull());
    expect(screen.queryByRole('region', { name: 'Tasks' })).toBeNull();
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

  const issues = [{ uid: 'issue-1', qualified_ref: 'example#abcd', title: 'Send revised notes', status: 'open', web_url: 'https://example.org/tasks/abcd' }, { uid: 'issue-2', qualified_ref: 'example#closed', title: 'Closed issue', status: 'closed' }];
  const issueLink = 'Open Send revised notes (example#abcd) in Kata';

  function groupFixture(messageId: number, first = Response.json({ truncated: false, issues })) {
    let response = first;
    let hold = false;
    let resolve: ((response: Response) => void) | undefined;
    const client = createAPIClient(async input => {
      const url = (input as Request).url;
      if (url.endsWith('/status')) return Response.json({ state: 'ready', project: 'example' });
      if (!url.includes('/kata/')) return Response.json(taskLookup({ tasks: url.includes('/messages/42/') ? [{ id: 'task-1', title: 'Task work' }] : [] }, url.includes('/messages/42/') ? 42 : 43));
      if (hold) return new Promise<Response>(done => { resolve = done; });
      return response.clone();
    });
    const readiness = new KataReadiness(client);
    const view = render(MessageTasks, { props: { client, messageId, title: 'Synthetic', email: true }, context: new Map([[kataReadinessKey, readiness]]) });
    return { view, readiness, respond: (next: Response) => { response = next; }, hold: () => { hold = true; }, resolve: () => resolve };
  }

  it('groups task and Kata rows under one heading', async () => {
    const { view, readiness } = groupFixture(42);
    try {
      const link = await screen.findByRole('link', { name: issueLink });
      expect(link.getAttribute('href')).toBe('https://example.org/tasks/abcd');
      expect(screen.getByText('Kata · example#closed · Closed')).toBeDefined();
      await screen.findByText('Task work');
      expect(screen.getAllByRole('heading', { name: 'Tasks' })).toHaveLength(1);
    } finally { view.unmount(); readiness.dispose(); }
  });

  it.each(['refresh', 'message change'])('updates Kata rows on %s', async change => {
    const group = groupFixture(42);
    try {
      await screen.findByRole('link', { name: issueLink });
      group.hold();
      await group.view.rerender(change === 'refresh' ? { refresh: 1 } : { messageId: 43 });
      await waitFor(() => expect(group.resolve()).toBeDefined());
      if (change === 'refresh') expect(screen.getByText('Send revised notes')).toBeDefined();
      else expect(screen.queryByText('Send revised notes')).toBeNull();
      group.resolve()!(Response.json({ truncated: false, issues: change === 'refresh' ? [{ ...issues[0], title: 'Refreshed notes' }] : issues }));
      expect(await screen.findByText(change === 'refresh' ? 'Refreshed notes' : 'Send revised notes')).toBeDefined();
    } finally { group.view.unmount(); group.readiness.dispose(); }
  });

  it.each([
    { name: 'incomplete empty result', truncated: true, empty: true, reload: false },
    { name: 'complete empty result', truncated: false, empty: true, reload: false },
    { name: 'recovery from incomplete result', truncated: true, empty: false, reload: true }
  ])('updates the group for $name', async ({ truncated, empty, reload }) => {
    const group = groupFixture(43);
    try {
      await screen.findByRole('link', { name: issueLink });
      group.respond(Response.json({ truncated, issues: empty ? [] : issues }));
      await group.view.rerender({ refresh: 1 });
      if (truncated) {
        expect(await screen.findByText('Linked Kata issues are incomplete.')).toBeDefined();
        if (!empty) expect(screen.getByRole('link', { name: issueLink })).toBeDefined();
        else {
          await waitFor(() => expect(screen.queryByRole('link')).toBeNull());
          expect(screen.getByRole('region', { name: 'Tasks' })).toBeDefined();
        }
      } else {
        await waitFor(() => expect(screen.queryByRole('region', { name: 'Tasks' })).toBeNull());
      }
      if (reload) {
        group.respond(Response.json({ truncated: false, issues }));
        await group.view.rerender({ refresh: 2 });
        await waitFor(() => expect(screen.queryByText('Linked Kata issues are incomplete.')).toBeNull());
        expect(screen.getByRole('link', { name: issueLink })).toBeDefined();
      }
    } finally { group.view.unmount(); group.readiness.dispose(); }
  });

  it('shows the group for a Kata lookup error', async () => {
    const group = groupFixture(43, Response.json({ message: 'Kata lookup failed' }, { status: 503 }));
    try {
      expect(await screen.findByText('Kata lookup failed')).toBeDefined();
      expect(screen.getByRole('region', { name: 'Tasks' })).toBeDefined();
    } finally { group.view.unmount(); group.readiness.dispose(); }
  });
});
