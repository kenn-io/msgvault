import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../../api/client';
import type { ArchiveMessageDetail } from '../../archive/types';
import MessageCard from './MessageCard.svelte';

function detail(overrides: Partial<ArchiveMessageDetail> = {}): ArchiveMessageDetail {
  return {
    id: 42,
    conversationId: 7,
    subject: 'Quarterly plan',
    sender: 'Alice Example <alice@example.com>',
    recipients: ['bob@example.com'],
    sentAt: '2026-01-01T12:00:00Z',
    snippet: 'Plan preview',
    body: 'Plain text fallback.',
    attachments: [],
    ...overrides
  };
}

describe('MessageCard', () => {
  it('collapses to one line of sender, snippet, and date that expands on click', async () => {
    const onToggle = vi.fn();
    render(MessageCard, {
      props: { message: detail(), expanded: false, onToggle }
    });

    const collapsed = screen.getByRole('button', {
      name: 'Expand message 42 from Alice Example <alice@example.com>'
    });
    expect(collapsed.textContent).toContain('Alice Example');
    expect(collapsed.textContent).toContain('Plan preview');
    expect(collapsed.getAttribute('aria-expanded')).toBe('false');

    await fireEvent.click(collapsed);
    expect(onToggle).toHaveBeenCalledWith(42);
  });

  it('renders the expanded header directly: sender, recipients, date, subject, then the body', async () => {
    const { container } = render(MessageCard, {
      props: { message: detail({ bodyHtml: '<p>Formatted body</p>' }), expanded: true, anchor: true }
    });

    const card = screen.getByRole('article', { name: 'Message 42' });
    expect(card.getAttribute('aria-current')).toBe('true');
    expect(card.textContent).toContain('Alice Example <alice@example.com>');
    expect(card.textContent).toContain('to bob@example.com');
    expect(card.textContent).toContain('Quarterly plan');
    await waitFor(() => expect(container.querySelector('iframe')).not.toBeNull());
    // Body renders without any frame chrome to click through.
    expect(screen.queryByRole('button', { name: /Enter archived content/ })).toBeNull();
    expect(screen.queryByRole('button', { name: /HTML/ })).toBeNull();
  });

  it('links each stored recording of a meeting for download', () => {
    const { unmount } = render(MessageCard, {
      props: {
        message: detail({ attachments: [{ id: 1, stored: true, filename: 'RE1.wav', mimeType: 'audio/wav', sizeBytes: 10 }] }),
        expanded: true
      }
    });
    expect(screen.queryByRole('list', { name: 'Attachments' }), 'other messages list none').toBeNull();
    unmount();

    render(MessageCard, {
      props: {
        message: detail({
          messageType: 'meeting_transcript',
          attachments: [
            { id: 1, stored: true, filename: 'RE1.wav', mimeType: 'audio/wav', sizeBytes: 10 },
            { id: 2, filename: 'RE2.wav', mimeType: 'audio/wav', sizeBytes: 0 }
          ]
        }),
        expanded: true
      }
    });

    const link = screen.getByRole('link', { name: 'RE1.wav' });
    expect(link.getAttribute('href')).toBe('/api/v1/files/1/content');
    expect(screen.queryByRole('link', { name: 'RE2.wav' })).toBeNull();
    expect(screen.getByText('RE2.wav (not downloaded)')).toBeTruthy();
  });

  it('collapses again from the expanded header', async () => {
    const onToggle = vi.fn();
    render(MessageCard, {
      props: { message: detail(), expanded: true, onToggle }
    });

    await fireEvent.click(screen.getByRole('button', {
      name: 'Collapse message 42 from Alice Example <alice@example.com>'
    }));
    expect(onToggle).toHaveBeenCalledWith(42);
  });

  it('renders plain text on the theme surface when text mode is selected', () => {
    const { container } = render(MessageCard, {
      props: { message: detail({ bodyHtml: '<p>Formatted body</p>' }), expanded: true, viewMode: 'text' }
    });

    expect(container.querySelector('pre')?.textContent).toBe('Plain text fallback.');
    expect(container.querySelector('iframe')).toBeNull();
  });

  it('offers plain text through a small overflow control, defaulting to HTML', async () => {
    const onViewModeChange = vi.fn();
    render(MessageCard, {
      props: {
        message: detail({ bodyHtml: '<p>Formatted body</p>' }),
        expanded: true,
        onViewModeChange
      }
    });

    await fireEvent.click(screen.getByText('⋯'));
    await fireEvent.click(screen.getByRole('button', { name: 'Show plain text' }));
    expect(onViewModeChange).toHaveBeenCalledWith(42, 'text');
  });

  it('switches the overflow control back to HTML from text mode', async () => {
    const onViewModeChange = vi.fn();
    render(MessageCard, {
      props: {
        message: detail({ bodyHtml: '<p>Formatted body</p>' }),
        expanded: true,
        viewMode: 'text',
        onViewModeChange
      }
    });

    await fireEvent.click(screen.getByText('⋯'));
    await fireEvent.click(screen.getByRole('button', { name: 'Show formatted HTML' }));
    expect(onViewModeChange).toHaveBeenCalledWith(42, 'html');
  });

  it('sanitizes framed HTML and blocks sender-controlled network requests', async () => {
    const { container } = render(MessageCard, {
      props: {
        message: detail({
          bodyHtml:
            '<script>parent.postMessage("stolen", "*")</script>' +
            '<img src="https://tracking.example/pixel.png">' +
            '<a href="//tracking.example/click">Open</a>' +
            '<p style="background:url(https://tracking.example/bg.png)">Safe text</p>'
        }),
        expanded: true
      }
    });

    await waitFor(() => expect(container.querySelector('iframe')?.getAttribute('srcdoc')).toContain('Safe text'));
    const srcdoc = container.querySelector('iframe')?.getAttribute('srcdoc') ?? '';
    expect(srcdoc).toContain('Content-Security-Policy');
    expect(srcdoc).toContain("default-src 'none'");
    // The only script is the same-origin static bridge; no inline script or
    // style survives into the archived document.
    expect(srcdoc.match(/<script\b/g)).toHaveLength(1);
    expect(srcdoc).toContain('src="http://localhost:3000/archived-frame.js"');
    expect(srcdoc).not.toMatch(/<script>|<style>/);
    expect(srcdoc).not.toContain('stolen');
    expect(srcdoc).not.toContain('tracking.example');
  });

  it('removes SVG URL attributes that can bypass HTML URL filtering', async () => {
    const { container } = render(MessageCard, {
      props: {
        message: detail({
          bodyHtml:
            '<svg><a xlink:href="https://tracking.example/click"><text>Open</text></a></svg>'
        }),
        expanded: true
      }
    });

    await waitFor(() => expect(container.querySelector('iframe')?.getAttribute('srcdoc')).toContain('Open'));
    const srcdoc = container.querySelector('iframe')?.getAttribute('srcdoc') ?? '';
    expect(srcdoc).toContain('Open');
    expect(srcdoc).not.toContain('tracking.example');
  });

  it('falls back to plain text when HTML contains only whitespace', () => {
    const { container } = render(MessageCard, {
      props: { message: detail({ bodyHtml: ' \n\t ' }), expanded: true }
    });

    expect(container.querySelector('pre')?.textContent).toBe('Plain text fallback.');
    expect(container.querySelector('iframe')).toBeNull();
    expect(screen.queryByText('⋯')).toBeNull();
  });

  it('shows a loading state instead of the body while an omitted body is fetched', () => {
    const { container } = render(MessageCard, {
      props: { message: detail({ body: '' }), expanded: true, bodyPending: true }
    });

    expect(screen.getByRole('status').textContent).toContain('Loading message');
    expect(container.querySelector('pre')).toBeNull();
    expect(container.querySelector('iframe')).toBeNull();
  });

  it('shows the body fetch error in place of the body', () => {
    const { container } = render(MessageCard, {
      props: { message: detail({ body: '' }), expanded: true, bodyError: 'Could not load message body' }
    });

    expect(screen.getByRole('alert').textContent).toContain('Could not load message body');
    expect(container.querySelector('pre')).toBeNull();
  });

  it('falls back to plain text when HTML is rejected by sanitization', () => {
    const { container } = render(MessageCard, {
      props: {
        message: detail({ bodyHtml: '<p>Unsafe body</p>' }),
        expanded: true,
        sanitizationFailed: true
      }
    });

    expect(screen.getByRole('alert').textContent).toMatch(/could not render HTML/i);
    expect(container.querySelector('pre')).not.toBeNull();
    expect(container.querySelector('iframe')).toBeNull();
    expect(screen.queryByText('⋯')).toBeNull();
  });

  it('requests recordings once for an expanded card with attachments', async () => {
    const fetchFn = vi.fn<typeof fetch>(async () => Response.json({ message_id: 42, recordings: [] }));
    const attachments = [{ id: 5, filename: 'voice.wav', mimeType: 'audio/wav', sizeBytes: 44 }];
    render(MessageCard, {
      props: { message: detail({ attachments }), expanded: true, client: createAPIClient(fetchFn) }
    });
    render(MessageCard, { props: { message: detail(), expanded: true, client: createAPIClient(fetchFn) } });
    render(MessageCard, {
      props: { message: detail({ attachments }), expanded: false, client: createAPIClient(fetchFn) }
    });

    await waitFor(() => expect(fetchFn).toHaveBeenCalledTimes(1));
    const request = fetchFn.mock.calls[0][0] as Request;
    expect(new URL(request.url).pathname).toBe('/api/v1/messages/42/recordings');
  });
});

