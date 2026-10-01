import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../../api/client';
import type { DirectoryReadBundle } from '../../directory/models';
import { DirectoryProfileController } from '../../directory/profile-controller.svelte';
import PersonRecordActions from './PersonRecordActions.svelte';

const when = '2026-08-01T00:00:00Z';

function person() {
  return { id: 7, revision: 3, display_name: 'Test User', participant_ids: [], vcard_uid: 'person-7', created_at: when, updated_at: when };
}

function renderActions(fetchFn: typeof fetch, personETag: string | null = '"person-7-r3"') {
  const bundle = {
    person: person(),
    etags: { person: personETag ?? undefined }, errors: {}
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
  it('renames the selected person with the current strong ETag', async () => {
    const requests: Request[] = [];
    renderActions(vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      return new Response(JSON.stringify({ ...person(), revision: 4, display_name: 'Renamed User' }), {
        headers: { 'Content-Type': 'application/json', ETag: '"person-7-r4"' }
      });
    }));

    await choose('Rename person');
    expect(screen.getByRole('group', { name: 'Rename person' })).toBeDefined();
    await fireEvent.input(screen.getByRole('textbox', { name: 'Display name' }), { target: { value: 'Renamed User' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Save display name' }));

    await waitFor(() => expect(requests).toHaveLength(1));
    expect(requests[0]!.method).toBe('PATCH');
    expect(requests[0]!.headers.get('If-Match')).toBe('"person-7-r3"');
    await expect(requests[0]!.clone().json()).resolves.toEqual({ display_name: 'Renamed User' });
  });

  it('locks rename controls while the non-abortable write is pending', async () => {
    let resolveRename!: (response: Response) => void;
    const pendingRename = new Promise<Response>((resolve) => { resolveRename = resolve; });
    const controller = renderActions(vi.fn<typeof fetch>(async () => pendingRename));

    await choose('Rename person');
    await fireEvent.click(screen.getByRole('button', { name: 'Save display name' }));

    await waitFor(() => expect(controller.mutationPending).toBe(true));
    expect(screen.getByRole('button', { name: 'Cancel rename' })).toHaveProperty('disabled', true);
    expect(screen.getByRole('button', { name: 'Renaming…' })).toHaveProperty('disabled', true);

    resolveRename(new Response(JSON.stringify({ ...person(), revision: 4 }), {
      headers: { 'Content-Type': 'application/json', ETag: '"person-7-r4"' }
    }));
    await waitFor(() => expect(controller.mutationPending).toBe(false));
  });

  it('requires confirmation and the current strong ETag before deleting a person', async () => {
    const requests: Request[] = [];
    renderActions(vi.fn<typeof fetch>(async (input) => {
      requests.push(input instanceof Request ? input : new Request(input));
      return new Response(null, { status: 204 });
    }));

    await choose('Delete person');
    expect(requests).toHaveLength(0);
    expect(screen.getByRole('group', { name: 'Confirm deleting person' })).toBeDefined();
    await fireEvent.click(screen.getByRole('button', { name: 'Confirm delete person' }));

    await waitFor(() => expect(requests).toHaveLength(1));
    expect(requests[0]!.method).toBe('DELETE');
    expect(requests[0]!.headers.get('If-Match')).toBe('"person-7-r3"');
  });

  it('locks delete confirmation while the non-abortable delete is pending', async () => {
    let resolveDelete!: (response: Response) => void;
    const pendingDelete = new Promise<Response>((resolve) => { resolveDelete = resolve; });
    const controller = renderActions(vi.fn<typeof fetch>(async () => pendingDelete));

    await choose('Delete person');
    await fireEvent.click(screen.getByRole('button', { name: 'Confirm delete person' }));

    await waitFor(() => expect(controller.mutationPending).toBe(true));
    expect(screen.getByRole('button', { name: 'Cancel delete' })).toHaveProperty('disabled', true);
    expect(screen.getByRole('button', { name: 'Deleting…' })).toHaveProperty('disabled', true);

    resolveDelete(new Response(null, { status: 204 }));
    await waitFor(() => expect(controller.mutationPending).toBe(false));
  });

  it('shows one of rename or delete at a time', async () => {
    renderActions(vi.fn());

    await choose('Rename person');
    await choose('Delete person');
    expect(screen.queryByRole('group', { name: 'Rename person' })).toBeNull();
    expect(screen.getByRole('group', { name: 'Confirm deleting person' })).toBeDefined();

    await choose('Rename person');
    expect(screen.queryByRole('group', { name: 'Confirm deleting person' })).toBeNull();
    expect(screen.getByRole('group', { name: 'Rename person' })).toBeDefined();
  });

  it('disables both write actions when the person cannot be written', async () => {
    renderActions(vi.fn(), null);

    await fireEvent.click(screen.getByRole('button', { name: 'More actions' }));
    expect(screen.getByRole('menuitem', { name: 'Rename person' }).getAttribute('aria-disabled')).toBe('true');
    expect(screen.getByRole('menuitem', { name: 'Delete person' }).getAttribute('aria-disabled')).toBe('true');
    expect(screen.getByRole('menuitem', { name: 'View profile history' }).getAttribute('aria-disabled')).toBeNull();

    await fireEvent.click(screen.getByRole('menuitem', { name: 'Rename person' }));
    expect(screen.queryByRole('group', { name: 'Rename person' })).toBeNull();
  });

  it('opens history and returns focus to the More actions trigger when it closes', async () => {
    renderActions(vi.fn<typeof fetch>(async () => Response.json({
      person: person(),
      names: [], contact_points: [], addresses: [], dates: [], categories: [], media: [], observations: []
    })));
    const trigger = screen.getByRole('button', { name: 'More actions' });

    await choose('View profile history');
    const dialog = await screen.findByRole('dialog', { name: 'Profile history' });
    await waitFor(() => expect(dialog.contains(document.activeElement)).toBe(true));
    await fireEvent.click(screen.getByRole('button', { name: 'Close profile history' }));

    await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Profile history' })).toBeNull());
    expect(document.activeElement).toBe(trigger);
  });
});
