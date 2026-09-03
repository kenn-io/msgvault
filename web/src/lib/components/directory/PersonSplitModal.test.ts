import { appShortcuts } from '@kenn-io/kit-ui';
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../../api/client';
import type { components } from '../../api/generated/schema';
import { PersonMergeHistoryController } from '../../directory/person-merge-history-controller.svelte';
import PersonSplitModal from './PersonSplitModal.svelte';

type MergeDetail = components['schemas']['PersonMergeDetail'];

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

function mergeDetail(participants: MergeDetail['participants'] = [{ merge_id: 41, participant_id: 701, origin_side: 'absorbed' }]): MergeDetail {
  return {
    merge: {
      id: 41, survivor_person_id: 7, absorbed_person_id: 9, current_person_id: 12,
      survivor_vcard_uid: 'synthetic-7', absorbed_vcard_uid: 'synthetic-9',
      survivor_revision_before: 3, absorbed_revision_before: 2, survivor_revision_after: 4,
      actor: 'web', snapshot_version: 1, snapshot_sha256: 'synthetic-digest', created_at: '2026-08-03T00:00:00Z'
    },
    participants, rows: [], splits: [], review_candidates: []
  };
}

async function preparedController(fetchFn: typeof fetch, participants?: MergeDetail['participants']) {
  const value = mergeDetail(participants);
  const routedFetch = vi.fn<typeof fetch>(async (input, init) => {
    const request = input instanceof Request ? input : new Request(input, init);
    if (new URL(request.url).pathname === '/api/v1/person-merges/41') return Response.json(value);
    return fetchFn(request);
  });
  const controller = new PersonMergeHistoryController(createAPIClient(routedFetch), 7);
  await controller.selectMerge(41);
  await controller.openSplit();
  return { controller };
}

function sourceResponse() {
  return Response.json({
    id: 12, revision: 4, display_name: 'Synthetic Source', participant_ids: [701],
    created_at: '2026-08-01T00:00:00Z', updated_at: '2026-08-02T00:00:00Z', vcard_uid: 'synthetic-12'
  }, { headers: { ETag: '"person-12-r4"' } });
}

function result(exactReversal: boolean) {
  return {
    exact_reversal: exactReversal, cache_state: 'ready', identity_revision: 8,
    source_person: { id: 12, revision: 5, display_name: 'Synthetic Source', participant_ids: [], created_at: '2026-08-01T00:00:00Z', updated_at: '2026-08-04T00:00:00Z', vcard_uid: 'synthetic-12' },
    new_person: { id: 19, revision: 1, display_name: 'Synthetic Restored', participant_ids: [701], created_at: '2026-08-04T00:00:00Z', updated_at: '2026-08-04T00:00:00Z', vcard_uid: 'synthetic-19' },
    split: { id: 55, merge_id: 41, source_person_id: 12, new_person_id: 19, new_person_uid: 'synthetic-19', source_revision_before: 4, source_revision_after: 5, exact_reversal: exactReversal, actor: 'web', created_at: '2026-08-04T00:00:00Z' },
    ambiguous_rows: exactReversal ? [] : [{ table_name: 'person_names', original_row_key: 'concealed-key', action: 'ambiguous' }],
    unrestored_rows: [], uid_alias_disposition: 'restored'
  };
}

