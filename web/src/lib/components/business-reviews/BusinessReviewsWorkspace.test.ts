import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import { afterEach, expect, it, vi } from 'vitest';
import { tick } from 'svelte';
import { createAPIClient } from '../../api/client';
import BusinessReviewsWorkspace from './BusinessReviewsWorkspace.svelte';

const review = { reviewId: 'r1', reviewer: { displayName: 'Example Reviewer', profilePhotoUrl: 'https://example.invalid/photo' }, starRating: 'FIVE', comment: '<b>Helpful service</b>', reviewReply: { comment: 'Previous response', reviewReplyState: 'PENDING', policyViolation: 'OFF_TOPIC' } };
const capabilities = { capabilities: [{ provider: 'google', available: true, read: true, reply: true }, { provider: 'facebook', available: false, read: false, reply: false, reason: 'Official ratings and reply contract unverified.' }], targets: [{ name: 'shop', provider: 'google', enabled: true, replies_enabled: true }, { name: 'other', provider: 'google', enabled: true, replies_enabled: false }] };
type Body = Record<string, unknown>;
function fixture(handle: (body: Body) => unknown | Promise<unknown>) {
  const seen: Body[] = [];
  const client = createAPIClient(async input => {
    const request = input as Request;
    expect(new URL(request.url).pathname).toBe('/api/v1/business-reviews/control');
    expect(request.method).toBe('POST');
    expect(request.cache).toBe('no-store');
    const body = await request.json() as Body;
    seen.push(body);
    const result = body.action === 'capabilities' ? capabilities : await handle(body);
    return result instanceof Response ? result : Response.json(result);
  });
  return { client, seen };
}
afterEach(cleanup);

