import { fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';
import { tick } from 'svelte';
import { createAPIClient } from '../../api/client';
import { chooseSelectOption } from '../../../test/kit-ui';
import KataEvidenceDialog from './KataEvidenceDialog.svelte';
import PeopleWorkspace from '../people/PeopleWorkspace.svelte';
import { DirectoryController } from '../../directory/controller.svelte';
import { DirectoryReviewController } from '../../directory/review-controller.svelte';
import { ExploreState } from '../../explore/state.svelte';
import { RelationshipsController } from '../../relationships/controller.svelte';
import { meetingFixtureResponse } from '../../meetings/fixtures.test-support';

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
    const when = '2026-07-19T10:00:00Z';
    const client = createAPIClient(async (input) => {
      const path = new URL((input as Request).url).pathname;
      const meeting = meetingFixtureResponse(path);
      if (meeting) return meeting;
      if (path === '/api/v1/participants/1') return Response.json({ id: 1, display_label: 'Example sender', partial_label: false, identifiers: [], activity_count: 1, file_count: 0, source_counts: [], first_at: when, last_at: when, cache_revision: 'cache-rel' });
      if (path.endsWith('/timeline')) return Response.json({
        canonical_id: 1, identity_revision: 1, cache_revision: 'cache-rel', total_count: 1,
        rows: [{ key: 'message:42', kind: 'email', occurred_at: when, preview: 'Preview', source_id: 1, match: {}, participant_ids: [], participant_labels: [], message_type: 'email', conversation_type: 'email', attachment_count: 0, attachment_size: 0, deleted_from_source: false, source_identifier: 'archive@example.com', source_type: 'gmail',
          title: 'Follow up', has_attachments: false, message_count: 1, anchor_message_id: 42, conversation_id: 70 }]
      });
      if (path === '/api/v1/conversations/70') return Response.json({
        id: 70, anchor_id: 42, has_before: false, has_after: false, total: 1,
        messages: [{ id: 42, conversation_id: 70, subject: 'Follow up', from: 'sender@example.com',
          to: ['inbox@example.com'], sent_at: when, snippet: 'Preview', body: 'Opening passage' }]
      });
      if (path.endsWith('/tasks/status')) return Response.json({ state: 'disabled' });
      if (path.endsWith('/tasks')) return Response.json({ state: 'disabled', complete: false, tasks: [], outbound_metadata: {} });
      return Response.json({ rows: [] });
    });
    const controller = new RelationshipsController(client, () => 'UTC');
    const predicate = { filters: [], presentation: 'table' as const };
    const directory = new DirectoryController(client);
    const state = new ExploreState(null);
    state.commitNavigation({ workspace: 'relationships', relationshipTarget: 'cluster:1' });
    const workspace = render(PeopleWorkspace, { props: {
      client, controller, directory, reviews: new DirectoryReviewController(client), onDuplicatesChanged: vi.fn(),
      predicate, target: 'cluster:1', filesOpen: false, urlState: state.current,
      onNavigate: vi.fn(), onReplaceNavigation: vi.fn(), onDraftNavigation: vi.fn(), onShowAllChange: vi.fn(), onFilesToggle: vi.fn(), onFiltersChange: vi.fn()
    } });
    await controller.openTarget('cluster:1', predicate);
    await fireEvent.click((await screen.findByText('Follow up')).closest('[role="row"]')!);
    const reading = await screen.findByRole('complementary', { name: /Reading pane/ });
    const onclose = vi.fn(async () => { await tick(); dialog.unmount(); });
    const dialog = render(KataEvidenceDialog, { target: reading, props: { client: h.client, selector, defaultTitle: 'Follow up', onclose } });
    await within(screen.getByRole('dialog', { name: 'Kata issue' })).findByText('Opening passage');
    if (saving) await fireEvent.click(screen.getByRole('button', { name: 'Create issue' }));
    const close = screen.getAllByRole('button', { name: 'Close' })[0];
    close.focus();
    await fireEvent.keyDown(close, { key: 'Escape' });
    expect(onclose).toHaveBeenCalledTimes(saving ? 0 : 1);
    expect(document.body.contains(reading)).toBe(true);
    if (saving) {
      finish(Response.json({ issue, replayed: false }, { status: 201 }));
      await screen.findByText('example#abcd');
      await fireEvent.keyDown(screen.getAllByRole('button', { name: 'Close' })[0], { key: 'Escape' });
    }
    expect(screen.queryByRole('dialog', { name: 'Kata issue' })).toBeNull();
    expect(screen.queryByRole('complementary', { name: /Reading pane/ })).toBe(reading);
    await fireEvent.keyDown(reading, { key: 'Escape' });
    await waitFor(() => expect(screen.queryByRole('complementary', { name: /Reading pane/ })).toBeNull());
    workspace.unmount();
    controller.destroy(); directory.destroy(); state.destroy();
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

  it('adds the previewed passage to an existing issue', async () => {
    const h = harness(async () => Response.json({ issue, replayed: false }));
    render(KataEvidenceDialog, { client: h.client, selector });
    await screen.findByText('Opening passage');
    await chooseSelectOption(screen.getByLabelText('Action'), 'Add to existing issue');
    await fireEvent.input(screen.getByLabelText('Kata issue ref'), { target: { value: 'example#abcd' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Add evidence' }));
    await screen.findByText('example#abcd');
    const linked = h.posted('/issues/example%23abcd/evidence');
    expect(await linked[0].json()).toEqual({ evidence: [passages[0].reference] });
  });
});
