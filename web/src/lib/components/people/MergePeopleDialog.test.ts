import { person as profile } from '../../directory/entity-fixtures.test-support';
import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { expect, it, vi } from 'vitest';
import { createAPIClient } from '../../api/client';
import { RelationshipsController } from '../../relationships/controller.svelte';
import { openTypeahead } from '../../../test/kit-ui';
import MergePeopleDialog from './MergePeopleDialog.svelte';

it.each([false, true])('merges saved profiles and promotes an observed choice only when needed: %s', async promote => {
  const requests: Request[] = [];
  let failProfile = promote;
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
    await waitFor(() => expect(onReconcile).toHaveBeenCalledOnce());
    const promotions = requests.filter(request => new URL(request.url).pathname === '/api/v1/people' && request.method === 'POST');
    expect(promotions).toHaveLength(promote ? 1 : 0);
    if (promote) expect(await promotions[0]!.json()).toEqual({ participant_id: 2 });
    expect(onOpenPerson).toHaveBeenCalledWith(7);
  } finally { view.unmount(); controller.destroy(); }
});

function renderRepresentativeLink(savedAnchor: boolean, missing = false) {
  const links: unknown[] = [];
  const client = createAPIClient(async input => {
    const request = input instanceof Request ? input : new Request(input);
    const path = new URL(request.url).pathname;
    if (path.endsWith('/directory')) return Response.json({ people: savedAnchor ? [] : [{ id: 7, display_name: 'Synthetic Saved' }] });
    if (path.endsWith('/participants/search')) return Response.json({ rows: savedAnchor ? [{ id: 1, display_label: 'Synthetic Observed' }] : [] });
    if (path.endsWith('/people/7')) return missing ? Response.json({ message: 'Target missing' }, { status: 404 }) : Response.json({ id: 7, participant_ids: [2, 3] });
    if (path.endsWith('/identity/links')) { links.push(await request.json()); return Response.json({ identity_revision: 2, cache_state: 'ready' }); }
    throw new Error(`Unexpected ${path}`);
  });
  const controller = new RelationshipsController(client, () => 'UTC');
  const onReconcile = vi.fn(async () => undefined), onClose = vi.fn();
  const view = render(MergePeopleDialog, { client, controller, personID: savedAnchor ? 7 : null, participantID: savedAnchor ? null : 1, name: 'Synthetic', onReconcile, onClose });
  return { view, controller, links, onClose, onReconcile };
}

it.each([false, true])('links a saved representative and an observed person without promotion: %s', async savedAnchor => {
  const { view, controller, links, onClose, onReconcile } = renderRepresentativeLink(savedAnchor);
  try {
    await fireEvent.input(await openTypeahead('Search people'), { target: { value: 'Synthetic' } });
    await fireEvent.mouseDown(await screen.findByRole('option', { name: savedAnchor ? /Synthetic Observed/ : /Synthetic Saved/ }));
    await fireEvent.click(screen.getByRole('button', { name: 'Merge' }));
    await waitFor(() => expect(onClose).toHaveBeenCalledOnce());
    expect(links).toEqual([{ participant_a: savedAnchor ? 2 : 1, participant_b: savedAnchor ? 1 : 2 }]);
    expect(onReconcile).toHaveBeenCalledOnce();
    expect(onReconcile).toHaveBeenCalledWith(expect.anything(), true);
  } finally { view.unmount(); controller.destroy(); }
});

it('leaves both people unchanged when the saved profile cannot be read', async () => {
  const { view, controller, links, onClose } = renderRepresentativeLink(false, true);
  try {
    await fireEvent.input(await openTypeahead('Search people'), { target: { value: 'Synthetic' } });
    await fireEvent.mouseDown(await screen.findByRole('option', { name: /Synthetic Saved/ }));
    await fireEvent.click(screen.getByRole('button', { name: 'Merge' }));
    await screen.findByText('Target missing');
    expect(links).toEqual([]);
    expect(onClose).not.toHaveBeenCalled();
  } finally { view.unmount(); controller.destroy(); }
});
