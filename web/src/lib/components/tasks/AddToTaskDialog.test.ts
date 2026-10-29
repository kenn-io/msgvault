import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { createAPIClient } from '../../api/client';
import { chooseSelectOption } from '../../../test/kit-ui';
import AddToTaskDialog from './AddToTaskDialog.svelte';

const metadata = { archive_uid: 'archive-a', message_id: 42, conversation_id: 7, source_type: 'gmail', source_identifier: 'archive@example.com', source_message_id: 'source-42', subject: 'Follow up', from: 'sender@example.com', sent_at: '2026-07-18T12:00:00Z' };

afterEach(() => vi.unstubAllGlobals());

describe('Add to task', () => {
  it('creates on plain HTTP with priority, labels, disclosure, and safe retries', async () => {
    vi.stubGlobal('crypto', { getRandomValues: globalThis.crypto.getRandomValues.bind(globalThis.crypto) });
    const writes: Request[] = [];
    const onclose = vi.fn();
    const client = createAPIClient(async (input) => {
      const request = input as Request;
      if (request.method === 'POST') {
        writes.push(request.clone());
        return writes.length === 1 ? Response.json({ message: 'Try again' }, { status: 503 }) : Response.json({ task: { id: 'task-1', title: 'Follow up' } }, { status: 201 });
      }
      return Response.json(request.url.endsWith('/status') ? { state: 'ready', project: 'example' } : { state: 'ready', complete: true, tasks: [], outbound_metadata: metadata });
    });
    render(AddToTaskDialog, { client, messageId: 42, title: 'Follow up', email: true, onclose });
    await screen.findByRole('textbox', { name: 'Task title' });
    expect(screen.getAllByRole('dialog')).toHaveLength(1);
    expect(screen.getByText('Metadata leaving the archive')).toBeDefined();
    await chooseSelectOption(screen.getByLabelText('Priority'), 'High');
    await fireEvent.input(screen.getByLabelText('Labels'), { target: { value: 'mail, urgent' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Create task' }));
    await screen.findByText('Try again');
    await fireEvent.click(screen.getByRole('button', { name: 'Create task' }));
    await waitFor(() => expect(onclose).toHaveBeenCalledOnce());
    expect(writes[0]!.headers.get('X-Request-Id')).toBe(writes[1]!.headers.get('X-Request-Id'));
    const first = await writes[0]!.json();
    expect(first).toMatchObject({ title: 'Follow up', priority: 'high', labels: ['mail', 'urgent'] });
    expect(await writes[1]!.json()).toEqual(first);
  });

  it('searches and links an existing task for this message', async () => {
    const writes: Request[] = [];
    const onclose = vi.fn();
    const client = createAPIClient(async (input) => {
      const request = input as Request;
      if (request.method === 'POST') { writes.push(request.clone()); return Response.json({ task: { id: 'task-2', title: 'Review notes' } }, { status: 201 }); }
      if (request.url.includes('/search?')) return Response.json({ tasks: [{ id: 'task-2', title: 'Review notes' }] });
      return Response.json(request.url.endsWith('/status') ? { state: 'ready', project: 'example' } : { state: 'ready', complete: true, tasks: [], outbound_metadata: metadata });
    });
    render(AddToTaskDialog, { client, messageId: 42, title: 'Follow up', email: true, onclose });
    await fireEvent.click(await screen.findByRole('radio', { name: 'Existing' }));
    await fireEvent.input(screen.getByRole('textbox', { name: 'Search tasks' }), { target: { value: 'Review' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Search' }));
    await fireEvent.click(await screen.findByRole('button', { name: 'Link Review notes' }));
    await waitFor(() => expect(onclose).toHaveBeenCalledOnce());
    expect(new URL(writes[0]!.url).pathname).toBe('/api/v1/messages/42/tasks');
    expect(await writes[0]!.json()).toMatchObject({ task_id: 'task-2' });
  });
});
