import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';
import { createAPIClient } from '../../api/client';
import AddToTaskDialog from './AddToTaskDialog.svelte';

const metadata = { archive_uid: 'archive-a', message_id: 42, conversation_id: 7, source_type: 'gmail', source_identifier: 'archive@example.com', source_message_id: 'source-42', subject: 'Follow up', from: 'sender@example.com', sent_at: '2026-07-18T12:00:00Z' };

describe('Add to task', () => {
  it.each(['create', 'cancel'])('closes its single dialog and reports only completed changes after %s', async (mode) => {
    const onclose = vi.fn();
    const onchanged = vi.fn();
    const client = createAPIClient(async (input) => {
      const request = input as Request;
      const path = new URL(request.url).pathname;
      if (request.method === 'POST') return Response.json({ task: { id: 'task-1', title: 'Follow up' } }, { status: 201 });
      return Response.json(path.endsWith('/status') ? { state: 'ready', project: 'example' } : { state: 'ready', complete: true, tasks: [], outbound_metadata: metadata });
    });
    const view = render(AddToTaskDialog, { props: { client, messageId: 42, title: 'Follow up', email: true, onclose, onchanged } });
    try {
      await screen.findByRole('button', { name: 'Cancel' });
      expect(screen.getAllByRole('dialog')).toHaveLength(1);
      await fireEvent.click(screen.getByRole('button', { name: mode === 'cancel' ? 'Cancel' : 'Create task' }));
      await waitFor(() => expect(onclose).toHaveBeenCalledOnce());
      expect(onchanged).not.toHaveBeenCalled();
      view.unmount();
      expect(onchanged).toHaveBeenCalledTimes(mode === 'cancel' ? 0 : 1);
    } finally { view.unmount(); }
  });
});
