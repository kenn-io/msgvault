import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../../api/client';
import { formatShortDate } from '../../util/format';
import { chooseSelectOption } from '../../../test/kit-ui';
import CreateTaskDialog from './CreateTaskDialog.svelte';

describe('CreateTaskDialog', () => {
  it('sends task fields and discloses metadata', async () => {
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      requests.push(input instanceof Request ? input : new Request(input));
      return Response.json({ task: { id: 'task-1', project: 'project', title: 'Edited', revision: 'r1' } }, { status: 201 });
    });
    const oncreated = vi.fn();
    const sentAt = '2026-07-18T12:00:37Z';
    render(CreateTaskDialog, {
      client: createAPIClient(fetchFn), messageId: 42, project: 'project', defaultTitle: 'Synthetic subject',
      archiveUID: 'archive-a', conversationId: 7, sourceType: 'gmail', sourceIdentifier: 'archive@example.com',
      sourceMessageId: 'source-42', subject: 'Synthetic subject', from: 'sender@example.com', sentAt,
      oncreated, onclose: vi.fn()
    });

    expect(screen.getByText('project')).toBeDefined();
    for (const [label, value] of [
      ['Message URL', `${location.origin}/messages/42`], ['Archive UID', 'archive-a'], ['Message ID', '42'],
      ['Conversation ID', '7'], ['Source type', 'gmail'], ['Source identifier', 'archive@example.com'],
      ['Source message ID', 'source-42'], ['Subject snapshot', 'Synthetic subject'], ['Sender snapshot', 'sender@example.com'],
    ]) {
      expect(screen.getByText(label).nextElementSibling?.textContent).toBe(value);
    }
    expect(screen.getByText('Sent at').nextElementSibling?.textContent).toBe('2026-07-18T12:00:37Z');
    expect(screen.getByLabelText('Description')).toHaveProperty('value', '');
    expect(screen.getByText(`Synthetic subject · sender@example.com · ${formatShortDate(sentAt)}`)).toBeDefined();
    expect(screen.getByText(/Bodies and attachments are never sent/i).closest('details')).toBeNull();
    await fireEvent.input(screen.getByLabelText('Task title'), { target: { value: 'Edited' } });
    await fireEvent.input(screen.getByLabelText('Description'), { target: { value: 'Notes' } });
    await chooseSelectOption(screen.getByLabelText('Priority'), 'High');
    await fireEvent.input(screen.getByLabelText('Labels'), { target: { value: 'mail, follow-up' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Create task' }));
    await waitFor(() => expect(oncreated).toHaveBeenCalledOnce());
    await expect(requests[0]!.clone().json()).resolves.toMatchObject({
      title: 'Edited', description: `Notes\n\n${location.origin}/messages/42`, priority: 'high', labels: ['mail', 'follow-up']
    });
    const body = await requests[0]!.clone().json() as { added_at?: string };
    expect(screen.getByText('Link added at').nextElementSibling?.textContent).toBe(body.added_at);
    expect(requests[0]!.headers.get('X-Request-Id')).toBeTruthy();
  });

  it('keeps the request ID and added_at stable across a failed browser retry', async () => {
    const requests: Request[] = [];
    let fail!: (response: Response) => void;
    const pending = new Promise<Response>(resolve => { fail = resolve; });
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      if (requests.length === 1) return pending;
      return Response.json({ task: { id: 'task-1', project: 'project', title: 'Synthetic', revision: 'r1' } }, { status: 201 });
    });
    render(CreateTaskDialog, {
      client: createAPIClient(fetchFn), messageId: 42, project: 'project', defaultTitle: 'Synthetic subject',
      archiveUID: 'archive-a', conversationId: 7, sourceType: 'gmail', sourceIdentifier: 'archive@example.com',
      sourceMessageId: 'source-42', subject: 'Synthetic subject', from: 'sender@example.com', sentAt: '2026-07-18T12:00:00Z'
    });

    await fireEvent.click(screen.getByRole('button', { name: 'Create task' }));
    expect(screen.getByRole('button', { name: 'Cancel' })).toHaveProperty('disabled', true);
    fail(Response.json({ message: 'Unavailable' }, { status: 503 }));
    expect((await screen.findByRole('alert')).textContent).toContain('Unavailable');
    expect(screen.getByRole('button', { name: 'Cancel' })).toHaveProperty('disabled', false);
    await fireEvent.click(screen.getByRole('button', { name: 'Create task' }));
    await waitFor(() => expect(requests).toHaveLength(2));
    expect(requests[0]!.headers.get('X-Request-Id')).toBe(requests[1]!.headers.get('X-Request-Id'));
    await expect(requests[0]!.clone().text()).resolves.toBe(await requests[1]!.clone().text());
  });

  it.each([
    ['title', async () => fireEvent.input(screen.getByLabelText('Task title'), { target: { value: 'Changed title' } })],
    ['description', async () => fireEvent.input(screen.getByLabelText('Description'), { target: { value: 'Changed notes' } })],
    ['priority', async () => chooseSelectOption(screen.getByLabelText('Priority'), 'High')],
    ['labels', async () => fireEvent.input(screen.getByLabelText('Labels'), { target: { value: 'changed' } })]
  ])('rotates request identity before sending an edited %s retry', async (_field, edit) => {
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      if (requests.length === 1) return Response.json({ message: 'Unavailable' }, { status: 503 });
      return Response.json({ task: { id: 'task-1', project: 'project', title: 'Synthetic', revision: 'r1' } }, { status: 201 });
    });
    render(CreateTaskDialog, {
      client: createAPIClient(fetchFn), messageId: 42, project: 'project', defaultTitle: 'Synthetic subject',
      archiveUID: 'archive-a', conversationId: 7, sourceType: 'gmail', sourceIdentifier: 'archive@example.com',
      sourceMessageId: 'source-42', subject: 'Synthetic subject', from: 'sender@example.com', sentAt: '2026-07-18T12:00:00Z'
    });

    await fireEvent.click(screen.getByRole('button', { name: 'Create task' }));
    await screen.findByRole('alert');
    const first = await requests[0]!.clone().json() as { added_at: string };
    await edit();
    await waitFor(() => expect(screen.queryByText(first.added_at)).toBeNull());
    await fireEvent.click(screen.getByRole('button', { name: 'Create task' }));
    await waitFor(() => expect(requests).toHaveLength(2));
    expect(requests[0]!.headers.get('X-Request-Id')).not.toBe(requests[1]!.headers.get('X-Request-Id'));
    const second = await requests[1]!.clone().json() as { added_at: string };
    expect(second.added_at).not.toBe(first.added_at);
  });
});
