import { fireEvent, render, screen } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';
import { createAPIClient } from '../../api/client';
import KataEvidenceDialog from './KataEvidenceDialog.svelte';

const selector = { kind: 'message' as const, message_id: 42, start_rune: 0, max_chars: 1000 };
const issue = { uid: 'issue-uid', ref: 'abcd', qualified_ref: 'example#abcd', project: 'example', title: 'Follow up', status: 'open', revision: '1' };

function evidence(start: number, excerpt: string, next?: number) {
  const reference = {
    version: 1, kind: 'message', archive_uid: 'archive-example', message_id: 42,
    source_type: 'email', source_identifier: 'archive@example.com', source_message_id: 'message-example',
    message: { body_sha256: 'a'.repeat(64), start_rune: start, end_rune: start + [...excerpt].length }
  };
  return { id: `${start}`.padStart(64, '0'), passage: `${start}:${[...excerpt].length}`.padStart(64, 'p'), reference, excerpt, display: { containing_title: 'Synthetic message' }, content_trust: 'untrusted', ...(next ? { next_rune: next } : {}) };
}

const passages: Record<number, ReturnType<typeof evidence>> = {
  0: evidence(0, 'Opening passage', 1000),
  500: evidence(500, 'Please send the revised budget by Friday.'),
};

function harness(create: (request: Request) => Promise<Response> = async () => Response.json({ issue, replayed: false }, { status: 201 })) {
  const requests: Request[] = [];
  const fetchFn = vi.fn<typeof fetch>(async (input) => {
    const request = input instanceof Request ? input : new Request(input);
    requests.push(request.clone());
    const path = new URL(request.url).pathname;
    if (path.endsWith('/evidence/prepare')) {
      const body = await request.json() as { selectors: { start_rune: number }[] };
      return Response.json({ evidence: [passages[body.selectors[0].start_rune]] });
    }
    if (path.endsWith('/kata/open-issues')) return Response.json({ issues: [issue], truncated: false });
    return create(request);
  });
  const posted = (suffix: string) => requests.filter((request) => request.method === 'POST' && new URL(request.url).pathname.endsWith(suffix));
  return { client: createAPIClient(fetchFn), posted };
}

