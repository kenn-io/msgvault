import { taskCreationClient, taskOutboundMetadata } from './fixtures.test-support';
import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';
import { createAPIClient } from '../../api/client';
import { KataReadiness, kataReadinessKey } from '../../kata/kata-ready.svelte';
import AddToTaskDialog from './AddToTaskDialog.svelte';

function pendingCreate() {
  let respond!: (response: Response) => void;
  const response = new Promise<Response>(resolve => { respond = resolve; });
  const onclose = vi.fn();
  const client = createAPIClient(async (input) => {
    const request = input as Request;
    if (request.method === 'POST') return response;
    return Response.json(request.url.endsWith('/status') ? { state: 'ready', project: 'example' } : { state: 'ready', complete: true, tasks: [], outbound_metadata: taskOutboundMetadata });
  });
  render(AddToTaskDialog, { client, messageId: 42, title: 'Follow up', email: true, onclose });
  return { respond, onclose };
}

describe('Add to task', () => {
  it.each([false, true])('opens Kata evidence for email=%s when Kata is ready', async (email) => {
    const client = createAPIClient(async (input) => Response.json((input as Request).url.endsWith('/status')
      ? { state: 'ready', project: 'example' }
      : { evidence: [] }));
    const readiness = new KataReadiness(client);
    let view: ReturnType<typeof render> | undefined;
    try {
      await waitFor(() => expect(readiness.ready).toBe(true));
      view = render(AddToTaskDialog, {
        props: { client, messageId: 42, title: 'Follow up', email, onclose: vi.fn() },
        context: new Map([[kataReadinessKey, readiness]])
      });
      if (email) {
        await fireEvent.click(await screen.findByRole('radio', { name: 'Kata' }));
      }
      expect(await screen.findByRole('textbox', { name: 'Issue title' })).toHaveProperty('value', 'Follow up');
    } finally { view?.unmount(); readiness.dispose(); }
  });

  it('keeps chat Kata unavailable until readiness succeeds', async () => {
    const client = createAPIClient(async () => Response.json({ state: 'disabled' }));
    const readiness = new KataReadiness(client);
    const view = render(AddToTaskDialog, {
      props: { client, messageId: 42, title: 'Chat', email: false, onclose: vi.fn() },
      context: new Map([[kataReadinessKey, readiness]])
    });
    try {
      await waitFor(() => expect(readiness.absent).toBe(true));
      expect(screen.queryByRole('textbox', { name: 'Issue title' })).toBeNull();
    } finally { view.unmount(); readiness.dispose(); }
  });

  it('guards the outer dialog while Kata saves', async () => {
    let finish!: (response: Response) => void;
    const pending = new Promise<Response>(resolve => { finish = resolve; });
    const reference = { version: 1, kind: 'message', archive_uid: 'archive-example', message_id: 42, source_type: 'chat', source_identifier: 'example', source_message_id: '42', message: { body_sha256: 'a'.repeat(64), start_rune: 0, end_rune: 5 } };
    const client = createAPIClient(async input => {
      const path = new URL((input as Request).url).pathname;
      if (path.endsWith('/status')) return Response.json({ state: 'ready' });
      if (path.endsWith('/prepare')) return Response.json({ evidence: [{ id: 'one', passage: 'one', reference, excerpt: 'Quote', display: {}, content_trust: 'untrusted' }] });
      return pending;
    });
    const readiness = new KataReadiness(client);
    await waitFor(() => expect(readiness.ready).toBe(true));
    const onclose = vi.fn();
    const view = render(AddToTaskDialog, { props: { client, messageId: 42, title: 'Chat', email: false, onclose }, context: new Map([[kataReadinessKey, readiness]]) });
    try {
      await screen.findByText('Quote');
      await fireEvent.click(screen.getByRole('button', { name: 'Create issue' }));
      await fireEvent.click(screen.getByRole('button', { name: 'Close' }));
      await fireEvent.keyDown(window, { key: 'Escape' });
      expect(onclose).not.toHaveBeenCalled();
      finish(Response.json({ issue: { uid: 'one', qualified_ref: 'example#abcd', status: 'open' }, replayed: false }));
      await screen.findByRole('button', { name: 'Done' });
      await fireEvent.click(screen.getByRole('button', { name: 'Close' }));
      expect(onclose).toHaveBeenCalledOnce();
    } finally { view.unmount(); readiness.dispose(); }
  });

  it('preserves a failed Tasks draft across provider and mode changes', async () => {
    const client = createAPIClient(async (input) => {
      const request = input as Request;
      if (request.method === 'POST') {
        return Response.json({ message: 'Response lost after create' }, { status: 503 });
      }
      return Response.json(request.url.endsWith('/status') ? { state: 'ready', project: 'example' }
        : { state: 'ready', complete: true, tasks: [], outbound_metadata: taskOutboundMetadata });
    });
    const readiness = new KataReadiness(client);
    let view: ReturnType<typeof render> | undefined;
    try {
      await waitFor(() => expect(readiness.ready).toBe(true));
      view = render(AddToTaskDialog, {
        props: { client, messageId: 42, title: 'Follow up', email: true, onclose: vi.fn() },
        context: new Map([[kataReadinessKey, readiness]])
      });
      await fireEvent.input(await screen.findByRole('textbox', { name: 'Task title' }), { target: { value: 'Revised follow up' } });
      await fireEvent.input(screen.getByLabelText('Description'), { target: { value: 'Draft notes' } });
      await fireEvent.click(screen.getByRole('button', { name: 'Create task' }));
      await screen.findByText('Response lost after create');
      await fireEvent.click(screen.getByRole('radio', { name: 'Existing' }));
      expect(screen.queryByRole('textbox', { name: 'Task title' })).toBeNull();
      await fireEvent.click(screen.getByRole('radio', { name: 'New' }));
      expect(screen.getByRole('textbox', { name: 'Task title' })).toHaveProperty('value', 'Revised follow up');
      expect(screen.getByLabelText('Description')).toHaveProperty('value', 'Draft notes');
      await fireEvent.click(screen.getByRole('radio', { name: 'Kata' }));
      expect(screen.queryByRole('textbox', { name: 'Task title' })).toBeNull();
      expect(screen.getAllByRole('dialog')).toHaveLength(1);
      await fireEvent.input(screen.getByLabelText('Issue title'), { target: { value: 'Kata draft' } });
      await fireEvent.input(screen.getByLabelText('Brief'), { target: { value: 'Brief draft' } });
      await fireEvent.click(screen.getByRole('radio', { name: 'Tasks' }));
      await fireEvent.click(screen.getByRole('radio', { name: 'Kata' }));
      await fireEvent.click(screen.getByRole('radio', { name: 'Existing' }));
      await fireEvent.click(screen.getByRole('radio', { name: 'New' }));
      expect(screen.getByLabelText('Issue title')).toHaveProperty('value', 'Kata draft');
      expect(screen.getByLabelText('Brief')).toHaveProperty('value', 'Brief draft');
      await fireEvent.click(screen.getByRole('radio', { name: 'Tasks' }));
      expect(await screen.findByRole('textbox', { name: 'Task title' })).toHaveProperty('value', 'Revised follow up');
      expect(screen.getByLabelText('Description')).toHaveProperty('value', 'Draft notes');
    } finally { view?.unmount(); readiness.dispose(); }
  });

  it.each(['create', 'cancel'])('closes its single dialog after %s', async (mode) => {
    const onclose = vi.fn();
    const client = taskCreationClient();
    const view = render(AddToTaskDialog, { props: { client, messageId: 42, title: 'Follow up', email: true, onclose } });
    try {
      await screen.findByRole('button', { name: 'Cancel' });
      expect(screen.getAllByRole('dialog')).toHaveLength(1);
      await fireEvent.click(screen.getByRole('button', { name: mode === 'cancel' ? 'Cancel' : 'Create task' }));
      await waitFor(() => expect(onclose).toHaveBeenCalledOnce());
    } finally { view.unmount(); }
  });

  it.each(['success', 'failure'])('guards dismissal while busy and releases it after %s', async (result) => {
    const { respond, onclose } = pendingCreate();
    await fireEvent.click(await screen.findByRole('button', { name: 'Create task' }));
    for (const label of ['Task title', 'Description', 'Priority', 'Labels']) {
      expect(screen.getByLabelText(label)).toHaveProperty('disabled', true);
    }
    const close = screen.getByRole('button', { name: 'Close' });
    close.focus();
    await fireEvent.keyDown(close, { key: 'Escape' });
    await fireEvent.pointerDown(document.querySelector('.kit-modal-overlay')!);
    await fireEvent.click(close);
    expect(onclose).not.toHaveBeenCalled();
    respond(result === 'success'
      ? Response.json({ task: { id: 'task-1', title: 'Follow up' } }, { status: 201 })
      : Response.json({ message: 'Unavailable' }, { status: 503 }));
    if (result === 'failure') {
      await screen.findByRole('alert');
      await fireEvent.keyDown(close, { key: 'Escape' });
      expect(onclose).toHaveBeenCalledOnce();
    } else await waitFor(() => expect(onclose).toHaveBeenCalledOnce());
  });
});
