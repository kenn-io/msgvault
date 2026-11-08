import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import { afterEach, expect, it, vi } from 'vitest';
import { tick } from 'svelte';
import { createAPIClient } from '../../api/client';
import GoogleReviewsWorkspace from './GoogleReviewsWorkspace.svelte';

const review = { reviewId: 'r1', reviewer: { displayName: 'Example Reviewer', profilePhotoUrl: 'https://example.invalid/photo' }, starRating: 'FIVE', comment: '<b>Helpful service</b>', reviewReply: { comment: 'Previous response', reviewReplyState: 'PENDING', policyViolation: 'OFF_TOPIC' } };
const capabilities = { capabilities: [{ provider: 'google', available: true, read: true, reply: true }, { provider: 'facebook', available: false, read: false, reply: false, reason: 'Official ratings and reply contract unverified.' }], targets: [{ name: 'shop', provider: 'google', enabled: true, replies_enabled: true }, { name: 'other', provider: 'google', enabled: true, replies_enabled: false }] };
type Body = Record<string, unknown>;
function fixture(handle: (body: Body) => unknown | Promise<unknown>) {
  const seen: Body[] = [];
  const client = createAPIClient(async input => {
    const request = input as Request;
    expect(request.cache).toBe('no-store');
    if (new URL(request.url).pathname === '/api/v1/google-reviews/archive/search') {
      expect(request.method).toBe('GET');
      const params = new URL(request.url).searchParams;
      const body = { action: 'archive_search', q: params.get('q'), target: params.get('target'), page: Number(params.get('page')), page_size: Number(params.get('page_size')) };
      seen.push(body);
      const result = await handle(body);
      return result instanceof Response ? result : Response.json(result);
    }
    expect(new URL(request.url).pathname).toBe('/api/v1/google-reviews/control');
    expect(request.method).toBe('POST');
    const body = await request.json() as Body;
    seen.push(body);
    const result = body.action === 'capabilities' ? capabilities : await handle(body);
    return result instanceof Response ? result : Response.json(result);
  });
  return { client, seen };
}
// Resolve the mocked HTTP response and wait until the actual response body has
// been consumed before allowing Svelte to flush the resulting request chain.
async function finishStale(finish: (value: unknown) => void, value: unknown) {
  const response = Response.json(value);
  const readText = response.text.bind(response);
  let consumed!: () => void;
  const bodyRead = new Promise<void>(resolve => { consumed = resolve; });
  response.text = async () => {
    const text = await readText();
    consumed();
    return text;
  };
  finish(response);
  await bodyRead;
  await new Promise(resolve => setTimeout(resolve, 0));
  await tick();
}
afterEach(cleanup);

