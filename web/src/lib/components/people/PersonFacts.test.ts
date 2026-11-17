import { cleanup, fireEvent, render, screen } from '@testing-library/svelte';
import { afterEach, expect, it, vi } from 'vitest';
import { createAPIClient } from '../../api/client';
import { requestOf } from '../../directory/entity-fixtures.test-support';
import PersonFacts from './PersonFacts.svelte';

afterEach(() => { cleanup(); vi.restoreAllMocks(); });

it('shows the field count and latest date in the closed header', async () => {
  const fetchFn = vi.fn<typeof fetch>(async input => {
    if (new URL(requestOf(input).url).pathname.endsWith('/fact-summary')) return Response.json({ claimed_fields: 73, latest_claim_at: '2026-11-01T12:00:00Z', has_values: false, has_fact_history: true });
    throw new Error('unexpected request');
  });
  render(PersonFacts, { client: createAPIClient(fetchFn), personID: 42 });
  const summary = await screen.findByText('73 fields · last found Nov 1');
  expect(summary.closest('header')?.textContent).toContain('Facts');
  expect(screen.getByRole('button', { name: 'Show' })).toBeDefined();
});

it('discards a summary response for the previous person', async () => {
  let oldRequest: Request | undefined;
  let resolveOld!: (response: Response) => void;
  const oldResponse = new Promise<Response>(resolve => { resolveOld = resolve; });
  const fetchFn = vi.fn<typeof fetch>(async input => {
    const request = requestOf(input);
    if (new URL(request.url).pathname.endsWith('/people/42/fact-summary')) { oldRequest = request; return oldResponse; }
    return Response.json({ claimed_fields: 2, latest_claim_at: '2026-11-03T12:00:00Z', has_values: false, has_fact_history: true });
  });
  const client = createAPIClient(fetchFn);
  const view = render(PersonFacts, { client, personID: 42 });
  await vi.waitFor(() => expect(oldRequest).toBeDefined());
  await view.rerender({ client, personID: 43 });
  expect(oldRequest!.signal.aborted).toBe(true);
  resolveOld(Response.json({ claimed_fields: 9, latest_claim_at: '2026-11-01T12:00:00Z', has_values: false, has_fact_history: true }));
  await vi.waitFor(() => expect(screen.getByText('2 fields · last found Nov 3')).toBeDefined());
  expect(screen.queryByText('9 fields · last found Nov 1')).toBeNull();
});

it('keeps Claims available beside Set up with no stored facts', async () => {
  const fetchFn = vi.fn<typeof fetch>(async input => {
    const path = new URL(requestOf(input).url).pathname;
    if (path.endsWith('/fact-summary')) return Response.json({ claimed_fields: 0, latest_claim_at: null, has_values: false, has_fact_history: false });
    if (path.endsWith('/person-fact-targets')) return Response.json({ targets: [] });
    if (path.endsWith('/fact-claims')) return Response.json({ claims: [] });
    if (path.endsWith('/fact-evidence')) return Response.json({ evidence: [] });
    if (path.endsWith('/fact-decisions')) return Response.json({ decisions: [] });
    if (path.endsWith('/fact-pins')) return Response.json({ pins: [] });
    throw new Error('unexpected request');
  });
  render(PersonFacts, { client: createAPIClient(fetchFn), personID: 42, onSetUp: vi.fn() });
  expect(screen.getByRole('button', { name: 'Set up Facts' })).toBeDefined();
  await fireEvent.click(screen.getByRole('button', { name: 'Show' }));
  expect(await screen.findByRole('radio', { name: 'Claims' })).toBeDefined();
});
