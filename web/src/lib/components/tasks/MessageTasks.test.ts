import { taskLookup } from './fixtures.test-support';
import { render, screen, waitFor } from '@testing-library/svelte';
import { describe, expect, it } from 'vitest';
import { createAPIClient } from '../../api/client';
import { KataReadiness, kataReadinessKey } from '../../kata/kata-ready.svelte';
import MessageTasks from './MessageTasks.svelte';

describe('Message task list', () => {
  it('shows the Tasks group for a partial index notice', async () => {
    const client = createAPIClient(async input => Response.json((input as Request).url.endsWith('/status')
      ? { state: 'ready', project: 'example' }
      : taskLookup({ state: 'partial', complete: false })));
    render(MessageTasks, { client, messageId: 42, title: 'Synthetic', email: true });
    expect(await screen.findByRole('region', { name: 'Tasks' })).toBeDefined();
  });

  it('passes setup suppression to task links', async () => {
    const client = createAPIClient(async input => Response.json((input as Request).url.endsWith('/status')
      ? { state: 'unreachable', project: 'example' }
      : taskLookup({ state: 'unavailable', complete: false })));
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

  it.each(['composition', 'refresh', 'message change', 'truncated', 'truncated empty', 'empty', 'error'])('updates the Tasks group for %s', async scenario => {
    let next = false;
    let held: ((response: Response) => void) | undefined;
    const issues = [{ uid: 'issue-1', qualified_ref: 'example#abcd', title: 'Send revised notes', status: 'open', web_url: 'https://example.org/tasks/abcd' }, { uid: 'issue-2', qualified_ref: 'example#closed', title: 'Closed issue', status: 'closed' }];
    const client = createAPIClient(async input => {
      const url = (input as Request).url;
      if (url.endsWith('/status')) return Response.json({ state: 'ready', project: 'example' });
      if (!url.includes('/kata/')) return Response.json(taskLookup({ tasks: url.includes('/messages/42/') ? [{ id: 'task-1', title: 'Task work' }] : [] }, url.includes('/messages/42/') ? 42 : 43));
      if (!next) return Response.json({ truncated: false, issues });
      if (scenario === 'refresh' || scenario === 'message change' || scenario === 'empty') return new Promise<Response>(resolve => { held = resolve; });
      if (scenario === 'error') return Response.json({ message: 'Kata lookup failed' }, { status: 503 });
      return Response.json({ truncated: true, issues: scenario === 'truncated empty' ? [] : issues });
    });
    const readiness = new KataReadiness(client);
    const view = render(MessageTasks, { props: { client, messageId: scenario === 'composition' || scenario === 'refresh' || scenario === 'message change' ? 42 : 43, title: 'Synthetic', email: true }, context: new Map([[kataReadinessKey, readiness]]) });
    try {
      const link = await screen.findByRole('link', { name: 'Open Send revised notes (example#abcd) in Kata' });
      if (scenario === 'composition') {
        expect(link.getAttribute('href')).toBe('https://example.org/tasks/abcd');
        expect(screen.getByText('Kata · example#closed · Closed')).toBeDefined();
        await screen.findByText('Task work');
        expect(screen.getAllByRole('heading', { name: 'Tasks' })).toHaveLength(1);
        return;
      }
      next = true;
      await view.rerender(scenario === 'message change' ? { messageId: 43 } : { refresh: 1 });
      if (scenario === 'refresh' || scenario === 'message change' || scenario === 'empty') {
        await waitFor(() => expect(held).toBeDefined());
        if (scenario === 'refresh') expect(screen.getByText('Send revised notes')).toBeDefined();
        if (scenario === 'message change') expect(screen.queryByText('Send revised notes')).toBeNull();
        held!(Response.json({ truncated: false, issues: scenario === 'empty' ? [] : scenario === 'refresh' ? [{ ...issues[0], title: 'Refreshed notes' }] : issues }));
        if (scenario === 'empty') {
          await waitFor(() => expect(screen.queryByRole('status', { hidden: true })).toBeNull());
          expect(screen.queryByRole('region', { name: 'Tasks' })).toBeNull();
        } else {
          expect(await screen.findByText(scenario === 'refresh' ? 'Refreshed notes' : 'Send revised notes')).toBeDefined();
        }
      } else if (scenario === 'error') {
        expect(await screen.findByText('Kata lookup failed')).toBeDefined();
        expect(screen.getByRole('region', { name: 'Tasks' })).toBeDefined();
      } else {
        expect(await screen.findByText('Linked Kata issues are incomplete.')).toBeDefined();
        if (scenario === 'truncated') expect(screen.getByRole('link', { name: 'Open Send revised notes (example#abcd) in Kata' })).toBeDefined();
        else await waitFor(() => expect(screen.queryByRole('link')).toBeNull());
        expect(screen.getByRole('region', { name: 'Tasks' })).toBeDefined();
      }
    } finally { view.unmount(); readiness.dispose(); }
  });
});
