import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../../api/client';
import CreateTaskDialog from './CreateTaskDialog.svelte';

describe('CreateTaskDialog', () => {
  it('fixes the project, sends task fields, and discloses each outbound metadata value', async () => {
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      requests.push(input instanceof Request ? input : new Request(input));
      return Response.json({ task: { id: 'task-1', project: 'project', title: 'Edited', revision: 'r1' } }, { status: 201 });
    });
    const oncreated = vi.fn();
    render(CreateTaskDialog, {
      client: createAPIClient(fetchFn), messageId: 42, project: 'project', defaultTitle: 'Synthetic subject',
      archiveUID: 'archive-a', conversationId: 7, sourceType: 'gmail', sourceIdentifier: 'archive@example.com',
      sourceMessageId: 'source-42', subject: 'Synthetic subject', from: 'sender@example.com', sentAt: '2026-07-18T12:00:00Z',
      oncreated, onclose: vi.fn()
    });

    expect(screen.getByText('project')).toBeDefined();
    for (const value of ['archive-a', '42', '7', 'gmail', 'archive@example.com', 'source-42', 'Synthetic subject', 'sender@example.com', '2026-07-18T12:00:00Z']) {
      expect(screen.getByText(value)).toBeDefined();
    }
    expect(screen.getByText(/Bodies and attachments are never sent/i)).toBeDefined();
    await fireEvent.input(screen.getByLabelText('Task title'), { target: { value: 'Edited' } });
    await fireEvent.input(screen.getByLabelText('Description'), { target: { value: 'Notes' } });
    await fireEvent.change(screen.getByLabelText('Priority'), { target: { value: 'high' } });
    await fireEvent.input(screen.getByLabelText('Labels'), { target: { value: 'mail, follow-up' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Create task' }));
    await waitFor(() => expect(oncreated).toHaveBeenCalledOnce());
    await expect(requests[0]!.clone().json()).resolves.toMatchObject({
      title: 'Edited', description: 'Notes', priority: 'high', labels: ['mail', 'follow-up']
    });
    const body = await requests[0]!.clone().json() as { added_at?: string };
    expect(body.added_at).toMatch(/^\d{4}-\d{2}-\d{2}T/);
    expect(requests[0]!.headers.get('X-Request-Id')).toBeTruthy();
  });

  it('keeps the request ID and added_at stable across a failed browser retry', async () => {
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
    await fireEvent.click(screen.getByRole('button', { name: 'Create task' }));
    await waitFor(() => expect(requests).toHaveLength(2));
    expect(requests[0]!.headers.get('X-Request-Id')).toBe(requests[1]!.headers.get('X-Request-Id'));
    await expect(requests[0]!.clone().text()).resolves.toBe(await requests[1]!.clone().text());
  });

  it.each([
    ['title', async () => fireEvent.input(screen.getByLabelText('Task title'), { target: { value: 'Changed title' } })],
    ['description', async () => fireEvent.input(screen.getByLabelText('Description'), { target: { value: 'Changed notes' } })],
    ['priority', async () => fireEvent.change(screen.getByLabelText('Priority'), { target: { value: 'high' } })],
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
