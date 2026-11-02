import { directoryPerson, mergeResult, person as profile } from '../../directory/entity-fixtures.test-support';
import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { expect, it, vi } from 'vitest';
import { createAPIClient } from '../../api/client';
import { RelationshipsController } from '../../relationships/controller.svelte';
import { openTypeahead } from '../../../test/kit-ui';
import MergePeopleDialog from './MergePeopleDialog.svelte';

it.each([
  { savedAnchor: true, promote: false, outcome: 'merge' },
  { savedAnchor: false, promote: false, outcome: 'unreadable saved profile' },
  { savedAnchor: true, promote: true, outcome: 'retry and merge' },
  { savedAnchor: true, promote: true, outcome: 'cancel' },
  { savedAnchor: false, promote: true, outcome: 'read failure' },
  { savedAnchor: false, promote: true, outcome: 'cancel' }
])('merges profiles with saved anchor=$savedAnchor, promotion=$promote, outcome=$outcome', async ({ savedAnchor, promote, outcome }) => {
  const requests: Request[] = [];
  const links: unknown[] = [];
  let failProfile = outcome === 'retry and merge' || outcome === 'read failure';
  const client = createAPIClient(async input => {
    const request = input instanceof Request ? input : new Request(input);
    requests.push(request.clone());
    const path = new URL(request.url).pathname;
    if (path === '/api/v1/people/directory') return Response.json({ people: savedAnchor && promote ? [] : [directoryPerson(savedAnchor ? 9 : 7)] });
    if (path === '/api/v1/participants/search') return Response.json({ rows: savedAnchor && promote ? [{ id: 2, display_label: 'Synthetic Two', identifiers: [], activity_count: 1 }] : [] });
    if (path === '/api/v1/people' && request.method === 'POST') return Response.json(profile(9));
    if (path === '/api/v1/identity/links') { links.push(await request.json()); return Response.json({ identity_revision: 2, cache_state: 'ready' }); }
    if (path === '/api/v1/people/7' || path === '/api/v1/people/9') {
      if (outcome === 'unreadable saved profile') return Response.json({ message: 'Target missing' }, { status: 404 });
      if (path.endsWith('/9') && failProfile) { failProfile = false; return Response.json({ error: 'unavailable' }, { status: 503 }); }
      const id = Number(path.split('/').at(-1));
      return Response.json(profile(id), { headers: { ETag: id === 7 ? '"person-7-r4"' : '"person-9-r2"' } });
    }
    if (path === '/api/v1/people/7/merge') return Response.json(mergeResult(profile(7)), { headers: { ETag: '"person-7-r5"' } });
    throw new Error(`Unexpected ${request.method} ${path}`);
  });
  const controller = new RelationshipsController(client, () => 'UTC');
  const onReconcile = vi.fn(async () => undefined);
  const onOpenPerson = vi.fn(), onClose = vi.fn();
  const view = render(MergePeopleDialog, { client, controller, personID: savedAnchor ? 7 : null, participantID: savedAnchor ? null : 1, name: 'Synthetic One', onReconcile, onOpenPerson, onClose });
  try {
    await fireEvent.input(await openTypeahead('Search people'), { target: { value: 'Synthetic' } });
    await fireEvent.mouseDown(await screen.findByRole('option', { name: savedAnchor ? /Synthetic Two/ : /Synthetic One/ }));
    await fireEvent.click(screen.getByRole('button', { name: 'Merge' }));
    if (outcome === 'unreadable saved profile') {
      await screen.findByText('Target missing');
      expect(links).toEqual([]);
      expect(onClose).not.toHaveBeenCalled();
      return;
    }
    if (outcome === 'retry and merge' || outcome === 'read failure') {
      await screen.findByText('Could not read both people. Retry the merge.');
      expect(onReconcile).toHaveBeenCalledWith(expect.anything(), false);
      if (outcome === 'read failure') return;
      await fireEvent.click(screen.getByRole('button', { name: 'Merge' }));
    }
    await screen.findByRole('dialog', { name: 'Resolve person merge' });
    if (promote) expect(onReconcile).toHaveBeenCalledWith(expect.anything(), false);
    if (outcome === 'cancel') {
      await fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));
      expect(onClose).toHaveBeenCalledOnce();
      return;
    }
    await fireEvent.click(screen.getByRole('radio', { name: 'Synthetic One (Person 7)' }));
    await fireEvent.click(screen.getByRole('checkbox', { name: /I understand/ }));
    await fireEvent.click(screen.getByRole('button', { name: 'Merge into selected survivor' }));
    await waitFor(() => expect(onOpenPerson).toHaveBeenCalledWith(7));
    const promotions = requests.filter(request => new URL(request.url).pathname === '/api/v1/people' && request.method === 'POST');
    expect(promotions).toHaveLength(promote ? 1 : 0);
    if (promote) expect(await promotions[0]!.json()).toEqual({ participant_id: 2 });
  } finally { view.unmount(); controller.destroy(); }
});