describe('PersonSplitModal', () => {
  it('names the actual source and selection in confirmation, then shows two fresh navigation actions with partial copy', async () => {
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      return request.method === 'GET' ? sourceResponse() : Response.json(result(false));
    });
    const { controller } = await preparedController(fetchFn);
    const onOpenPerson = vi.fn();
    render(PersonSplitModal, { controller, onClose: vi.fn(), onOpenPerson });

    await fireEvent.click(screen.getByRole('checkbox', { name: /Participant 701/ }));
    await fireEvent.click(screen.getByRole('checkbox', { name: /I confirm splitting Participant 701 from Synthetic Source \(Person 12\)/ }));
    await fireEvent.click(screen.getByRole('button', { name: 'Create restored person' }));

    expect(await screen.findByRole('heading', { name: 'Split completed' })).toBeDefined();
    expect(document.body.textContent?.toLowerCase()).not.toContain('undo');
    expect(screen.getByText(/partial/i).textContent?.toLowerCase()).toContain('partial');
    expect(document.body.textContent).not.toContain('concealed-key');
    await fireEvent.click(screen.getByRole('button', { name: 'Open source profile Synthetic Source (Person 12)' }));
    await fireEvent.click(screen.getByRole('button', { name: 'Open restored profile Synthetic Restored (Person 19)' }));
    expect(onOpenPerson.mock.calls).toEqual([[12], [19]]);
  });

  it('allows an explicitly confirmed empty absorbed lineage', async () => {
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      return request.method === 'GET' ? sourceResponse() : Response.json(result(true));
    });
    const { controller } = await preparedController(fetchFn, []);
    render(PersonSplitModal, { controller, onClose: vi.fn(), onOpenPerson: vi.fn() });

    expect(screen.getByText(/recorded no absorbed-lineage participants/)).toBeDefined();
    await fireEvent.click(screen.getByRole('checkbox', { name: /I confirm splitting the zero-participant lineage/ }));
    await fireEvent.click(screen.getByRole('button', { name: 'Create restored person' }));

    await screen.findByRole('heading', { name: 'Split completed' });
    const post = requests.find((request) => request.method === 'POST')!;
    await expect(post.clone().json()).resolves.toEqual({ merge_id: 41, participant_ids: [] });
  });

  it('blocks dismissal and root shortcuts while split work is pending', async () => {
    let resolveSplit!: (response: Response) => void;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      if (request.method === 'GET') return sourceResponse();
      return new Promise<Response>((resolve) => { resolveSplit = resolve; });
    });
    const { controller } = await preparedController(fetchFn);
    const onClose = vi.fn();
    const rootShortcut = vi.fn();
    const unregister = appShortcuts.register('x', rootShortcut);
    try {
      render(PersonSplitModal, { controller, onClose, onOpenPerson: vi.fn() });
      await fireEvent.click(screen.getByRole('checkbox', { name: /Participant 701/ }));
      await fireEvent.click(screen.getByRole('checkbox', { name: /I confirm splitting/ }));
      await fireEvent.click(screen.getByRole('button', { name: 'Create restored person' }));
      await waitFor(() => expect(controller.splitPending).toBe(true));

      expect(screen.getByRole('button', { name: 'Cancel' })).toHaveProperty('disabled', true);
      expect(screen.queryByRole('button', { name: 'Close split merged profile' })).toBeNull();
      await fireEvent.keyDown(window, { key: 'Escape' });
      await fireEvent.pointerDown(document.querySelector('.kit-modal-overlay')!);
      appShortcuts.handleKeydown(new KeyboardEvent('keydown', { key: 'x', cancelable: true }));
      expect(onClose).not.toHaveBeenCalled();
      expect(rootShortcut).not.toHaveBeenCalled();

      resolveSplit(Response.json({ error: 'person_split_invalid_participants', message: 'Eligibility changed' }, { status: 409 }));
      expect((await screen.findByRole('alert')).textContent).toContain('Eligibility changed');
    } finally {
      unregister();
    }
  });

  it('keeps failed stale recovery visibly blocked and focuses the GET-only retry through failure and success', async () => {
    const retrySource = deferredResponse();
    const retryDetail = deferredResponse();
    const requests: Request[] = [];
    let sourceReads = 0;
    let detailReads = 0;
    let postAttempts = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      const path = new URL(request.url).pathname;
      if (path === '/api/v1/person-merges/41') {
        detailReads += 1;
        if (detailReads === 2) return Response.json(mergeDetail());
        if (detailReads === 3) return retryDetail.promise;
        return Response.json(mergeDetail());
      }
      if (path === '/api/v1/people/12') {
        sourceReads += 1;
        if (sourceReads === 1) return sourceResponse();
        if (sourceReads === 2) return Response.json({ error: 'unavailable', message: 'Automatic source reload failed' }, { status: 503 });
        if (sourceReads === 3) return retrySource.promise;
        return Response.json({
          id: 12, revision: 6, display_name: 'Fresh Synthetic Source', participant_ids: [701],
          created_at: '2026-08-01T00:00:00Z', updated_at: '2026-08-05T00:00:00Z', vcard_uid: 'synthetic-12'
        }, { headers: { ETag: '"person-12-r6"' } });
      }
      postAttempts += 1;
      return Response.json({ error: 'person_merge_revision_conflict', message: 'Source changed' }, { status: 409 });
    });
    const controller = new PersonMergeHistoryController(createAPIClient(fetchFn), 7);
    await controller.selectMerge(41);
    await controller.openSplit();
    controller.setParticipantSelected(701, true);
    controller.confirmSplit();
    await controller.submitSplit();
    const onClose = vi.fn();
    render(PersonSplitModal, { controller, onClose, onOpenPerson: vi.fn() });

    expect(screen.getByText(/stale and cannot be submitted/)).toBeDefined();
    expect(screen.getByText(/Synthetic Source/)).toBeDefined();
    expect(screen.queryByRole('checkbox', { name: /I confirm splitting/ })).toBeNull();
    expect(screen.getByRole('button', { name: 'Create restored person' })).toHaveProperty('disabled', true);

    await fireEvent.click(screen.getByRole('button', { name: 'Retry source and merge detail' }));
    await waitFor(() => expect(controller.sourceLoading).toBe(true));
    expect(screen.getByRole('button', { name: 'Cancel' })).toHaveProperty('disabled', true);
    expect(screen.queryByRole('button', { name: 'Close split merged profile' })).toBeNull();

    retrySource.resolve(Response.json({ error: 'unavailable', message: 'Source still unavailable' }, { status: 503 }));
    retryDetail.resolve(Response.json(mergeDetail()));
    const retry = await screen.findByRole('button', { name: 'Retry source and merge detail' });
    await waitFor(() => expect(document.activeElement).toBe(retry));
    expect(requests.filter((request) => request.method === 'POST')).toHaveLength(1);

    await fireEvent.click(retry);
    const participant = await screen.findByRole('checkbox', { name: 'Participant 701' });
    await waitFor(() => expect(document.activeElement).toBe(participant));
    expect(screen.queryByText(/stale and cannot be submitted/)).toBeNull();
    expect(requests.filter((request) => request.method === 'POST')).toHaveLength(1);
    expect(onClose).not.toHaveBeenCalled();
  });
});

function deferredResponse() {
  let resolve!: (response: Response) => void;
  const promise = new Promise<Response>((next) => { resolve = next; });
  return { promise, resolve };
}
