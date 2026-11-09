import { tick } from 'svelte';
import { emptyProfile } from '../../directory/profile-fixtures.test-support';
import { fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../../api/client';
import type { DirectoryReadBundle } from '../../directory/models';
import { DirectoryProfileController } from '../../directory/profile-controller.svelte';
import { PersonTrackingController } from '../../directory/person-tracking-controller.svelte';
import PersonRecordActions from './PersonRecordActions.svelte';

const when = '2026-08-01T00:00:00Z';

afterEach(() => vi.restoreAllMocks());

function person() {
  return { id: 7, revision: 3, display_name: 'Test User', participant_ids: [], vcard_uid: 'person-7', created_at: when, updated_at: when };
}

function renderActions(fetchFn: typeof fetch, personETag: string | null = '"person-7-r3"', onSetUpTracking?: () => void, onAnnounce?: (text: string) => void) {
  const bundle = {
    person: person(),
    etags: { person: personETag ?? undefined }, errors: {}
  } satisfies DirectoryReadBundle;
  const client = createAPIClient(fetchFn);
  const controller = new DirectoryProfileController(client, 7, bundle);
  render(PersonRecordActions, { client, controller, personID: 7, onSetUpTracking, onAnnounce });
  return controller;
}

async function choose(action: string): Promise<void> {
  await fireEvent.click(screen.getByRole('button', { name: 'More actions' }));
  await fireEvent.click(screen.getByRole('menuitem', { name: action }));
}

describe('PersonRecordActions', () => {
  it.each([
    ['saved', 'Keeping this profile updated.'],
    ['destroyed after settlement', null],
  ] as const)('handles a save started before the menu closes: %s', async (outcome, announcement) => {
    let settled = false;
    const original = PersonTrackingController.prototype.setTracked;
    const settlement = outcome === 'destroyed after settlement' ? vi.spyOn(PersonTrackingController.prototype, 'setTracked').mockImplementation(async function (this: PersonTrackingController, desired) {
      const result = await original.call(this, desired);
      this.destroy();
      settled = true;
      return result;
    }) : undefined;
    let release!: () => void;
    const held = new Promise<void>(resolve => { release = resolve; });
    const onAnnounce = vi.fn();
    let saved = false;
    let failed = false;
    const methods: string[] = [];
    renderActions(async input => {
      const request = input as Request;
      methods.push(request.method);
      if (request.method !== 'PUT') {
        return Response.json({ person_id: 7, tracked: saved, tracked_at: saved ? '2026-11-05T12:00:00Z' : null });
      }
      const { tracked } = await request.json();
      await held;
      if (outcome === 'saved' && !failed) { failed = true; return Response.json({ message: 'Save unavailable' }, { status: 503 }); }
      saved = tracked;
      return Response.json({ person_id: 7, tracked, tracked_at: '2026-11-05T12:00:00Z' });
    }, undefined, undefined, onAnnounce);
    await fireEvent.click(screen.getByRole('button', { name: 'More actions' }));
    const item = await screen.findByRole('menuitemcheckbox', { name: /Keep this profile updated/ });
    await waitFor(() => expect(item.textContent?.trim()).toBe('Keep this profile updated'));
    await fireEvent.click(item);
    expect(item.textContent).toContain('Turning on…');
    expect(screen.getByRole('menu')).toBeDefined();
    await fireEvent.keyDown(item, { key: 'Escape' });
    await waitFor(() => expect(screen.queryByRole('menu')).toBeNull());
    await fireEvent.click(screen.getByRole('button', { name: 'More actions' }));
    expect(screen.getByRole('menuitemcheckbox', { name: /Keep this profile updated/ }).textContent).toContain('Turning on…');
    expect(methods).toEqual(['GET', 'PUT']);
    if (outcome === 'destroyed after settlement') {
      await fireEvent.keyDown(screen.getByRole('menu'), { key: 'Escape' });
      await waitFor(() => expect(screen.queryByRole('menu')).toBeNull());
    }
    release();
    if (outcome === 'destroyed after settlement') {
      await waitFor(() => expect(settled).toBe(true));
      await tick();
      expect(onAnnounce).not.toHaveBeenCalled();
      settlement?.mockRestore();
      return;
    }
    await waitFor(() => expect(onAnnounce).toHaveBeenCalledWith("Couldn't save profile updates."));
    const retryItem = await screen.findByRole('menuitemcheckbox', { name: /Keep this profile updated/ });
    await waitFor(() => expect(retryItem.textContent).toContain("Couldn't save · Retry"));
    await fireEvent.click(retryItem);
    await waitFor(() => expect(screen.getByRole('menuitemcheckbox', { name: /Keep this profile updated/ }).getAttribute('aria-checked')).toBe('true'));
    await waitFor(() => expect(onAnnounce).toHaveBeenCalledWith(announcement));
    settlement?.mockRestore();
  });

  it('reads the state again each time the menu opens', async () => {
    let tracked = true;
    const reads: string[] = [];
    renderActions(async input => {
      reads.push(new URL((input as Request).url).pathname);
      return Response.json({ person_id: 7, tracked, tracked_at: tracked ? when : null });
    });
    await tick();
    expect(reads).toEqual([]);
    await fireEvent.click(screen.getByRole('button', { name: 'More actions' }));
    await waitFor(() => expect(screen.getByRole('menuitemcheckbox', { name: /Keep this profile updated/ }).getAttribute('aria-checked')).toBe('true'));
    await fireEvent.keyDown(screen.getByRole('menu'), { key: 'Escape' });
    await waitFor(() => expect(screen.queryByRole('menu')).toBeNull());
    // A split that hands the absorbed profile's enrollment back turns it off here.
    expect(reads).toEqual(['/api/v1/people/7/tracking']);
    tracked = false;
    await fireEvent.click(screen.getByRole('button', { name: 'More actions' }));
    await waitFor(() => expect(screen.getByRole('menuitemcheckbox', { name: /Keep this profile updated/ }).getAttribute('aria-checked')).toBe('false'));
  });

  it.each([false, true])('offers setup while preserving the ability to turn existing tracking off: %s', async initiallyTracked => {
    const setUp = vi.fn(), writes: Request[] = [];
    let tracked = initiallyTracked;
    let reads = 0;
    renderActions(vi.fn<typeof fetch>(async input => {
      const request = input instanceof Request ? input : new Request(input);
      if (request.method === 'GET' && ++reads === 1) return Response.json({}, { status: 503 });
      if (request.method === 'PUT') { writes.push(request); tracked = (await request.json()).tracked; }
      return Response.json({ person_id: 7, tracked, tracked_at: null });
    }), undefined, setUp);
    await fireEvent.click(screen.getByRole('button', { name: 'More actions' }));
    const item = await screen.findByRole('menuitemcheckbox', { name: /Keep this profile updated/ });
    await waitFor(() => expect(item.textContent).toContain("Couldn't check"));
    expect(item.textContent).not.toContain('Set up…');
    await fireEvent.click(item);
    await waitFor(() => expect(item.textContent).not.toContain("Couldn't check"));
    await waitFor(() => expect(item.getAttribute('aria-checked')).toBe(String(initiallyTracked)));
    if (initiallyTracked) {
      expect(item.textContent).not.toContain('Set up…');
      await fireEvent.click(item);
      await waitFor(() => expect(item.getAttribute('aria-checked')).toBe('false'));
      expect(writes).toHaveLength(1);
      expect(setUp).not.toHaveBeenCalled();
    }
    await waitFor(() => expect(item.textContent).toContain('Set up…'));
    await fireEvent.click(item);
    expect(setUp).toHaveBeenCalledOnce();
  });

  it('renames the selected person with the current strong ETag', async () => {
    const requests: Request[] = [];
    renderActions(vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      if (request.method === 'GET') return Response.json({ message: 'Readback unavailable' }, { status: 503 });
      requests.push(request);
      return new Response(JSON.stringify({ ...person(), revision: 4, display_name: 'Renamed User' }), {
        headers: { 'Content-Type': 'application/json', ETag: '"person-7-r4"' }
      });
    }));

    await choose('Rename…');
    expect(screen.getByRole('dialog', { name: 'Rename person' })).toBeDefined();
    await fireEvent.input(screen.getByRole('textbox', { name: 'Display name' }), { target: { value: 'Renamed User' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Save' }));

    await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Rename person' })).toBeNull());
    expect(requests).toHaveLength(1);
    expect(requests[0]!.method).toBe('PATCH');
    expect(requests[0]!.headers.get('If-Match')).toBe('"person-7-r3"');
    await expect(requests[0]!.clone().json()).resolves.toEqual({ display_name: 'Renamed User' });
  });

  it('locks the rename input through the pending write and deferred readback', async () => {
    let resolveRename!: (response: Response) => void;
    let releaseReadback!: () => void;
    const pendingRename = new Promise<Response>(resolve => resolveRename = resolve);
    const pendingReadback = new Promise<void>(resolve => releaseReadback = resolve);
    const renamed = { ...person(), revision: 4, display_name: 'Renamed User' };
    const controller = renderActions(vi.fn<typeof fetch>(async input => {
      const request = input instanceof Request ? input : new Request(input);
      if (request.method !== 'GET') return pendingRename;
      await pendingReadback;
      const path = new URL(request.url).pathname;
      if (path.endsWith('/profile')) return Response.json(emptyProfile(renamed), { headers: { ETag: '"person-7-r4"' } });
      if (path.endsWith('/attributes')) return Response.json({ person_id: 7, attributes: [] });
      if (path.endsWith('/attribute-definitions')) return Response.json({ definitions: [] });
      if (path.endsWith('/contact-state')) return Response.json({ person_id: 7, cadence_status: 'unknown', interaction_count: 0, computed_at: when, stale: false });
      return Response.json(renamed, { headers: { ETag: '"person-7-r4"' } });
    }));
    try {
      await choose('Rename…');
      const input = screen.getByRole('textbox', { name: 'Display name' });
      await fireEvent.input(input, { target: { value: 'Renamed User' } });
      await fireEvent.click(screen.getByRole('button', { name: 'Save' }));

      await waitFor(() => expect(controller.mutationPending).toBe(true));
      expect(input).toHaveProperty('disabled', true);
      expect(screen.getByRole('button', { name: 'Cancel' })).toHaveProperty('disabled', true);
      expect(screen.getByRole('button', { name: 'Saving…' })).toHaveProperty('disabled', true);

      resolveRename(Response.json(renamed, { headers: { ETag: '"person-7-r4"' } }));
      await waitFor(() => expect(controller.reloadPending).toBe(true));
      expect(controller.mutationPending).toBe(false);
      expect(input).toHaveProperty('disabled', true);
      expect(input).toHaveProperty('value', 'Renamed User');
      releaseReadback();
      await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Rename person' })).toBeNull());
    } finally {
      resolveRename(Response.json(renamed, { headers: { ETag: '"person-7-r4"' } }));
      releaseReadback();
      await waitFor(() => expect(controller.reloadPending).toBe(false));
      controller.destroy();
    }
  });

  it('requires confirmation and the current strong ETag before deleting a person', async () => {
    const requests: Request[] = [];
    renderActions(vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      if (request.method !== 'GET' || !request.url.endsWith('/tracking')) requests.push(request);
      return new Response(null, { status: 204 });
    }));

    await choose('Delete…');
    expect(requests).toHaveLength(0);
    expect(screen.getByRole('dialog', { name: 'Delete Test User?' })).toBeDefined();
    await fireEvent.click(screen.getByRole('button', { name: 'Delete' }));

    await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Delete Test User?' })).toBeNull());
    expect(requests).toHaveLength(1);
    expect(requests[0]!.method).toBe('DELETE');
    expect(requests[0]!.headers.get('If-Match')).toBe('"person-7-r3"');
  });

  it('locks delete confirmation while the non-abortable delete is pending', async () => {
    let resolveDelete!: (response: Response) => void;
    const pendingDelete = new Promise<Response>((resolve) => { resolveDelete = resolve; });
    const controller = renderActions(vi.fn<typeof fetch>(async () => pendingDelete));

    await choose('Delete…');
    await fireEvent.click(screen.getByRole('button', { name: 'Delete' }));

    await waitFor(() => expect(controller.mutationPending).toBe(true));
    expect(screen.getByRole('button', { name: 'Cancel' })).toHaveProperty('disabled', true);
    expect(screen.getByRole('button', { name: 'Delete' })).toHaveProperty('disabled', true);

    resolveDelete(new Response(null, { status: 204 }));
    await waitFor(() => expect(controller.mutationPending).toBe(false));
  });

  it('disables both write actions when the person cannot be written', async () => {
    renderActions(vi.fn(), null);

    await fireEvent.click(screen.getByRole('button', { name: 'More actions' }));
    expect(screen.getByRole('menuitem', { name: 'Rename…' }).getAttribute('aria-disabled')).toBe('true');
    expect(screen.getByRole('menuitem', { name: 'Delete…' }).getAttribute('aria-disabled')).toBe('true');
    expect(screen.getByRole('menuitem', { name: 'History' }).getAttribute('aria-disabled')).toBeNull();
    expect(screen.getByRole('menuitem', { name: 'Publish to address book…' }).getAttribute('aria-disabled')).toBeNull();

    await fireEvent.click(screen.getByRole('menuitem', { name: 'Rename…' }));
    expect(screen.queryByRole('dialog', { name: 'Rename person' })).toBeNull();
  });

  it('opens history and returns focus to the More actions trigger when it closes', async () => {
    renderActions(vi.fn<typeof fetch>(async () => Response.json({
      person: person(),
      names: [], contact_points: [], addresses: [], dates: [], categories: [], media: [], observations: []
    })));
    const trigger = screen.getByRole('button', { name: 'More actions' });

    await choose('History');
    const dialog = await screen.findByRole('dialog', { name: 'History' });
    await waitFor(() => expect(dialog.contains(document.activeElement)).toBe(true));
    await fireEvent.click(within(dialog).getByRole('button', { name: 'Close' }));

    await waitFor(() => expect(screen.queryByRole('dialog', { name: 'History' })).toBeNull());
    expect(document.activeElement).toBe(trigger);
  });
});
