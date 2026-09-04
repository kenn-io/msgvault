import { afterEach, describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../api/client';
import { CardDAVPublicationController } from './publication-controller.svelte';

const forbidden = {
  raw_vcard: 'BEGIN:VCARD\nFN:FORBIDDEN\nEND:VCARD',
  url: 'https://forbidden.example.test/dav',
  href: '/forbidden/contact.vcf',
  credential: 'forbidden-private-credential'
};

function publication(personID: number, state: 'unpublished' | 'published' | 'pending' | 'conflict', overrides: Record<string, unknown> = {}) {
  return {
    person_id: personID,
    state,
    desired: state === 'published',
    address_book: { id: 5, name: 'Synthetic contacts', ...forbidden },
    ...forbidden,
    ...overrides
  };
}

function requestOf(input: RequestInfo | URL): Request {
  return input instanceof Request ? input : new Request(input);
}

function deferredResponse() {
  let resolve!: (response: Response) => void;
  const promise = new Promise<Response>((settle) => { resolve = settle; });
  return { promise, resolve };
}

afterEach(() => vi.restoreAllMocks());

describe('CardDAVPublicationController', () => {
  it('uses only exact durable-person GET, POST, and DELETE routes and projects safe fields', async () => {
    const facts: Array<{ method: string; path: string }> = [];
    let state: 'unpublished' | 'published' = 'unpublished';
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      const path = new URL(request.url).pathname;
      facts.push({ method: request.method, path });
      if (request.method === 'POST') state = 'published';
      if (request.method === 'DELETE') state = 'unpublished';
      return Response.json(publication(7, state, { desired: state === 'published' }));
    });
    const controller = new CardDAVPublicationController(createAPIClient(fetchFn));

    await controller.setPerson(7);
    expect(await controller.publish()).toEqual({ kind: 'confirmed', action: 'publish' });
    expect(await controller.unpublish()).toEqual({ kind: 'confirmed', action: 'unpublish' });

    expect(facts).toEqual([
      { method: 'GET', path: '/api/v1/carddav/publications/7' },
      { method: 'POST', path: '/api/v1/carddav/publications/7' },
      { method: 'DELETE', path: '/api/v1/carddav/publications/7' }
    ]);
    expect(facts.some(({ path }) => path.includes('/participants/'))).toBe(false);
    expect(controller.publication).toEqual({
      person_id: 7,
      state: 'unpublished',
      desired: false,
      address_book: { id: 5, name: 'Synthetic contacts' }
    });
    expect(JSON.stringify(controller.publication)).not.toMatch(/FORBIDDEN|forbidden/i);
    controller.destroy();
  });

  it('suppresses duplicate mutation and reconciles an ambiguous result with one GET and no replay', async () => {
    const mutation = deferredResponse();
    let gets = 0;
    let posts = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      if (request.method === 'POST') {
        posts += 1;
        return mutation.promise;
      }
      gets += 1;
      return Response.json(publication(7, gets === 1 ? 'unpublished' : 'published', { desired: gets > 1 }));
    });
    const controller = new CardDAVPublicationController(createAPIClient(fetchFn));
    await controller.setPerson(7);

    const first = controller.publish();
    expect(await controller.publish()).toEqual({ kind: 'ignored' });
    mutation.resolve(Response.json({ error: 'carddav_publication_pending' }, { status: 409 }));
    expect(await first).toEqual({ kind: 'reconciled', action: 'publish' });

    expect([posts, gets]).toEqual([1, 2]);
    expect(controller.publication?.state).toBe('published');
    expect(controller.stateUnknown).toBe(false);
    controller.destroy();
  });

  it('locks mutation after failed ambiguous reconciliation and GET-only retry recovers', async () => {
    let gets = 0;
    let posts = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      if (request.method === 'POST') {
        posts += 1;
        return Response.json({ error: 'unavailable' }, { status: 503 });
      }
      gets += 1;
      if (gets === 2) return Response.json({ error: 'carddav_unavailable' }, { status: 503 });
      return Response.json(publication(7, gets === 1 ? 'unpublished' : 'published', { desired: gets > 1 }));
    });
    const controller = new CardDAVPublicationController(createAPIClient(fetchFn));
    await controller.setPerson(7);

    expect(await controller.publish()).toEqual({ kind: 'unknown', action: 'publish' });
    expect(controller.publication).toBeUndefined();
    expect(controller.unavailable).toBe(true);
    expect(controller.stateUnknown).toBe(true);
    expect(await controller.publish()).toEqual({ kind: 'ignored' });
    await controller.retryState();

    expect([posts, gets]).toEqual([1, 3]);
    expect(controller.publication?.state).toBe('published');
    expect(controller.stateUnknown).toBe(false);
    controller.destroy();
  });

  it('reconciles a rejected mutation once and locks changes behind GET-only retry when reconciliation fails', async () => {
    const requests: Array<{ method: string; path: string }> = [];
    let gets = 0;
    let posts = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      const path = new URL(request.url).pathname;
      requests.push({ method: request.method, path });
      if (request.method === 'POST') {
        posts += 1;
        throw new TypeError('synthetic transport failure');
      }
      gets += 1;
      if (gets === 2) throw new TypeError('synthetic reconciliation failure');
      return Response.json(publication(7, gets === 1 ? 'unpublished' : 'published', { desired: gets > 1 }));
    });
    const controller = new CardDAVPublicationController(createAPIClient(fetchFn));
    await controller.setPerson(7);

    expect(await controller.publish()).toEqual({ kind: 'unknown', action: 'publish' });
    expect(controller.publication).toBeUndefined();
    expect(controller.stateUnknown).toBe(true);
    expect(controller.canPublish()).toBe(false);
    expect(await controller.publish()).toEqual({ kind: 'ignored' });
    await controller.retryState();

    expect([posts, gets]).toEqual([1, 3]);
    expect(requests).toEqual([
      { method: 'GET', path: '/api/v1/carddav/publications/7' },
      { method: 'POST', path: '/api/v1/carddav/publications/7' },
      { method: 'GET', path: '/api/v1/carddav/publications/7' },
      { method: 'GET', path: '/api/v1/carddav/publications/7' }
    ]);
    expect(controller.publication?.state).toBe('published');
    expect(controller.stateUnknown).toBe(false);
    controller.destroy();
  });

  it('projects only typed CardDAV unavailable as optional state and recovers on a later load', async () => {
    let configured = false;
    const fetchFn = vi.fn<typeof fetch>(async () => configured
      ? Response.json(publication(7, 'unpublished'))
      : Response.json({ error: 'carddav_unavailable', message: forbidden.credential }, { status: 503 }));
    const controller = new CardDAVPublicationController(createAPIClient(fetchFn));

    await controller.setPerson(7);
    expect(controller.unavailable).toBe(true);
    expect(controller.publication).toBeUndefined();
    expect(controller.error).toBeNull();
    expect(controller.stateUnknown).toBe(false);

    configured = true;
    await controller.load();
    expect(controller.unavailable).toBe(false);
    expect(controller.publication?.state).toBe('unpublished');
    expect(fetchFn).toHaveBeenCalledTimes(2);
    controller.destroy();
  });

  it('invalidates a confirmed publication after a failed refresh and blocks mutations', async () => {
    let reads = 0;
    let posts = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      if (request.method === 'POST') {
        posts += 1;
        return Response.json(publication(7, 'published'));
      }
      reads += 1;
      if (reads === 1) return Response.json(publication(7, 'unpublished'));
      return Response.json({ error: 'unavailable' }, { status: 503 });
    });
    const controller = new CardDAVPublicationController(createAPIClient(fetchFn));
    await controller.setPerson(7);
    expect(controller.canPublish()).toBe(true);

    await controller.retryState();

    expect(controller.stateUnknown).toBe(true);
    expect(controller.canPublish()).toBe(false);
    expect(await controller.publish()).toEqual({ kind: 'ignored' });
    expect(posts).toBe(0);
    controller.destroy();
  });

  it('clears synchronously and ignores an old person response after same-controller reuse', async () => {
    const oldPerson = deferredResponse();
    const signals = new Map<number, AbortSignal>();
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      const id = Number(new URL(request.url).pathname.split('/').at(-1));
      signals.set(id, request.signal);
      if (id === 7) return oldPerson.promise;
      return Response.json(publication(9, 'published', {
        address_book: { id: 6, name: 'Current contacts' }
      }));
    });
    const controller = new CardDAVPublicationController(createAPIClient(fetchFn));

    const first = controller.setPerson(7);
    const second = controller.setPerson(9);
    expect(controller.personID).toBe(9);
    expect(controller.publication).toBeUndefined();
    expect(signals.get(7)?.aborted).toBe(true);
    await second;
    oldPerson.resolve(Response.json(publication(7, 'published', {
      address_book: { id: 5, name: 'Old contacts' }
    })));
    await first;

    expect(controller.personID).toBe(9);
    expect(controller.publication?.address_book?.name).toBe('Current contacts');
    expect(JSON.stringify(controller)).not.toContain('Old contacts');
    controller.destroy();
  });

  it('aborts reads and mutations and blocks late display and status state after destroy', async () => {
    const mutation = deferredResponse();
    const signals: AbortSignal[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      signals.push(request.signal);
      if (request.method === 'POST') return mutation.promise;
      return Response.json(publication(7, 'unpublished'));
    });
    const controller = new CardDAVPublicationController(createAPIClient(fetchFn));
    await controller.setPerson(7);
    const pending = controller.publish();

    controller.destroy();
    expect(signals.at(-1)?.aborted).toBe(true);
    mutation.resolve(Response.json(publication(7, 'published')));
    expect(await pending).toEqual({ kind: 'ignored' });
    expect(controller.publication?.state).toBe('unpublished');
    expect(controller.announcement).toBeNull();
  });

  it.each([
    publication(7, 'pending', { desired: true, pending_operation: 'create' }),
    publication(7, 'conflict', { desired: true, conflict_id: 41 })
  ])('never mutates a generated $state publication', async (response) => {
    const methods: string[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      methods.push(request.method);
      return Response.json(response);
    });
    const controller = new CardDAVPublicationController(createAPIClient(fetchFn));
    await controller.setPerson(7);

    expect(await controller.publish()).toEqual({ kind: 'ignored' });
    expect(await controller.unpublish()).toEqual({ kind: 'ignored' });
    expect(methods).toEqual(['GET']);
    controller.destroy();
  });

  it('aborts a mutation on person switch and never writes its old status or focus into the new person', async () => {
    const mutation = deferredResponse();
    let person7Reads = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      const path = new URL(request.url).pathname;
      if (request.method === 'POST') return mutation.promise;
      if (path.endsWith('/7')) {
        person7Reads += 1;
        return Response.json(publication(7, 'unpublished'));
      }
      return Response.json(publication(9, 'published', {
        address_book: { id: 6, name: 'Current contacts' }
      }));
    });
    const controller = new CardDAVPublicationController(createAPIClient(fetchFn));
    await controller.setPerson(7);
    const oldMutation = controller.publish();
    await controller.setPerson(9);
    mutation.resolve(Response.json(publication(7, 'published', {
      address_book: { id: 5, name: 'Old contacts' }
    })));

    expect(await oldMutation).toEqual({ kind: 'ignored' });
    expect(person7Reads).toBe(1);
    expect(controller.personID).toBe(9);
    expect(controller.publication?.address_book?.name).toBe('Current contacts');
    expect(controller.announcement).toBeNull();
    controller.destroy();
  });
});
