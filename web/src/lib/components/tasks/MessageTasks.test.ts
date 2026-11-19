import { taskLookup } from './fixtures.test-support';
import { render, screen, waitFor } from '@testing-library/svelte';
import { describe, expect, it } from 'vitest';
import { createAPIClient } from '../../api/client';
import { KataReadiness, kataReadinessKey } from '../../kata/kata-ready.svelte';
import MessageTasks from './MessageTasks.svelte';

describe('Message task list', () => {
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

  const issues = [{ uid: 'issue-1', project: 'example', ref: 'abcd', revision: '1', qualified_ref: 'example#abcd', title: 'Send revised notes', status: 'open', web_url: 'https://example.org/tasks/abcd' }, { uid: 'issue-2', project: 'example', ref: 'closed', revision: '1', qualified_ref: 'example#closed', title: 'Closed issue', status: 'closed' }];
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

  it.each([
    { name: 'refresh', props: { refresh: 1 }, pendingVisible: true, nextIssues: [{ ...issues[0], title: 'Refreshed notes' }], nextTitle: 'Refreshed notes' },
    { name: 'message change', props: { messageId: 43 }, pendingVisible: false, nextIssues: issues, nextTitle: 'Send revised notes' }
  ])('updates Kata rows on $name', async ({ props, pendingVisible, nextIssues, nextTitle }) => {
    const group = groupFixture(42);
    try {
      await screen.findByRole('link', { name: issueLink });
      group.hold();
      await group.view.rerender(props);
      await waitFor(() => expect(group.resolve()).toBeDefined());
      expect(screen.queryByText('Send revised notes') !== null).toBe(pendingVisible);
      group.resolve()!(Response.json({ truncated: false, issues: nextIssues }));
      expect(await screen.findByText(nextTitle)).toBeDefined();
    } finally { group.view.unmount(); group.readiness.dispose(); }
  });

  it.each([
    { name: 'partial index notice', integration: 'ready', state: 'partial', showSetup: true, visible: true },
    { name: 'suppressed setup', integration: 'unreachable', state: 'unavailable', showSetup: false, visible: false }
  ])('updates the Tasks group for $name', async ({ integration, state, showSetup, visible }) => {
    const client = createAPIClient(async input => Response.json((input as Request).url.endsWith('/status')
      ? { state: integration, project: 'example' }
      : taskLookup({ state, complete: false })));
    render(MessageTasks, { client, messageId: 42, title: 'Synthetic', email: true, showSetup });
    await waitFor(() => expect(screen.queryByRole('status', { hidden: true })).toBeNull());
    expect(screen.queryByRole('region', { name: 'Tasks' }) !== null).toBe(visible);
  });

  it.each([
    { name: 'incomplete empty result', initial: () => Response.json({ truncated: false, issues }), initialText: 'Send revised notes', steps: [{ truncated: true, rows: [], visible: true, incomplete: true, link: false }] },
    { name: 'complete empty result', initial: () => Response.json({ truncated: false, issues }), initialText: 'Send revised notes', steps: [{ truncated: false, rows: [], visible: false, incomplete: false, link: false }] },
    { name: 'Kata lookup error', initial: () => Response.json({ message: 'Kata lookup failed' }, { status: 503 }), initialText: 'Kata lookup failed', steps: [{ truncated: false, rows: [], visible: false, incomplete: false, link: false }] },
    { name: 'recovery from incomplete result', initial: () => Response.json({ truncated: false, issues }), initialText: 'Send revised notes', steps: [{ truncated: true, rows: issues, visible: true, incomplete: true, link: true }, { truncated: false, rows: issues, visible: true, incomplete: false, link: true }] }
  ])('updates linked Kata visibility for $name', async ({ initial, initialText, steps }) => {
    const group = groupFixture(43, initial());
    try {
      await screen.findByText(initialText);
      for (const [index, step] of steps.entries()) {
        group.respond(Response.json({ truncated: step.truncated, issues: step.rows }));
        await group.view.rerender({ refresh: index + 1 });
        await waitFor(() => {
          expect(screen.queryByText('Loading linked Kata issues…')).toBeNull();
          expect(screen.queryByRole('region', { name: 'Tasks' }) !== null).toBe(step.visible);
          expect(screen.queryByText('Linked Kata issues are incomplete.') !== null).toBe(step.incomplete);
          expect(screen.queryByText('Kata lookup failed')).toBeNull();
          expect(screen.queryByRole('link', { name: issueLink }) !== null).toBe(step.link);
        });
      }
    } finally { group.view.unmount(); group.readiness.dispose(); }
  });
});