it.each([
  { state: 'ready', shown: true },
  { state: 'unavailable', shown: false },
])('shows the Kata action only when the integration is $state, asking once per page', async ({ state, shown }) => {
  const fetchFn = vi.fn<typeof fetch>(async () => Response.json({ state, project: 'example', message: 'Kata is unavailable' }));
  const { createAPIClient } = await import('../../api/client');
  const { KataReadiness, kataReadinessKey } = await import('../../kata/kata-ready.svelte');
  const client = createAPIClient(fetchFn);
  const context = new Map([[kataReadinessKey, new KataReadiness(client)]]);
  render(MessageCard, { props: { message: detail(), expanded: true, client }, context });
  render(MessageCard, { props: { message: detail(), expanded: true, client }, context });
  await waitFor(() => expect(fetchFn).toHaveBeenCalledOnce());
  await new Promise((resolve) => setTimeout(resolve, 20));
  expect(screen.queryAllByRole('button', { name: 'Create Kata issue' })).toHaveLength(shown ? 2 : 0);
  expect(screen.queryByText('Kata is unavailable')).toBeNull();
  expect(fetchFn).toHaveBeenCalledOnce();
});

it.each([
  { name: 'a failed check', first: () => Promise.reject(new TypeError('network down')) },
  { name: 'Kata out of reach', first: async () => Response.json({ state: 'unreachable', project: 'example' }) },
])('asks Kata again shortly after $name, so the mounted card shows the action', async ({ first }) => {
  vi.useFakeTimers({ shouldAdvanceTime: true });
  try {
    const fetchFn = vi.fn<typeof fetch>()
      .mockImplementationOnce(first)
      .mockImplementation(async () => Response.json({ state: 'ready', project: 'example' }));
    const { createAPIClient } = await import('../../api/client');
    const { KATA_RETRY_MS, KataReadiness, kataReadinessKey } = await import('../../kata/kata-ready.svelte');
    const client = createAPIClient(fetchFn);
    const readiness = new KataReadiness(client);
    render(MessageCard, { props: { message: detail(), expanded: true, client }, context: new Map([[kataReadinessKey, readiness]]) });
    await waitFor(() => expect(fetchFn).toHaveBeenCalledOnce());
    await vi.advanceTimersByTimeAsync(KATA_RETRY_MS - 1000);
    expect(fetchFn).toHaveBeenCalledOnce();
    expect(screen.queryByRole('button', { name: 'Create Kata issue' })).toBeNull();
    await vi.advanceTimersByTimeAsync(1000);
    expect(await screen.findByRole('button', { name: 'Create Kata issue' })).toBeTruthy();
    expect(fetchFn).toHaveBeenCalledTimes(2);
    readiness.dispose();
  } finally {
    vi.useRealTimers();
  }
});
