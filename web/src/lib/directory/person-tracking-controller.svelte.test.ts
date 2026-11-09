import { requestOf } from './entity-fixtures.test-support';
import { describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../api/client';
import { PersonTrackingController } from './person-tracking-controller.svelte';

function deferredResponse() {
  let resolve!: (response: Response) => void;
  const promise = new Promise<Response>((settle) => { resolve = settle; });
  return { promise, resolve };
}

describe('PersonTrackingController', () => {
  it('loads the exact durable-person tracking state', async () => {
    const requests: Array<{ method: string; path: string }> = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      const url = new URL(request.url);
      requests.push({
        method: request.method,
        path: url.pathname
      });
      if (url.pathname === '/api/v1/people/7/tracking') {
        return Response.json({ person_id: 7, tracked: true, tracked_at: null });
      }
      throw new Error(`Unexpected ${request.method} ${url.pathname}`);
    });
    const controller = new PersonTrackingController(createAPIClient(fetchFn));

    await controller.setPerson(7);

    expect(requests).toEqual([
      { method: 'GET', path: '/api/v1/people/7/tracking' }
    ]);
    expect(controller.tracking).toEqual({ person_id: 7, tracked: true, tracked_at: null });
    controller.destroy();
  });

  it('replaces and aborts tracking contexts', async () => {
    const lateTracking = deferredResponse();
    const requests: Request[] = [];
    const controller = new PersonTrackingController(createAPIClient(async input => {
      const request = requestOf(input);
      requests.push(request);
      const path = new URL(request.url).pathname;
      if (path.endsWith('/7/tracking')) return lateTracking.promise;
      return Response.json({ person_id: 9, tracked: false, tracked_at: null });
    }));
    const first = controller.setPerson(7);
    await controller.setPerson(9);
    expect(requests[0]!.signal.aborted).toBe(true);
    lateTracking.resolve(Response.json({ person_id: 7, tracked: true, tracked_at: null }));
    await first;
    expect(controller.personID).toBe(9);
    expect(controller.tracking?.tracked).toBe(false);
    controller.destroy();
  });

  it('sends one exact replacement body, suppresses duplicates, and commits only matching success', async () => {
    const mutation = deferredResponse();
    const facts: Array<{ method: string; path: string; tracked?: boolean; headerNames: string[] }> = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      const path = new URL(request.url).pathname;
      const body = request.method === 'PUT' ? await request.clone().json() as { tracked?: unknown } : undefined;
      facts.push({
        method: request.method,
        path,
        ...(typeof body?.tracked === 'boolean' ? { tracked: body.tracked } : {}),
        headerNames: [...request.headers.keys()].filter((name) => name !== 'content-type')
      });
      if (request.method === 'PUT') return mutation.promise;
      if (path.endsWith('/tracking')) return Response.json({ person_id: 7, tracked: false, tracked_at: null });
      throw new Error(`Unexpected ${request.method} ${new URL(request.url).pathname}`);
    });
    const controller = new PersonTrackingController(createAPIClient(fetchFn));
    await controller.setPerson(7);

    const first = controller.setTracked(true);
    expect(controller.pending).toBe(true);
    expect(await controller.setTracked(false)).toEqual({ kind: 'ignored' });
    mutation.resolve(Response.json({ person_id: 7, tracked: true, tracked_at: '2026-08-29T00:00:00Z' }));

    expect(await first).toEqual({ kind: 'confirmed', desired: true });
    expect(controller.tracking?.tracked).toBe(true);
    expect(controller.announcement).toBe('Keeping this profile updated.');
    expect(facts.filter(({ method }) => method === 'PUT')).toEqual([{
      method: 'PUT', path: '/api/v1/people/7/tracking', tracked: true, headerNames: []
    }]);
    controller.destroy();
  });

  it('retains confirmed state for known mutation failures without reconciliation or replay', async () => {
    let gets = 0;
    let puts = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      const path = new URL(request.url).pathname;
      if (request.method === 'PUT') {
        puts += 1;
        return Response.json({ error: 'not_found', message: 'forbidden private error' }, { status: 404 });
      }
      if (path.endsWith('/tracking')) {
        gets += 1;
        return Response.json({ person_id: 7, tracked: false, tracked_at: null });
      }
      throw new Error(`Unexpected ${request.method} ${new URL(request.url).pathname}`);
    });
    const controller = new PersonTrackingController(createAPIClient(fetchFn));
    await controller.setPerson(7);

    expect(await controller.setTracked(true)).toEqual({ kind: 'error', desired: true });
    expect(controller.tracking?.tracked).toBe(false);
    expect(controller.trackingError).toBe("Couldn't save.");
    expect([puts, gets]).toEqual([1, 1]);
    controller.destroy();
  });

  it.each(['transport', 'server', 'mismatched'] as const)(
    'reconciles an ambiguous %s result with one GET and never replays the PUT',
    async (failure) => {
      let gets = 0;
      let puts = 0;
      const fetchFn = vi.fn<typeof fetch>(async (input) => {
        const request = requestOf(input);
        const path = new URL(request.url).pathname;
        if (request.method === 'PUT') {
          puts += 1;
          if (failure === 'transport') throw new TypeError('synthetic transport failure');
          if (failure === 'server') return Response.json({ error: 'unavailable' }, { status: 503 });
          return Response.json({ person_id: 9, tracked: true, tracked_at: '2026-08-29T00:00:00Z' });
        }
        if (path.endsWith('/tracking')) {
          gets += 1;
          return Response.json({
            person_id: 7,
            tracked: gets > 1,
            tracked_at: gets > 1 ? '2026-08-29T00:00:00Z' : null
          });
        }
        throw new Error(`Unexpected ${request.method} ${new URL(request.url).pathname}`);
      });
      const controller = new PersonTrackingController(createAPIClient(fetchFn));
      await controller.setPerson(7);

      expect(await controller.setTracked(true)).toEqual({ kind: 'reconciled', desired: true });
      expect([puts, gets]).toEqual([1, 2]);
      expect(controller.tracking?.tracked).toBe(true);
      expect(controller.stateUnknown).toBe(false);
      expect(controller.announcement).toBe('Keeping this profile updated.');
      controller.destroy();
    }
  );

  it('says a save failed when the read-back shows the switch where it was, and Retry saves it again', async () => {
    let gets = 0;
    let puts = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      const path = new URL(request.url).pathname;
      if (request.method === 'PUT') {
        puts += 1;
        if (puts === 1) throw new TypeError('synthetic transport failure');
        return Response.json({ person_id: 7, tracked: true, tracked_at: '2026-08-29T00:00:00Z' });
      }
      if (path.endsWith('/tracking')) {
        gets += 1;
        return Response.json({ person_id: 7, tracked: false, tracked_at: null });
      }
      throw new Error(`Unexpected ${request.method} ${new URL(request.url).pathname}`);
    });
    const controller = new PersonTrackingController(createAPIClient(fetchFn));
    await controller.setPerson(7);

    expect(await controller.setTracked(true)).toEqual({ kind: 'error', desired: true });
    expect([puts, gets]).toEqual([1, 2]);
    expect(controller.failedSave).toBe(true);
    expect(controller.stateUnknown).toBe(false);
    expect(controller.trackingError).toBe("Couldn't save.");

    expect(await controller.retrySave()).toEqual({ kind: 'confirmed', desired: true });
    expect([puts, gets]).toEqual([2, 2]);
    expect(controller.failedSave).toBeNull();
    expect(controller.trackingError).toBeNull();
    expect(controller.tracking?.tracked).toBe(true);
    controller.destroy();
  });

  it.each(['reconciliation', 'refresh'] as const)('locks mutation after failed %s and recovers through GET-only retry', async failure => {
    let gets = 0, puts = 0;
    const controller = new PersonTrackingController(createAPIClient(async input => {
      const request = requestOf(input);
      if (request.method === 'PUT') { puts += 1; throw new TypeError('synthetic transport failure'); }
      if (new URL(request.url).pathname.endsWith('/tracking')) {
        gets += 1;
        if (gets === 2) return Response.json({ error: 'unavailable' }, { status: 503 });
        return Response.json({ person_id: 7, tracked: gets > 2, tracked_at: gets > 2 ? '2026-08-29T00:00:00Z' : null });
      }
      throw new Error(`Unexpected ${request.method} ${new URL(request.url).pathname}`);
    }));
    await controller.setPerson(7);
    if (failure === 'refresh') await controller.retryTracking();
    else expect(await controller.setTracked(true)).toEqual({ kind: 'unknown', desired: true });
    if (failure === 'refresh') expect(controller.trackingError).toBe("Couldn't check this profile's updates.");
    expect(controller.stateUnknown).toBe(true);
    expect(controller.tracking).toBeUndefined();
    expect(await controller.setTracked(true)).toEqual({ kind: 'ignored' });
    await controller.retryTracking();
    expect([puts, gets]).toEqual([failure === 'refresh' ? 0 : 1, 3]);
    expect(controller.tracking?.tracked).toBe(true);
    expect(controller.stateUnknown).toBe(false);
    expect(controller.trackingError).toBeNull();
    controller.destroy();
  });

  it('aborts pending work on destroy and blocks late state and announcements', async () => {
    const mutation = deferredResponse();
    const signals: AbortSignal[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      signals.push(request.signal);
      const path = new URL(request.url).pathname;
      if (request.method === 'PUT') return mutation.promise;
      if (path.endsWith('/tracking')) return Response.json({ person_id: 7, tracked: false, tracked_at: null });
      throw new Error(`Unexpected ${request.method} ${new URL(request.url).pathname}`);
    });
    const controller = new PersonTrackingController(createAPIClient(fetchFn));
    await controller.setPerson(7);
    const pending = controller.setTracked(true);

    controller.destroy();
    expect(signals.at(-1)?.aborted).toBe(true);
    mutation.resolve(Response.json({ person_id: 7, tracked: true, tracked_at: '2026-08-29T00:00:00Z' }));
    expect(await pending).toEqual({ kind: 'ignored' });
    expect(controller.tracking?.tracked).toBe(false);
    expect(controller.announcement).toBeNull();
  });
});
