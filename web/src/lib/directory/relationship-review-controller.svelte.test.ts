import { requestOf } from './entity-fixtures.test-support';
import { describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../api/client';
import type { RelationshipReview as GeneratedRelationshipReview } from '../api/generated/models';
import { RelationshipReviewController } from './relationship-review-controller.svelte';

type GeneratedReview = GeneratedRelationshipReview;

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
      property: 'forbidden-vcard-property',
      group: 'forbidden-vcard-group',
      prop_id: 'forbidden-vcard-prop-id',
      pid: ['forbidden-vcard-pid'],
      altid: 'forbidden-vcard-altid',
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
    ...overrides,
  };
}

function deferredResponse() {
  let resolve!: (response: Response) => void;
  const promise = new Promise<Response>((settle) => {
    resolve = settle;
  });
  return { promise, resolve };
}

describe('RelationshipReviewController', () => {
  it(
    'loads only safe review fields',
    async () => {
      const requests: Array<{ method: string; path: string; query: Array<[string, string]> }> = [];
      const fetchFn = vi.fn<typeof fetch>(async (input) => {
        const request = requestOf(input);
        const url = new URL(request.url);
        requests.push({ method: request.method, path: url.pathname, query: [...url.searchParams.entries()] });
        return Response.json({ reviews: [generatedReview({ person_id: Number(url.searchParams.get('person_id')) })] });
      });
      const controller = new RelationshipReviewController(createAPIClient(fetchFn));

      controller.applyPerson(7);
      await vi.waitFor(() => expect(controller.loading).toBe(false));

      expect(requests).toEqual([
        {
          method: 'GET',
          path: '/api/v1/person-relationship-reviews',
          query: [['person_id', '7']],
        },
      ]);
      expect(controller.rows).toEqual([
        {
          id: 41,
          person_id: 7,
          matched_person_id: 9,
          raw_related_value: 'urn:uuid:synthetic-related-person',
          raw_related_type: 'friend',
          value_kind: 'uri',
          status: 'pending',
          source: 'vcard_import',
          created_at: '2026-08-01T10:00:00Z',
          updated_at: '2026-08-02T11:00:00Z',
          reviewed_at: '2026-08-03T12:00:00Z',
        },
      ]);
      expect(JSON.stringify(controller.rows)).not.toMatch(/forbidden|source_ref|accepted_relationship/i);
      controller.destroy();
    },
  );

  it('supersedes work when the selected owner changes', async () => {
    const pending = deferredResponse();
    let oldSignal!: AbortSignal;
    let calls = 0;
    const controller = new RelationshipReviewController(createAPIClient(async input => {
      const request = requestOf(input);
      const personID = Number(new URL(request.url).searchParams.get('person_id'));
      if (++calls === 2) { oldSignal = request.signal; return pending.promise; }
      return Response.json({ reviews: [generatedReview({ person_id: personID })] });
    }));
    controller.applyPerson(7);
    await vi.waitFor(() => expect(controller.rows[0]?.person_id).toBe(7));
    const oldLoad = controller.load();
    controller.applyPerson(8);
    expect(oldSignal.aborted).toBe(true);
    expect(controller.rows).toEqual([]);
    await vi.waitFor(() => expect(controller.rows[0]?.person_id).toBe(8));
    pending.resolve(Response.json({ reviews: [generatedReview()] }));
    await oldLoad;
    expect(controller.rows[0]?.person_id).toBe(8);
    controller.destroy();
  });

  it('rejects malformed rows, normalizes null to empty, and retries only on demand', async () => {
    let attempts = 0;
    const fetchFn = vi.fn<typeof fetch>(async () => {
      attempts += 1;
      if (attempts === 1) {
        return Response.json({ error: 'unavailable', message: 'forbidden-service-detail' }, { status: 503 });
      }
      if (attempts === 2) return Response.json({ reviews: [generatedReview({ id: 0 })] });
      return Response.json({ reviews: null });
    });
    const controller = new RelationshipReviewController(createAPIClient(fetchFn));
    controller.applyPerson(7);
    await vi.waitFor(() => expect(controller.error).not.toBeNull());

    expect(attempts).toBe(1);
    expect(controller.rows).toEqual([]);
    expect(controller.error).toBe('Unable to load imported relationship reviews.');
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(attempts).toBe(1);

    await controller.retry();
    expect(attempts).toBe(2);
    expect(controller.rows).toEqual([]);
    expect(controller.error).toBe('Unable to load imported relationship reviews.');
    await controller.retry();
    expect(attempts).toBe(3);
    expect(controller.rows).toEqual([]);
    expect(controller.error).toBeNull();
    controller.destroy();
  });

  it('destroy aborts the active request and blocks late state', async () => {
    const pending = deferredResponse();
    let signal!: AbortSignal;
    const controller = new RelationshipReviewController(
      createAPIClient(
        vi.fn<typeof fetch>(async (input) => {
          signal = requestOf(input).signal;
          return pending.promise;
        }),
      ),
    );
    controller.applyPerson(7);
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
