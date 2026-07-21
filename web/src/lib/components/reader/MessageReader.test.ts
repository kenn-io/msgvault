import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';

import type { ArchiveMessageDetail } from '../../archive/types';
import MessageReader from './MessageReader.svelte';

function detail(overrides: Partial<ArchiveMessageDetail> = {}): ArchiveMessageDetail {
  return {
    id: 42,
    conversationId: 7,
    subject: 'Quarterly plan',
    sender: 'alice@example.com',
    recipients: ['bob@example.com'],
    sentAt: '2026-01-01T12:00:00Z',
    snippet: 'Plan preview',
    body: 'Plain text fallback.',
    attachments: [],
    ...overrides
  };
}

describe('MessageReader', () => {
  it('renders plain text when text mode is selected', () => {
    const { container } = render(MessageReader, {
      props: { message: detail({ bodyHtml: '<p>Formatted body</p>' }), viewMode: 'text' }
    });

    expect(container.querySelector('pre')?.textContent).toBe('Plain text fallback.');
    expect(container.querySelector('iframe')).toBeNull();
  });

  it('renders non-empty HTML in an isolated frame when HTML mode is selected', async () => {
    const { container } = render(MessageReader, {
      props: { message: detail({ bodyHtml: '<p>Formatted body</p>' }), viewMode: 'html' }
    });

    await waitFor(() => expect(container.querySelector('iframe')).not.toBeNull());
    const frame = container.querySelector('iframe');
    expect(frame?.getAttribute('title')).toBe('Message body');
    expect(frame?.hasAttribute('sandbox')).toBe(true);
    await waitFor(() =>
      expect(container.querySelector('iframe')?.getAttribute('srcdoc')).toContain(
        '<p>Formatted body</p>'
      )
    );
    expect(container.querySelector('pre')).toBeNull();
  });

  it('sanitizes framed HTML and blocks sender-controlled network requests', async () => {
    const { container } = render(MessageReader, {
      props: {
        message: detail({
          bodyHtml:
            '<script>parent.postMessage("stolen", "*")</script>' +
            '<img src="https://tracking.example/pixel.png">' +
            '<a href="//tracking.example/click">Open</a>' +
            '<p style="background:url(https://tracking.example/bg.png)">Safe text</p>'
        }),
        viewMode: 'html'
      }
    });

    await waitFor(() => expect(container.querySelector('iframe')?.getAttribute('srcdoc')).toContain('Safe text'));
    const srcdoc = container.querySelector('iframe')?.getAttribute('srcdoc') ?? '';
    expect(srcdoc).toContain('Content-Security-Policy');
    expect(srcdoc).toContain("default-src 'none'");
    expect(srcdoc).toContain('Safe text');
    expect(srcdoc.match(/<script>/g)).toHaveLength(1);
    expect(srcdoc).not.toContain('stolen');
    expect(srcdoc).not.toContain('tracking.example');
  });

  it('removes SVG URL attributes that can bypass HTML URL filtering', async () => {
    const { container } = render(MessageReader, {
      props: {
        message: detail({
          bodyHtml:
            '<svg><a xlink:href="https://tracking.example/click"><text>Open</text></a></svg>'
        }),
        viewMode: 'html'
      }
    });

    await waitFor(() => expect(container.querySelector('iframe')?.getAttribute('srcdoc')).toContain('Open'));
    const srcdoc = container.querySelector('iframe')?.getAttribute('srcdoc') ?? '';
    expect(srcdoc).toContain('Open');
    expect(srcdoc).not.toContain('tracking.example');
  });

  it('falls back to plain text when HTML contains only whitespace', () => {
    const { container } = render(MessageReader, {
      props: { message: detail({ bodyHtml: ' \n\t ' }), viewMode: 'html' }
    });

    expect(container.querySelector('pre')?.textContent).toBe('Plain text fallback.');
    expect(container.querySelector('iframe')).toBeNull();
    expect(screen.queryByRole('button', { name: 'HTML' })).toBeNull();
  });

  it('falls back to plain text when HTML is unavailable or rejected', () => {
    const { container } = render(MessageReader, {
      props: {
        message: detail({ bodyHtml: '<p>Unsafe body</p>' }),
        viewMode: 'html',
        sanitizationFailed: true
      }
    });

    expect(screen.getByRole('alert').textContent).toMatch(/could not render HTML/i);
    expect(container.querySelector('pre')).not.toBeNull();
    expect(container.querySelector('iframe')).toBeNull();
    expect(screen.queryByRole('button', { name: 'HTML' })).toBeNull();
  });

  it('reports explicit HTML and text selections to the owner', async () => {
    const onViewModeChange = vi.fn();
    render(MessageReader, {
      props: {
        message: detail({ bodyHtml: '<p>Formatted body</p>' }),
        viewMode: 'html',
        onViewModeChange
      }
    });

    await fireEvent.click(screen.getByRole('button', { name: 'Text' }));

    expect(onViewModeChange).toHaveBeenCalledWith(42, 'text');
  });
});
