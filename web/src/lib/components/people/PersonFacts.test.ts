import { cleanup, fireEvent, render, screen } from '@testing-library/svelte';
import { afterEach, expect, it, vi } from 'vitest';
import { createAPIClient } from '../../api/client';
import { DirectoryEntityController } from '../../directory/entity-controller.svelte';
import { employment, requestOf } from '../../directory/entity-fixtures.test-support';
import PersonFacts from './PersonFacts.svelte';

afterEach(() => { cleanup(); vi.restoreAllMocks(); });

it('shows the exact summary even when there are more than 50 claims', async () => {
  const fetchFn = vi.fn<typeof fetch>(async input => {
    if (new URL(requestOf(input).url).pathname.endsWith('/fact-summary')) return Response.json({ claimed_fields: 73, latest_claim_at: '2026-11-01T12:00:00Z', has_stored_facts: true });
    throw new Error('unexpected request');
  });
  render(PersonFacts, { client: createAPIClient(fetchFn), personID: 42 });
  await vi.waitFor(() => expect(screen.getByRole('heading', { name: 'Facts' }).closest('summary')?.textContent).toContain('73 fields · last found Nov 1'));
});

it('cancels the old summary when the person changes', async () => {
  let oldRequest: Request | undefined;
  let resolveOld!: (response: Response) => void;
  const oldResponse = new Promise<Response>(resolve => { resolveOld = resolve; });
  const fetchFn = vi.fn<typeof fetch>(async input => {
    const request = requestOf(input);
    if (new URL(request.url).pathname.includes('/42/')) { oldRequest = request; return oldResponse; }
    return Response.json({ claimed_fields: 2, latest_claim_at: '2026-11-03T12:00:00Z', has_stored_facts: true });
  });
  const client = createAPIClient(fetchFn);
  const view = render(PersonFacts, { client, personID: 42 });
  await vi.waitFor(() => expect(oldRequest).toBeDefined());
  await view.rerender({ client, personID: 43 });
  expect(oldRequest!.signal.aborted).toBe(true);
  resolveOld(Response.json({ claimed_fields: 9, latest_claim_at: '2026-11-01T12:00:00Z', has_stored_facts: true }));
  await vi.waitFor(() => expect(screen.getByRole('heading', { name: 'Facts' }).closest('summary')?.textContent).toContain('2 fields'));
});

it('opens Facts after the first saved job with lookups off', async () => {
  let hasStored = false;
  const fetchFn = vi.fn<typeof fetch>(async input => {
    if (new URL(requestOf(input).url).pathname.endsWith('/fact-summary')) return Response.json({ claimed_fields: 0, latest_claim_at: null, has_stored_facts: hasStored });
    throw new Error('unexpected request');
  });
  const client = createAPIClient(fetchFn), entity = new DirectoryEntityController(client, 42);
  render(PersonFacts, { client, personID: 42, entity, onSetUp: vi.fn() });
  await screen.findByRole('button', { name: /Set up/ });
  hasStored = true;
  entity.employments = [employment(11, 1)];
  await screen.findByText('No claims yet');
  expect(screen.getByRole('heading', { name: 'Facts' }).closest('summary')).not.toBeNull();
  entity.destroy();
});

it('keeps Facts openable and retries a failed summary', async () => {
  let fail = true;
  const fetchFn = vi.fn<typeof fetch>(async input => {
    const path = new URL(requestOf(input).url).pathname;
    if (path.endsWith('/fact-summary')) return fail ? new Response(null, { status: 503 }) : Response.json({ claimed_fields: 0, latest_claim_at: null, has_stored_facts: true });
    if (path.endsWith('/person-fact-targets')) return Response.json({ targets: [] });
    if (path.endsWith('/fact-claims')) return Response.json({ claims: [] });
    if (path.endsWith('/fact-evidence')) return Response.json({ evidence: [] });
    if (path.endsWith('/fact-decisions')) return Response.json({ decisions: [] });
    if (path.endsWith('/fact-pins')) return Response.json({ pins: [] });
    throw new Error('unexpected request');
  });
  render(PersonFacts, { client: createAPIClient(fetchFn), personID: 42, onSetUp: vi.fn() });
  await fireEvent.click(screen.getByRole('heading', { name: 'Facts' }));
  await screen.findByText("Couldn't load facts summary.");
  fail = false;
  await fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
  await vi.waitFor(() => expect(screen.queryByText("Couldn't load facts summary.")).toBeNull());
});
