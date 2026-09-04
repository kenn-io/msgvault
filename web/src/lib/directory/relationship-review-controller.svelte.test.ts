import { describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../api/client';
import type { components } from '../api/generated/schema';
import {
  RelationshipReviewController,
  type RelationshipReviewState
} from './relationship-review-controller.svelte';

type GeneratedReview = components['schemas']['RelationshipReview'];

function generatedReview(overrides: Partial<GeneratedReview> = {}): GeneratedReview {
  return {
    id: 41,
    person_id: 7,
    matched_person_id: 9,
    raw_related_value: 'urn:uuid:synthetic-related-person',
    raw_related_type: 'friend',
    value_kind: 'uri',
    status: 'pending',
    source: 'vcard_import',
    vcard_identity: {
      property: 'forbidden-vcard-property', group: 'forbidden-vcard-group',
      prop_id: 'forbidden-vcard-prop-id', pid: ['forbidden-vcard-pid'], altid: 'forbidden-vcard-altid'
    },
    source_ref: 'forbidden-source-ref',
    source_resource_uid: 'forbidden-resource-uid',
    created_by: 'forbidden-created-actor',
    reviewed_by: 'forbidden-reviewed-actor',
    accepted_relationship_id: 73,
    created_at: '2026-08-01T10:00:00Z',
    updated_at: '2026-08-02T11:00:00Z',
    reviewed_at: '2026-08-03T12:00:00Z',
    href: 'https://forbidden.example.test/related',
    authorization: 'forbidden-credential',
    raw_vcard: 'BEGIN:VCARD\nforbidden-raw-vcard\nEND:VCARD',
    ...overrides
  };
}

function deferredResponse() {
  let resolve!: (response: Response) => void;
  const promise = new Promise<Response>((settle) => { resolve = settle; });
  return { promise, resolve };
}

function requestOf(input: RequestInfo | URL): Request {
  return input instanceof Request ? input : new Request(input);
}

describe('RelationshipReviewController', () => {
  it.each(['pending', 'accepted', 'rejected'] as const)(
    'uses one exact generated unpaged GET for %s and projects only safe fields',
    async (state) => {
      const requests: Array<{ method: string; path: string; query: Array<[string, string]> }> = [];
      const fetchFn = vi.fn<typeof fetch>(async (input) => {
        const request = requestOf(input);
        const url = new URL(request.url);
        requests.push({ method: request.method, path: url.pathname, query: [...url.searchParams.entries()] });
        return Response.json({ reviews: [generatedReview({ status: state })] });
      });
      const controller = new RelationshipReviewController(createAPIClient(fetchFn));

      controller.applyContext(true, state, false);
      await vi.waitFor(() => expect(controller.loading).toBe(false));

      expect(requests).toEqual([{
        method: 'GET', path: '/api/v1/person-relationship-reviews', query: [['status', state]]
      }]);
      expect(controller.rows).toEqual([{
        id: 41,
        person_id: 7,
        matched_person_id: 9,
        raw_related_value: 'urn:uuid:synthetic-related-person',
        raw_related_type: 'friend',
        value_kind: 'uri',
        status: state,
        source: 'vcard_import',
        created_at: '2026-08-01T10:00:00Z',
        updated_at: '2026-08-02T11:00:00Z',
        reviewed_at: '2026-08-03T12:00:00Z'
      }]);
      expect(JSON.stringify(controller.rows)).not.toMatch(/forbidden|source_ref|accepted_relationship/i);
      controller.destroy();
    }
  );

  it('commits a user state once before replacing context and never commits restored state', async () => {
    const order: string[] = [];
    const commit = vi.fn((patch: { relationshipReviewState?: RelationshipReviewState }) => {
      order.push(`commit:${patch.relationshipReviewState}`);
    });
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const state = new URL(requestOf(input).url).searchParams.get('status');
      order.push(`get:${state}`);
      return Response.json({ reviews: [] });
    });
    const controller = new RelationshipReviewController(createAPIClient(fetchFn), commit);
    controller.applyContext(true, 'pending', false);
    await vi.waitFor(() => expect(controller.loading).toBe(false));
    order.length = 0;

    controller.setState('accepted');
    await vi.waitFor(() => expect(controller.loading).toBe(false));
    expect(order).toEqual(['commit:accepted', 'get:accepted']);
    expect(commit).toHaveBeenCalledOnce();
    expect(commit).toHaveBeenCalledWith({ relationshipReviewState: 'accepted' });

    order.length = 0;
    controller.applyContext(true, 'rejected', true);
    await vi.waitFor(() => expect(controller.loading).toBe(false));
    expect(order).toEqual(['get:rejected']);
    expect(commit).toHaveBeenCalledOnce();
    controller.destroy();
  });

  it('clears rows synchronously and ignores old work on state change and same-state restoration', async () => {
    const firstReplacement = deferredResponse();
    const restoredReplacement = deferredResponse();
    const requests: Request[] = [];
    let call = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      requests.push(request);
      call += 1;
      if (call === 1) return Response.json({ reviews: [generatedReview()] });
      if (call === 2) return firstReplacement.promise;
      if (call === 3) return restoredReplacement.promise;
      throw new Error('Unexpected request');
    });
    const controller = new RelationshipReviewController(createAPIClient(fetchFn));
    controller.applyContext(true, 'pending', false);
    await vi.waitFor(() => expect(controller.rows).toHaveLength(1));

    controller.setState('accepted');
    expect(controller.rows).toEqual([]);
    expect(controller.error).toBeNull();
    controller.applyContext(true, 'accepted', true);
    expect(requests[1]!.signal.aborted).toBe(true);
    expect(controller.rows).toEqual([]);

    firstReplacement.resolve(Response.json({ reviews: [generatedReview({ id: 51, status: 'accepted' })] }));
    restoredReplacement.resolve(Response.json({ error: 'unavailable', message: 'forbidden-stale-error' }, { status: 503 }));
    await vi.waitFor(() => expect(controller.loading).toBe(false));
    expect(controller.rows).toEqual([]);
    expect(controller.error).toBe('Unable to load imported relationship reviews.');
    expect(controller.error).not.toContain('forbidden');
    expect(requests).toHaveLength(3);
    controller.destroy();
  });

  it('aborts and clears when inactive, and late work cannot reactivate the surface', async () => {
    const pending = deferredResponse();
    let request!: Request;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      request = requestOf(input);
      return pending.promise;
    });
    const controller = new RelationshipReviewController(createAPIClient(fetchFn));
    controller.applyContext(true, 'pending', false);
    await vi.waitFor(() => expect(controller.loading).toBe(true));

    controller.applyContext(false, 'pending', false);
    expect(controller.active).toBe(false);
    expect(controller.rows).toEqual([]);
    expect(controller.loading).toBe(false);
    expect(request.signal.aborted).toBe(true);
    pending.resolve(Response.json({ reviews: [generatedReview()] }));
    await Promise.resolve();
    await Promise.resolve();
    expect(controller.active).toBe(false);
    expect(controller.rows).toEqual([]);
  });

  it('normalizes null to empty and retries an empty failed context only on demand', async () => {
    let attempts = 0;
    const fetchFn = vi.fn<typeof fetch>(async () => {
      attempts += 1;
      if (attempts === 1) {
        return Response.json({ error: 'unavailable', message: 'forbidden-service-detail' }, { status: 503 });
      }
      return Response.json({ reviews: null });
    });
    const controller = new RelationshipReviewController(createAPIClient(fetchFn));
    controller.applyContext(true, 'pending', false);
    await vi.waitFor(() => expect(controller.error).not.toBeNull());

    expect(attempts).toBe(1);
    expect(controller.rows).toEqual([]);
    expect(controller.error).toBe('Unable to load imported relationship reviews.');
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(attempts).toBe(1);

    await controller.retry();
    expect(attempts).toBe(2);
    expect(controller.rows).toEqual([]);
    expect(controller.error).toBeNull();
    expect(controller.lastSuccessfulState).toBe('pending');
    controller.destroy();
  });

  it('surfaces invalid-status responses as a bounded programming error', async () => {
    const controller = new RelationshipReviewController(createAPIClient(vi.fn<typeof fetch>(async () =>
      Response.json({ error: 'invalid_status', message: 'forbidden-validator-detail' }, { status: 400 })
    )));

    controller.applyContext(true, 'pending', false);
    await vi.waitFor(() => expect(controller.loading).toBe(false));

    expect(controller.error).toBe('The imported relationship review state is invalid.');
    expect(controller.rows).toEqual([]);
    controller.destroy();
  });

  it('rejects a row whose status does not match the selected queue', async () => {
    const controller = new RelationshipReviewController(createAPIClient(vi.fn<typeof fetch>(async () =>
      Response.json({ reviews: [generatedReview({ status: 'accepted' })] })
    )));

    controller.applyContext(true, 'pending', false);
    await vi.waitFor(() => expect(controller.loading).toBe(false));

    expect(controller.rows).toEqual([]);
    expect(controller.error).toBe('Unable to load imported relationship reviews.');
    controller.destroy();
  });

  it('destroy aborts the active request and blocks late state', async () => {
    const pending = deferredResponse();
    let signal!: AbortSignal;
    const controller = new RelationshipReviewController(createAPIClient(vi.fn<typeof fetch>(async (input) => {
      signal = requestOf(input).signal;
      return pending.promise;
    })));
    controller.applyContext(true, 'pending', false);
    await vi.waitFor(() => expect(controller.loading).toBe(true));

    controller.destroy();
    expect(signal.aborted).toBe(true);
    expect(controller.loading).toBe(false);
    pending.resolve(Response.json({ reviews: [generatedReview()] }));
    await Promise.resolve();
    await Promise.resolve();
    expect(controller.rows).toEqual([]);
  });
});