it('loads targets, paginates live reviews and escapes detail without fetching profile photos', async () => {
  const { client, seen } = fixture(body => body.action === 'get' ? { review } : { reviews: [review], next_page_token: body.page_token ? undefined : 'page2' });
  render(BusinessReviewsWorkspace, { client });
  await screen.findByText(/Facebook.*unavailable/i);
  await fireEvent.change(screen.getByRole('combobox', { name: 'Target' }), { target: { value: 'shop' } });
  await fireEvent.click(await screen.findByRole('button', { name: /Example Reviewer/ }));
  expect(await screen.findByText('<b>Helpful service</b>')).toBeDefined();
  expect(screen.getByText('Previous response')).toBeDefined();
  expect(within(screen.getByRole('article', { name: 'Review detail' })).getByText(/PENDING/)).toBeDefined();
  expect(within(screen.getByRole('article', { name: 'Review detail' })).getByText(/OFF_TOPIC/)).toBeDefined();
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
  render(BusinessReviewsWorkspace, { client });
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
  await fireEvent.click(within(dialog).getByRole('button', { name: 'Confirm and submit' }));
  await screen.findByText(/Outcome unknown/);
  expect(screen.queryByRole('button', { name: 'Confirm and submit' })).toBeNull();
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
      return Response.json({ error: failure === 'unrecognized conflict' ? 'proxy_conflict' : 'business_reviews_preview_changed', message: 'private-confirm-canary' }, { status: failure === 'forged server error' ? 500 : 409 });
    }
    return { receipt: { id: 'receipt1', status: known ? 'failed' : 'unknown', created_at: '', updated_at: '' } };
  });
  render(BusinessReviewsWorkspace, { client });
  await screen.findByRole('option', { name: 'shop' });
  await fireEvent.change(screen.getByLabelText('Target'), { target: { value: 'shop' } });
  await fireEvent.click(await screen.findByRole('button', { name: /Example Reviewer/ }));
  await fireEvent.input(await screen.findByLabelText('Public response'), { target: { value: 'Original response.' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Preview response' }));
  await fireEvent.click(within(await screen.findByRole('dialog')).getByRole('button', { name: 'Confirm and submit' }));
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
  await fireEvent.click(screen.getByRole('button', { name: 'Check receipt' }));
  await screen.findByText('receipt1');
  expect(seen.filter(body => body.action === 'receipt')).toEqual([{ action: 'receipt', receipt_id: 'receipt1' }]);
  if (known) {
    await screen.findByText(/Submission failed/);
    await fireEvent.click(screen.getByRole('button', { name: 'Refresh reviews' }));
    await fireEvent.click(await screen.findByRole('button', { name: /Example Reviewer/ }));
    await fireEvent.input(await screen.findByLabelText('Public response'), { target: { value: 'Fresh response.' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Preview response' }));
    expect(within(await screen.findByRole('dialog')).getByText('receipt2')).toBeDefined();
    expect(seen.filter(body => body.action === 'preview_reply')).toHaveLength(2);
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
      if (status === 'lost response') throw new TypeError('Connection lost');
      return { receipt: { id: 'receipt1', status, created_at: '', updated_at: '' } };
    }
    return { receipt: { id: 'receipt1', status: status === 'lost response' || status === 'unknown' ? 'unknown' : 'submitted', created_at: '', updated_at: '' } };
  });
  const view = render(BusinessReviewsWorkspace, { client });
  await screen.findByRole('option', { name: 'shop' });
  await fireEvent.change(screen.getByLabelText('Target'), { target: { value: 'shop' } });
  await fireEvent.click(await screen.findByRole('button', { name: /Example Reviewer/ }));
  await fireEvent.input(await screen.findByLabelText('Public response'), { target: { value: 'Thanks.' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Preview response' }));
  const dialog = await screen.findByRole('dialog', { name: 'Confirm public response' });
  await fireEvent.click(within(dialog).getByRole('button', { name: 'Confirm and submit' }));
  const notice = status === 'lost response' || status === 'unknown' ? /Outcome unknown/ : status === 'submitting' ? /Submission is in progress/ : /Receipt is awaiting confirmation/;
  await screen.findByText(notice);
  function expectRecovery() {
    expect(screen.getByText(notice)).toBeDefined();
    expect(screen.getByRole('button', { name: 'Check receipt' })).toBeDefined();
    if (status !== 'unknown') expect(screen.queryByLabelText('Public response')).toBeNull();
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
  await fireEvent.click(screen.getByRole('button', { name: 'Check receipt' }));
  await screen.findByText(status === 'lost response' || status === 'unknown' ? /Outcome unknown/ : /Submitted to Google/);
  await waitFor(() => expect((screen.getByRole('button', { name: 'Check receipt' }) as HTMLButtonElement).disabled).toBe(false));
  expect(seen.filter(body => body.action === 'receipt')).toEqual([{ action: 'receipt', receipt_id: 'receipt1' }]);
  expect(seen.filter(body => body.action === 'confirm_reply')).toEqual([{ action: 'confirm_reply', confirmation_token: 'token1', confirmed: true }]);
  expect(seen.filter(body => body.action === 'get').map(body => body.review_id)).toEqual(['r1', 'r1', 'r2']);
  expect(seen.some(body => body.action === 'list' && body.page_token === 'page2')).toBe(true);
  await fireEvent.click(secondRow);
  await fireEvent.input(await screen.findByLabelText('Public response'), { target: { value: 'Fresh response.' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Preview response' }));
  expect(within(await screen.findByRole('dialog')).getByTestId('exact-response').textContent).toBe('Fresh response.');
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
  render(BusinessReviewsWorkspace, { client });
  await screen.findByRole('option', { name: 'shop' });
  await fireEvent.change(screen.getByLabelText('Target'), { target: { value: 'shop' } });
  await fireEvent.click(await screen.findByRole('button', { name: /Example Reviewer/ }));
  await fireEvent.input(await screen.findByLabelText('Public response'), { target: { value: 'Original response.' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Preview response' }));
  await fireEvent.click(within(await screen.findByRole('dialog')).getByRole('button', { name: 'Confirm and submit' }));
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
  await fireEvent.click(within(fresh).getByRole('button', { name: 'Confirm and submit' }));
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
  expect(screen.queryByLabelText('Public response')).toBeNull();
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
  render(BusinessReviewsWorkspace, { client });
  await screen.findByRole('option', { name: 'shop' });
  await fireEvent.change(screen.getByLabelText('Target'), { target: { value: 'shop' } });
  await fireEvent.click(await screen.findByRole('button', { name: /Example Reviewer/ }));
  await fireEvent.input(await screen.findByLabelText('Public response'), { target: { value: 'Thanks.' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Preview response' }));
  await fireEvent.click(within(await screen.findByRole('dialog')).getByRole('button', { name: 'Confirm and submit' }));
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
    if (body.action === 'confirm_reply') return { receipt: { id: 'receipt1', status: 'submitting', created_at: '', updated_at: '' } };
    return new Promise(resolve => finish = resolve);
  });
  const view = render(BusinessReviewsWorkspace, { client });
  await screen.findByRole('option', { name: 'shop' });
  await fireEvent.change(screen.getByLabelText('Target'), { target: { value: 'shop' } });
  await fireEvent.click(await screen.findByRole('button', { name: /Example Reviewer/ }));
  await fireEvent.input(await screen.findByLabelText('Public response'), { target: { value: 'Thanks.' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Preview response' }));
  await fireEvent.click(within(await screen.findByRole('dialog')).getByRole('button', { name: 'Confirm and submit' }));
  await screen.findByText(/Submission is in progress/);
  await fireEvent.click(screen.getByRole('button', { name: 'Check receipt' }));
  await waitFor(() => expect(finish).toBeTypeOf('function'));
  await fireEvent.change(screen.getByLabelText('Target'), { target: { value: 'other' } });
  await screen.findByText('No reviews on this page.');
  finish({ receipt: { id: 'receipt1', status: 'unknown', created_at: '', updated_at: '' } });
  await tick();
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

it('ignores late detail and list responses after changing target and clears content on errors', async () => {
  let finish!: (value: unknown) => void;
  const { client } = fixture(body => {
    if (body.target === 'other') return { reviews: [] };
    if (body.action === 'get') return new Promise(resolve => finish = resolve);
    return { reviews: [review] };
  });
  render(BusinessReviewsWorkspace, { client });
  await screen.findByRole('option', { name: 'shop' });
  await fireEvent.change(screen.getByLabelText('Target'), { target: { value: 'shop' } });
  await fireEvent.click(await screen.findByRole('button', { name: /Example Reviewer/ }));
  await fireEvent.change(screen.getByLabelText('Target'), { target: { value: 'other' } });
  await screen.findByText('No reviews on this page.');
  finish({ review });
  await tick();
  expect(screen.queryByText('<b>Helpful service</b>')).toBeNull();
  expect(screen.queryByRole('textbox', { name: 'Public response' })).toBeNull();
});

it('clears loaded content on a denied detail and allows a fresh read', async () => {
  const { client } = fixture(body => body.action === 'list' ? { reviews: [review] } : Response.json({ message: 'Business review access denied' }, { status: 403 }));
  render(BusinessReviewsWorkspace, { client });
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
  const view = render(BusinessReviewsWorkspace, { client });
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
  const { client } = fixture(body => body.target === 'shop' ? new Promise(resolve => finish = resolve) : { reviews: [] });
  const view = render(BusinessReviewsWorkspace, { client });
  await screen.findByRole('option', { name: 'shop' });
  await fireEvent.change(screen.getByLabelText('Target'), { target: { value: 'shop' } });
  await waitFor(() => expect(finish).toBeTypeOf('function'));
  await fireEvent.change(screen.getByLabelText('Target'), { target: { value: 'other' } });
  await screen.findByText('No reviews on this page.');
  finish({ reviews: [review] });
  await tick();
  expect(screen.queryByRole('button', { name: /Example Reviewer/ })).toBeNull();
  await fireEvent.change(screen.getByLabelText('Target'), { target: { value: 'shop' } });
  view.unmount();
  finish({ reviews: [review] });
  await tick();
  expect(screen.queryByText('Example Reviewer')).toBeNull();
});

it('renders rating-only anonymous reviews and never offers confirmation on a denied preview', async () => {
  const anonymous = { reviewId: 'anon', reviewer: { isAnonymous: true }, starRating: 'THREE' };
  const { client, seen } = fixture(body => {
    if (body.action === 'list') return { reviews: [anonymous] };
    if (body.action === 'get') return { review: anonymous };
    return Response.json({}, { status: 403 });
  });
  render(BusinessReviewsWorkspace, { client });
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
  render(BusinessReviewsWorkspace, { client });
  await screen.findByRole('option', { name: 'shop' });
  await fireEvent.change(screen.getByLabelText('Target'), { target: { value: 'shop' } });
  const row = await screen.findByRole('button', { name: /Example Reviewer/ });
  expect(within(row).getByText(/PENDING/)).toBeDefined();
  expect(within(row).getByText(/OFF_TOPIC/)).toBeDefined();
  await fireEvent.click(row);
  await screen.findByText('Previous response');
  expect(within(row).getByText(/PENDING/)).toBeDefined();
});
