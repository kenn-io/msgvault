import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';
import { createAPIClient } from '../../api/client';
import { KataReadiness, kataReadinessKey } from '../../kata/kata-ready.svelte';
import { chooseSelectOption } from '../../../test/kit-ui';
import AddToTaskDialog from './AddToTaskDialog.svelte';

const metadata = { archive_uid: 'archive-a', message_id: 42, conversation_id: 7, source_type: 'gmail', source_identifier: 'archive@example.com', source_message_id: 'source-42', subject: 'Follow up', from: 'sender@example.com', sent_at: '2026-07-18T12:00:00Z' };

describe('Add to task', () => {
  it.each(['create', 'kata-create', 'cancel'])('closes its single dialog and reports only completed changes after %s', async (mode) => {
    const onclose = vi.fn();
    const onchanged = vi.fn();
    const client = createAPIClient(async (input) => {
      const request = input as Request;
      const path = new URL(request.url).pathname;
      if (path.endsWith('/evidence/prepare')) return Response.json({ evidence: [{ id: 'evidence', excerpt: 'Quoted text', reference: { version: 1, kind: 'message', message_id: 42, message: { start_rune: 0, end_rune: 11, body_sha256: 'a'.repeat(64) } } }] });
      if (request.method === 'POST') return Response.json(path.includes('/kata/') ? { issue: { uid: 'issue-1', qualified_ref: 'example#abcd', status: 'open' }, replayed: false } : { task: { id: 'task-1', title: 'Follow up' } }, { status: 201 });
      return Response.json(path.endsWith('/status') ? { state: 'ready', project: 'example' } : { state: 'ready', complete: true, tasks: [], outbound_metadata: metadata });
    });
    const readiness = new KataReadiness(client);
    const view = render(AddToTaskDialog, { props: { client, messageId: 42, title: 'Follow up', email: true, onclose, onchanged }, context: new Map([[kataReadinessKey, readiness]]) });
    try {
      await screen.findByRole('textbox', { name: 'Task title' });
      expect(screen.getAllByRole('dialog')).toHaveLength(1);
      if (mode === 'kata-create') {
        await waitFor(() => expect(readiness.ready).toBe(true));
        await chooseSelectOption(screen.getByRole('combobox', { name: /^Provider:/ }), 'Kata');
        await fireEvent.click(screen.getByRole('button', { name: 'Add quote to Kata…' }));
        await screen.findByText('Quoted text');
        await fireEvent.click(screen.getByRole('button', { name: 'Create issue' }));
        await screen.findByText(/Saved to/);
        await fireEvent.click(screen.getAllByRole('button', { name: 'Close' }).at(-1)!);
      } else await fireEvent.click(screen.getByRole('button', { name: mode === 'cancel' ? 'Cancel' : 'Create task' }));
      await waitFor(() => expect(onclose).toHaveBeenCalledOnce());
      expect(onchanged).not.toHaveBeenCalled();
      view.unmount();
      expect(onchanged).toHaveBeenCalledTimes(mode === 'cancel' ? 0 : 1);
    } finally { view.unmount(); readiness.dispose(); }
  });
});
