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
  it.each(['create', 'link', 'kata-create', 'kata-link'])('refreshes both evidence lists after %s succeeds and closes', async (mode) => {
    const { KataReadiness, kataReadinessKey } = await import('../../kata/kata-ready.svelte');
    const { chooseSelectOption } = await import('../../../test/kit-ui');
    let mutated = false;
    const client = createAPIClient(async (input) => {
      const request = input as Request;
      const path = new URL(request.url).pathname;
      if (path.endsWith('/evidence/prepare')) return Response.json({ evidence: [{ id: 'evidence', excerpt: 'Quoted text', reference: { version: 1, kind: 'message', message_id: 42, message: { start_rune: 0, end_rune: 11, body_sha256: 'a'.repeat(64) } } }] });
      if (request.method === 'POST') { mutated = true; return Response.json(path.includes('/kata/') ? { issue: { uid: 'issue-1', qualified_ref: 'example#abcd', status: 'open' }, replayed: false } : { task: { id: 'task-1', title: 'Updated task' } }, { status: 201 }); }
      if (path.endsWith('/status')) return Response.json({ state: 'ready', project: 'example' });
      if (path.includes('/search')) return Response.json({ tasks: [{ id: 'task-1', title: 'Existing task' }] });
      if (path.endsWith('/tasks')) return Response.json({ state: 'ready', complete: true, tasks: [{ id: 'task-1', title: mutated ? 'Updated task' : 'Initial task' }], outbound_metadata: { archive_uid: 'archive-a', conversation_id: 7 } });
      return Response.json({ issues: [{ uid: 'issue-1', qualified_ref: 'example#abcd', title: mutated ? 'Updated quote' : 'Initial quote', status: 'open' }] });
    });
    const readiness = new KataReadiness(client);
    const view = render(MessageCard, { props: { client, message: detail({ messageType: 'email' }), expanded: true }, context: new Map([[kataReadinessKey, readiness]]) });
    try {
      await screen.findByText('Initial task');
      await screen.findByText(/example#abcd: Initial quote/);
      await fireEvent.click(screen.getByRole('button', { name: 'More actions for message 42' }));
      await fireEvent.click(screen.getByRole('menuitem', { name: 'Add to task…' }));
      if (mode.startsWith('kata')) {
        await chooseSelectOption(screen.getByRole('combobox', { name: /^Provider:/ }), 'Kata');
        await fireEvent.click(screen.getByRole('button', { name: 'Add quote to Kata…' }));
        await screen.findByText('Quoted text');
        if (mode === 'kata-link') {
          await chooseSelectOption(screen.getByRole('combobox', { name: /^Action:/ }), 'Add to existing issue');
          await fireEvent.input(screen.getByRole('textbox', { name: 'Kata issue ref' }), { target: { value: 'example#abcd' } });
        }
        await fireEvent.click(screen.getByRole('button', { name: mode === 'kata-link' ? 'Add evidence' : 'Create issue' }));
        await screen.findByText(/Saved to/);
        expect(screen.getByText('Initial task')).toBeDefined();
        await fireEvent.click(screen.getAllByRole('button', { name: 'Close' }).at(-1)!);
      } else if (mode === 'link') {
        await fireEvent.click(await screen.findByRole('radio', { name: 'Existing' }));
        await fireEvent.input(screen.getByLabelText('Search tasks'), { target: { value: 'Existing' } });
        await fireEvent.click(screen.getByRole('button', { name: 'Search' }));
        await fireEvent.click(await screen.findByRole('button', { name: 'Link Existing task' }));
      } else await fireEvent.click(await screen.findByRole('button', { name: 'Create task' }));
      await screen.findByText('Updated task');
      expect(await screen.findByText(/example#abcd: Updated quote/)).toBeDefined();
    } finally { view.unmount(); readiness.dispose(); }
  });
  it('keeps both evidence lists visible when Add to task is cancelled', async () => {
    const { KataReadiness, kataReadinessKey } = await import('../../kata/kata-ready.svelte');
    const reads: string[] = [];
    const client = createAPIClient(async (input) => {
      const path = new URL((input as Request).url).pathname;
      reads.push(path);
      if (path.endsWith('/status')) return Response.json({ state: 'ready', project: 'example' });
      if (path.endsWith('/tasks')) {
        if (reads.filter((value) => value.endsWith('/tasks')).length > 2) return new Promise<Response>(() => {});
        return Response.json({ state: 'ready', complete: true, tasks: [{ id: 'task-1', title: 'Archived task' }], outbound_metadata: {} });
      }
      if (reads.filter((value) => value.endsWith('/issues')).length > 1) return new Promise<Response>(() => {});
      return Response.json({ issues: [{ uid: 'issue-1', qualified_ref: 'example#abcd', title: 'Archived quote', status: 'open' }] });
    });
    const readiness = new KataReadiness(client);
    const view = render(MessageCard, { props: { client, message: detail({ messageType: 'email' }), expanded: true }, context: new Map([[kataReadinessKey, readiness]]) });
    try {
      await screen.findByText('Archived task');
      await screen.findByText(/example#abcd: Archived quote/);
      await fireEvent.click(screen.getByRole('button', { name: 'More actions for message 42' }));
      await fireEvent.click(screen.getByRole('menuitem', { name: 'Add to task…' }));
      await screen.findByRole('button', { name: 'Create task' });
      await fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));
      expect(screen.queryByText('Loading linked tasks…')).toBeNull();
      expect(screen.getByText('Archived task')).toBeDefined();
      expect(screen.getByText(/example#abcd: Archived quote/)).toBeDefined();
    } finally { view.unmount(); readiness.dispose(); }
  });
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
      props: { message: detail({ subject: '' }), expanded: true, onToggle }
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
    const client = createAPIClient(async (input) => (input as Request).url.includes('/cli/message/raw') ? Response.json({}, { status: 404 }) : Response.json({ state: 'disabled', tasks: [] }));
    render(MessageCard, {
      props: {
        message: detail({ bodyHtml: '<p>Formatted body</p>' }),
        expanded: true,
        client,
        onViewModeChange
      }
    });

    await fireEvent.click(screen.getByRole('button', { name: 'More actions for message 42' }));
    await fireEvent.click(screen.getByRole('menuitem', { name: 'Download original' }));
    await screen.findByText('The original message isn’t available.');
    await fireEvent.click(screen.getByRole('button', { name: 'More actions for message 42' }));
    expect(screen.queryByText('The original message isn’t available.')).toBeNull();
    await fireEvent.click(screen.getByRole('menuitem', { name: 'Show plain text' }));
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

    await fireEvent.click(screen.getByRole('button', { name: 'More actions for message 42' }));
    await fireEvent.click(screen.getByRole('menuitem', { name: 'Show formatted HTML' }));
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

  it('keeps action items visible while an omitted body is fetched', async () => {
    const { meetingAction, meetingActions } = await import('../../meetings/fixtures.test-support');
    const client = createAPIClient(async () => Response.json(meetingActions({ rows: [meetingAction()] })));
    const { container } = render(MessageCard, {
      props: { client, message: detail({ body: '', messageType: 'meeting_transcript' }), expanded: true, bodyPending: true }
    });

    expect(screen.getByText('Loading message…')).toBeDefined();
    expect(await screen.findByText('Prepare review')).toBeDefined();
    expect(container.querySelector('pre')).toBeNull();
    expect(container.querySelector('iframe')).toBeNull();
  });

  it('shows linked tasks alongside a body fetch error', async () => {
    const client = createAPIClient(async (input) => Response.json((input as Request).url.endsWith('/status') ? { state: 'ready' } : { state: 'ready', complete: true, tasks: [{ id: 'task-1', title: 'Review archived notes' }] }));
    const { container } = render(MessageCard, {
      props: { client, message: detail({ body: '' }), expanded: true, bodyError: 'Could not load message body' }
    });

    expect(screen.getByRole('alert').textContent).toContain('Could not load message body');
    expect(container.querySelector('pre')).toBeNull();
    expect(await screen.findByText('Review archived notes')).toBeDefined();
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

    await waitFor(() => expect(fetchFn.mock.calls.filter(([input]) => new URL((input as Request).url).pathname.endsWith('/recordings'))).toHaveLength(1));
    const request = fetchFn.mock.calls.find(([input]) => new URL((input as Request).url).pathname.endsWith('/recordings'))![0] as Request;
    expect(new URL(request.url).pathname).toBe('/api/v1/messages/42/recordings');
  });
});

it.each([{ state: 'ready', shown: true }, { state: 'unavailable', shown: false }])('offers the Kata task flow when $state', async ({ state, shown }) => {
  const fetchFn = vi.fn<typeof fetch>(async (input) => {
    const path = new URL((input as Request).url).pathname;
    return Response.json(path.endsWith('/issues') ? { issues: [] } : { state, project: 'example' });
  });
  const { KataReadiness, kataReadinessKey } = await import('../../kata/kata-ready.svelte');
  const client = createAPIClient(fetchFn);
  const readiness = new KataReadiness(client);
  const context = new Map([[kataReadinessKey, readiness]]);
  const view = render(MessageCard, { props: { message: detail({ messageType: 'imessage' }), expanded: true, client }, context });
  if (shown) {
    await fireEvent.click(await screen.findByRole('button', { name: 'More actions for message 42' }));
    expect(screen.getByRole('menuitem', { name: 'Add to task…' })).toBeDefined();
  } else {
    await waitFor(() => expect(readiness.absent).toBe(true));
    expect(screen.queryByRole('button', { name: 'More actions for message 42' })).toBeNull();
  }
  view.unmount();
  readiness.dispose();
});

it('leaves labels without archived participant IDs as plain text', () => {
  render(MessageCard, { message: detail(), expanded: true, onOpenPerson: vi.fn() });
  expect(screen.getByText('Alice Example <alice@example.com>')).toBeDefined();
  expect(screen.queryByRole('button', { name: 'Alice Example <alice@example.com>' })).toBeNull();
  expect(screen.queryByRole('button', { name: 'bob@example.com' })).toBeNull();
});
