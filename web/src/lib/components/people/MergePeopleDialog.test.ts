import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { expect, it, vi } from 'vitest';
import { createAPIClient } from '../../api/client';
import { RelationshipsController } from '../../relationships/controller.svelte';
import { openTypeahead } from '../../../test/kit-ui';
import MergePeopleDialog from './MergePeopleDialog.svelte';

it.each([false, true])('merges saved profiles with both revisions, promoting an observed choice only when needed: %s', async promote => {
  const requests: Request[] = [];
  let failProfile = promote;
  const profile = (id: number) => ({ id, revision: id === 7 ? 4 : 2, display_name: id === 7 ? 'Synthetic One' : 'Synthetic Two', participant_ids: id === 7 ? [] : [2], created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z', vcard_uid: `example-${id}` });
  const client = createAPIClient(async input => {
    const request = input instanceof Request ? input : new Request(input);
    requests.push(request.clone());
    const path = new URL(request.url).pathname;
    if (path === '/api/v1/people/directory') return Response.json({ people: promote ? [] : [profile(9)] });
    if (path === '/api/v1/participants/search') return Response.json({ rows: promote ? [{ id: 2, display_label: 'Synthetic Two', identifiers: [], activity_count: 1 }] : [] });
    if (path === '/api/v1/people' && request.method === 'POST') return Response.json(profile(9));
    if (path === '/api/v1/people/7' || path === '/api/v1/people/9') {
      if (path.endsWith('/9') && failProfile) { failProfile = false; return Response.json({ error: 'unavailable' }, { status: 503 }); }
      const id = Number(path.split('/').at(-1));
      return Response.json(profile(id), { headers: { ETag: id === 7 ? '"person-7-r4"' : '"person-9-r2"' } });
    }
    if (path === '/api/v1/people/7/merge') return Response.json({ person: profile(7), cache_state: 'ready', identity_revision: 3, merge: { id: 41 }, review_candidates: [] }, { headers: { ETag: '"person-7-r5"' } });
    throw new Error(`Unexpected ${request.method} ${path}`);
  });
  const controller = new RelationshipsController(client, () => 'UTC');
  const onReconcile = vi.fn(async () => undefined), onOpenPerson = vi.fn();
  const view = render(MergePeopleDialog, { client, controller, personID: 7, participantID: null, name: 'Synthetic One', onReconcile, onOpenPerson, onClose: vi.fn() });
  try {
    await fireEvent.input(await openTypeahead('Search people'), { target: { value: 'Synthetic' } });
    await fireEvent.mouseDown(await screen.findByRole('option', { name: /Synthetic Two/ }));
    await fireEvent.click(screen.getByRole('button', { name: 'Merge' }));
    if (promote) {
      await screen.findByText('Could not read both people. Retry the merge.');
      await fireEvent.click(screen.getByRole('button', { name: 'Merge' }));
    }
    await screen.findByRole('dialog', { name: 'Resolve person merge' });
    await fireEvent.click(screen.getByRole('radio', { name: 'Synthetic One (Person 7)' }));
    await fireEvent.click(screen.getByRole('checkbox', { name: /I understand/ }));
    await fireEvent.click(screen.getByRole('button', { name: 'Merge into selected survivor' }));
    await waitFor(() => expect(onOpenPerson).toHaveBeenCalledWith(7));
    const promotions = requests.filter(request => new URL(request.url).pathname === '/api/v1/people' && request.method === 'POST');
    expect(promotions).toHaveLength(promote ? 1 : 0);
    if (promote) expect(await promotions[0]!.json()).toEqual({ participant_id: 2 });
    const mutation = requests.find(request => new URL(request.url).pathname.endsWith('/7/merge'))!;
    expect(mutation.headers.get('If-Match')).toBe('"person-7-r4", "person-9-r2"');
    expect(await mutation.json()).toEqual({ absorbed_person_id: 9 });
    expect(onReconcile).toHaveBeenCalledOnce();
  } finally { view.unmount(); controller.destroy(); }
});

it('links an observed person to a saved profile representative and reconciles the view', async () => {
  const links: unknown[] = [];
  const client = createAPIClient(async input => {
    const request = input instanceof Request ? input : new Request(input);
    const path = new URL(request.url).pathname;
    if (path.endsWith('/directory')) return Response.json({ people: [{ id: 7, display_name: 'Synthetic Saved' }] });
    if (path.endsWith('/participants/search')) return Response.json({ rows: [{ id: 2, display_label: 'Duplicate Saved', profile: { id: 7 } }] });
    if (path.endsWith('/people/7')) return Response.json({ id: 7, participant_ids: [2, 3] });
    if (path.endsWith('/identity/links')) { links.push(await request.json()); return Response.json({ identity_revision: 2, cache_state: 'ready' }); }
    throw new Error(`Unexpected ${path}`);
  });
  const controller = new RelationshipsController(client, () => 'UTC');
  const onReconcile = vi.fn(async () => undefined), onClose = vi.fn();
  const view = render(MergePeopleDialog, { client, controller, personID: null, participantID: 1, name: 'Synthetic Observed', onReconcile, onClose });
  try {
    await fireEvent.input(await openTypeahead('Search people'), { target: { value: 'Synthetic' } });
    await fireEvent.mouseDown(await screen.findByRole('option', { name: /Synthetic Saved/ }));
    expect(screen.queryByRole('option', { name: /Duplicate Saved/ })).toBeNull();
    await fireEvent.click(screen.getByRole('button', { name: 'Merge' }));
    await waitFor(() => expect(onClose).toHaveBeenCalledOnce());
    expect(links).toEqual([{ participant_a: 1, participant_b: 2 }]);
    expect(onReconcile).toHaveBeenCalledOnce();
  } finally { view.unmount(); controller.destroy(); }
});