it('starts backfill, reports API exhaustion limits, and searches archived reviews', async () => {
  const result = {
    total: 1,
    page: 1,
    page_size: 20,
    results: [{ target: 'shop', review: { ...review, createTime: '2025-01-02T03:04:05Z' }, archived_at: '2026-11-08T12:00:00Z' }],
  };
  const { client, seen } = fixture(body => {
    if (body.action === 'backfill') return { archive_sync: { target: 'shop', status: 'running', pages_fetched: 1, reviews_fetched: 20, total_review_count: 30, archived_review_count: 20, coverage_note: 'API pagination does not guarantee lifetime coverage.' } };
    if (body.action === 'archive_status') return { archive_sync: { target: 'shop', status: 'api_exhausted', pages_fetched: 2, reviews_fetched: 30, total_review_count: 30, archived_review_count: 30, coverage_note: 'API pagination does not guarantee lifetime coverage.' } };
    if (body.action === 'archive_search') return result;
    return { reviews: [] };
  });
  render(GoogleReviewsWorkspace, { client });
  await screen.findByRole('option', { name: 'shop' });
  await fireEvent.change(screen.getByLabelText('Target'), { target: { value: 'shop' } });
  await screen.findByText('No more pages');
  await fireEvent.click(screen.getByRole('button', { name: 'Import review history' }));
  await screen.findByText('Importing');
  expect(screen.getByText('API pagination does not guarantee lifetime coverage.')).toBeDefined();
  await fireEvent.click(screen.getByRole('button', { name: 'Refresh archive status' }));
  await screen.findByText('No more pages');
  await fireEvent.input(screen.getByLabelText('Search archived reviews'), { target: { value: 'helpful service' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Search archive' }));
  expect(await screen.findByText('<b>Helpful service</b>')).toBeDefined();
  expect(seen.find(item => item.action === 'archive_search')).toEqual({ action: 'archive_search', q: 'helpful service', target: 'shop', page: 1, page_size: 20 });
});

it('searches all archived targets when no target filter is selected', async () => {
  const { client, seen } = fixture(body => body.action === 'archive_search' ? {
    total: 1,
    page: 1,
    page_size: 20,
    results: [{ target: 'shop', review: { ...review, createTime: '2025-01-02T03:04:05Z' }, archived_at: '2026-11-08T12:00:00Z' }],
  } : { reviews: [] });
  render(GoogleReviewsWorkspace, { client });
  await fireEvent.input(await screen.findByLabelText('Search archived reviews'), { target: { value: 'helpful service' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Search archive' }));
  expect(await screen.findByText('1 matching archived review.')).toBeDefined();
  expect(seen.find(item => item.action === 'archive_search')).toEqual({ action: 'archive_search', q: 'helpful service', target: null, page: 1, page_size: 20 });
});

it('resets archive pagination when the query changes', async () => {
  const { client, seen } = fixture(body => body.action === 'archive_search' ? {
    total: 40, page: body.page, page_size: 20, results: [],
  } : { reviews: [] });
  render(GoogleReviewsWorkspace, { client });
  const query = await screen.findByLabelText('Search archived reviews');
  await fireEvent.input(query, { target: { value: 'first query' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Search archive' }));
  await screen.findByText('Page 1 of 2');
  await fireEvent.input(query, { target: { value: 'second query' } });
  expect(screen.queryByRole('button', { name: 'Next' })).toBeNull();
  await fireEvent.click(screen.getByRole('button', { name: 'Search archive' }));
  await screen.findByText('Page 1 of 2');
  expect(seen.filter(item => item.action === 'archive_search').map(item => [item.q, item.page])).toEqual([
    ['first query', 1], ['second query', 1],
  ]);
});

it('explains that a cursor-cycle retry restarts from the first page', async () => {
  const { client } = fixture(body => body.action === 'archive_status' ? {
    archive_sync: { target: 'shop', status: 'partial', pages_fetched: 1, reviews_fetched: 20, archived_review_count: 20, last_error_code: 'cursor_cycle', coverage_note: 'API pagination does not guarantee lifetime coverage.' },
  } : { reviews: [] });
  render(GoogleReviewsWorkspace, { client });
  await screen.findByRole('option', { name: 'shop' });
  await fireEvent.change(screen.getByLabelText('Target'), { target: { value: 'shop' } });
  expect(await screen.findByText(/Google repeated a page\. Restart from the first page; existing reviews are kept\./)).toBeDefined();
});

it('loads targets, paginates live reviews and escapes detail without fetching profile photos', async () => {
  const { client, seen } = fixture(body => body.action === 'get' ? { review } : { reviews: [review], next_page_token: body.page_token ? undefined : 'page2' });
  render(GoogleReviewsWorkspace, { client });
  await screen.findByText(/Facebook.*unavailable/i);
  await fireEvent.change(screen.getByRole('combobox', { name: 'Target' }), { target: { value: 'shop' } });
  await fireEvent.click(await screen.findByRole('button', { name: /Example Reviewer/ }));
  expect(await screen.findByText('<b>Helpful service</b>')).toBeDefined();
  expect(screen.getByText('Previous response')).toBeDefined();
  expect(screen.getByText(/Response status: PENDING/)).toBeDefined();
  expect(screen.getByText(/Policy status: OFF_TOPIC/)).toBeDefined();
  expect(document.querySelector('img')).toBeNull();
  expect(document.querySelector('b')).toBeNull();
  await fireEvent.click(screen.getByRole('button', { name: 'Next page' }));
  await waitFor(() => expect(seen.at(-1)).toEqual({ action: 'list', target: 'shop', page_token: 'page2', page_size: 20 }));
  expect(screen.queryByText('Previous response')).toBeNull();
});

it('previews exact text then confirms once, and looks up the pre-issued receipt after response loss', async () => {
  const comment = '  Thank you!\nSee you soon.  ';
  const { client, seen } = fixture(body => {
    if (body.action === 'list') return { reviews: [review] };
    if (body.action === 'get') return { review };
    if (body.action === 'preview_reply') return { preview: { review, comment, confirmation_token: 'token1', receipt_id: 'receipt1', expires_at: '2099-01-01T00:00:00Z' } };
    if (body.action === 'confirm_reply') throw new TypeError('Connection lost');
    return { receipt: { id: 'receipt1', status: 'submitted', created_at: '', updated_at: '' } };
  });
  render(GoogleReviewsWorkspace, { client });
  await screen.findByRole('option', { name: 'shop' });
  await fireEvent.change(screen.getByLabelText('Target'), { target: { value: 'shop' } });
  await fireEvent.click(await screen.findByRole('button', { name: /Example Reviewer/ }));
  await fireEvent.input(await screen.findByRole('textbox', { name: 'Public response' }), { target: { value: comment } });
  await fireEvent.click(screen.getByRole('button', { name: 'Preview response' }));
  const dialog = await screen.findByRole('dialog', { name: 'Confirm public response' });
  expect(within(dialog).getByTestId('exact-response').textContent).toBe(comment);
  expect(within(dialog).getByText('shop')).toBeDefined();
  expect(within(dialog).getByText('receipt1')).toBeDefined();
  expect(within(dialog).getByText('Previous response')).toBeDefined();
  expect(seen.filter(b => b.action === 'confirm_reply')).toHaveLength(0);
  await fireEvent.click(within(dialog).getByRole('button', { name: 'Submit' }));
  await screen.findByText(/Outcome unknown/);
  expect(screen.queryByRole('button', { name: 'Submit' })).toBeNull();
  await fireEvent.click(screen.getByRole('button', { name: 'Check receipt' }));
  await screen.findByText(/Submitted to Google/);
  expect(seen.filter(b => b.action === 'preview_reply')).toEqual([{ action: 'preview_reply', target: 'shop', review_id: 'r1', comment }]);
  expect(seen.filter(b => b.action === 'confirm_reply')).toEqual([{ action: 'confirm_reply', confirmation_token: 'token1', confirmed: true }]);
  expect(seen.at(-1)).toEqual({ action: 'receipt', receipt_id: 'receipt1' });
});

it.each(['recognized conflict', 'unrecognized conflict', 'forged server error', 'lost response', 'plain error'])('handles %s confirmation without replay or upstream text', async failure => {
  let previews = 0;
  const known = failure === 'recognized conflict';
  const { client, seen } = fixture(body => {
    if (body.action === 'list') return { reviews: [review] };
    if (body.action === 'get') return { review };
    if (body.action === 'preview_reply') {
      previews++;
      return { preview: { review, comment: body.comment, confirmation_token: `token${previews}`, receipt_id: `receipt${previews}`, expires_at: '2099-01-01T00:00:00Z' } };
    }
    if (body.action === 'confirm_reply') {
      if (failure === 'lost response') throw new TypeError('private-confirm-canary');
      if (failure === 'plain error') throw new Error('private-confirm-canary');
      return Response.json({ error: failure === 'unrecognized conflict' ? 'proxy_conflict' : 'google_reviews_preview_changed', message: 'private-confirm-canary' }, { status: failure === 'forged server error' ? 500 : 409 });
    }
    return { receipt: { id: 'receipt1', status: known ? 'failed' : 'unknown', created_at: '', updated_at: '' } };
  });
  render(GoogleReviewsWorkspace, { client });
  await screen.findByRole('option', { name: 'shop' });
  await fireEvent.change(screen.getByLabelText('Target'), { target: { value: 'shop' } });
  await fireEvent.click(await screen.findByRole('button', { name: /Example Reviewer/ }));
  await fireEvent.input(await screen.findByLabelText('Public response'), { target: { value: 'Original response.' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Preview response' }));
  await fireEvent.click(within(await screen.findByRole('dialog')).getByRole('button', { name: 'Submit' }));
  if (known) {
    expect((await screen.findByRole('alert')).textContent).toBe('Review changed or preview expired. Refresh and create a new preview.');
    expect(screen.queryByText(/Outcome unknown/)).toBeNull();
  } else {
    await screen.findByText(/Outcome unknown/);
    expect(screen.queryByRole('alert')).toBeNull();
  }
  expect(document.body.textContent).not.toContain('private-confirm-canary');
  expect(screen.queryByRole('dialog')).toBeNull();
  expect(screen.queryByLabelText('Public response')).toBeNull();
  expect(seen.filter(body => body.action === 'confirm_reply')).toEqual([{ action: 'confirm_reply', confirmation_token: 'token1', confirmed: true }]);
  if (known) {
    expect(screen.queryByRole('button', { name: 'Check receipt' })).toBeNull();
    await fireEvent.click(screen.getByRole('button', { name: 'Refresh reviews' }));
    await fireEvent.click(await screen.findByRole('button', { name: /Example Reviewer/ }));
    await fireEvent.input(await screen.findByLabelText('Public response'), { target: { value: 'Fresh response.' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Preview response' }));
    expect(within(await screen.findByRole('dialog')).getByText('receipt2')).toBeDefined();
    expect(seen.filter(body => body.action === 'preview_reply')).toHaveLength(2);
    expect(seen.filter(body => body.action === 'receipt')).toHaveLength(0);
  } else {
    await fireEvent.click(screen.getByRole('button', { name: 'Check receipt' }));
    await screen.findByText('receipt1');
    expect(seen.filter(body => body.action === 'receipt')).toEqual([{ action: 'receipt', receipt_id: 'receipt1' }]);
  }
  expect(seen.filter(body => body.action === 'confirm_reply')).toHaveLength(1);
  expect(document.body.textContent).not.toContain('private-confirm-canary');
});

it.each(['lost response', 'unknown', 'submitting', 'pending_confirmation'])('preserves %s receipt recovery through refresh, detail and pagination without resubmission', async status => {
  const second = { ...review, reviewId: 'r2', reviewer: { displayName: 'Another Reviewer' } };
  const { client, seen } = fixture(body => {
    if (body.action === 'list') return { reviews: [body.page_token ? second : review], next_page_token: body.page_token ? undefined : 'page2' };
    if (body.action === 'get') return { review: body.review_id === 'r2' ? second : review };
    if (body.action === 'preview_reply') return { preview: { review, comment: body.comment, confirmation_token: 'token1', receipt_id: 'receipt1', expires_at: '2099-01-01T00:00:00Z' } };
    if (body.action === 'confirm_reply') {
      if (status === 'lost response' || status === 'submitting' || status === 'pending_confirmation') throw new TypeError('Connection lost');
      return { receipt: { id: 'receipt1', status, created_at: '', updated_at: '' } };
    }
    return { receipt: { id: 'receipt1', status: status === 'lost response' ? 'unknown' : status, created_at: '', updated_at: '' } };
  });
  const view = render(GoogleReviewsWorkspace, { client });
  await screen.findByRole('option', { name: 'shop' });
  await fireEvent.change(screen.getByLabelText('Target'), { target: { value: 'shop' } });
  await fireEvent.click(await screen.findByRole('button', { name: /Example Reviewer/ }));
  await fireEvent.input(await screen.findByLabelText('Public response'), { target: { value: 'Thanks.' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Preview response' }));
  const dialog = await screen.findByRole('dialog', { name: 'Confirm public response' });
  await fireEvent.click(within(dialog).getByRole('button', { name: 'Submit' }));
  const notice = status === 'lost response' || status === 'unknown' ? /Outcome unknown/ : status === 'submitting' ? /Submission is in progress/ : /Receipt is awaiting confirmation/;
  await screen.findByText(status === 'lost response' || status === 'submitting' || status === 'pending_confirmation' ? /Outcome unknown/ : notice);
  await fireEvent.click(screen.getByRole('button', { name: 'Check receipt' }));
  await screen.findByText(notice);
  function expectRecovery() {
    expect(screen.getByText(notice)).toBeDefined();
    expect(screen.getByRole('button', { name: 'Check receipt' })).toBeDefined();
    if (status !== 'unknown' && status !== 'lost response') expect(screen.queryByLabelText('Public response')).toBeNull();
  }
  await fireEvent.click(screen.getByRole('button', { name: 'Refresh reviews' }));
  const firstRow = await screen.findByRole('button', { name: /Example Reviewer/ });
  expectRecovery();
  await fireEvent.click(firstRow);
  await screen.findByRole('article', { name: 'Review detail' });
  expectRecovery();
  await fireEvent.click(screen.getByRole('button', { name: 'Next page' }));
  const secondRow = await screen.findByRole('button', { name: /Another Reviewer/ });
  expectRecovery();
  await fireEvent.click(secondRow);
  await screen.findByRole('article', { name: 'Review detail' });
  expectRecovery();
  expect(seen.filter(body => body.action === 'receipt')).toEqual([{ action: 'receipt', receipt_id: 'receipt1' }]);
  expect(seen.filter(body => body.action === 'confirm_reply')).toEqual([{ action: 'confirm_reply', confirmation_token: 'token1', confirmed: true }]);
  expect(seen.filter(body => body.action === 'get').map(body => body.review_id)).toEqual(['r1', 'r1', 'r2']);
  expect(seen.some(body => body.action === 'list' && body.page_token === 'page2')).toBe(true);
  expect(seen.filter(body => body.action === 'preview_reply')).toHaveLength(1);
  expect(seen.filter(body => body.action === 'confirm_reply')).toHaveLength(1);
  view.unmount();
  expect(screen.queryByRole('button', { name: 'Check receipt' })).toBeNull();
  expect(localStorage.length).toBe(0);
  expect(sessionStorage.length).toBe(0);
});

it.each(['lost response', 'unknown'])('allows a deliberate fresh preview after %s, persistent unknown receipt and successful live inspection', async initial => {
  const inspected = { ...review, reviewReply: { comment: 'Freshly inspected response' } };
  let finishDetail!: (value: unknown) => void;
  let reads = 0;
  let previews = 0;
  const { client, seen } = fixture(body => {
    if (body.action === 'list') return { reviews: [review] };
    if (body.action === 'get') return ++reads === 1 ? { review } : new Promise(resolve => finishDetail = resolve);
    if (body.action === 'preview_reply') {
      previews++;
      return { preview: { review: previews === 1 ? review : inspected, comment: body.comment, confirmation_token: `token${previews}`, receipt_id: `receipt${previews}`, expires_at: '2099-01-01T00:00:00Z' } };
    }
    if (body.action === 'confirm_reply') {
      if (body.confirmation_token === 'token2') throw new TypeError('New confirmation response lost');
      if (initial === 'lost response') throw new TypeError('Connection lost');
    }
    return { receipt: { id: body.action === 'receipt' ? body.receipt_id : 'receipt1', status: 'unknown', created_at: '', updated_at: '' } };
  });
  render(GoogleReviewsWorkspace, { client });
  await screen.findByRole('option', { name: 'shop' });
  await fireEvent.change(screen.getByLabelText('Target'), { target: { value: 'shop' } });
  await fireEvent.click(await screen.findByRole('button', { name: /Example Reviewer/ }));
  await fireEvent.input(await screen.findByLabelText('Public response'), { target: { value: 'Original response.' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Preview response' }));
  await fireEvent.click(within(await screen.findByRole('dialog')).getByRole('button', { name: 'Submit' }));
  await screen.findByText(/Outcome unknown/);
  await fireEvent.click(screen.getByRole('button', { name: 'Check receipt' }));
  await screen.findByText('receipt1');
  expect(screen.queryByLabelText('Public response')).toBeNull();
  await fireEvent.click(screen.getByRole('button', { name: 'Refresh reviews' }));
  await fireEvent.click(await screen.findByRole('button', { name: /Example Reviewer/ }));
  await waitFor(() => expect(finishDetail).toBeTypeOf('function'));
  expect(screen.queryByLabelText('Public response')).toBeNull();
  finishDetail({ review: inspected });
  await fireEvent.input(await screen.findByLabelText('Public response'), { target: { value: 'New intentional response.' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Preview response' }));
  const fresh = await screen.findByRole('dialog', { name: 'Confirm public response' });
  expect(within(fresh).getByTestId('exact-response').textContent).toBe('New intentional response.');
  expect(within(fresh).getByText('Freshly inspected response')).toBeDefined();
  expect(within(fresh).getByText('receipt2')).toBeDefined();
  expect(screen.getByText('receipt1')).toBeDefined();
  expect(seen.filter(body => body.action === 'receipt')).toEqual([{ action: 'receipt', receipt_id: 'receipt1' }]);
  expect(seen.filter(body => body.action === 'confirm_reply')).toEqual([{ action: 'confirm_reply', confirmation_token: 'token1', confirmed: true }]);
  expect(seen.filter(body => body.action === 'preview_reply')).toHaveLength(2);
  await fireEvent.click(within(fresh).getByRole('button', { name: 'Submit' }));
  await screen.findByText(/Outcome unknown/);
  await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
  expect(seen.filter(body => body.action === 'confirm_reply')).toEqual([
    { action: 'confirm_reply', confirmation_token: 'token1', confirmed: true },
    { action: 'confirm_reply', confirmation_token: 'token2', confirmed: true },
  ]);
  expect(screen.queryByText('receipt1')).toBeNull();
  await fireEvent.click(screen.getByRole('button', { name: 'Refresh reviews' }));
  await fireEvent.click(await screen.findByRole('button', { name: /Example Reviewer/ }));
  await waitFor(() => expect(reads).toBe(3));
  finishDetail({ review: inspected });
  await screen.findByRole('article', { name: 'Review detail' });
  await screen.findByText(/Outcome unknown\. The live review was inspected\./);
  expect(screen.getByLabelText('Public response')).toBeDefined();
  await fireEvent.click(screen.getByRole('button', { name: 'Check receipt' }));
  await screen.findByText('receipt2');
  expect(screen.getByLabelText('Public response')).toBeDefined();
  expect(seen.filter(body => body.action === 'receipt')).toEqual([
    { action: 'receipt', receipt_id: 'receipt1' },
    { action: 'receipt', receipt_id: 'receipt2' },
  ]);
});

it.each(['submitted', 'failed'])('allows a fresh preview after navigating from a resolved %s receipt', async status => {
  const { client, seen } = fixture(body => {
    if (body.action === 'list') return { reviews: [review] };
    if (body.action === 'get') return { review };
    if (body.action === 'preview_reply') return { preview: { review, comment: body.comment, confirmation_token: 'token1', receipt_id: 'receipt1', expires_at: '2099-01-01T00:00:00Z' } };
    return { receipt: { id: 'receipt1', status, created_at: '', updated_at: '' } };
  });
  render(GoogleReviewsWorkspace, { client });
  await screen.findByRole('option', { name: 'shop' });
  await fireEvent.change(screen.getByLabelText('Target'), { target: { value: 'shop' } });
  await fireEvent.click(await screen.findByRole('button', { name: /Example Reviewer/ }));
  await fireEvent.input(await screen.findByLabelText('Public response'), { target: { value: 'Thanks.' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Preview response' }));
  await fireEvent.click(within(await screen.findByRole('dialog')).getByRole('button', { name: 'Submit' }));
  await screen.findByText(status === 'submitted' ? /Submitted to Google/ : /Submission failed/);
  await fireEvent.click(screen.getByRole('button', { name: 'Refresh reviews' }));
  await fireEvent.click(await screen.findByRole('button', { name: /Example Reviewer/ }));
  await fireEvent.input(await screen.findByLabelText('Public response'), { target: { value: 'Fresh response.' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Preview response' }));
  expect(within(await screen.findByRole('dialog')).getByTestId('exact-response').textContent).toBe('Fresh response.');
  expect(seen.filter(body => body.action === 'confirm_reply')).toHaveLength(1);
});

it('clears unresolved recovery on target switch and ignores a late receipt response', async () => {
  let finish!: (value: unknown) => void;
  const { client, seen } = fixture(body => {
    if (body.action === 'list') return { reviews: body.target === 'shop' ? [review] : [] };
    if (body.action === 'get') return { review };
    if (body.action === 'preview_reply') return { preview: { review, comment: body.comment, confirmation_token: 'token1', receipt_id: 'receipt1', expires_at: '2099-01-01T00:00:00Z' } };
    if (body.action === 'confirm_reply') throw new TypeError('Connection lost');
    if (body.action === 'receipt') return new Promise(resolve => finish = resolve);
    return {};
  });
  const view = render(GoogleReviewsWorkspace, { client });
  await screen.findByRole('option', { name: 'shop' });
  await fireEvent.change(screen.getByLabelText('Target'), { target: { value: 'shop' } });
  await fireEvent.click(await screen.findByRole('button', { name: /Example Reviewer/ }));
  await fireEvent.input(await screen.findByLabelText('Public response'), { target: { value: 'Thanks.' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Preview response' }));
  await fireEvent.click(within(await screen.findByRole('dialog')).getByRole('button', { name: 'Submit' }));
  await screen.findByText(/Outcome unknown/);
  await fireEvent.click(screen.getByRole('button', { name: 'Check receipt' }));
  await waitFor(() => expect(finish).toBeTypeOf('function'));
  await fireEvent.change(screen.getByLabelText('Target'), { target: { value: 'other' } });
  await screen.findByText('No reviews on this page.');
  await finishStale(finish, { receipt: { id: 'receipt1', status: 'submitting', created_at: '', updated_at: '' } });
  expect(screen.queryByRole('button', { name: 'Check receipt' })).toBeNull();
  expect(screen.queryByText(/Outcome unknown|Submission is in progress/)).toBeNull();
  expect(screen.queryByText('receipt1')).toBeNull();
  await fireEvent.change(screen.getByLabelText('Target'), { target: { value: 'shop' } });
  await screen.findByRole('button', { name: /Example Reviewer/ });
  expect(screen.queryByRole('button', { name: 'Check receipt' })).toBeNull();
  expect(seen.filter(body => body.action === 'confirm_reply')).toHaveLength(1);
  view.unmount();
  expect(screen.queryByText('Example Reviewer')).toBeNull();
});

it.each(['expired receipt', 'daemon restart'])('requires live inspection before a fresh preview after %s', async scenario => {
  let previews = 0;
  let confirmations = 0;
  let daemonRestarted = false;
  const { client, seen } = fixture(body => {
    if (body.action === 'list') return { reviews: [review] };
    if (body.action === 'get') return { review };
    if (body.action === 'preview_reply') {
      previews++;
      return { preview: { review, comment: body.comment, confirmation_token: `token${previews}`, receipt_id: `receipt${previews}`, expires_at: '2099-01-01T00:00:00Z' } };
    }
    if (body.action === 'confirm_reply') {
      confirmations++;
      if (scenario === 'daemon restart') daemonRestarted = true;
      throw new TypeError('Connection lost');
    }
    if (body.action === 'receipt' && (scenario === 'expired receipt' || daemonRestarted)) {
      return Response.json({ error: 'google_reviews_receipt_not_found' }, { status: 404 });
    }
    return { receipt: { id: body.receipt_id, status: 'unknown', created_at: '', updated_at: '' } };
  });
  render(GoogleReviewsWorkspace, { client });
  await screen.findByRole('option', { name: 'shop' });
  await fireEvent.change(screen.getByLabelText('Target'), { target: { value: 'shop' } });
  await fireEvent.click(await screen.findByRole('button', { name: /Example Reviewer/ }));
  await fireEvent.input(await screen.findByLabelText('Public response'), { target: { value: 'Original response.' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Preview response' }));
  await fireEvent.click(within(await screen.findByRole('dialog')).getByRole('button', { name: 'Submit' }));
  await screen.findByText(/Outcome unknown/);
  await fireEvent.click(screen.getByRole('button', { name: 'Check receipt' }));
  await screen.findByText('Receipt unavailable. Inspect the live review before creating a fresh preview; do not retry the old confirmation.');
  expect(screen.queryByLabelText('Public response')).toBeNull();
  await fireEvent.click(screen.getByRole('button', { name: 'Inspect live review' }));
  await screen.findByRole('article', { name: 'Review detail' });
  expect(screen.getByLabelText('Public response')).toBeDefined();
  await fireEvent.input(screen.getByLabelText('Public response'), { target: { value: 'Fresh intentional response.' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Preview response' }));
  expect(within(await screen.findByRole('dialog')).getByTestId('exact-response').textContent).toBe('Fresh intentional response.');
  expect(seen.filter(body => body.action === 'get')).toEqual([{ action: 'get', target: 'shop', review_id: 'r1' }, { action: 'get', target: 'shop', review_id: 'r1' }]);
  expect(seen.filter(body => body.action === 'preview_reply')).toHaveLength(2);
  expect(confirmations).toBe(1);
});

it('uses the requested workspace copy, preview failure text, and submit label', async () => {
  let actionable = false;
  const { client } = fixture(body => {
    if (body.action === 'list') return { reviews: [review] };
    if (body.action === 'get') return { review };
    if (body.action === 'preview_reply') {
      return actionable
        ? { preview: { review, comment: body.comment, confirmation_token: 'token1', receipt_id: 'receipt1', expires_at: '2099-01-01T00:00:00Z' } }
        : { preview: { review, comment: body.comment } };
    }
    return {};
  });
  render(GoogleReviewsWorkspace, { client });
  expect(screen.getByText('Use your own approved Google project.')).toBeDefined();
  await screen.findByRole('option', { name: 'shop' });
  await fireEvent.change(screen.getByLabelText('Target'), { target: { value: 'shop' } });
  const row = await screen.findByRole('button', { name: /Example Reviewer/ });
  await waitFor(() => expect((row as HTMLButtonElement).disabled).toBe(false));
  await fireEvent.click(row);
  await screen.findByRole('article', { name: 'Review detail' });
  await fireEvent.input(screen.getByLabelText('Public response'), { target: { value: 'Thanks.' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Preview response' }));
  expect((await screen.findByRole('alert')).textContent).toBe('Preview unavailable. Check owner permission to submit replies.');
  await fireEvent.click(screen.getByRole('button', { name: 'Refresh reviews' }));
  const refreshedRow = await screen.findByRole('button', { name: /Example Reviewer/ });
  await waitFor(() => expect((refreshedRow as HTMLButtonElement).disabled).toBe(false));
  await fireEvent.click(refreshedRow);
  await screen.findByRole('article', { name: 'Review detail' });
  actionable = true;
  await fireEvent.input(screen.getByLabelText('Public response'), { target: { value: 'Thanks again.' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Preview response' }));
  expect(within(await screen.findByRole('dialog')).getByRole('button', { name: 'Submit' })).toBeDefined();
});

it('ignores late detail and list responses after changing target and clears content on errors', async () => {
  let finish!: (value: unknown) => void;
  const { client } = fixture(body => {
    if (body.target === 'other') return { reviews: [] };
    if (body.action === 'get') return new Promise(resolve => finish = resolve);
    return { reviews: [review] };
  });
  render(GoogleReviewsWorkspace, { client });
  await screen.findByRole('option', { name: 'shop' });
  await fireEvent.change(screen.getByLabelText('Target'), { target: { value: 'shop' } });
  await fireEvent.click(await screen.findByRole('button', { name: /Example Reviewer/ }));
  await fireEvent.change(screen.getByLabelText('Target'), { target: { value: 'other' } });
  await screen.findByText('No reviews on this page.');
  await finishStale(finish, { review });
  expect(screen.queryByText('<b>Helpful service</b>')).toBeNull();
  expect(screen.queryByRole('textbox', { name: 'Public response' })).toBeNull();
});

it('clears loaded content on a denied detail and allows a fresh read', async () => {
  const { client } = fixture(body => body.action === 'list' ? { reviews: [review] } : body.action === 'archive_status' ? {} : Response.json({ message: 'Google Reviews access denied' }, { status: 403 }));
  render(GoogleReviewsWorkspace, { client });
  await screen.findByRole('option', { name: 'shop' });
  await fireEvent.change(screen.getByLabelText('Target'), { target: { value: 'shop' } });
  await fireEvent.click(await screen.findByRole('button', { name: /Example Reviewer/ }));
  await screen.findByRole('alert');
  expect(screen.queryByRole('button', { name: /Example Reviewer/ })).toBeNull();
  expect(screen.queryByRole('textbox', { name: 'Public response' })).toBeNull();
  expect(screen.getByRole('button', { name: 'Refresh reviews' })).toBeDefined();
});

it('shows read-only policy without a response editor and clears everything on unmount', async () => {
  const { client } = fixture(body => body.action === 'list' ? { reviews: [review] } : { review });
  const view = render(GoogleReviewsWorkspace, { client });
  await screen.findByRole('option', { name: 'other' });
  await fireEvent.change(screen.getByLabelText('Target'), { target: { value: 'other' } });
  await fireEvent.click(await screen.findByRole('button', { name: /Example Reviewer/ }));
  await screen.findByText('Responses are disabled for this target.');
  expect(screen.queryByRole('textbox', { name: 'Public response' })).toBeNull();
  view.unmount();
  expect(screen.queryByText('<b>Helpful service</b>')).toBeNull();
  expect(localStorage.length).toBe(0);
  expect(sessionStorage.length).toBe(0);
});

it('ignores a late page after target switch and after unmount', async () => {
  let finish!: (value: unknown) => void;
  const { client } = fixture(body => body.action === 'list' && body.target === 'shop' ? new Promise(resolve => finish = resolve) : { reviews: [] });
  const view = render(GoogleReviewsWorkspace, { client });
  await screen.findByRole('option', { name: 'shop' });
  await fireEvent.change(screen.getByLabelText('Target'), { target: { value: 'shop' } });
  await waitFor(() => expect(finish).toBeTypeOf('function'));
  await fireEvent.change(screen.getByLabelText('Target'), { target: { value: 'other' } });
  await screen.findByText('No reviews on this page.');
  await finishStale(finish, { reviews: [review] });
  expect(screen.queryByRole('button', { name: /Example Reviewer/ })).toBeNull();
  const previousFinish = finish;
  await fireEvent.change(screen.getByLabelText('Target'), { target: { value: 'shop' } });
  await waitFor(() => expect(finish).not.toBe(previousFinish));
  view.unmount();
  await finishStale(finish, { reviews: [review] });
  expect(screen.queryByText('Example Reviewer')).toBeNull();
});

it('renders rating-only anonymous reviews and never offers confirmation on a denied preview', async () => {
  const anonymous = { reviewId: 'anon', reviewer: { isAnonymous: true }, starRating: 'THREE' };
  const { client, seen } = fixture(body => {
    if (body.action === 'list') return { reviews: [anonymous] };
    if (body.action === 'get') return { review: anonymous };
    if (body.action === 'archive_status') return {};
    return Response.json({}, { status: 403 });
  });
  render(GoogleReviewsWorkspace, { client });
  await screen.findByRole('option', { name: 'shop' });
  await fireEvent.change(screen.getByLabelText('Target'), { target: { value: 'shop' } });
  await fireEvent.click(await screen.findByRole('button', { name: /Anonymous reviewer/ }));
  await screen.findByText('Rating only; no review text.');
  await fireEvent.input(screen.getByLabelText('Public response'), { target: { value: 'Thanks.' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Preview response' }));
  await screen.findByRole('alert');
  expect(screen.queryByRole('dialog')).toBeNull();
  expect(screen.queryByText('Rating only; no review text.')).toBeNull();
  expect(seen.some(body => body.action === 'confirm_reply')).toBe(false);
});

it('shows moderation from list metadata when the detail endpoint omits it', async () => {
  const { reviewReply, ...withoutReply } = review;
  const detail = { ...withoutReply, reviewReply: { comment: reviewReply.comment } };
  const { client } = fixture(body => body.action === 'list' ? { reviews: [review] } : { review: detail });
  render(GoogleReviewsWorkspace, { client });
  await screen.findByRole('option', { name: 'shop' });
  await fireEvent.change(screen.getByLabelText('Target'), { target: { value: 'shop' } });
  const row = await screen.findByRole('button', { name: /Example Reviewer/ });
  expect(within(row).getByText(/PENDING/)).toBeDefined();
  expect(within(row).getByText(/OFF_TOPIC/)).toBeDefined();
  await fireEvent.click(row);
  await screen.findByText('Previous response');
  expect(within(row).getByText(/PENDING/)).toBeDefined();
});
