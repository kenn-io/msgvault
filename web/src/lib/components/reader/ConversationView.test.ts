import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../../api/client';
import ConversationView from './ConversationView.svelte';

function message(id: number, type = 'email') {
  return {
    id,
    conversation_id: 7,
    subject: `Message ${id}`,
    message_type: type,
    from: 'alice@example.com',
    to: ['bob@example.com'],
    sent_at: `2026-01-0${id}T12:00:00Z`,
    snippet: `Preview ${id}`,
    labels: [],
    has_attachments: false,
    size_bytes: 10,
    body: `Body ${id}`,
    body_html: `<p>Body ${id}</p>`,
    attachments: []
  };
}

describe('ConversationView', () => {
  it('loads the anchored chronological email thread and highlights the selected item', async () => {
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      return Response.json({
        id: 7, anchor_id: 2, messages: [message(1), message(2), message(3)],
        has_before: false, has_after: false, total: 3
      });
    });

    render(ConversationView, {
      props: { client: createAPIClient(fetchFn), conversationId: 7, anchorId: 2, onBack: vi.fn() }
    });

    expect(await screen.findByRole('heading', { name: 'Conversation' })).toBeTruthy();
    expect((await screen.findByRole('article', { name: 'Selected message 2' })).getAttribute('aria-current')).toBe('true');
    expect(screen.getByRole('button', { name: /Open message 1/ })).toBeTruthy();
    const url = new URL(requests[0]!.url);
    expect(url.pathname).toBe('/api/v1/conversations/7');
    expect(url.searchParams.get('anchor')).toBe('2');
  });

  it('sends optional start/end bounds when scoping to a chat burst day', async () => {
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      return Response.json({
        id: 7, anchor_id: 2, messages: [message(2)], has_before: false, has_after: false, total: 1
      });
    });

    render(ConversationView, {
      props: {
        client: createAPIClient(fetchFn), conversationId: 7, anchorId: 2, onBack: vi.fn(),
        start: '2026-07-19T00:00:00.000Z', end: '2026-07-20T00:00:00.000Z'
      }
    });

    await screen.findByRole('article', { name: 'Selected message 2' });
    const url = new URL(requests[0]!.url);
    expect(url.searchParams.get('start')).toBe('2026-07-19T00:00:00.000Z');
    expect(url.searchParams.get('end')).toBe('2026-07-20T00:00:00.000Z');
  });

  it('switches the selected conversation message to plain text', async () => {
    const fetchFn = vi.fn<typeof fetch>(async () => Response.json({
      id: 7, anchor_id: 2, messages: [message(2)], has_before: false, has_after: false, total: 1
    }));
    render(ConversationView, {
      props: { client: createAPIClient(fetchFn), conversationId: 7, anchorId: 2, onBack: vi.fn() }
    });

    await screen.findByRole('article', { name: 'Selected message 2' });
    await fireEvent.click(screen.getByRole('button', { name: 'Text' }));
    expect(screen.getByRole('button', { name: 'Text' }).getAttribute('aria-pressed')).toBe('true');
    expect(screen.getByText('Body 2')).toBeDefined();
  });

  it('drills chat conversation rows to individual messages without losing conversation context', async () => {
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      const anchor = Number(new URL(request.url).searchParams.get('anchor'));
      return Response.json({
        id: 7, anchor_id: anchor, messages: [message(1, 'imessage'), message(2, 'imessage')],
        has_before: false, has_after: false, total: 2
      });
    });
    const client = createAPIClient(fetchFn);
    const onBack = vi.fn();
    let rendered: ReturnType<typeof render>;
    const onAnchorChange = vi.fn((nextAnchor: number) => {
      void rendered.rerender({
        client, conversationId: 7, anchorId: nextAnchor, onBack, onAnchorChange
      });
    });
    rendered = render(ConversationView, {
      props: { client, conversationId: 7, anchorId: 2, onBack, onAnchorChange }
    });

    await fireEvent.click(await screen.findByRole('button', { name: /Open message 1/ }));

    await waitFor(() => expect(onAnchorChange).toHaveBeenCalledWith(1));
    await waitFor(() => expect(requests).toHaveLength(2));
    expect(new URL(requests[1]!.url).searchParams.get('anchor')).toBe('1');
  });

  it('aborts superseded anchors and ignores stale resolve or reject settlements', async () => {
    const pending = new Map<number, {
      request: Request;
      resolve: (response: Response) => void;
      reject: (error: unknown) => void;
    }>();
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const anchor = Number(new URL(request.url).searchParams.get('anchor'));
      if (anchor === 2) return Response.json({
        id: 7, anchor_id: 2, messages: [message(1), message(2), message(3)],
        has_before: false, has_after: false, total: 3
      });
      return await new Promise<Response>((resolve, reject) => {
        pending.set(anchor, { request, resolve, reject });
      });
    });
    const client = createAPIClient(fetchFn);
    const onBack = vi.fn();
    let rendered: ReturnType<typeof render>;
    const onAnchorChange = (nextAnchor: number): void => {
      void rendered.rerender({
        client, conversationId: 7, anchorId: nextAnchor, onBack, onAnchorChange
      });
    };
    rendered = render(ConversationView, {
      props: { client, conversationId: 7, anchorId: 2, onBack, onAnchorChange }
    });
    await screen.findByRole('article', { name: 'Selected message 2' });

    await fireEvent.click(screen.getByRole('button', { name: /Open message 1/ }));
    await waitFor(() => expect(pending.get(1)).toBeDefined());
    await fireEvent.click(screen.getByRole('button', { name: /Open message 3/ }));
    await waitFor(() => expect(pending.get(3)).toBeDefined());

    expect(pending.get(1)?.request.signal.aborted).toBe(true);
    pending.get(3)?.resolve(Response.json({
      id: 7, anchor_id: 3, messages: [message(1), message(2), message(3)],
      has_before: false, has_after: false, total: 3
    }));
    pending.get(1)?.reject(new TypeError('stale network failure'));

    expect(await screen.findByRole('article', { name: 'Selected message 3' })).toBeTruthy();
    expect(screen.queryByRole('alert')).toBeNull();
    expect(fetchFn).toHaveBeenCalledTimes(3);
  });

  it('surfaces a named network failure without an unhandled rejection', async () => {
    const fetchFn = vi.fn<typeof fetch>(async () => {
      throw new TypeError('connection refused');
    });
    render(ConversationView, {
      props: {
        client: createAPIClient(fetchFn), conversationId: 7, anchorId: 2, onBack: vi.fn()
      }
    });

    expect((await screen.findByRole('alert')).textContent).toContain('Conversation network error');
  });

  it('silently ignores AbortError request settlements', async () => {
    const fetchFn = vi.fn<typeof fetch>(async () => {
      throw new DOMException('superseded', 'AbortError');
    });
    render(ConversationView, {
      props: {
        client: createAPIClient(fetchFn), conversationId: 7, anchorId: 2, onBack: vi.fn()
      }
    });

    await waitFor(() => expect(fetchFn).toHaveBeenCalledOnce());
    expect(screen.queryByRole('alert')).toBeNull();
  });

  it('keeps Back in the shell-owned sticky toolbar and names unavailable states', async () => {
    const onBack = vi.fn();
    const fetchFn = vi.fn<typeof fetch>(async () => Response.json({
      error: 'conversation_unavailable', message: 'Conversation details are unavailable'
    }, { status: 503 }));
    render(ConversationView, {
      props: { client: createAPIClient(fetchFn), conversationId: 7, anchorId: 2, onBack }
    });

    expect((await screen.findByRole('alert')).textContent).toContain('Conversation details are unavailable');
    await fireEvent.click(screen.getByRole('button', { name: 'Back from conversation' }));
    expect(onBack).toHaveBeenCalledOnce();
  });
});
