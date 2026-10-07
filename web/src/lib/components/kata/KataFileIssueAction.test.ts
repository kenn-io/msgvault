import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';
import { createAPIClient } from '../../api/client';
import { KataReadiness, kataReadinessKey } from '../../kata/kata-ready.svelte';
import KataFileIssueAction from './KataFileIssueAction.svelte';

describe('KataFileIssueAction', () => {
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
