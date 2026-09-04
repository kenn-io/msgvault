import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../../api/client';
import { PersonTrackingController } from '../../directory/person-tracking-controller.svelte';
import PersonTrackingControl from './PersonTrackingControl.svelte';

const forbidden = {
  fingerprint: 'forbidden-catalog-fingerprint',
  key: 'forbidden-target-key',
  revision: `sha256:${'a'.repeat(64)}`,
  universal_id: 'forbidden-universal-id',
  href: 'https://forbidden.example.test/profile',
  credential: 'forbidden-private-credential'
};

function requestOf(input: RequestInfo | URL): Request {
  return input instanceof Request ? input : new Request(input);
}

function catalog(sensitive: boolean) {
  return {
    version: 'v1',
    fingerprint: forbidden.fingerprint,
    targets: [{
      kind: 'attribute', key: forbidden.key, revision: forbidden.revision,
      slug: sensitive ? 'private-note' : 'timezone', universal_id: forbidden.universal_id,
      description: sensitive ? 'Private note' : 'Time zone', value_type: 'text',
      cardinality: 'single', sensitive, choices: null, fields: null,
      href: forbidden.href, credential: forbidden.credential
    }]
  };
}

function deferredResponse() {
  let resolve!: (response: Response) => void;
  const promise = new Promise<Response>((settle) => { resolve = settle; });
  return { promise, resolve };
}

