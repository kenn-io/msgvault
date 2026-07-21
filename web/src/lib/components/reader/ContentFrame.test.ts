import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../../api/client';
import ContentFrame from './ContentFrame.svelte';

const onePixelPNG = Uint8Array.from([
  137, 80, 78, 71, 13, 10, 26, 10, 0, 0, 0, 13, 73, 72, 68, 82
]);

function deferred<T>(): {
  promise: Promise<T>;
  resolve: (value: T) => void;
  reject: (reason: unknown) => void;
} {
  let resolve!: (value: T) => void;
  let reject!: (reason: unknown) => void;
  const promise = new Promise<T>((resolvePromise, rejectPromise) => {
    resolve = resolvePromise;
    reject = rejectPromise;
  });
  return { promise, resolve, reject };
}

describe('ContentFrame', () => {
  it('uses an opaque scripted sandbox that is focusable but never same-origin', async () => {
    const { container } = render(ContentFrame, {
      props: { messageId: 42, html: '<p>Archived content</p>', title: 'Archived message' }
    });

    await waitFor(() => expect(container.querySelector('iframe')).not.toBeNull());
    const frame = container.querySelector('iframe');
    expect(frame?.getAttribute('sandbox')).toBe('allow-scripts');
    expect(frame?.getAttribute('sandbox')).not.toContain('allow-same-origin');
    // The frame is a first-class focus target: keyboard users Tab into it
    // and the bridge forwards Escape back out. No entry gating remains.
    expect(frame?.getAttribute('tabindex')).toBe('0');
    expect(frame?.hasAttribute('inert')).toBe(false);
    await waitFor(() =>
      expect(container.querySelector('iframe')?.getAttribute('srcdoc')).toContain('Archived content')
    );
  });

  it('renders archived content immediately, with no entry gating chrome', async () => {
    const { container } = render(ContentFrame, {
      props: { messageId: 42, html: '<p>Archived content</p>', title: 'Archived message' }
    });

    await waitFor(() => expect(container.querySelector('iframe')).not.toBeNull());
    expect(screen.queryByRole('button', { name: /Enter archived content/ })).toBeNull();
    expect(screen.queryByRole('button', { name: /Exit archived content/ })).toBeNull();
    expect(screen.queryByText(/Archived content active/)).toBeNull();
    expect(container.querySelector('.content-entry-shield')).toBeNull();
  });

  it('keeps a quiet preparing state until delayed CID resolution completes', async () => {
    const response = deferred<Response>();
    const fetchFn = vi.fn<typeof fetch>(async () => await response.promise);
    const { container } = render(ContentFrame, {
      props: {
        client: createAPIClient(fetchFn),
        messageId: 42,
        html: '<img src="cid:slow@example.com" alt="Slow">',
        title: 'Archived message'
      }
    });

    await waitFor(() => expect(fetchFn).toHaveBeenCalledOnce());
    expect(screen.getByRole('status').textContent).toContain('Preparing message');
    expect(container.querySelector('iframe')).toBeNull();

    response.resolve(new Response(onePixelPNG, { headers: { 'Content-Type': 'image/png' } }));
    await waitFor(() => expect(container.querySelector('iframe')).not.toBeNull());
    await fireEvent.load(container.querySelector('iframe')!);
    expect(screen.queryByText(/Preparing message/)).toBeNull();
  });

  it('publishes a failed CID placeholder inside the rendered document', async () => {
    const { container } = render(ContentFrame, {
      props: {
        client: createAPIClient(vi.fn<typeof fetch>(async () => new Response('missing', { status: 404 }))),
        messageId: 42,
        html: '<img src="cid:missing@example.com" alt="Missing">',
        title: 'Archived message'
      }
    });

    await waitFor(() => {
      expect(container.querySelector('iframe')?.getAttribute('srcdoc')).toContain(
        'Inline image unavailable: Missing'
      );
    });
  });

  it('aborts old message work and ignores its stale completion after a replacement is ready', async () => {
    const firstResponse = deferred<Response>();
    let firstRequest: Request | undefined;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input as Request;
      if (new URL(request.url).searchParams.get('cid') === 'old@example.com') {
        firstRequest = request;
        return await firstResponse.promise;
      }
      return new Response(onePixelPNG, { headers: { 'Content-Type': 'image/png' } });
    });
    const client = createAPIClient(fetchFn);
    const rendered = render(ContentFrame, {
      props: {
        client, messageId: 1, html: '<img src="cid:old@example.com">', title: 'Archived message'
      }
    });
    await waitFor(() => expect(firstRequest).toBeDefined());

    await rendered.rerender({
      client, messageId: 2, html: '<img src="cid:new@example.com" alt="New">', title: 'Archived message'
    });
    expect(firstRequest?.signal.aborted).toBe(true);
    await waitFor(() => {
      const srcdoc = rendered.container.querySelector('iframe')?.getAttribute('srcdoc') ?? '';
      expect(srcdoc).toContain('data:image/png;base64,');
    });
    const replacementDocument = rendered.container.querySelector('iframe')?.getAttribute('srcdoc');

    firstResponse.resolve(new Response(onePixelPNG, { headers: { 'Content-Type': 'image/png' } }));
    await Promise.resolve();
    await Promise.resolve();
    expect(rendered.container.querySelector('iframe')?.getAttribute('srcdoc')).toBe(replacementDocument);
  });

  it('fetches CID images in the authenticated shell and embeds validated bytes as data', async () => {
    const fetchFn = vi.fn<typeof fetch>(async () => new Response(onePixelPNG, {
      headers: { 'Content-Type': 'image/png' }
    }));
    const { container } = render(ContentFrame, {
      props: {
        client: createAPIClient(fetchFn),
        messageId: 42,
        html: '<img src="cid:logo@example.com" alt="Logo">',
        title: 'Archived message'
      }
    });

    await waitFor(() => expect(fetchFn).toHaveBeenCalledOnce());
    const request = fetchFn.mock.calls[0]?.[0] as Request;
    expect(new URL(request.url).pathname).toBe('/api/v1/messages/42/inline');
    expect(new URL(request.url).searchParams.get('cid')).toBe('logo@example.com');
    expect(request.headers.has('Authorization')).toBe(false);
    await waitFor(() => {
      const srcdoc = container.querySelector('iframe')?.getAttribute('srcdoc') ?? '';
      expect(srcdoc).toContain('src="data:image/png;base64,');
      expect(srcdoc).not.toMatch(/\/api\/v1\/messages|cid:/);
    });
  });

  it.each([
    ['non-image MIME', new Response(onePixelPNG, { headers: { 'Content-Type': 'text/html' } })],
    ['oversized bytes', new Response(new Uint8Array(5 * 1024 * 1024 + 1), { headers: { 'Content-Type': 'image/png' } })],
    ['HTTP failure', new Response('missing', { status: 404, headers: { 'Content-Type': 'application/json' } })]
  ])('keeps a URL-free CID failure placeholder for %s', async (_name, response) => {
    const fetchFn = vi.fn<typeof fetch>(async () => response);
    const { container } = render(ContentFrame, {
      props: {
        client: createAPIClient(fetchFn),
        messageId: 42,
        html: '<img src="cid:logo@example.com" alt="Logo">',
        title: 'Archived message'
      }
    });

    await waitFor(() => {
      const srcdoc = container.querySelector('iframe')?.getAttribute('srcdoc') ?? '';
      expect(srcdoc).toContain('Inline image unavailable: Logo');
      expect(srcdoc).not.toMatch(/\/api\/v1\/messages|cid:/);
    });
  });

  it('aborts shell-owned CID work when the frame is destroyed', async () => {
    let request: Request | undefined;
    const response = deferred<Response>();
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      request = input as Request;
      return await response.promise;
    });
    const rendered = render(ContentFrame, {
      props: {
        client: createAPIClient(fetchFn),
        messageId: 42,
        html: '<img src="cid:logo@example.com">',
        title: 'Archived message'
      }
    });

    await waitFor(() => expect(request).toBeDefined());
    rendered.unmount();

    expect(request?.signal.aborted).toBe(true);
    response.resolve(new Response(onePixelPNG, { headers: { 'Content-Type': 'image/png' } }));
    await Promise.resolve();
    await Promise.resolve();
  });

  it('keeps remote image consent as one quiet inline notice and rebuilds only after consent', async () => {
    const { container } = render(ContentFrame, {
      props: {
        messageId: 42,
        html: '<img src="https://images.example/chart.png" alt="Chart">',
        title: 'Archived message'
      }
    });
    const consent = await screen.findByRole('button', { name: 'Load 1 remote image' });
    expect(screen.getByText('1 remote image is not loaded.')).toBeDefined();
    await waitFor(() => {
      const srcdoc = container.querySelector('iframe')?.getAttribute('srcdoc');
      expect(srcdoc).toContain('Remote image blocked: Chart');
      expect(srcdoc).not.toContain('images.example');
    });
    await fireEvent.load(container.querySelector('iframe')!);

    await fireEvent.click(consent);

    await waitFor(() =>
      expect(container.querySelector('iframe')?.getAttribute('srcdoc')).toContain(
        'https://images.example/chart.png'
      )
    );
    expect(screen.queryByRole('button', { name: /remote image/ })).toBeNull();
  });

  it('detaches the consent-superseded frame before the newly capable document renders', async () => {
    const { container } = render(ContentFrame, {
      props: {
        messageId: 42,
        html: '<p>Words</p><img src="https://images.example/chart.png" alt="Chart">',
        title: 'Archived message'
      }
    });
    await waitFor(() => expect(container.querySelector('iframe')?.getAttribute('srcdoc')).toContain('Words'));
    await fireEvent.load(container.querySelector('iframe')!);
    const priorFrame = container.querySelector('iframe');

    await fireEvent.click(screen.getByRole('button', { name: 'Load 1 remote image' }));

    expect(container.querySelector('iframe')).toBeNull();
    expect(priorFrame?.isConnected).toBe(false);
    await waitFor(() =>
      expect(container.querySelector('iframe')?.getAttribute('srcdoc')).toContain('images.example')
    );
  });

  it('applies bridge-reported content heights to the frame element', async () => {
    const { container } = render(ContentFrame, {
      props: { messageId: 42, html: '<p>Archived content</p>', title: 'Archived message' }
    });
    await waitFor(() => expect(container.querySelector('iframe')).not.toBeNull());
    const frame = container.querySelector('iframe') as HTMLIFrameElement;
    const nonce = /data-bridge-nonce="([^"]+)"/.exec(frame.getAttribute('srcdoc') ?? '')?.[1];
    expect(nonce).toBeTruthy();

    window.dispatchEvent(new MessageEvent('message', {
      source: frame.contentWindow,
      origin: 'null',
      data: { channel: 'msgvault-archived-content', nonce, type: 'height', height: 732 }
    }));

    await waitFor(() => expect(frame.style.height).toBe('732px'));
  });
});