describe('KataEvidenceDialog', () => {
  it('saves exactly the passage it previews, paging in overlapping half windows', async () => {
    const h = harness();
    render(KataEvidenceDialog, { client: h.client, selector, defaultTitle: 'Follow up' });
    expect(await screen.findByText('Opening passage')).toBeDefined();
    expect(screen.getByRole('button', { name: 'Earlier passage' })).toHaveProperty('disabled', true);

    await fireEvent.click(screen.getByRole('button', { name: 'Later passage' }));
    expect(await screen.findByText('Please send the revised budget by Friday.')).toBeDefined();
    expect(await h.posted('/evidence/prepare')[1].json()).toEqual({ selectors: [{ ...selector, start_rune: 500 }] });
    expect(screen.getByRole('button', { name: 'Later passage' })).toHaveProperty('disabled', true);

    await fireEvent.click(screen.getByRole('button', { name: 'Create issue' }));
    expect(await screen.findByText('example#abcd')).toBeDefined();
    const sent = await h.posted('/integrations/kata/issues')[0].json();
    expect(sent).toEqual({ title: 'Follow up', evidence: [passages[500].reference] });
  });

  it.each([false, true])('keeps Escape inside the Kata dialog while saving=%s', async (saving) => {
    let finish!: (response: Response) => void;
    const pending = new Promise<Response>(resolve => { finish = resolve; });
    const h = harness(async () => pending);
    const onclose = vi.fn();
    render(KataEvidenceDialog, { client: h.client, selector, defaultTitle: 'Follow up', onclose });
    await screen.findByText('Opening passage');
    expect(screen.getAllByRole('button', { name: 'Close' })).toHaveLength(1);
    if (saving) await fireEvent.click(screen.getByRole('button', { name: 'Create issue' }));
    const close = screen.getAllByRole('button', { name: 'Close' })[0];
    close.focus();
    await fireEvent.keyDown(close, { key: 'Escape' });
    expect(onclose).toHaveBeenCalledTimes(saving ? 0 : 1);
    if (saving) {
      finish(Response.json({ issue, replayed: false }, { status: 201 }));
      await screen.findByText('example#abcd');
      await fireEvent.keyDown(screen.getAllByRole('button', { name: 'Close' })[0], { key: 'Escape' });
    }
    expect(onclose).toHaveBeenCalledOnce();
  });

  const chunk = { kind: 'document_chunk' as const, message_id: 42, attachment_id: 12, extraction_id: 'extract-8', chunk_key: 'chunk-1', start_rune: 0, max_chars: 1000 };
  it.each([
    { kind: 'message', text: 'Café 🙂 send the budget by Friday', quote: 'send the budget', initial: selector, earlier: false },
    // A file chunk opens at a search hit late in the chunk and pages back before narrowing.
    { kind: 'document_chunk', text: 'x'.repeat(600) + 'Café 🙂 ship the draft', quote: 'ship the draft', initial: { ...chunk, start_rune: 500 }, earlier: true },
  ])('narrows a $kind citation to the selected text, counting runes', async ({ text, quote, initial, earlier }) => {
    type Wanted = { kind: string; start_rune: number; end_rune?: number; max_chars?: number };
    const requests: Wanted[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const wanted = (await request.json() as { selectors: Wanted[] }).selectors[0];
      requests.push(wanted);
      const runes = [...text];
      const end = wanted.end_rune ?? Math.min(runes.length, wanted.start_rune + (wanted.max_chars ?? runes.length));
      const range = { start_rune: wanted.start_rune, end_rune: end };
      const located = wanted.kind === 'message'
        ? { message: { body_sha256: 'a'.repeat(64), ...range } }
        : { attachment_id: 12, occurrence_key: 'occurrence', document_chunk: { canonical_blob_hash: 'a'.repeat(64), extraction_id: 'extract-8', manifest_checksum: 'b'.repeat(64), chunk_key: 'chunk-1', chunk_checksum: 'c'.repeat(64), ...range } };
      const reference = { version: 1, kind: wanted.kind, archive_uid: 'archive-example', message_id: 42, source_type: 'email', source_identifier: 'archive@example.com', source_message_id: 'message-example', ...located };
      const excerpt = runes.slice(wanted.start_rune, end).join('');
      return Response.json({ evidence: [{ id: `${wanted.start_rune}`.padStart(64, '0'), passage: `${wanted.start_rune}:${end}`.padStart(64, 'p'), reference, excerpt, display: { filename: 'transcript.txt' }, content_trust: 'untrusted', ...(end < runes.length ? { next_rune: end } : {}) }] });
    });
    render(KataEvidenceDialog, { client: createAPIClient(fetchFn), selector: initial, defaultTitle: 'Follow up' });
    if (earlier) {
      await screen.findByText([...text].slice(initial.start_rune).join(''));
      expect(screen.getByRole('button', { name: 'Later passage' })).toHaveProperty('disabled', true);
      await fireEvent.click(screen.getByRole('button', { name: 'Earlier passage' }));
    }
    const node = (await screen.findByText(text)).firstChild as Text;
    const from = text.indexOf(quote);
    const selection = document.createRange();
    selection.setStart(node, from);
    selection.setEnd(node, from + quote.length);
    window.getSelection()?.removeAllRanges();
    window.getSelection()?.addRange(selection);

    await fireEvent.click(screen.getByRole('button', { name: 'Quote selection' }));
    expect(await screen.findByText(quote)).toBeDefined();
    const { max_chars: _window, ...exact } = initial;
    const start = [...text.slice(0, from)].length;
    expect(requests.at(-1)).toEqual({ ...exact, start_rune: start, end_rune: start + [...quote].length });
    // The selection ends the text, so a narrowed citation leaves nothing later to page to.
    expect(screen.getByRole('button', { name: 'Later passage' })).toHaveProperty('disabled', true);
  });

  it.each([
    { name: 'a replay', first: () => Response.json({ issue: { ...issue, status: 'closed' }, replayed: true }), role: 'status', says: 'Already filed as example#abcd (closed).' },
    { name: 'a deleted issue', first: () => Response.json({ error: 'kata_issue_deleted', message: 'The issue this key filed was deleted in Kata or is not visible to this credential; use a new idempotency key, or restore the issue in Kata' }, { status: 409 }), role: 'alert', says: /not visible to this credential/ },
  ])('says when the issue already exists and can file a new one after $name', async ({ first, role, says }) => {
    const h = harness(async () => h.posted('/integrations/kata/issues').length === 1
      ? first()
      : Response.json({ issue: { ...issue, qualified_ref: 'example#efgh' }, replayed: false }, { status: 201 }));
    render(KataEvidenceDialog, { client: h.client, selector, defaultTitle: 'Follow up' });
    await screen.findByText('Opening passage');
    await fireEvent.click(screen.getByRole('button', { name: 'Create issue' }));
    expect((await screen.findByRole(role)).textContent).toMatch(says);
    await fireEvent.click(screen.getByRole('button', { name: 'File as a new issue' }));
    expect((await screen.findByText('example#efgh')).closest('p')?.textContent).toBe('Saved to example#efgh.');
    const keys = h.posted('/integrations/kata/issues').map((posted) => posted.headers.get('Idempotency-Key'));
    expect(keys).toHaveLength(2);
    expect(keys[1]).not.toBe(keys[0]);
  });

  it('derives retry keys from the request, so they survive a lost response and remount', async () => {
    const second = { ...issue, qualified_ref: 'example#efgh' };
    const lost = harness(async () => lost.posted('/integrations/kata/issues').length === 1
      ? Response.json({ issue, replayed: true })
      : Response.json({ error: 'kata_unavailable', message: 'Kata is unavailable' }, { status: 503 }));
    const first = render(KataEvidenceDialog, { client: lost.client, selector, defaultTitle: 'Follow up' });
    await screen.findByText('Opening passage');
    await fireEvent.click(screen.getByRole('button', { name: 'Create issue' }));
    await fireEvent.click(await screen.findByRole('button', { name: 'File as a new issue' }));
    await screen.findByRole('alert');
    first.unmount();

    // The server created the second issue before the response was lost, so every
    // key now replays, even after the first issue moved to another project.
    const moved = { ...issue, qualified_ref: 'other#wxyz' };
    const remounted = harness(async (request) => Response.json({ issue: request.headers.get('Idempotency-Key') === lost.posted('/integrations/kata/issues')[1].headers.get('Idempotency-Key') ? second : moved, replayed: true }));
    render(KataEvidenceDialog, { client: remounted.client, selector, defaultTitle: 'Follow up' });
    await screen.findByText('Opening passage');
    // An edit put back leaves the request, and so its key, unchanged.
    await fireEvent.input(screen.getByLabelText('Issue title'), { target: { value: 'Edited title' } });
    await fireEvent.input(screen.getByLabelText('Issue title'), { target: { value: 'Follow up' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Create issue' }));
    await fireEvent.click(await screen.findByRole('button', { name: 'File as a new issue' }));
    expect((await screen.findByText('example#efgh')).closest('p')?.textContent).toBe('Already filed as example#efgh (open).');
    const keys = (h: ReturnType<typeof harness>) => h.posted('/integrations/kata/issues').map((request) => request.headers.get('Idempotency-Key'));
    expect(keys(remounted)).toEqual(keys(lost));
  });

  it('filters one list by title fragments and refs, and accepts an unlisted ref', async () => {
    const reads = vi.fn();
    const posts: Request[] = [];
    const client = createAPIClient(async input => {
      const request = input as Request;
      if (request.url.endsWith('/open-issues')) {
        reads();
        return Response.json({ issues: [{ ...issue, title: 'Send the budget', ref: 'k3f9', qualified_ref: 'example#k3f9' }, { ...issue, title: 'C# guide mentions example#older' }], truncated: true });
      }
      if (request.url.endsWith('/prepare')) return Response.json({ evidence: [passages[0]] });
      posts.push(request);
      return Response.json({ issue, replayed: false });
    });
    render(KataEvidenceDialog, { client, selector, embedded: true });
    await screen.findByText('Opening passage');
    await fireEvent.click(screen.getByRole('radio', { name: 'Existing' }));
    await screen.findByRole('radio', { name: 'Send the budget · example#k3f9' });
    expect(screen.getByText('25 most recently updated open issues. Enter a ref for others.')).toBeDefined();
    const filter = screen.getByRole('searchbox', { name: 'Filter open issues' });
    for (const text of ['bud', 'K3F9', 'C# guide']) {
      await fireEvent.input(filter, { target: { value: text } });
      const title = text === 'C# guide' ? 'C# guide mentions example#older · example#abcd' : 'Send the budget · example#k3f9';
      await screen.findByRole('radio', { name: title });
      await screen.findByRole('radio', { name: `Add quote to ${text}` });
    }
    expect(reads).toHaveBeenCalledOnce();
    await fireEvent.input(filter, { target: { value: 'EXAMPLE#K3F9' } });
    await screen.findByRole('radio', { name: 'Send the budget · example#k3f9' });
    expect(screen.queryByRole('radio', { name: 'Add quote to EXAMPLE#K3F9' })).toBeNull();
    await fireEvent.input(filter, { target: { value: ' example#older ' } });
    await screen.findByRole('radio', { name: 'C# guide mentions example#older · example#abcd' });
    await fireEvent.click(await screen.findByRole('radio', { name: 'Add quote to example#older' }));
    await fireEvent.click(screen.getByRole('button', { name: 'Add quote' }));
    await screen.findByText('example#abcd');
    expect(new URL(posts[0].url).pathname).toBe('/api/v1/integrations/kata/issues/example%23older/evidence');
    expect(await posts[0].json()).toEqual({ evidence: [passages[0].reference] });
  });

  it('keeps a new issue draft across modes and adds the previewed passage to an existing issue', async () => {
    const h = harness(async () => Response.json({ issue, replayed: false }));
    render(KataEvidenceDialog, { client: h.client, selector });
    await screen.findByText('Opening passage');
    await fireEvent.input(screen.getByLabelText('Issue title'), { target: { value: 'Kata draft' } });
    await fireEvent.input(screen.getByLabelText('Brief'), { target: { value: 'Brief draft' } });
    await fireEvent.click(screen.getByRole('radio', { name: 'Existing' }));
    await fireEvent.click(screen.getByRole('radio', { name: 'New' }));
    expect(screen.getByLabelText('Issue title')).toHaveProperty('value', 'Kata draft');
    expect(screen.getByLabelText('Brief')).toHaveProperty('value', 'Brief draft');
    await fireEvent.click(screen.getByRole('radio', { name: 'Existing' }));
    await fireEvent.click(await screen.findByRole('radio', { name: 'Follow up · example#abcd' }));
    await fireEvent.click(screen.getByRole('button', { name: 'Add quote' }));
    await screen.findByText('example#abcd');
    const linked = h.posted('/issues/example%23abcd/evidence');
    expect(await linked[0].json()).toEqual({ evidence: [passages[0].reference] });
  });

  it('links a typed reference when the open issue list is unavailable', async () => {
    const posts: Request[] = [];
    const client = createAPIClient(async input => {
      const request = input as Request;
      if (request.url.endsWith('/open-issues')) return Response.json({ message: 'List unavailable' }, { status: 503 });
      if (request.url.endsWith('/prepare')) return Response.json({ evidence: [passages[0]] });
      posts.push(request);
      return Response.json({ issue, replayed: false });
    });
    render(KataEvidenceDialog, { client, selector, embedded: true });
    await screen.findByText('Opening passage');
    await fireEvent.click(screen.getByRole('radio', { name: 'Existing' }));
    await screen.findByRole('alert');
    await fireEvent.input(screen.getByRole('searchbox', { name: 'Filter open issues' }), { target: { value: 'example#abcd' } });
    await fireEvent.click(await screen.findByRole('radio', { name: 'Add quote to example#abcd' }));
    await fireEvent.click(screen.getByRole('button', { name: 'Add quote' }));
    await screen.findByText('example#abcd');
    expect(new URL(posts[0].url).pathname).toBe('/api/v1/integrations/kata/issues/example%23abcd/evidence');
  });
});
