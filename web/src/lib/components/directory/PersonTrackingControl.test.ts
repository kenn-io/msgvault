import { requestOf } from '../../directory/entity-fixtures.test-support';
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

function catalog(sensitive: boolean) {
  return {
    version: 'v1',
    fingerprint: forbidden.fingerprint,
    targets: [{
      kind: 'attribute', key: forbidden.key, revision: forbidden.revision,
      slug: sensitive ? 'private_note' : 'time_zone', universal_id: forbidden.universal_id,
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
      if (path.endsWith('/attribute-definitions')) return Response.json({ definitions: [] });
      return Response.json(catalog(false));
    });
    render(PersonTrackingControl, { client: createAPIClient(fetchFn), personID: 7 });

    expect(await screen.findByRole('heading', { name: 'Fields it updates' })).toBeDefined();
    const toggle = await screen.findByRole('switch', { name: /Update automatically — (On|Off)/ }) as HTMLInputElement;
    expect(toggle.checked).toBe(false);
    expect(toggle.disabled).toBe(false);
    expect(screen.getByRole('switch', { name: 'Update automatically — Off' })).toBeDefined();
    expect(screen.getByText('Uses archived conversations when updates run.')).toBeDefined();
    expect(document.querySelector('.tracked-time')).toBeNull();
    expect(screen.getByText('Time zone')).toBeDefined();
    expect(screen.queryByText('Since')).toBeNull();
    expect(document.body.textContent).not.toMatch(/forbidden/i);
    expect(document.body.innerHTML).not.toMatch(/forbidden\.example/i);
  });

  it('uses asynchronously loaded labels, slug fallbacks, and a compact Work history row', async () => {
    const definitions = deferredResponse();
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const path = new URL(requestOf(input).url).pathname;
      if (path.endsWith('/attribute-definitions')) return definitions.promise;
      if (path.endsWith('/tracking')) return Response.json({ person_id: 7, tracked: true, tracked_at: null });
      const target = catalog(false).targets[0]!;
      return Response.json({ targets: [target, { ...target, slug: 'favorite_foods' }, { ...target, slug: 'conversation_topics' }, { ...target, kind: 'employment', slug: 'employment' }] });
    });
    const view = render(PersonTrackingControl, { client: createAPIClient(fetchFn), personID: 7 });
    expect(await screen.findByText('Time zone')).toBeDefined();
    expect(screen.getByText('Favorite foods')).toBeDefined();
    expect(screen.getByText('Conversation topics')).toBeDefined();
    expect(screen.getByText('Work history')).toBeDefined();
    expect(screen.getByText('Employer, title, and dates')).toBeDefined();
    expect(screen.getByRole('switch', { name: 'Update automatically — On' })).toBeDefined();
    expect(document.querySelector('.tracked-time')).toBeNull();
    definitions.resolve(Response.json({ definitions: [{ slug: 'conversation_topics', label: 'Conversation starters' }] }));
    expect(await screen.findByText('Conversation starters')).toBeDefined();

    view.unmount();
    render(PersonTrackingControl, { client: createAPIClient(vi.fn<typeof fetch>(async input => {
      const path = new URL(requestOf(input).url).pathname;
      if (path.endsWith('/attribute-definitions')) return Response.json({ message: 'Registry temporarily unavailable' }, { status: 503 });
      if (path.endsWith('/tracking')) return Response.json({ person_id: 7, tracked: false, tracked_at: null });
      return Response.json({ targets: [{ kind: 'attribute', slug: 'favorites_food', sensitive: false }] });
    })), personID: 7 });
    expect(await screen.findByText('Favorites food')).toBeDefined();
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
      if (path.endsWith('/attribute-definitions')) return Response.json({ definitions: [] });
      return Response.json(catalog(false));
    });
    render(PersonTrackingControl, { client: createAPIClient(fetchFn), personID: 7, onAnnounce });

    const toggle = await screen.findByRole('switch', { name: /Update automatically — (On|Off)/ });
    toggle.focus();
    await fireEvent.click(toggle);

    await waitFor(() => expect(onAnnounce).toHaveBeenCalledOnce());
    expect((screen.getByRole('switch') as HTMLInputElement).checked).toBe(true);
    await waitFor(() => expect(document.activeElement).toBe(screen.getByRole('switch')));
    expect(puts).toBe(1);
    expect(onAnnounce).toHaveBeenCalledWith('Profile maintenance tracking enabled.');
    expect(screen.getByText('Since')).toBeDefined();
    expect(document.querySelector('time')?.getAttribute('datetime')).toBe('2026-08-29T01:00:00Z');
    expect(document.querySelector('time')?.textContent).not.toBe('2026-08-29T01:00:00Z');
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
      if (path.endsWith('/attribute-definitions')) return Response.json({ definitions: [] });
      return Response.json(catalog(false));
    });
    render(PersonTrackingControl, { client: createAPIClient(fetchFn), personID: 7 });

    await fireEvent.click(await screen.findByRole('switch', { name: /Update automatically — (On|Off)/ }));
    const retry = await screen.findByRole('button', { name: 'Retry' }) as HTMLButtonElement;
    expect(screen.queryByRole('switch')).toBeNull();
    expect(screen.queryByText('Uses archived conversations when updates run.')).toBeNull();
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
      if (url.pathname.endsWith('/attribute-definitions')) return Response.json({ definitions: [] });
      if (url.pathname.endsWith('/tracking')) {
        const id = Number(url.pathname.split('/').at(-2));
        return Response.json({ person_id: id, tracked: false, tracked_at: null });
      }
      queries.push(url.search);
      return Response.json(catalog(url.searchParams.get('include_sensitive') === 'true'));
    });
    const view = render(PersonTrackingControl, { client: createAPIClient(fetchFn), personID: 7 });

    expect(await screen.findByText('Time zone')).toBeDefined();
    const reveal = await screen.findByRole('button', { name: 'Show sensitive fields' });
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
      if (new URL(request.url).pathname.endsWith('/attribute-definitions')) return Response.json({ definitions: [] });
      if (new URL(request.url).pathname.endsWith('/tracking')) {
        return Response.json({ person_id: 7, tracked: false, tracked_at: null });
      }
      return Response.json({ error: 'unavailable', message: forbidden.credential }, { status: 503 });
    });
    render(PersonTrackingControl, { client: createAPIClient(fetchFn), personID: 7 });

    expect((await screen.findByRole('switch', { name: /Update automatically — (On|Off)/ }) as HTMLInputElement).disabled).toBe(false);
    expect((await screen.findByRole('alert')).textContent).toContain('Unable to load eligible profile fields.');
    expect(document.body.textContent).not.toContain(forbidden.credential);
    expect(screen.queryByRole('button', { name: 'Show sensitive fields' })).toBeNull();
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
      if (url.pathname.endsWith('/attribute-definitions')) return Response.json({ definitions: [] });
      return Response.json(catalog(false));
    });
    const view = render(PersonTrackingControl, {
      client: createAPIClient(fetchFn), personID: 7, onAnnounce
    });
    const oldToggle = await screen.findByRole('switch', { name: /Update automatically — (On|Off)/ });
    oldToggle.focus();
    await fireEvent.click(oldToggle);

    expect((screen.getByRole('switch') as HTMLInputElement).disabled).toBe(true);
    expect(document.activeElement).toBe(oldToggle);
    await fireEvent.click(oldToggle);
    expect(puts).toBe(1);

    await view.rerender({ client: createAPIClient(fetchFn), personID: 9, onAnnounce });
    const currentToggle = await screen.findByRole('switch', { name: /Update automatically — (On|Off)/ });
    currentToggle.focus();
    mutation.resolve(Response.json({ person_id: 7, tracked: true, tracked_at: '2026-08-29T01:00:00Z' }));
    await Promise.resolve();
    await Promise.resolve();

    expect(document.activeElement).toBe(currentToggle);
    expect((currentToggle as HTMLInputElement).checked).toBe(false);
    expect(onAnnounce).not.toHaveBeenCalled();
  });

  it('shows the pending change in the switch status until saving finishes', async () => {
    let mutation = deferredResponse();
    let tracked = false;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      const path = new URL(request.url).pathname;
      if (request.method === 'PUT') return mutation.promise;
      if (path.endsWith('/tracking')) return Response.json({ person_id: 7, tracked, tracked_at: null });
      if (path.endsWith('/attribute-definitions')) return Response.json({ definitions: [] });
      return Response.json(catalog(false));
    });
    const view = render(PersonTrackingControl, { client: createAPIClient(fetchFn), personID: 7 });
    await fireEvent.click(await screen.findByRole('switch'));
    expect(screen.getByText('Turning on…')).toBeDefined();
    expect(screen.getByRole('switch', { name: 'Update automatically — Off' })).toBeDefined();
    mutation.resolve(Response.json({ person_id: 7, tracked: true, tracked_at: null }));
    expect(await screen.findByRole('switch', { name: 'Update automatically — On' })).toBeDefined();
    expect(document.querySelector('.tracked-time')).toBeNull();
    view.unmount();

    tracked = true;
    mutation = deferredResponse();
    render(PersonTrackingControl, { client: createAPIClient(fetchFn), personID: 7 });
    await fireEvent.click(await screen.findByRole('switch'));
    expect(screen.getByText('Turning off…')).toBeDefined();
    mutation.resolve(Response.json({ person_id: 7, tracked: false, tracked_at: null }));
    expect(await screen.findByRole('switch', { name: 'Update automatically — Off' })).toBeDefined();
    expect(document.querySelector('.tracked-time')).toBeNull();
  });

  it('offers sensitive disclosure only after a catalog has loaded and keeps the empty state compact', async () => {
    const fields = deferredResponse();
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const path = new URL(requestOf(input).url).pathname;
      if (path.endsWith('/tracking')) return Response.json({ person_id: 7, tracked: false, tracked_at: null });
      if (path.endsWith('/attribute-definitions')) return Response.json({ definitions: [] });
      return fields.promise;
    });
    render(PersonTrackingControl, { client: createAPIClient(fetchFn), personID: 7 });
    expect(await screen.findByText('Loading profile fields…')).toBeDefined();
    expect(screen.queryByRole('button', { name: 'Show sensitive fields' })).toBeNull();
    fields.resolve(Response.json({ targets: [] }));
    expect(await screen.findByText('No fields are set up for automatic updates.')).toBeDefined();
    expect(screen.getByRole('button', { name: 'Show sensitive fields' })).toBeDefined();
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
      if (url.pathname.endsWith('/attribute-definitions')) return Response.json({ definitions: [] });
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
      await fireEvent.click(await screen.findByRole('switch', { name: /Update automatically — (On|Off)/ }));
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
      if (path.endsWith('/attribute-definitions')) return Response.json({ definitions: [] });
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
      await fireEvent.click(await screen.findByRole('switch', { name: /Update automatically — (On|Off)/ }));
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
      if (url.pathname.endsWith('/attribute-definitions')) return Response.json({ definitions: [] });
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
      const retry = await screen.findByRole('button', { name: 'Retry' });
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
