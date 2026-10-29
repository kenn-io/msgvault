import { render, screen, waitFor } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';
import { createAPIClient } from '../../api/client';
import { KataReadiness, kataReadinessKey } from '../../kata/kata-ready.svelte';
import MessageTasks from './MessageTasks.svelte';

describe('Message task list', () => {
  it('reloads persisted links after refresh', async () => {
    let title = 'Review notes';
    const client = createAPIClient(async (input) => {
      const request = input as Request;
      return Response.json(request.url.endsWith('/status') ? { state: 'ready' } : { state: 'ready', complete: true, tasks: [{ id: 'task-1', title }] });
    });
    const view = render(MessageTasks, { client, messageId: 42, title: 'Synthetic', email: true });
    expect(await screen.findByText('Review notes')).toBeDefined();
    title = 'Review revised notes';
    await view.rerender({ refresh: 1 });
    expect(await screen.findByText('Review revised notes')).toBeDefined();
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
