import { act, fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { expect, it, vi } from 'vitest';
import { createAPIClient } from '../../api/client';
import { DirectoryProfileController } from '../../directory/profile-controller.svelte';
import type { DirectoryReadBundle } from '../../directory/models';
import { PendingUndo } from '../../people/pending-undo.svelte';
import PersonFields from './PersonFields.svelte';

const when = '2026-08-01T00:00:00Z';
const person = { id: 7, revision: 1, display_name: 'Example Person', participant_ids: [], vcard_uid: 'example-7', created_at: when, updated_at: when };
const envelope = { id: 11, ordinal: 2, source: 'user', created_at: when, updated_at: when, vcard: {} };
function profile() { return { person, names: [], contact_points: [], addresses: [], dates: [], categories: [], media: [] } as NonNullable<DirectoryReadBundle['structuredProfile']>; }
function harness(initial = profile(), fail = false) {
  let stored = initial;
  const writes: Request[] = [];
  const client = createAPIClient(async (input) => {
    const request = input instanceof Request ? input : new Request(input);
    const path = new URL(request.url).pathname;
    if (request.method === 'PATCH') {
      writes.push(request.clone());
      if (fail) return Response.json({ error: 'person_revision_conflict', message: 'Profile changed elsewhere' }, { status: 412 });
      const body = await request.json();
      if (body.contact_points) stored = { ...stored, contact_points: (body.contact_points.add ?? []).map((point: object) => ({ ...point, person_id: 7, envelope: { ...envelope, id: 12 } })) };
      return Response.json(stored, { headers: { ETag: '"person-7-r2"' } });
    }
    if (path.endsWith('/profile')) return Response.json(stored, { headers: { ETag: '"person-7-r2"' } });
    if (path.endsWith('/attributes')) return Response.json({ attributes: [] });
    if (path.endsWith('/definitions')) return Response.json({ definitions: [] });
    return Response.json(person, { headers: { ETag: '"person-7-r2"' } });
  });
  const controller = new DirectoryProfileController(client, 7, { person, structuredProfile: initial, etags: { person: '"person-7-r1"', structuredProfile: '"person-7-r1"' }, errors: {} });
  const pending = new PendingUndo();
  const view = render(PersonFields, { controller, pending });
  return { controller, pending, writes, view };
}

it('adds an email through its row, saves with the revision, and retains it on reload', async () => {
  const { controller, view, writes } = harness();
  await fireEvent.click(screen.getByRole('button', { name: 'Add email' }));
  await fireEvent.input(screen.getByRole('textbox', { name: 'Email' }), { target: { value: 'new@example.com' } });
  await fireEvent.click(screen.getByRole('button', { name: /^Save$/ }));
  await screen.findByText('new@example.com');
  expect(writes[0]!.headers.get('If-Match')).toBe('"person-7-r1"');
  await expect(writes[0]!.json()).resolves.toEqual({ contact_points: { add: [{ address_kind: 'email', original_value: 'new@example.com', envelope: { source: 'user' } }], supersede: [] } });
  await act(() => controller.reload());
  expect(screen.getByText('new@example.com')).toBeDefined();
  view.unmount(); controller.destroy();
});

it('Undo restores a removed row without writing, while expiry supersedes its exact identity', async () => {
  const initial = profile();
  initial.contact_points = [{ person_id: 7, address_kind: 'email', original_value: 'old@example.com', normalized_value: 'old@example.com', normalization: 'email', normalization_version: 1, envelope }];
  const { controller, pending, view, writes } = harness(initial);
  await fireEvent.click(screen.getByRole('button', { name: 'Remove Email' }));
  expect(screen.queryByText('old@example.com')).toBeNull();
  expect(writes).toHaveLength(0);
  await act(() => pending.undo());
  expect(screen.getByText('old@example.com')).toBeDefined();
  await fireEvent.click(screen.getByRole('button', { name: 'Remove Email' }));
  await act(() => pending.flush());
  expect(screen.queryByText('old@example.com')).toBeNull();
  await expect(writes[0]!.json()).resolves.toEqual({ contact_points: { supersede: [11] } });
  view.unmount(); controller.destroy();
});

it('a failed removal restores the row and exposes reload recovery', async () => {
  const initial = profile();
  initial.contact_points = [{ person_id: 7, address_kind: 'email', original_value: 'old@example.com', normalized_value: 'old@example.com', normalization: 'email', normalization_version: 1, envelope }];
  const { controller, pending, view } = harness(initial, true);
  await fireEvent.click(screen.getByRole('button', { name: 'Remove Email' }));
  await act(() => pending.flush());
  expect(screen.getByText('old@example.com')).toBeDefined();
  expect(screen.getAllByRole('alert').map((item) => item.textContent).join(' ')).toContain('Profile changed elsewhere');
  expect(screen.getByRole('button', { name: 'Reload' })).toBeDefined();
  view.unmount(); controller.destroy();
});