function renderRepresentativeLink(savedAnchor: boolean) {
  const links: unknown[] = [];
  let participantID = savedAnchor ? null : 1;
  let finishProfile!: () => void;
  const profileRequested = vi.fn();
  const client = createAPIClient(async input => {
    const request = input instanceof Request ? input : new Request(input);
    const path = new URL(request.url).pathname;
    if (path.endsWith('/directory')) return Response.json({ people: savedAnchor ? [] : [directoryPerson(7, { display_name: 'Synthetic Saved' })] });
    if (path.endsWith('/participants/search')) return Response.json({ rows: savedAnchor ? [{ id: 1, display_label: 'Synthetic Observed' }] : [] });
    if (path.endsWith('/people/7')) {
      if (savedAnchor) return Response.json(profile(7, 4, [2, 3]));
      profileRequested();
      return new Promise<Response>(resolve => { finishProfile = () => resolve(Response.json(profile(7, 4, [2, 3]))); });
    }
    if (path.endsWith('/identity/links')) { links.push(await request.json()); return Response.json({ identity_revision: 2, cache_state: 'ready' }); }
    throw new Error(`Unexpected ${path}`);
  });
  const controller = new RelationshipsController(client, () => 'UTC');
  const onReconcile = vi.fn(async () => undefined), onClose = vi.fn();
  // A live caller input stays unread in the saved-only search until the deferred link resumes.
  const view = render(MergePeopleDialog, { client, controller, personID: savedAnchor ? 7 : null, get participantID() { return participantID; }, name: 'Synthetic', onReconcile, onClose });
  return { view, controller, links, onClose, onReconcile, profileRequested, finishProfile: () => finishProfile(), changeOrigin: () => participantID = 99 };
}

it.each([false, true])('links a saved representative and an observed person without promotion: %s', async savedAnchor => {
  const { view, controller, links, onClose, onReconcile, profileRequested, finishProfile, changeOrigin } = renderRepresentativeLink(savedAnchor);
  try {
    await fireEvent.input(await openTypeahead('Search people'), { target: { value: 'Synthetic' } });
    await fireEvent.mouseDown(await screen.findByRole('option', { name: savedAnchor ? /Synthetic Observed/ : /Synthetic Saved/ }));
    await fireEvent.click(screen.getByRole('button', { name: 'Merge' }));
    if (!savedAnchor) {
      await waitFor(() => expect(profileRequested).toHaveBeenCalledOnce());
      view.unmount();
      changeOrigin();
      finishProfile();
    }
    await waitFor(() => expect(onReconcile).toHaveBeenCalledOnce());
    // The unmounted dialog has nothing left to close.
    expect(onClose).toHaveBeenCalledTimes(savedAnchor ? 1 : 0);
    expect(links).toEqual([{ participant_a: savedAnchor ? 2 : 1, participant_b: savedAnchor ? 1 : 2 }]);
    expect(onReconcile).toHaveBeenCalledWith(expect.anything(), true);
  } finally { view.unmount(); controller.destroy(); }
});
