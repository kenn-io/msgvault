import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../../api/client';
import { FactLedgerController } from '../../directory/fact-ledger-controller.svelte';
import {
  DirectoryReviewController,
  IDENTITY_REVIEW_PAGE_LIMIT,
  type IdentityMatchCandidate
} from '../../directory/review-controller.svelte';
import DirectoryReviewCentre from './DirectoryReviewCentre.svelte';
import { RelationshipReviewController } from '../../directory/relationship-review-controller.svelte';

afterEach(() => cleanup());

function candidate(id: number, state = 'candidate'): IdentityMatchCandidate {
  return {
    id,
    left_id: id * 10,
    left_kind: 'beeper_user',
    right_id: id * 10 + 1,
    right_kind: 'participant',
    basis: 'stable_provider_id',
    source: 'synthetic',
    state,
    evidence: [],
    created_at: '2026-08-01T10:00:00Z',
    updated_at: '2026-08-02T11:00:00Z'
  };
}

function page(rows: IdentityMatchCandidate[], offset = 0): Response {
  return Response.json({ candidates: rows, limit: IDENTITY_REVIEW_PAGE_LIMIT, offset });
}

function deferredResponse() {
  let resolve!: (response: Response) => void;
  const promise = new Promise<Response>((next) => { resolve = next; });
  return { promise, resolve };
}

function requestOf(input: RequestInfo | URL): Request {
  return input instanceof Request ? input : new Request(input);
}

function renderReview(controller: DirectoryReviewController) {
  return render(DirectoryReviewCentre, {
    controller,
    relationshipController: new RelationshipReviewController(controller.apiClient),
    factController: new FactLedgerController(controller.apiClient),
    directoryPersonID: null
  });
}