describe('PersonTrackingControl', () => {
  it('renders truthful untracked state and only the allow-listed catalog projection', async () => {
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const path = new URL(requestOf(input).url).pathname;
      if (path.endsWith('/tracking')) return Response.json({ person_id: 7, tracked: false, tracked_at: null });
      return Response.json(catalog(false));
    });
    render(PersonTrackingControl, { client: createAPIClient(fetchFn), personID: 7 });

    expect(await screen.findByRole('heading', { name: 'Profile maintenance' })).toBeDefined();
    const toggle = await screen.findByRole('switch', { name: 'Track this person for profile maintenance' }) as HTMLInputElement;
    expect(toggle.checked).toBe(false);
    expect(toggle.disabled).toBe(false);
    expect(screen.getByText(/eligible for future automatic profile maintenance/i)).toBeDefined();
    expect(screen.getByText('Time zone')).toBeDefined();
    expect(screen.getByText('Attribute · text · single')).toBeDefined();
    expect(screen.queryByText('Tracked since')).toBeNull();
    expect(document.body.textContent).not.toMatch(/forbidden/i);
    expect(document.body.innerHTML).not.toMatch(/forbidden\.example/i);
  });

  it('submits once, announces once, shows tracked time, and returns focus to the connected toggle', async () => {
    let puts = 0;
    const onAnnounce = vi.fn();
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      const path = new URL(request.url).pathname;
      if (request.method === 'PUT') {
        puts += 1;
        expect(await request.clone().json()).toEqual({ tracked: true });
        return Response.json({ person_id: 7, tracked: true, tracked_at: '2026-08-29T01:00:00Z' });
      }
      if (path.endsWith('/tracking')) return Response.json({ person_id: 7, tracked: false, tracked_at: null });
      return Response.json(catalog(false));
    });
    render(PersonTrackingControl, { client: createAPIClient(fetchFn), personID: 7, onAnnounce });

    const toggle = await screen.findByRole('switch', { name: 'Track this person for profile maintenance' });
    toggle.focus();
    await fireEvent.click(toggle);

    await waitFor(() => expect(onAnnounce).toHaveBeenCalledOnce());
    expect((screen.getByRole('switch') as HTMLInputElement).checked).toBe(true);
    await waitFor(() => expect(document.activeElement).toBe(screen.getByRole('switch')));
    expect(puts).toBe(1);
    expect(onAnnounce).toHaveBeenCalledWith('Profile maintenance tracking enabled.');
    expect(screen.getByText('Tracked since')).toBeDefined();
    expect(document.querySelector('time')?.getAttribute('datetime')).toBe('2026-08-29T01:00:00Z');
  });

  it('locks an unknown mutation result behind a GET-only retry', async () => {
    let gets = 0;
    let puts = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      const path = new URL(request.url).pathname;
      if (request.method === 'PUT') {
        puts += 1;
        throw new TypeError('synthetic connection reset');
      }
      if (path.endsWith('/tracking')) {
        gets += 1;
        if (gets === 2) throw new TypeError('synthetic reconciliation failure');
        return Response.json({ person_id: 7, tracked: gets > 2, tracked_at: gets > 2 ? '2026-08-29T01:00:00Z' : null });
      }
      return Response.json(catalog(false));
    });
    render(PersonTrackingControl, { client: createAPIClient(fetchFn), personID: 7 });

    await fireEvent.click(await screen.findByRole('switch', { name: 'Track this person for profile maintenance' }));
    const retry = await screen.findByRole('button', { name: 'Retry profile maintenance state' }) as HTMLButtonElement;
    expect(screen.queryByRole('switch')).toBeNull();
    await waitFor(() => expect(retry.disabled).toBe(false));
    await fireEvent.click(retry);

    await waitFor(() => expect((screen.getByRole('switch') as HTMLInputElement).disabled).toBe(false));
    expect((screen.getByRole('switch') as HTMLInputElement).checked).toBe(true);
    expect([puts, gets]).toEqual([1, 3]);
  });

  it('reveals sensitive definitions only by keyboard activation and clears them on person replacement', async () => {
    const queries: string[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      const url = new URL(request.url);
      if (url.pathname.endsWith('/tracking')) {
        const id = Number(url.pathname.split('/').at(-2));
        return Response.json({ person_id: id, tracked: false, tracked_at: null });
      }
      queries.push(url.search);
      return Response.json(catalog(url.searchParams.get('include_sensitive') === 'true'));
    });
    const view = render(PersonTrackingControl, { client: createAPIClient(fetchFn), personID: 7 });

    expect(await screen.findByText('Time zone')).toBeDefined();
    const reveal = await screen.findByRole('button', { name: 'Show sensitive eligible fields' });
    expect((reveal as HTMLButtonElement).disabled).toBe(false);
    reveal.focus();
    await fireEvent.click(reveal);
    expect(await screen.findByText('Private note')).toBeDefined();
    expect(screen.getByText('Sensitive')).toBeDefined();
    expect(queries).toEqual(['?include_sensitive=false', '?include_sensitive=true']);

    await view.rerender({ client: createAPIClient(fetchFn), personID: 9 });
    expect(await screen.findByText('Time zone')).toBeDefined();
    expect(screen.queryByText('Private note')).toBeNull();
    expect(queries).toEqual(['?include_sensitive=false', '?include_sensitive=true', '?include_sensitive=false']);
  });

  it('keeps a loaded tracking toggle usable when catalog loading fails', async () => {
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      if (new URL(request.url).pathname.endsWith('/tracking')) {
        return Response.json({ person_id: 7, tracked: false, tracked_at: null });
      }
      return Response.json({ error: 'unavailable', message: forbidden.credential }, { status: 503 });
    });
    render(PersonTrackingControl, { client: createAPIClient(fetchFn), personID: 7 });

    expect((await screen.findByRole('switch', { name: 'Track this person for profile maintenance' }) as HTMLInputElement).disabled).toBe(false);
    expect((await screen.findByRole('alert')).textContent).toContain('Unable to load eligible profile fields.');
    expect(document.body.textContent).not.toContain(forbidden.credential);
  });

  it('keeps pending focus connected and blocks stale focus or announcement after person replacement', async () => {
    const mutation = deferredResponse();
    const onAnnounce = vi.fn();
    let puts = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      const url = new URL(request.url);
      if (request.method === 'PUT') {
        puts += 1;
        return mutation.promise;
      }
      if (url.pathname.endsWith('/tracking')) {
        const id = Number(url.pathname.split('/').at(-2));
        return Response.json({ person_id: id, tracked: false, tracked_at: null });
      }
      return Response.json(catalog(false));
    });
    const view = render(PersonTrackingControl, {
      client: createAPIClient(fetchFn), personID: 7, onAnnounce
    });
    const oldToggle = await screen.findByRole('switch', { name: 'Track this person for profile maintenance' });
    oldToggle.focus();
    await fireEvent.click(oldToggle);

    expect((screen.getByRole('switch') as HTMLInputElement).disabled).toBe(true);
    expect(document.activeElement).toBe(oldToggle);
    await fireEvent.click(oldToggle);
    expect(puts).toBe(1);

    await view.rerender({ client: createAPIClient(fetchFn), personID: 9, onAnnounce });
    const currentToggle = await screen.findByRole('switch', { name: 'Track this person for profile maintenance' });
    currentToggle.focus();
    mutation.resolve(Response.json({ person_id: 7, tracked: true, tracked_at: '2026-08-29T01:00:00Z' }));
    await Promise.resolve();
    await Promise.resolve();

    expect(document.activeElement).toBe(currentToggle);
    expect((currentToggle as HTMLInputElement).checked).toBe(false);
    expect(onAnnounce).not.toHaveBeenCalled();
  });

  it('suppresses mutation announcement and focus when the person changes after controller settlement', async () => {
    const onAnnounce = vi.fn();
    const focusKeeper = document.createElement('button');
    focusKeeper.textContent = 'Keep focus';
    document.body.append(focusKeeper);
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      const url = new URL(request.url);
      const id = Number(url.pathname.split('/').at(-2));
      if (request.method === 'PUT') {
        return Response.json({ person_id: 7, tracked: true, tracked_at: '2026-08-29T01:00:00Z' });
      }
      if (url.pathname.endsWith('/tracking')) {
        return Response.json({ person_id: id, tracked: false, tracked_at: null });
      }
      return Response.json(catalog(false));
    });
    let view!: ReturnType<typeof render>;
    let replaced = false;
    const original = PersonTrackingController.prototype.setTracked;
    const settlement = vi.spyOn(PersonTrackingController.prototype, 'setTracked').mockImplementation(async function (
      this: PersonTrackingController, desired
    ) {
      const outcome = await original.call(this, desired);
      await view.rerender({ client: createAPIClient(fetchFn), personID: 9, onAnnounce });
      focusKeeper.focus();
      replaced = true;
      return outcome;
    });
    try {
      view = render(PersonTrackingControl, { client: createAPIClient(fetchFn), personID: 7, onAnnounce });
      await fireEvent.click(await screen.findByRole('switch', { name: 'Track this person for profile maintenance' }));
      await waitFor(() => expect(replaced).toBe(true));
      await Promise.resolve();
      await Promise.resolve();

      expect(onAnnounce).not.toHaveBeenCalled();
      expect(document.activeElement).toBe(focusKeeper);
    } finally {
      settlement.mockRestore();
      focusKeeper.remove();
    }
  });

  it('suppresses a settled mutation announcement when its controller context is destroyed before resume', async () => {
    const onAnnounce = vi.fn();
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      const path = new URL(request.url).pathname;
      if (request.method === 'PUT') {
        return Response.json({ person_id: 7, tracked: true, tracked_at: '2026-08-29T01:00:00Z' });
      }
      if (path.endsWith('/tracking')) return Response.json({ person_id: 7, tracked: false, tracked_at: null });
      return Response.json(catalog(false));
    });
    let destroyed = false;
    const original = PersonTrackingController.prototype.setTracked;
    const settlement = vi.spyOn(PersonTrackingController.prototype, 'setTracked').mockImplementation(async function (
      this: PersonTrackingController, desired
    ) {
      const outcome = await original.call(this, desired);
      this.destroy();
      destroyed = true;
      return outcome;
    });
    try {
      render(PersonTrackingControl, { client: createAPIClient(fetchFn), personID: 7, onAnnounce });
      await fireEvent.click(await screen.findByRole('switch', { name: 'Track this person for profile maintenance' }));
      await waitFor(() => expect(destroyed).toBe(true));
      await Promise.resolve();
      await Promise.resolve();

      expect(onAnnounce).not.toHaveBeenCalled();
    } finally {
      settlement.mockRestore();
    }
  });

  it('suppresses retry focus when a same-person context replaces it after controller settlement', async () => {
    const focusKeeper = document.createElement('button');
    focusKeeper.textContent = 'Keep focus';
    document.body.append(focusKeeper);
    let trackingReads = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      const url = new URL(request.url);
      if (url.pathname.endsWith('/tracking')) {
        trackingReads += 1;
        if (trackingReads === 1) return Response.json({ error: 'unavailable' }, { status: 503 });
        return Response.json({ person_id: 7, tracked: false, tracked_at: null });
      }
      return Response.json(catalog(false));
    });
    let replaced = false;
    const original = PersonTrackingController.prototype.retryTracking;
    const settlement = vi.spyOn(PersonTrackingController.prototype, 'retryTracking').mockImplementation(async function (
      this: PersonTrackingController
    ) {
      await original.call(this);
      await this.setPerson(7);
      focusKeeper.focus();
      replaced = true;
    });
    try {
      render(PersonTrackingControl, { client: createAPIClient(fetchFn), personID: 7 });
      const retry = await screen.findByRole('button', { name: 'Retry profile maintenance state' });
      await fireEvent.click(retry);
      await waitFor(() => expect(replaced).toBe(true));
      await Promise.resolve();
      await Promise.resolve();

      expect(document.activeElement).toBe(focusKeeper);
      expect(trackingReads).toBe(3);
    } finally {
      settlement.mockRestore();
      focusKeeper.remove();
    }
  });
});
