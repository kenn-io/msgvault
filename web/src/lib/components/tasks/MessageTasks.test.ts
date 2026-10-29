import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';
import { createAPIClient } from '../../api/client';
import { KataReadiness, kataReadinessKey } from '../../kata/kata-ready.svelte';
import MessageTasks from './MessageTasks.svelte';

describe('Message task list', () => {
  it('reloads persisted links and unlinks from the concrete message', async () => {
    let linked = true;
    const removed: string[] = [];
    const client = createAPIClient(async (input) => {
      const request = input as Request;
      if (request.method === 'DELETE') { removed.push(new URL(request.url).pathname); linked = false; return Response.json({ task: { id: 'task-1' } }); }
      return Response.json(request.url.endsWith('/status') ? { state: 'ready' } : { state: 'ready', complete: true, tasks: linked ? [{ id: 'task-1', title: 'Review notes' }] : [] });
    });
    const view = render(MessageTasks, { client, messageId: 42, title: 'Synthetic', email: true });
    expect(await screen.findByText('Review notes')).toBeDefined();
    expect(screen.queryByRole('button', { name: 'Create task' })).toBeNull();
    await view.rerender({ refresh: 1 });
    expect(await screen.findByText('Review notes')).toBeDefined();
    await fireEvent.click(screen.getByRole('button', { name: 'Unlink Review notes' }));
    await screen.findByText('No linked tasks.');
    expect(removed).toEqual(['/api/v1/messages/42/tasks/task-1']);
  });

  it('shows linked Kata issues and refreshes them after the task dialog closes', async () => {
    let title = 'Send notes';
    const client = createAPIClient(async (input) => Response.json((input as Request).url.endsWith('/status') ? { state: 'ready' } : { issues: [{ uid: 'issue-1', qualified_ref: 'example#abcd', title, status: 'open', web_url: 'https://example.org/tasks/abcd' }] }));
    const readiness = new KataReadiness(client);
    const view = render(MessageTasks, { props: { client, messageId: 42, title: 'Synthetic' }, context: new Map([[kataReadinessKey, readiness]]) });
    const link = await screen.findByRole('link', { name: 'example#abcd: Send notes' });
    expect(link.getAttribute('href')).toBe('https://example.org/tasks/abcd');
    title = 'Send revised notes';
    await view.rerender({ refresh: 1 });
    await waitFor(() => expect(screen.getByRole('link', { name: 'example#abcd: Send revised notes' })).toBeDefined());
    view.unmount(); readiness.dispose();
  });
});
