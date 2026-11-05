import { emptyProfile } from '../../directory/profile-fixtures.test-support';
import { definition } from '../../directory/attribute-fixtures.test-support';
import { fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../../api/client';
import type { DirectoryReadBundle } from '../../directory/models';
import { DirectoryProfileController } from '../../directory/profile-controller.svelte';
import PersonRecordActions from './PersonRecordActions.svelte';
import PersonFields from '../people/PersonFields.svelte';

const when = '2026-08-01T00:00:00Z';

function person() {
  return { id: 7, revision: 3, display_name: 'Test User', participant_ids: [], vcard_uid: 'person-7', created_at: when, updated_at: when };
}

function renderActions(fetchFn: typeof fetch, personETag: string | null = '"person-7-r3"', initial: Partial<DirectoryReadBundle> = {}) {
  const bundle = {
    person: person(),
    etags: { person: personETag ?? undefined }, errors: {}, ...initial
  } satisfies DirectoryReadBundle;
  const client = createAPIClient(fetchFn);
  const controller = new DirectoryProfileController(client, 7, bundle);
  render(PersonRecordActions, { client, controller, personID: 7 });
  return controller;
}

async function choose(action: string): Promise<void> {
  await fireEvent.click(screen.getByRole('button', { name: 'More actions' }));
  await fireEvent.click(screen.getByRole('menuitem', { name: action }));
}

describe('PersonRecordActions', () => {
  it('loads labels from Messages without delaying opening or replacing a newer dialog', async () => {
    let resolveDefinitions!: (response: Response) => void;
    const pendingDefinitions = new Promise<Response>(resolve => resolveDefinitions = resolve);
    const fetchFn = vi.fn<typeof fetch>(async input => {
      const path = new URL((input as Request).url).pathname;
      if (path.endsWith('/attribute-definitions')) return pendingDefinitions;
      if (path.endsWith('/tracking')) return Response.json({ person_id: 7, tracked: false, tracked_at: null });
      return Response.json({ targets: [{ kind: 'attribute', slug: 'conversation_topics', sensitive: false }] });
    });
    renderActions(fetchFn);
    await choose('Keep this profile updated');
    const dialog = await screen.findByRole('dialog', { name: "Keep Test User's profile updated" });
    expect(await within(dialog).findByText('Conversation topics')).toBeDefined();
    await waitFor(() => expect(fetchFn.mock.calls.some(([input]) => new URL((input as Request).url).pathname.endsWith('/attribute-definitions'))).toBe(true));
    resolveDefinitions(Response.json({ definitions: [definition({ slug: 'conversation_topics', label: 'Conversation starters' })] }));
    expect(await within(dialog).findByText('Conversation starters')).toBeDefined();
    await fireEvent.click(within(dialog).getByRole('button', { name: 'Close' }));
    const lateDefinitions = new Promise<Response>(resolve => resolveDefinitions = resolve);
    fetchFn.mockImplementation(async input => {
      const path = new URL((input as Request).url).pathname;
      if (path.endsWith('/attribute-definitions')) return lateDefinitions;
      if (path.endsWith('/tracking')) return Response.json({ person_id: 7, tracked: false, tracked_at: null });
      return Response.json({ targets: [] });
    });
    await choose('Keep this profile updated');
    await fireEvent.click(within(await screen.findByRole('dialog')).getByRole('button', { name: 'Close' }));
    await choose('Publish to address book');
    resolveDefinitions(Response.json({ definitions: [definition({ slug: 'conversation_topics', label: 'Conversation starters' })] }));
    expect(await screen.findByRole('dialog', { name: 'Publish to address book' })).toBeDefined();
  });

  it('opens named profile updates and returns focus after Close', async () => {
    renderActions(vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      return Response.json(new URL(request.url).pathname.endsWith('/tracking')
        ? { person_id: 7, tracked: false, tracked_at: null }
        : { targets: [] });
    }));
    const trigger = screen.getByRole('button', { name: 'More actions' });
    await choose('Keep this profile updated');
    const dialog = await screen.findByRole('dialog', { name: "Keep Test User's profile updated" });
    await fireEvent.click(within(dialog).getByRole('button', { name: 'Close' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
    expect(document.activeElement).toBe(trigger);
  });

  it('opens with embedded labels and preserves loaded fields when the registry is unavailable', async () => {
    const fetchFn = vi.fn<typeof fetch>(async input => {
      const request = input instanceof Request ? input : new Request(input);
      const path = new URL(request.url).pathname;
      if (path.endsWith('/attribute-definitions')) return Response.json({ message: 'Registry temporarily unavailable' }, { status: 503 });
      if (path.endsWith('/tracking')) return Response.json({ person_id: 7, tracked: false, tracked_at: null });
      return Response.json({ targets: [{ kind: 'attribute', slug: 'favorites_food', sensitive: false }] });
    });
    const controller = renderActions(fetchFn, '"person-7-r3"', {
      structuredProfile: emptyProfile(person()),
      attributes: { person_id: 7, attributes: [{ definition: definition({ slug: 'favorites_food', label: 'Favorite food', is_sensitive: false }), current: [], history: [] }] }
    });
    render(PersonFields, { controller });
    await choose('Keep this profile updated');
    const dialog = await screen.findByRole('dialog', { name: "Keep Test User's profile updated" });
    expect(await within(dialog).findByText('Favorite food')).toBeDefined();
    expect(fetchFn.mock.calls.map(([input]) => new URL((input as Request).url).pathname).sort()).toEqual([
      '/api/v1/attribute-definitions', '/api/v1/people/7/tracking', '/api/v1/person-fact-targets'
    ]);
    await fireEvent.click(within(dialog).getByRole('button', { name: 'Close' }));
    expect(screen.queryByRole('alert')).toBeNull();
    expect(screen.queryByRole('button', { name: 'Retry fields' })).toBeNull();
    await fireEvent.click(screen.getByRole('button', { name: 'More fields' }));
    expect(screen.getByRole('menuitem', { name: 'Favorite food' })).toBeDefined();
  });

  it('prefers registry labels over embedded labels for the same definition', async () => {
    renderActions(vi.fn<typeof fetch>(async input => {
      const request = input instanceof Request ? input : new Request(input);
      if (new URL(request.url).pathname.endsWith('/tracking')) return Response.json({ person_id: 7, tracked: false, tracked_at: null });
      return Response.json({ targets: [{ kind: 'attribute', slug: 'favorites_food', sensitive: false }] });
    }), '"person-7-r3"', {
      definitions: { definitions: [definition({ slug: 'favorites_food', label: 'Preferred food' })] },
      attributes: { person_id: 7, attributes: [{ definition: definition({ slug: 'favorites_food', label: 'Favorite food' }), current: [], history: [] }] }
    });
    await choose('Keep this profile updated');
    expect(await screen.findByText('Preferred food')).toBeDefined();
    expect(screen.queryByText('Favorite food')).toBeNull();
  });

  it('uses the slug fallback when attribute definitions are unavailable', async () => {
    renderActions(vi.fn<typeof fetch>(async input => {
      const request = input instanceof Request ? input : new Request(input);
      const path = new URL(request.url).pathname;
      if (path.endsWith('/attribute-definitions')) return Response.json({ message: 'Registry temporarily unavailable' }, { status: 503 });
      if (path.endsWith('/tracking')) return Response.json({ person_id: 7, tracked: false, tracked_at: null });
      return Response.json({ targets: [{ kind: 'attribute', slug: 'favorites_food', sensitive: false }] });
    }));
    await choose('Keep this profile updated');
    const dialog = await screen.findByRole('dialog', { name: "Keep Test User's profile updated" });
    expect(await within(dialog).findByText('Favorites food')).toBeDefined();
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

    await choose('Rename');
    expect(screen.getByRole('group', { name: 'Rename person' })).toBeDefined();
    await fireEvent.input(screen.getByRole('textbox', { name: 'Display name' }), { target: { value: 'Renamed User' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Save display name' }));

    await waitFor(() => expect(screen.queryByRole('group', { name: 'Rename person' })).toBeNull());
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
      await choose('Rename');
      const input = screen.getByRole('textbox', { name: 'Display name' });
      await fireEvent.input(input, { target: { value: 'Renamed User' } });
      await fireEvent.click(screen.getByRole('button', { name: 'Save display name' }));

      await waitFor(() => expect(controller.mutationPending).toBe(true));
      expect(input).toHaveProperty('disabled', true);
      expect(screen.getByRole('button', { name: 'Cancel rename' })).toHaveProperty('disabled', true);
      expect(screen.getByRole('button', { name: 'Renaming…' })).toHaveProperty('disabled', true);

      resolveRename(Response.json(renamed, { headers: { ETag: '"person-7-r4"' } }));
      await waitFor(() => expect(controller.reloadPending).toBe(true));
      expect(controller.mutationPending).toBe(false);
      expect(input).toHaveProperty('disabled', true);
      expect(input).toHaveProperty('value', 'Renamed User');
      releaseReadback();
      await waitFor(() => expect(screen.queryByRole('group', { name: 'Rename person' })).toBeNull());
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
      requests.push(request);
      return new Response(null, { status: 204 });
    }));

    await choose('Delete');
    expect(requests).toHaveLength(0);
    expect(screen.getByRole('group', { name: 'Confirm deleting person' })).toBeDefined();
    await fireEvent.click(screen.getByRole('button', { name: 'Confirm delete person' }));

    await waitFor(() => expect(screen.queryByRole('group', { name: 'Confirm deleting person' })).toBeNull());
    expect(requests).toHaveLength(1);
    expect(requests[0]!.method).toBe('DELETE');
    expect(requests[0]!.headers.get('If-Match')).toBe('"person-7-r3"');
  });

  it('locks delete confirmation while the non-abortable delete is pending', async () => {
    let resolveDelete!: (response: Response) => void;
    const pendingDelete = new Promise<Response>((resolve) => { resolveDelete = resolve; });
    const controller = renderActions(vi.fn<typeof fetch>(async () => pendingDelete));

    await choose('Delete');
    await fireEvent.click(screen.getByRole('button', { name: 'Confirm delete person' }));

    await waitFor(() => expect(controller.mutationPending).toBe(true));
    expect(screen.getByRole('button', { name: 'Cancel delete' })).toHaveProperty('disabled', true);
    expect(screen.getByRole('button', { name: 'Deleting…' })).toHaveProperty('disabled', true);

    resolveDelete(new Response(null, { status: 204 }));
    await waitFor(() => expect(controller.mutationPending).toBe(false));
  });

  it('disables both write actions when the person cannot be written', async () => {
    renderActions(vi.fn(), null);

    await fireEvent.click(screen.getByRole('button', { name: 'More actions' }));
    expect(screen.getByRole('menuitem', { name: 'Rename' }).getAttribute('aria-disabled')).toBe('true');
    expect(screen.getByRole('menuitem', { name: 'Delete' }).getAttribute('aria-disabled')).toBe('true');
    expect(screen.getByRole('menuitem', { name: 'History' }).getAttribute('aria-disabled')).toBeNull();

    await fireEvent.click(screen.getByRole('menuitem', { name: 'Rename' }));
    expect(screen.queryByRole('group', { name: 'Rename person' })).toBeNull();
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
