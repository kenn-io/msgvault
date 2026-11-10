import { directoryPerson, mergeResult, person as profile } from '../../directory/entity-fixtures.test-support';
import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { expect, it, vi } from 'vitest';
import { createAPIClient } from '../../api/client';
import { RelationshipsController } from '../../relationships/controller.svelte';
import { openTypeahead } from '../../../test/kit-ui';
import MergePeopleDialog from './MergePeopleDialog.svelte';
import PersonBindingConflictModal from '../directory/PersonBindingConflictModal.svelte';
import { dismissFlash, FlashBanner } from '@kenn-io/kit-ui';

vi.mock('../directory/PersonBindingConflictModal.svelte', { spy: true });

it.each([
  { savedAnchor: true, promote: false, outcome: 'stale merge' },
  { savedAnchor: false, outcome: 'selected saved profile read fails' },
  { savedAnchor: true, promote: true, outcome: 'retry and merge' },
  { savedAnchor: true, promote: true, outcome: 'cancel' },
  { savedAnchor: false, promote: true, outcome: 'read failure' }
])('handles merge and promotion outcomes with saved anchor=$savedAnchor, outcome=$outcome', async ({ savedAnchor, promote, outcome }) => {
  const requests: Request[] = [];
  let failProfile = outcome === 'retry and merge' || outcome === 'read failure';
  const client = createAPIClient(async input => {
    const request = input instanceof Request ? input : new Request(input);
    requests.push(request.clone());
    const path = new URL(request.url).pathname;
    if (path === '/api/v1/people/directory') return Response.json({ people: savedAnchor && promote ? [] : [directoryPerson(savedAnchor ? 9 : 7)] });
    if (path === '/api/v1/participants/search') return Response.json({ rows: savedAnchor && promote ? [{ id: 2, display_label: 'Synthetic Two', identifiers: [], activity_count: 1 }] : [] });
    if (path === '/api/v1/people' && request.method === 'POST') return Response.json(profile(9));
    if (path === '/api/v1/people/7' || path === '/api/v1/people/9') {
      if (outcome === 'selected saved profile read fails') return Response.json({ message: 'Target missing' }, { status: 404 });
      if (path.endsWith('/9') && failProfile) { failProfile = false; return Response.json({ error: 'unavailable' }, { status: 503 }); }
      const id = Number(path.split('/').at(-1));
      return Response.json(profile(id), { headers: { ETag: id === 7 ? '"person-7-r4"' : '"person-9-r2"' } });
    }
    throw new Error(`Unexpected ${request.method} ${path}`);
  });
  const controller = new RelationshipsController(client, () => 'UTC');
  const context = controller.personMergeContextSnapshot();
  const onReconcile = vi.fn(async () => undefined);
  const onOpenPerson = vi.fn(), onClose = vi.fn(() => { if (outcome === 'stale merge') view.unmount(); });
  const view = render(MergePeopleDialog, { client, controller, personID: savedAnchor ? 7 : null, participantID: savedAnchor ? null : 1, name: 'Synthetic One', onReconcile, onOpenPerson, onClose });
  const banner = outcome === 'stale merge' ? render(FlashBanner) : null;
  try {
    await fireEvent.input(await openTypeahead('Search people'), { target: { value: 'Synthetic' } });
    await fireEvent.mouseDown(await screen.findByRole('option', { name: savedAnchor ? /Synthetic Two/ : /Synthetic One/ }));
    await fireEvent.click(screen.getByRole('button', { name: 'Merge' }));
    if (outcome === 'selected saved profile read fails') {
      await screen.findByText('Target missing');
      expect(onClose).not.toHaveBeenCalled();
      return;
    }
    if (outcome === 'retry and merge' || outcome === 'read failure') {
      await screen.findByText("Couldn't load both people.");
      expect(onReconcile).toHaveBeenCalledWith(expect.anything(), false);
      if (outcome === 'read failure') return;
      await fireEvent.click(screen.getByRole('button', { name: 'Merge' }));
    }
    await screen.findByRole('dialog', { name: 'Resolve person merge' });
    const modal = vi.mocked(PersonBindingConflictModal).mock.calls.at(-1)![1];
    expect(modal.conflict.profiles.map(item => item.person.id).sort()).toEqual([7, 9]);
    if (promote) expect(onReconcile).toHaveBeenCalledWith(expect.anything(), false);
    if (outcome === 'cancel') {
      modal.onClose();
      expect(onClose).toHaveBeenCalledOnce();
      return;
    }
    controller.query = 'Another person';
    if (outcome === 'stale merge') {
      await modal.onSuccess({ result: mergeResult(profile(7), 'stale'), survivor: profile(7), responseETag: '"person-7-r5"' });
      await waitFor(() => expect(onClose).toHaveBeenCalledOnce());
      expect(await screen.findByText("People merged. Couldn't refresh the people list.")).toBeDefined();
      expect(onOpenPerson).toHaveBeenCalledWith(7);
      expect(onReconcile).toHaveBeenLastCalledWith(context, false, expect.objectContaining({ survivor: expect.objectContaining({ id: 7 }) }));
      expect(requests.filter(request => new URL(request.url).pathname === '/api/v1/people' && request.method === 'POST')).toHaveLength(0);
      return;
    }
    await modal.onSuccess({ result: mergeResult(profile(7)), survivor: profile(7), responseETag: '"person-7-r5"' });
    await waitFor(() => expect(onOpenPerson).toHaveBeenCalledWith(7));
    expect(onReconcile).toHaveBeenLastCalledWith(context, false, expect.objectContaining({ survivor: expect.objectContaining({ id: 7 }) }));
    const promotions = requests.filter(request => new URL(request.url).pathname === '/api/v1/people' && request.method === 'POST');
    expect(promotions).toHaveLength(promote ? outcome === 'retry and merge' ? 2 : 1 : 0);
    if (promote) expect(await promotions[0]!.json()).toEqual({ participant_id: 2 });
  } finally { view.unmount(); banner?.unmount(); dismissFlash(); controller.destroy(); }
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
  const onReconcile = vi.fn(async () => undefined), onClose = vi.fn(), onLinked = vi.fn();
  // A live caller input stays unread in the saved-only search until the deferred link resumes.
  const view = render(MergePeopleDialog, { client, controller, personID: savedAnchor ? 7 : null, get participantID() { return participantID; }, name: 'Synthetic', onReconcile, onClose, onLinked });
  return { view, controller, links, onClose, onLinked, onReconcile, profileRequested, finishProfile: () => finishProfile(), changeOrigin: () => participantID = 99 };
}

async function checkRepresentativeLink(savedAnchor: boolean) {
  const { view, controller, links, onClose, onLinked, onReconcile, profileRequested, finishProfile, changeOrigin } = renderRepresentativeLink(savedAnchor);
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
    if (savedAnchor) { expect(onClose).toHaveBeenCalledOnce(); expect(onLinked).toHaveBeenCalledOnce(); }
    else { expect(onClose).not.toHaveBeenCalled(); expect(onLinked).not.toHaveBeenCalled(); }
    expect(links).toEqual([{ participant_a: savedAnchor ? 2 : 1, participant_b: savedAnchor ? 1 : 2 }]);
    expect(onReconcile).toHaveBeenCalledWith(expect.anything(), true);
  } finally { view.unmount(); controller.destroy(); }
}

it.each([true, false])('links the captured representative and suppresses callbacks after disposal, saved anchor=%s', checkRepresentativeLink);
