import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { createAPIClient } from '../../api/client';
import AddToTaskDialog from './AddToTaskDialog.svelte';

const metadata = { archive_uid: 'archive-a', message_id: 42, conversation_id: 7, source_type: 'gmail', source_identifier: 'archive@example.com', source_message_id: 'source-42', subject: 'Follow up', from: 'sender@example.com', sent_at: '2026-07-18T12:00:00Z' };

afterEach(() => vi.unstubAllGlobals());

describe('Add to task', () => {
  it('creates on plain HTTP and closes its single dialog', async () => {
    vi.stubGlobal('crypto', { getRandomValues: globalThis.crypto.getRandomValues.bind(globalThis.crypto) });
    const onclose = vi.fn();
    const client = createAPIClient(async (input) => {
      const request = input as Request;
      if (request.method === 'POST') {
        return Response.json({ task: { id: 'task-1', title: 'Follow up' } }, { status: 201 });
      }
      return Response.json(request.url.endsWith('/status') ? { state: 'ready', project: 'example' } : { state: 'ready', complete: true, tasks: [], outbound_metadata: metadata });
    });
    render(AddToTaskDialog, { client, messageId: 42, title: 'Follow up', email: true, onclose });
    await screen.findByRole('textbox', { name: 'Task title' });
    expect(screen.getAllByRole('dialog')).toHaveLength(1);
    await fireEvent.click(screen.getByRole('button', { name: 'Create task' }));
    await waitFor(() => expect(onclose).toHaveBeenCalledOnce());
  });
});