describe('DirectoryReviewCentre', () => {
  it('selects the read-only imported relationship queue without identity requests', async () => {
    const calls: Array<{ method: string; path: string; status: string | null }> = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      const url = new URL(request.url);
      calls.push({ method: request.method, path: url.pathname, status: url.searchParams.get('status') });
      return Response.json({ reviews: [] });
    });
    const review = new DirectoryReviewController(createAPIClient(fetchFn));
    const relationships = new RelationshipReviewController(createAPIClient(fetchFn));
    render(DirectoryReviewCentre, { controller: review, relationshipController: relationships });

    await fireEvent.click(screen.getByRole('radio', { name: 'Imported relationships' }));

    expect(await screen.findByRole('heading', { name: 'Imported relationships' })).toBeDefined();
    expect(calls).toEqual([{ method: 'GET', path: '/api/v1/person-relationship-reviews', status: 'pending' }]);
    expect(screen.getByText('Imported relationship reviews are read-only in the browser until generated decision operations are available.')).toBeDefined();
  });
  it('replaces an accept decision with the shared merge modal without replaying acceptance', async () => {
    const conflict = {
      error: 'person_merge_required',
      message: 'Choose a survivor',
      profiles: [
        { etag: '"person-7-r4"', person: { id: 7, revision: 4, display_name: 'Synthetic One' } },
        { etag: '"person-9-r2"', person: { id: 9, revision: 2, display_name: 'Synthetic Two' } }
      ]
    };
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      requests.push(request);
      return Response.json(conflict, { status: 409 });
    });
    const controller = new DirectoryReviewController(createAPIClient(fetchFn));
    controller.rows = [candidate(17)];
    renderReview(controller);

    await fireEvent.click(screen.getByRole('button', { name: 'Link identities' }));
    await fireEvent.click(screen.getByRole('dialog', { name: 'Link identities' }).querySelector('button.kit-button--solid')!);
    await fireEvent.click(await screen.findByRole('button', { name: 'Resolve merge' }));

    expect(screen.queryByRole('dialog', { name: 'Link identities' })).toBeNull();
    expect(screen.getByRole('dialog', { name: 'Resolve person merge' })).toBeDefined();
    expect(screen.getAllByRole('dialog')).toHaveLength(1);
    expect(requests.filter((request) => new URL(request.url).pathname.endsWith('/accept'))).toHaveLength(1);
  });

  it('changes identity state through the controller and commits the URL filter', async () => {
    const requests: Request[] = [];
    const commit = vi.fn();
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      requests.push(request);
      const state = new URL(request.url).searchParams.get('state') ?? 'candidate';
      return page([candidate(state === 'conflict' ? 22 : 17, state)]);
    });
    const controller = new DirectoryReviewController(createAPIClient(fetchFn), commit);
    await controller.loadIdentityPage();
    renderReview(controller);

    expect(screen.getByRole('radiogroup', { name: 'Review type' })).toBeDefined();
    expect(screen.getByRole('radiogroup', { name: 'Identity review state' })).toBeDefined();
    await fireEvent.click(screen.getByRole('radio', { name: 'Conflict' }));

    await screen.findByRole('heading', { name: 'Identity match 22' });
    expect(controller.reviewKind).toBe('identity');
    expect(controller.identityState).toBe('conflict');
    expect(commit).toHaveBeenLastCalledWith({ reviewKind: 'identity', identityState: 'conflict' });
    const last = requests.at(-1)!;
    expect(new URL(last.url).searchParams.get('state')).toBe('conflict');
    expect(new URL(last.url).searchParams.get('offset')).toBe('0');
  });

  it.each([{ mode: 'selection' as const }, { mode: 'restoration' as const }])(
    'shows the fact-contract gate after $mode without inventing requests, while identity still loads normally',
    async ({ mode }) => {
      const requests: Request[] = [];
      const fetchFn = vi.fn<typeof fetch>(async (input) => {
        const request = requestOf(input);
        requests.push(request);
        return page([candidate(17)]);
      });
      const commit = vi.fn();
      const apiClient = createAPIClient(fetchFn);
      const controller = new DirectoryReviewController(apiClient, commit);
      const factController = new FactLedgerController(apiClient);
      if (mode === 'restoration') {
        controller.applyURLState({ reviewKind: 'fact', identityState: 'candidate' }, true);
      }
      render(DirectoryReviewCentre, {
        controller,
        relationshipController: new RelationshipReviewController(apiClient),
        factController,
        directoryPersonID: null
      });

      if (mode === 'selection') {
        await fireEvent.click(screen.getByRole('radio', { name: 'Fact review' }));
      }

      expect(screen.getByRole('region', { name: 'Fact review' })).toBeDefined();
      expect(screen.getByText('Choose a person in Directory to inspect their fact ledger')).toBeDefined();
      expect(screen.queryByRole('button', { name: /accept|reject|unsure|link identities|keep separate/i })).toBeNull();
      expect(fetchFn).not.toHaveBeenCalled();
      if (mode === 'selection') expect(commit).toHaveBeenCalledWith({ reviewKind: 'fact' });

      await fireEvent.click(screen.getByRole('radio', { name: 'Identity matches' }));
      expect(await screen.findByRole('heading', { name: 'Identity match 17' })).toBeDefined();
      expect(requests).toHaveLength(1);
      expect(new URL(requests[0]!.url).pathname).toBe('/api/v1/identity/match-candidates');
    }
  );

  it('retains the current rows under a page loading overlay, then exposes page failure and retry', async () => {
    const nextPage = deferredResponse();
    let nextAttempts = 0;
    const firstRows = [candidate(1)];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const offset = new URL(requestOf(input).url).searchParams.get('offset');
      if (offset === '100') {
        nextAttempts += 1;
        if (nextAttempts === 1) return nextPage.promise;
        return page([candidate(101)], 100);
      }
      return page(firstRows);
    });
    const controller = new DirectoryReviewController(createAPIClient(fetchFn));
    controller.rows = firstRows;
    renderReview(controller);

    void controller.loadIdentityPage(100);
    await waitFor(() => expect(controller.loading).toBe(true));
    expect(screen.getByRole('status', { name: 'Loading next review page' })).toBeDefined();
    expect(screen.getByRole('heading', { name: 'Identity match 1' })).toBeDefined();
    expect(screen.getByRole('button', { name: 'Previous page' })).toHaveProperty('disabled', true);
    expect(screen.getByRole('button', { name: 'Next page' })).toHaveProperty('disabled', true);

    nextPage.resolve(Response.json({ error: 'unavailable', message: 'Next page unavailable' }, { status: 503 }));
    expect((await screen.findByRole('alert')).textContent).toContain('Next page unavailable');
    expect(screen.getByRole('heading', { name: 'Identity match 1' })).toBeDefined();
    await fireEvent.click(screen.getByRole('button', { name: 'Retry identity matches' }));
    await screen.findByRole('heading', { name: 'Identity match 101' });
    expect(controller.offset).toBe(100);
    expect(screen.getByRole('button', { name: 'Previous page' })).toHaveProperty('disabled', false);
  });

  it('distinguishes initial load failure from an empty queue and retries page zero', async () => {
    let attempts = 0;
    const fetchFn = vi.fn<typeof fetch>(async () => {
      attempts += 1;
      if (attempts === 1) {
        return Response.json({ error: 'unavailable', message: 'Queue unavailable' }, { status: 503 });
      }
      return page([]);
    });
    const controller = new DirectoryReviewController(createAPIClient(fetchFn));
    await controller.loadIdentityPage();
    renderReview(controller);

    expect(screen.getByRole('alert').textContent).toContain('Queue unavailable');
    expect(screen.queryByText('No identity matches in this queue.')).toBeNull();
    await fireEvent.click(screen.getByRole('button', { name: 'Retry identity matches' }));

    expect(await screen.findByText('No identity matches in this queue.')).toBeDefined();
    expect(screen.queryByRole('alert')).toBeNull();
    expect(attempts).toBe(2);
  });

  it('keeps Previous navigation when a successful nonzero page is empty', async () => {
    const offsets: number[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const offset = Number(new URL(requestOf(input).url).searchParams.get('offset'));
      offsets.push(offset);
      return offset === 100 ? page([], 100) : page([candidate(17)], 0);
    });
    const controller = new DirectoryReviewController(createAPIClient(fetchFn));
    await controller.loadIdentityPage(100);
    renderReview(controller);

    expect(screen.getByText('No identity matches in this queue.')).toBeDefined();
    const previous = screen.getByRole('button', { name: 'Previous page' });
    expect(previous).toHaveProperty('disabled', false);
    await fireEvent.click(previous);

    expect(await screen.findByRole('heading', { name: 'Identity match 17' })).toBeDefined();
    expect(controller.offset).toBe(0);
    expect(offsets).toEqual([100, 0]);
  });

  it.each([
    {
      name: 'identity review to fact review',
      target: { reviewKind: 'fact' as const, identityState: 'candidate' as const },
      focusHeading: 'Fact review'
    },
    {
      name: 'candidate review to conflict review',
      target: { reviewKind: 'identity' as const, identityState: 'conflict' as const },
      focusHeading: 'Identity matches'
    },
    {
      name: 'the same visible candidate state with a new history generation',
      target: { reviewKind: 'identity' as const, identityState: 'candidate' as const },
      focusHeading: 'Identity matches'
    }
  ])('invalidates an open decision across $name', async ({ target, focusHeading }) => {
    const requests: Request[] = [];
    const current = candidate(17);
    const accepted = candidate(17, 'accepted');
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      requests.push(request);
      if (request.method === 'POST') {
        return Response.json({ candidate: accepted, identity_revision: 4, cache_state: 'stale' });
      }
      const state = new URL(request.url).searchParams.get('state') ?? 'candidate';
      return page([candidate(state === 'conflict' ? 88 : 17, state)]);
    });
    const apiClient = createAPIClient(fetchFn);
    const controller = new DirectoryReviewController(apiClient);
    const factController = new FactLedgerController(apiClient);
    controller.rows = [current];
    render(DirectoryReviewCentre, {
      controller,
      relationshipController: new RelationshipReviewController(apiClient),
      factController,
      directoryPersonID: null
    });

    await fireEvent.click(screen.getByRole('button', { name: 'Link identities' }));
    controller.applyURLState(target, true);

    await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Link identities' })).toBeNull());
    const heading = screen.getByRole('heading', { name: focusHeading });
    await waitFor(() => expect(document.activeElement).toBe(heading));
    expect(document.activeElement?.isConnected).toBe(true);
    expect(controller.reviewKind).toBe(target.reviewKind);
    expect(controller.identityState).toBe(target.identityState);
    expect(requests.filter((request) => request.method === 'POST')).toHaveLength(0);
  });

  it('closes a successful decision and returns focus to the originating row action', async () => {
    const requests: Request[] = [];
    const current = candidate(17);
    const accepted = candidate(17, 'accepted');
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      requests.push(request);
      if (request.method === 'POST') {
        return Response.json({ candidate: accepted, identity_revision: 4, cache_state: 'stale' });
      }
      return page([current]);
    });
    const controller = new DirectoryReviewController(createAPIClient(fetchFn));
    controller.rows = [current];
    renderReview(controller);
    const trigger = screen.getByRole('button', { name: 'Link identities' });
    trigger.focus();

    await fireEvent.click(trigger);
    await fireEvent.input(screen.getByRole('textbox', { name: 'Decision notes' }), {
      target: { value: 'Confirmed by synthetic fixture' }
    });
    await fireEvent.click(screen.getByRole('dialog', { name: 'Link identities' }).querySelector('button.kit-button--solid')!);

    await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Link identities' })).toBeNull());
    const restoredAction = screen.getByRole('button', { name: 'Link identities' });
    await waitFor(() => expect(document.activeElement).toBe(restoredAction));
    expect(screen.getByRole('status').textContent).toContain('Identity match accepted.');
    expect(requests.filter((request) => request.method === 'POST')).toHaveLength(1);
  });

  it('returns focus to a stable live fallback when reconciliation removes the originating row', async () => {
    const current = candidate(17);
    const accepted = candidate(17, 'accepted');
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      if (request.method === 'POST') {
        return Response.json({ candidate: accepted, identity_revision: 4, cache_state: 'stale' });
      }
      return page([]);
    });
    const controller = new DirectoryReviewController(createAPIClient(fetchFn));
    controller.rows = [current];
    renderReview(controller);
    const trigger = screen.getByRole('button', { name: 'Link identities' });
    trigger.focus();

    await fireEvent.click(trigger);
    await fireEvent.click(screen.getByRole('dialog', { name: 'Link identities' }).querySelector('button.kit-button--solid')!);

    await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Link identities' })).toBeNull());
    expect(screen.getByText('No identity matches in this queue.')).toBeDefined();
    const heading = screen.getByRole('heading', { name: 'Identity matches' });
    await waitFor(() => expect(document.activeElement).toBe(heading));
    expect(document.activeElement?.isConnected).toBe(true);
  });

  it('keeps committed success visible when reconciliation fails and returns focus to the row', async () => {
    const current = candidate(17);
    const accepted = candidate(17, 'accepted');
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      if (request.method === 'POST') {
        return Response.json({ candidate: accepted, identity_revision: 4, cache_state: 'stale' });
      }
      return Response.json({ error: 'unavailable', message: 'Reload failed' }, { status: 503 });
    });
    const controller = new DirectoryReviewController(createAPIClient(fetchFn));
    controller.rows = [current];
    renderReview(controller);

    await fireEvent.click(screen.getByRole('button', { name: 'Link identities' }));
    await fireEvent.click(screen.getByRole('dialog', { name: 'Link identities' }).querySelector('button.kit-button--solid')!);

    await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Link identities' })).toBeNull());
    const row = screen.getByRole('article', { name: 'Identity match 17' });
    expect(row.textContent).toContain('accepted');
    expect(screen.getByRole('status').textContent).toContain('Identity match accepted.');
    expect(screen.getByRole('alert').textContent).toContain('Reload failed');
    await waitFor(() => expect(document.activeElement).toBe(row));
  });
});
