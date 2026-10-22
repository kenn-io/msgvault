import { act, fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { createAPIClient } from '../../api/client';
import { createCaptureClient } from '../../telemetry/capture.test-support';
import { KataReadiness, kataReadinessKey } from '../../kata/kata-ready.svelte';
import KataFileIssueAction from './KataFileIssueAction.svelte';

afterEach(() => vi.restoreAllMocks());

describe('KataFileIssueAction', () => {
  it('discards dismissed searches and reports an accepted passage search', async () => {
    const requests: Request[] = [];
    let release!: (response: Response) => void;
    const client = createAPIClient(vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request.clone());
      const path = new URL(request.url).pathname;
      if (path.endsWith('/status')) return Response.json({ state: 'ready', project: 'example', message: 'Ready' });
      if (path.endsWith('/documents/search') && new URL(request.url).searchParams.get('q') === 'dismissed') return new Promise<Response>((resolve) => { release = resolve; });
      return Response.json({ results: [] });
    }));
    render(KataFileIssueAction, { props: { client, attachmentID: 12, messageID: 42, filename: 'transcript.txt' }, context: new Map([[kataReadinessKey, new KataReadiness(client)]]) });
    await fireEvent.click(await screen.findByRole('button', { name: 'Create Kata issue from file' }));
    await fireEvent.input(screen.getByLabelText('Find a passage'), { target: { value: 'dismissed' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Search this file' }));
    await waitFor(() => expect(release).toBeDefined());
    await fireEvent.keyDown(window, { key: 'Escape' });
    await act(async () => { release(Response.json({ results: [] })); });
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
    expect(requests.filter((request) => new URL(request.url).pathname.endsWith('/telemetry/events'))).toHaveLength(0);
    await fireEvent.click(screen.getByRole('button', { name: 'Create Kata issue from file' }));
    await fireEvent.input(screen.getByLabelText('Find a passage'), { target: { value: 'missing' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Search this file' }));
    await screen.findByText('No indexed passage in this file matches.');
    await waitFor(() => expect(requests.filter((request) => new URL(request.url).pathname.endsWith('/telemetry/events'))).toHaveLength(2));
    const events = await Promise.all(requests.filter((request) => new URL(request.url).pathname.endsWith('/telemetry/events')).map((request) => request.json()));
    expect(events).toEqual(expect.arrayContaining([
      { event: 'search_run', properties: { surface: 'web', search_type: 'text' } },
      { event: 'search_empty', properties: { surface: 'web', search_type: 'text' } },
    ]));
  });

  it.each(['visible', 'close', 'unmount', 'replace'])('reports a failed passage search only while displayed: %s', async (action) => {
    const visibility = vi.spyOn(document, 'visibilityState', 'get').mockReturnValue(action === 'visible' ? 'visible' : 'hidden');
    const events: unknown[] = [], requests: Promise<void>[] = [];
    const client = createCaptureClient(async (input) => {
      const request = input as Request, path = new URL(request.url).pathname;
      if (path.endsWith('/status')) return Response.json({ state: 'ready', project: 'example', message: 'Ready' });
      return Response.json({ message: 'Search unavailable' }, { status: 500 });
    }, events, requests);
    const view = render(KataFileIssueAction, { props: { client, attachmentID: 12, messageID: 42, filename: 'transcript.txt' }, context: new Map([[kataReadinessKey, new KataReadiness(client)]]) });
    await fireEvent.click(await screen.findByRole('button', { name: 'Create Kata issue from file' }));
    await fireEvent.input(screen.getByLabelText('Find a passage'), { target: { value: 'needle' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Search this file' }));
    expect((await screen.findByRole('alert')).textContent).toContain('Search unavailable');
    if (action !== 'visible') {
      if (action === 'unmount') view.unmount();
      else if (action === 'close') await fireEvent.keyDown(window, { key: 'Escape' });
      else await fireEvent.click(screen.getByRole('button', { name: 'Search this file' }));
      if (action === 'replace') await screen.findByRole('alert');
      visibility.mockReturnValue('visible');
      await fireEvent(window, new Event('focus'));
      await Promise.all(requests);
      expect(events).toEqual(action === 'replace' ? [{ event: 'error_shown', properties: { surface: 'web', kind: 'search' } }] : []);
      return;
    }
    await waitFor(() => expect(events).toEqual([{ event: 'error_shown', properties: { surface: 'web', kind: 'search' } }]));
    await fireEvent.keyDown(window, { key: 'Escape' });
    await fireEvent.click(screen.getByRole('button', { name: 'Create Kata issue from file' }));
    expect((await screen.findByRole('alert')).textContent).toContain('Search unavailable');
    await Promise.all(requests);
    expect(events).toHaveLength(1);
  });

  it('opens the citation window at a search excerpt late in a transcript', async () => {
    const requests: Request[] = [];
    const excerpt = '🙂 we agreed to ship the draft next week';
    const client = createAPIClient(vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request.clone());
      const path = new URL(request.url).pathname;
      if (path.endsWith('/status')) return Response.json({ state: 'ready', project: 'example', message: 'Ready' });
      if (path.endsWith('/documents/search')) return Response.json({ results: [
        { attachment_id: 12, message_id: 42, extraction_id: 'extract-8', chunk_key: 'chunk-0', excerpt: 'transcript.txt', excerpt_start_rune: 0, highlight_start: 0, highlight_end: 10, matched_signals: ['filename'] },
        { attachment_id: 12, message_id: 42, extraction_id: 'extract-8', chunk_key: 'chunk-1', excerpt, excerpt_start_rune: 2198, highlight_start: 0, highlight_end: 2, matched_signals: ['content', 'filename'] },
      ] });
      return Response.json({ evidence: [] });
    }));
    render(KataFileIssueAction, { props: { client, attachmentID: 12, messageID: 42, filename: 'transcript.txt' }, context: new Map([[kataReadinessKey, new KataReadiness(client)]]) });
    await fireEvent.click(await screen.findByRole('button', { name: 'Create Kata issue from file' }));
    await fireEvent.input(screen.getByLabelText('Find a passage'), { target: { value: 'ship' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Search this file' }));
    const use = await screen.findByRole('button', { name: 'Use passage 1' });
    expect(screen.queryByRole('button', { name: 'Use passage 2' }), 'a filename-only match is not a passage').toBeNull();
    await fireEvent.click(use);
    await waitFor(() => expect(requests.some((request) => new URL(request.url).pathname.endsWith('/evidence/prepare'))).toBe(true));
    const prepare = requests.find((request) => new URL(request.url).pathname.endsWith('/evidence/prepare'))!;
    expect(await prepare.json()).toEqual({ selectors: [{ kind: 'document_chunk', attachment_id: 12, message_id: 42, extraction_id: 'extract-8', chunk_key: 'chunk-1', start_rune: 2198, max_chars: 1000 }] });
  });
});
