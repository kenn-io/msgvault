import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../../api/client';
import ConversationView from './ConversationView.svelte';

function message(id: number, type = 'email') {
  return {
    id,
    conversation_id: 7,
    subject: `Message subject ${id}`,
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
  it('loads the thread with the anchor expanded and the rest as one-line collapsed cards', async () => {
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
      props: { client: createAPIClient(fetchFn), conversationId: 7, anchorId: 2 }
    });

    const anchorCard = await screen.findByRole('article', { name: 'Message 2' });
    expect(anchorCard.getAttribute('aria-current')).toBe('true');
    expect(anchorCard.textContent).toContain('alice@example.com');
    expect(anchorCard.textContent).toContain('to bob@example.com');
    expect(screen.getByRole('button', { name: /Expand message 1/ })).toBeTruthy();
    expect(screen.getByRole('button', { name: /Expand message 3/ })).toBeTruthy();
    expect(screen.queryByRole('article', { name: 'Message 1' })).toBeNull();
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
        client: createAPIClient(fetchFn), conversationId: 7, anchorId: 2,
        start: '2026-07-19T00:00:00.000Z', end: '2026-07-20T00:00:00.000Z'
      }
    });

    await screen.findByRole('article', { name: 'Message 2' });
    const url = new URL(requests[0]!.url);
    expect(url.searchParams.get('start')).toBe('2026-07-19T00:00:00.000Z');
    expect(url.searchParams.get('end')).toBe('2026-07-20T00:00:00.000Z');
  });

  it('switches an expanded message to plain text through its overflow control', async () => {
    const fetchFn = vi.fn<typeof fetch>(async () => Response.json({
      id: 7, anchor_id: 2, messages: [message(2)], has_before: false, has_after: false, total: 1
    }));
    render(ConversationView, {
      props: { client: createAPIClient(fetchFn), conversationId: 7, anchorId: 2 }
    });

    await screen.findByRole('article', { name: 'Message 2' });
    await fireEvent.click(screen.getByText('⋯'));
    await fireEvent.click(screen.getByRole('button', { name: 'Show plain text' }));
    expect(screen.getByText('Body 2')).toBeDefined();
  });

  it('expands additional messages inline without refetching and reports the anchor change', async () => {
    const fetchFn = vi.fn<typeof fetch>(async () => Response.json({
      id: 7, anchor_id: 2, messages: [message(1, 'imessage'), message(2, 'imessage')],
      has_before: false, has_after: false, total: 2
    }));
    const onAnchorChange = vi.fn();
    render(ConversationView, {
      props: { client: createAPIClient(fetchFn), conversationId: 7, anchorId: 2, onAnchorChange }
    });

    await screen.findByRole('article', { name: 'Message 2' });
    await fireEvent.click(screen.getByRole('button', { name: /Expand message 1/ }));

    // Both stay expanded — a real thread, not a single-open stack.
    expect(await screen.findByRole('article', { name: 'Message 1' })).toBeTruthy();
    expect(screen.getByRole('article', { name: 'Message 2' })).toBeTruthy();
    expect(onAnchorChange).toHaveBeenCalledWith(1);
    expect(fetchFn).toHaveBeenCalledTimes(1);
  });

  it('collapses an expanded message back to its one-line card', async () => {
    const fetchFn = vi.fn<typeof fetch>(async () => Response.json({
      id: 7, anchor_id: 2, messages: [message(1), message(2)],
      has_before: false, has_after: false, total: 2
    }));
    render(ConversationView, {
      props: { client: createAPIClient(fetchFn), conversationId: 7, anchorId: 2 }
    });

    await screen.findByRole('article', { name: 'Message 2' });
    await fireEvent.click(screen.getByRole('button', { name: /Collapse message 2/ }));
    expect(screen.queryByRole('article', { name: 'Message 2' })).toBeNull();
    expect(screen.getByRole('button', { name: /Expand message 2/ })).toBeTruthy();
    expect(fetchFn).toHaveBeenCalledTimes(1);
  });

  it('refetches only when the requested anchor falls outside the loaded window', async () => {
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      const anchor = Number(new URL(request.url).searchParams.get('anchor'));
      return Response.json({
        id: 7, anchor_id: anchor,
        messages: anchor > 90 ? [message(anchor % 10), { ...message(9), id: anchor }] : [message(1), message(2)],
        has_before: false, has_after: false, total: 2
      });
    });
    const client = createAPIClient(fetchFn);
    const rendered = render(ConversationView, {
      props: { client, conversationId: 7, anchorId: 2 }
    });
    await screen.findByRole('article', { name: 'Message 2' });

    await rendered.rerender({ client, conversationId: 7, anchorId: 99 });
    await waitFor(() => expect(requests).toHaveLength(2));
    expect(new URL(requests[1]!.url).searchParams.get('anchor')).toBe('99');
    expect(await screen.findByRole('article', { name: 'Message 99' })).toBeTruthy();
  });

  it('hides the previous thread while a different conversation loads', async () => {
    const pending = new Map<number, (response: Response) => void>();
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const conversation = Number(new URL(request.url).pathname.split('/').at(-1));
      return await new Promise<Response>((resolve) => {
        pending.set(conversation, resolve);
      });
    });
    const client = createAPIClient(fetchFn);
    const rendered = render(ConversationView, {
      props: { client, conversationId: 7, anchorId: 2 }
    });
    await waitFor(() => expect(pending.get(7)).toBeDefined());
    pending.get(7)!(Response.json({
      id: 7, anchor_id: 2, messages: [message(1), message(2)],
      has_before: false, has_after: false, total: 2
    }));
    await screen.findByRole('article', { name: 'Message 2' });

    await rendered.rerender({ client, conversationId: 8, anchorId: 80 });
    await waitFor(() => expect(pending.get(8)).toBeDefined());

    // The stale thread disappears immediately; loading is shown while B pends.
    expect(screen.queryByRole('article', { name: 'Message 2' })).toBeNull();
    expect(screen.queryByRole('button', { name: /Expand message 1/ })).toBeNull();
    expect(screen.getByRole('status').textContent).toContain('Loading conversation');

    pending.get(8)!(Response.json({
      id: 8, anchor_id: 80, messages: [{ ...message(9), id: 80 }],
      has_before: false, has_after: false, total: 1
    }));
    expect(await screen.findByRole('article', { name: 'Message 80' })).toBeTruthy();
  });

  it('ignores a late response from an abandoned conversation after navigating away', async () => {
    const pending = new Map<number, (response: Response) => void>();
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const conversation = Number(new URL(request.url).pathname.split('/').at(-1));
      return await new Promise<Response>((resolve) => {
        pending.set(conversation, resolve);
      });
    });
    const client = createAPIClient(fetchFn);
    const rendered = render(ConversationView, {
      props: { client, conversationId: 7, anchorId: 2 }
    });
    await waitFor(() => expect(pending.get(7)).toBeDefined());

    await rendered.rerender({ client, conversationId: 8, anchorId: 80 });
    await waitFor(() => expect(pending.get(8)).toBeDefined());
    pending.get(8)!(Response.json({
      id: 8, anchor_id: 80, messages: [{ ...message(9), id: 80 }],
      has_before: false, has_after: false, total: 1
    }));
    await screen.findByRole('article', { name: 'Message 80' });

    pending.get(7)!(Response.json({
      id: 7, anchor_id: 2, messages: [message(1), message(2)],
      has_before: false, has_after: false, total: 2
    }));
    await new Promise((resolve) => setTimeout(resolve, 0));

    expect(screen.getByRole('article', { name: 'Message 80' })).toBeTruthy();
    expect(screen.queryByRole('article', { name: 'Message 2' })).toBeNull();
    expect(screen.queryByRole('alert')).toBeNull();
  });

  it('aborts a superseded window load and ignores stale resolve or reject settlements', async () => {
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
    const rendered = render(ConversationView, {
      props: { client, conversationId: 7, anchorId: 2 }
    });
    await screen.findByRole('article', { name: 'Message 2' });

    await rendered.rerender({ client, conversationId: 7, anchorId: 98 });
    await waitFor(() => expect(pending.get(98)).toBeDefined());
    await rendered.rerender({ client, conversationId: 7, anchorId: 99 });
    await waitFor(() => expect(pending.get(99)).toBeDefined());

    expect(pending.get(98)?.request.signal.aborted).toBe(true);
    pending.get(99)?.resolve(Response.json({
      id: 7, anchor_id: 99, messages: [message(1), { ...message(9), id: 99 }],
      has_before: false, has_after: false, total: 2
    }));
    pending.get(98)?.reject(new TypeError('stale network failure'));

    expect(await screen.findByRole('article', { name: 'Message 99' })).toBeTruthy();
    expect(screen.queryByRole('alert')).toBeNull();
    expect(fetchFn).toHaveBeenCalledTimes(3);
  });

  it('surfaces a named network failure without an unhandled rejection', async () => {
    const fetchFn = vi.fn<typeof fetch>(async () => {
      throw new TypeError('connection refused');
    });
    render(ConversationView, {
      props: { client: createAPIClient(fetchFn), conversationId: 7, anchorId: 2 }
    });

    expect((await screen.findByRole('alert')).textContent).toContain('Conversation network error');
  });

  it('silently ignores AbortError request settlements', async () => {
    const fetchFn = vi.fn<typeof fetch>(async () => {
      throw new DOMException('superseded', 'AbortError');
    });
    render(ConversationView, {
      props: { client: createAPIClient(fetchFn), conversationId: 7, anchorId: 2 }
    });

    await waitFor(() => expect(fetchFn).toHaveBeenCalledOnce());
    expect(screen.queryByRole('alert')).toBeNull();
  });

  it('names unavailable conversation states', async () => {
    const fetchFn = vi.fn<typeof fetch>(async () => Response.json({
      error: 'conversation_unavailable', message: 'Conversation details are unavailable'
    }, { status: 503 }));
    render(ConversationView, {
      props: { client: createAPIClient(fetchFn), conversationId: 7, anchorId: 2 }
    });

    expect((await screen.findByRole('alert')).textContent).toContain('Conversation details are unavailable');
  });

  it('notes bounded windows with quiet notices instead of chrome', async () => {
    const fetchFn = vi.fn<typeof fetch>(async () => Response.json({
      id: 7, anchor_id: 2, messages: [message(2)], has_before: true, has_after: true, total: 40
    }));
    render(ConversationView, {
      props: { client: createAPIClient(fetchFn), conversationId: 7, anchorId: 2 }
    });

    await screen.findByRole('article', { name: 'Message 2' });
    expect(screen.getByText(/Earlier messages are outside this view — showing 1 of 40./)).toBeTruthy();
    expect(screen.getByText(/Later messages are outside this view./)).toBeTruthy();
  });
});
