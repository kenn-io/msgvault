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
  it('uses an opaque scripted sandbox without same-origin authority', async () => {
    const { container } = render(ContentFrame, {
      props: { messageId: 42, html: '<p>Archived content</p>', title: 'Archived message' }
    });

    await waitFor(() => expect(container.querySelector('iframe')).not.toBeNull());
    const frame = container.querySelector('iframe');
    expect(frame?.getAttribute('sandbox')).toBe('allow-scripts');
    expect(frame?.getAttribute('sandbox')).not.toContain('allow-same-origin');
    expect(frame?.getAttribute('tabindex')).toBe('-1');
    expect((frame as HTMLIFrameElement & { inert: boolean }).inert).toBe(true);
    await waitFor(() =>
      expect(container.querySelector('iframe')?.getAttribute('srcdoc')).toContain('Archived content')
    );
  });

  it('keeps focus in the shell until explicit content entry', async () => {
    render(ContentFrame, {
      props: { messageId: 42, html: '<p>Archived content</p>', title: 'Archived message' }
    });
    const enter = screen.getByRole('button', { name: 'Enter archived content' });
    await waitFor(() => expect(document.querySelector('iframe')).not.toBeNull());
    await fireEvent.load(document.querySelector('iframe')!);
    enter.focus();

    expect(screen.queryByText('Archived content active')).toBeNull();
    await fireEvent.click(enter);

    expect(screen.getByText('Archived content active')).toBeTruthy();
    expect(screen.getByRole('button', { name: 'Exit archived content' })).toBeTruthy();
    expect((document.querySelector('iframe') as HTMLIFrameElement & { inert: boolean }).inert).toBe(false);
  });

  it('keeps entry unavailable until delayed CID resolution and the final iframe load', async () => {
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
    const enter = screen.getByRole('button', { name: 'Enter archived content' });
    expect((enter as HTMLButtonElement).disabled).toBe(true);
    expect(container.querySelector('iframe')).toBeNull();

    response.resolve(new Response(onePixelPNG, { headers: { 'Content-Type': 'image/png' } }));
    await waitFor(() => expect(container.querySelector('iframe')).not.toBeNull());
    expect((enter as HTMLButtonElement).disabled).toBe(true);
    await fireEvent.load(container.querySelector('iframe')!);
    expect((enter as HTMLButtonElement).disabled).toBe(false);
  });

  it('synchronously exits and focuses stable shell chrome before consent replaces an entered frame', async () => {
    const onContentFocusChange = vi.fn();
    const { container } = render(ContentFrame, {
      props: {
        messageId: 42,
        html: '<p>Words</p><img src="https://images.example/chart.png" alt="Chart">',
        title: 'Archived message',
        onContentFocusChange
      }
    });
    await waitFor(() => expect(container.querySelector('iframe')?.getAttribute('srcdoc')).toContain('Words'));
    await fireEvent.load(container.querySelector('iframe')!);
    await fireEvent.click(screen.getByRole('button', { name: 'Enter archived content' }));
    expect(screen.getByText('Archived content active')).toBeTruthy();

    await fireEvent.click(screen.getByRole('button', { name: 'Load 1 remote image' }));

    expect(screen.queryByText('Archived content active')).toBeNull();
    expect(onContentFocusChange).toHaveBeenLastCalledWith(false);
    expect(container.querySelector('iframe')).toBeNull();
    expect(document.activeElement).toBe(screen.getByLabelText('Archived content controls'));
    const enter = screen.getByRole('button', { name: 'Enter archived content' });
    expect((enter as HTMLButtonElement).disabled).toBe(true);
    await waitFor(() => expect(container.querySelector('iframe')).not.toBeNull());
    expect((enter as HTMLButtonElement).disabled).toBe(true);
    await fireEvent.load(container.querySelector('iframe')!);
    expect((enter as HTMLButtonElement).disabled).toBe(false);
  });

  it('publishes a failed CID placeholder but still requires the final iframe load before entry', async () => {
    const { container } = render(ContentFrame, {
      props: {
        client: createAPIClient(vi.fn<typeof fetch>(async () => new Response('missing', { status: 404 }))),
        messageId: 42,
        html: '<img src="cid:missing@example.com" alt="Missing">',
        title: 'Archived message'
      }
    });

    const enter = screen.getByRole('button', { name: 'Enter archived content' });
    expect((enter as HTMLButtonElement).disabled).toBe(true);
    await waitFor(() => {
      expect(container.querySelector('iframe')?.getAttribute('srcdoc')).toContain(
        'Inline image unavailable: Missing'
      );
    });
    expect((enter as HTMLButtonElement).disabled).toBe(true);
    await fireEvent.load(container.querySelector('iframe')!);
    expect((enter as HTMLButtonElement).disabled).toBe(false);
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

  it('keeps remote image consent in the shell and rebuilds only after consent', async () => {
    const { container } = render(ContentFrame, {
      props: {
        messageId: 42,
        html: '<img src="https://images.example/chart.png" alt="Chart">',
        title: 'Archived message'
      }
    });
    const consent = await screen.findByRole('button', { name: 'Load 1 remote image' });
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
  });
});
