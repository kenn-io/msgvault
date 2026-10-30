import { render, screen, waitFor } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';
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
      return Response.json(request.url.endsWith('/status') ? { state: 'ready' } : { state: 'ready', complete: true, tasks: [{ id: 'task-1', title }] });
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
    const link = await screen.findByRole('link', { name: 'example#abcd: Send notes' });
    expect(link.getAttribute('href')).toBe('https://example.org/tasks/abcd');
    title = 'Send revised notes';
    await view.rerender({ refresh: 1 });
    await waitFor(() => expect(screen.getByRole('link', { name: 'example#abcd: Send revised notes' })).toBeDefined());
    truncated = true;
    await view.rerender({ refresh: 2 });
    expect(await screen.findByText('Linked Kata issues are incomplete.')).toBeDefined();
    expect(screen.getByRole('link', { name: 'example#abcd: Send revised notes' })).toBeDefined();
    empty = true;
    await view.rerender({ refresh: 3 });
    await waitFor(() => expect(screen.queryByRole('link')).toBeNull());
    expect(await screen.findByText('Linked Kata issues are incomplete.')).toBeDefined();
    expect(screen.queryByText('No linked Kata issues.')).toBeNull();
    truncated = false;
    await view.rerender({ refresh: 4 });
    expect(await screen.findByText('No linked Kata issues.')).toBeDefined();
    expect(screen.queryByText('Linked Kata issues are incomplete.')).toBeNull();
    view.unmount(); readiness.dispose();
  });
});
