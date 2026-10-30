import { taskCreationClient, taskOutboundMetadata } from './fixtures.test-support';
import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';
import { createAPIClient } from '../../api/client';
import { KataReadiness, kataReadinessKey } from '../../kata/kata-ready.svelte';
import { chooseSelectOption } from '../../../test/kit-ui';
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
        await chooseSelectOption(await screen.findByRole('combobox', { name: /^Provider:/ }), 'Kata');
        await fireEvent.click(screen.getByRole('button', { name: 'Add quote to Kata…' }));
      }
      expect(await screen.findByRole('textbox', { name: 'Issue title' })).toHaveProperty('value', 'Follow up');
    } finally { view?.unmount(); readiness.dispose(); }
  });

  it('preserves a failed Tasks draft and retry identity across provider changes', async () => {
    const creates: Request[] = [];
    const client = createAPIClient(async (input) => {
      const request = input as Request;
      if (request.method === 'POST') {
        creates.push(request.clone());
        return creates.length === 1
          ? Response.json({ message: 'Response lost after create' }, { status: 503 })
          : Response.json({ task: { id: 'task-1', title: 'Revised follow up' } }, { status: 201 });
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
      await fireEvent.click(screen.getByRole('button', { name: 'Create task' }));
      await screen.findByText('Response lost after create');
      const provider = screen.getByRole('combobox', { name: /^Provider:/ });
      await chooseSelectOption(provider, 'Kata');
      expect(screen.queryByRole('textbox', { name: 'Task title' })).toBeNull();
      expect(screen.getByRole('button', { name: 'Add quote to Kata…' })).toBeDefined();
      await chooseSelectOption(provider, 'Tasks');
      expect(await screen.findByRole('textbox', { name: 'Task title' })).toHaveProperty('value', 'Revised follow up');
      await fireEvent.click(screen.getByRole('button', { name: 'Create task' }));
      await waitFor(() => expect(creates).toHaveLength(2));
      expect(creates[1].headers.get('X-Request-Id')).toBe(creates[0].headers.get('X-Request-Id'));
      const initial = await creates[0].json();
      expect(initial.added_at).toBeTruthy();
      expect(await creates[1].json()).toEqual(initial);
    } finally { view?.unmount(); readiness.dispose(); }
  });

  it.each(['create', 'cancel'])('closes its single dialog and reports only completed changes after %s', async (mode) => {
    const onclose = vi.fn();
    const onchanged = vi.fn();
    const client = taskCreationClient();
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

  it.each(['success', 'failure'])('guards dismissal while busy and releases it after %s', async (result) => {
    const { respond, onclose } = pendingCreate();
    await fireEvent.click(await screen.findByRole('button', { name: 'Create task' }));
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
