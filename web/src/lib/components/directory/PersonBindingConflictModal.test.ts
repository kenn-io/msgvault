import { mergeResult, person, requestOf } from '../../directory/entity-fixtures.test-support';
import { appShortcuts } from '@kenn-io/kit-ui';
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { afterEach, describe, expect, it, vi } from 'vitest';

import * as requestID from '../../api/request-id';
import { createAPIClient } from '../../api/client';
import type { PersonMergeSuccess, ValidatedPersonMergeRequired } from '../../directory/person-merge';
import PersonBindingConflictModal from './PersonBindingConflictModal.svelte';

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.restoreAllMocks();
});

function conflict(): ValidatedPersonMergeRequired {
  return {
    error: 'person_merge_required',
    message: 'Choose a survivor',
    profiles: [
      { person: person(7, 4, [70], 'Synthetic One'), etag: '"person-7-r4"' },
      { person: person(9, 2, [90], 'Synthetic Two'), etag: '"person-9-r2"' },
    ],
  };
}


function renderModal(
  fetchFn: typeof fetch,
  overrides: Partial<{
    onOpenProfile: (personID: number) => void;
    onSuccess: (success: PersonMergeSuccess) => void | Promise<void>;
    onClose: () => void;
  }> = {},
) {
  const onOpenProfile = overrides.onOpenProfile ?? vi.fn();
  const onSuccess = overrides.onSuccess ?? vi.fn();
  const onClose = overrides.onClose ?? vi.fn();
  const rendered = render(PersonBindingConflictModal, {
    client: createAPIClient(fetchFn),
    conflict: conflict(),
    onOpenProfile,
    onSuccess,
    onClose,
  });
  return { ...rendered, onOpenProfile, onSuccess, onClose };
}

async function selectSurvivor(label = 'Synthetic One (Person 7)'): Promise<void> {
  await fireEvent.click(screen.getByRole('radio', { name: label }));
  await fireEvent.click(screen.getByRole('checkbox', { name: /I understand this consolidates both profiles/i }));
}

describe('PersonBindingConflictModal', () => {
  it('sends the exact survivor-first merge request and reports operation-result metadata once', async () => {
    const requests: Request[] = [];
    const merged = person(7, 5, [70], 'Synthetic One');
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      requests.push(request);
      return Response.json(mergeResult(merged, 'stale'), { headers: { ETag: '"person-7-r5"' } });
    });
    vi.spyOn(requestID, 'createBrowserRequestID').mockReturnValue('123-456-789-12');
    const { onSuccess, onClose } = renderModal(fetchFn);

    await selectSurvivor();
    await fireEvent.click(screen.getByRole('button', { name: 'Merge into selected survivor' }));

    await waitFor(() => expect(onSuccess).toHaveBeenCalledOnce());
    expect(requests).toHaveLength(1);
    expect(new URL(requests[0]!.url).pathname).toBe('/api/v1/people/7/merge');
    expect(requests[0]!.headers.get('If-Match')).toBe('"person-7-r4", "person-9-r2"');
    expect(requests[0]!.headers.get('Idempotency-Key')).toBe('123-456-789-12');
    await expect(requests[0]!.clone().json()).resolves.toEqual({ absorbed_person_id: 9 });
    expect(onSuccess).toHaveBeenCalledWith({
      result: mergeResult(merged, 'stale'),
      survivor: merged,
      responseETag: '"person-7-r5"',
    });
    expect(onClose).not.toHaveBeenCalled();
    expect(screen.queryByRole('button', { name: 'Merge into selected survivor' })).toBeNull();
  });

  it('reuses one request ID only for an explicit unchanged transport retry', async () => {
    const requests: Request[] = [];
    const merged = person(7, 5, [70], 'Synthetic One');
    let attempts = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      requests.push(requestOf(input));
      attempts += 1;
      if (attempts === 1) throw new TypeError('connection reset');
      return Response.json(mergeResult(merged), { headers: { ETag: '"person-7-r5"' } });
    });
    const requestIDSpy = vi.spyOn(requestID, 'createBrowserRequestID').mockReturnValueOnce('123-456-789-12');
    const { onSuccess } = renderModal(fetchFn);

    await selectSurvivor();
    await fireEvent.click(screen.getByRole('button', { name: 'Merge into selected survivor' }));
    expect((await screen.findByRole('alert')).textContent).toContain('connection reset');
    await fireEvent.click(screen.getByRole('button', { name: 'Merge into selected survivor' }));

    await waitFor(() => expect(onSuccess).toHaveBeenCalledOnce());
    expect(requests.map((request) => request.headers.get('Idempotency-Key'))).toEqual([
      '123-456-789-12',
      '123-456-789-12',
    ]);
    expect(requestIDSpy).toHaveBeenCalledOnce();
  });

  it('changing the survivor clears confirmation and rotates the full-snapshot key', async () => {
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      requests.push(request);
      throw new TypeError('offline');
    });
    vi.spyOn(requestID, 'createBrowserRequestID')
      .mockReturnValueOnce('123-456-789-12')
      .mockReturnValueOnce('234-567-890-23');
    renderModal(fetchFn);

    await selectSurvivor();
    await fireEvent.click(screen.getByRole('button', { name: 'Merge into selected survivor' }));
    await screen.findByRole('alert');
    await fireEvent.click(screen.getByRole('radio', { name: 'Synthetic Two (Person 9)' }));
    expect(screen.getByRole('checkbox', { name: /I understand this consolidates both profiles/i })).toHaveProperty(
      'checked',
      false,
    );
    expect(screen.getByRole('button', { name: 'Merge into selected survivor' })).toHaveProperty('disabled', true);
    await fireEvent.click(screen.getByRole('checkbox', { name: /I understand this consolidates both profiles/i }));
    await fireEvent.click(screen.getByRole('button', { name: 'Merge into selected survivor' }));

    await waitFor(() => expect(requests).toHaveLength(2));
    expect(new URL(requests[1]!.url).pathname).toBe('/api/v1/people/9/merge');
    expect(requests[1]!.headers.get('If-Match')).toBe('"person-9-r2", "person-7-r4"');
    expect(requests.map((request) => request.headers.get('Idempotency-Key'))).toEqual([
      '123-456-789-12',
      '234-567-890-23',
    ]);
  });

  it('reloads both exact profiles atomically and requires a fresh survivor selection and confirmation', async () => {
    const requests: Request[] = [];
    const refreshedSeven = person(7, 5, [70], 'Synthetic One Updated');
    const refreshedNine = person(9, 3, [90], 'Synthetic Two Updated');
    let mergeAttempts = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      requests.push(request);
      const path = new URL(request.url).pathname;
      if (request.method === 'GET' && path === '/api/v1/people/7') {
        return Response.json(refreshedSeven, { headers: { ETag: '"person-7-r5"' } });
      }
      if (request.method === 'GET' && path === '/api/v1/people/9') {
        return Response.json(refreshedNine, { headers: { ETag: '"person-9-r3"' } });
      }
      mergeAttempts += 1;
      if (mergeAttempts === 1) {
        return Response.json({ error: 'person_merge_revision_conflict', message: 'Reload profiles' }, { status: 409 });
      }
      return Response.json(mergeResult(person(7, 6, [70], 'Synthetic One Updated')), { headers: { ETag: '"person-7-r6"' } });
    });
    vi.spyOn(requestID, 'createBrowserRequestID')
      .mockReturnValueOnce('123-456-789-12')
      .mockReturnValueOnce('234-567-890-23');
    const { onSuccess } = renderModal(fetchFn);

    await selectSurvivor();
    await fireEvent.click(screen.getByRole('button', { name: 'Merge into selected survivor' }));

    expect(await screen.findByText('Synthetic One Updated (Person 7)')).toBeDefined();
    expect(screen.getByText('Synthetic Two Updated (Person 9)')).toBeDefined();
    expect(
      requests
        .filter((request) => request.method === 'GET')
        .map((request) => new URL(request.url).pathname)
        .sort(),
    ).toEqual(['/api/v1/people/7', '/api/v1/people/9']);
    expect(requests.filter((request) => request.method === 'POST')).toHaveLength(1);
    const refreshedSurvivor = screen.getByRole('radio', { name: 'Synthetic One Updated (Person 7)' });
    const refreshedAbsorbed = screen.getByRole('radio', { name: 'Synthetic Two Updated (Person 9)' });
    const confirmation = screen.getByRole('checkbox', { name: /I understand this consolidates both profiles/i });
    const submit = screen.getByRole('button', { name: 'Merge into selected survivor' });
    expect(refreshedSurvivor.getAttribute('aria-checked')).toBe('false');
    expect(refreshedAbsorbed.getAttribute('aria-checked')).toBe('false');
    expect(confirmation).toHaveProperty('checked', false);
    expect(confirmation).toHaveProperty('disabled', true);
    expect(submit).toHaveProperty('disabled', true);

    await fireEvent.click(confirmation);
    await fireEvent.click(submit);
    expect(requests.filter((request) => request.method === 'POST')).toHaveLength(1);

    await fireEvent.click(refreshedSurvivor);
    await fireEvent.click(confirmation);
    await fireEvent.click(submit);
    await waitFor(() => expect(onSuccess).toHaveBeenCalledOnce());
    const posts = requests.filter((request) => request.method === 'POST');
    expect(posts).toHaveLength(2);
    expect(posts[1]!.headers.get('If-Match')).toBe('"person-7-r5", "person-9-r3"');
    expect(posts[1]!.headers.get('Idempotency-Key')).toBe('234-567-890-23');
  });

  it.each([
    { status: 409, error: 'person_merge_idempotency_conflict' },
    { status: 409, error: 'person_carddav_published' },
    { status: 412, error: 'precondition_failed' },
  ])('does not reload or reuse a key for application failure $status/$error', async ({ status, error }) => {
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      requests.push(requestOf(input));
      return Response.json({ error, message: 'Application failure' }, { status });
    });
    const requestIDSpy = vi
      .spyOn(requestID, 'createBrowserRequestID')
      .mockReturnValueOnce('123-456-789-12')
      .mockReturnValueOnce('234-567-890-23');
    renderModal(fetchFn);

    await selectSurvivor();
    await fireEvent.click(screen.getByRole('button', { name: 'Merge into selected survivor' }));
    expect((await screen.findByRole('alert')).textContent).toContain('Application failure');
    expect(requests.filter((request) => request.method === 'GET')).toHaveLength(0);
    expect(screen.getByRole('checkbox', { name: /I understand this consolidates both profiles/i })).toHaveProperty(
      'checked',
      false,
    );
    await fireEvent.click(screen.getByRole('checkbox', { name: /I understand this consolidates both profiles/i }));
    await fireEvent.click(screen.getByRole('button', { name: 'Merge into selected survivor' }));

    await waitFor(() => expect(requests).toHaveLength(2));
    expect(requests.map((request) => request.headers.get('Idempotency-Key'))).toEqual([
      '123-456-789-12',
      '234-567-890-23',
    ]);
    expect(requestIDSpy).toHaveBeenCalledTimes(2);
  });

  it.each([
    {
      name: 'a wrong self-consistent person ID',
      responses: {
        7: { body: person(8, 5, [80], 'Wrong Person'), etag: '"person-8-r5"' },
        9: { body: person(9, 3, [90], 'Synthetic Two Updated'), etag: '"person-9-r3"' },
      },
    },
    {
      name: 'swapped self-consistent person IDs',
      responses: {
        7: { body: person(9, 3, [90], 'Synthetic Two Updated'), etag: '"person-9-r3"' },
        9: { body: person(7, 5, [70], 'Synthetic One Updated'), etag: '"person-7-r5"' },
      },
    },
  ])('rejects the entire stale reload when it returns $name', async ({ responses }) => {
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      requests.push(request);
      const path = new URL(request.url).pathname;
      if (request.method === 'POST') {
        return Response.json({ error: 'person_merge_revision_conflict', message: 'Reload profiles' }, { status: 409 });
      }
      const requestedID = Number(path.split('/').at(-1));
      const response = responses[requestedID as keyof typeof responses];
      return Response.json(response.body, { headers: { ETag: response.etag } });
    });
    renderModal(fetchFn);

    await selectSurvivor();
    await fireEvent.click(screen.getByRole('button', { name: 'Merge into selected survivor' }));

    expect((await screen.findByRole('alert')).textContent).toContain("couldn't be reloaded");
    expect(screen.getByRole('radio', { name: 'Synthetic One (Person 7)' })).toBeDefined();
    expect(screen.getByRole('radio', { name: 'Synthetic Two (Person 9)' })).toBeDefined();
    expect(screen.getByRole('checkbox', { name: /I understand this consolidates both profiles/i })).toHaveProperty(
      'checked',
      false,
    );
    expect(screen.getByRole('button', { name: 'Merge into selected survivor' })).toHaveProperty('disabled', true);
    expect(requests.filter((request) => request.method === 'POST')).toHaveLength(1);
  });

  it('retains both original snapshots when either stale reload is unusable', async () => {
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      requests.push(request);
      const path = new URL(request.url).pathname;
      if (request.method === 'GET' && path === '/api/v1/people/7') {
        return Response.json(person(7, 5, [70], 'Synthetic One Updated'), { headers: { ETag: '"person-7-r5"' } });
      }
      if (request.method === 'GET') return Response.json(person(9, 3, [90], 'Synthetic Two Updated'));
      return Response.json({ error: 'person_merge_revision_conflict', message: 'Reload profiles' }, { status: 409 });
    });
    renderModal(fetchFn);

    await selectSurvivor();
    await fireEvent.click(screen.getByRole('button', { name: 'Merge into selected survivor' }));

    expect((await screen.findByRole('alert')).textContent).toContain("couldn't be reloaded");
    expect(screen.getByText('Synthetic One (Person 7)')).toBeDefined();
    expect(screen.getByText('Synthetic Two (Person 9)')).toBeDefined();
    expect(screen.queryByText('Synthetic One Updated (Person 7)')).toBeNull();
  });

  it('blocks reconfirmation and another merge POST after the stale profile reload fails', async () => {
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      requests.push(request);
      const path = new URL(request.url).pathname;
      if (request.method === 'POST') {
        return Response.json({ error: 'person_merge_revision_conflict', message: 'Reload profiles' }, { status: 409 });
      }
      if (path === '/api/v1/people/7') {
        return Response.json(person(7, 5, [70], 'Synthetic One Updated'), { headers: { ETag: '"person-7-r5"' } });
      }
      return Response.json(person(9, 3, [90], 'Synthetic Two Updated'));
    });
    renderModal(fetchFn);

    await selectSurvivor();
    await fireEvent.click(screen.getByRole('button', { name: 'Merge into selected survivor' }));

    expect((await screen.findByRole('alert')).textContent).toContain("couldn't be reloaded");
    const survivor = screen.getByRole('radio', { name: 'Synthetic One (Person 7)' });
    const confirmation = screen.getByRole('checkbox', { name: /I understand this consolidates both profiles/i });
    const submit = screen.getByRole('button', { name: 'Merge into selected survivor' });
    expect(survivor).toHaveProperty('disabled', true);
    expect(confirmation).toHaveProperty('disabled', true);
    expect(submit).toHaveProperty('disabled', true);
    await waitFor(() =>
      expect(screen.getByRole('button', { name: 'Retry profile reload' })).toHaveProperty('disabled', false),
    );

    await fireEvent.click(confirmation);
    await fireEvent.click(submit);
    expect(requests.filter((request) => request.method === 'POST')).toHaveLength(1);
  });

  it('keeps merge readiness invalid when an explicit profile reload retry also fails', async () => {
    const requests: Request[] = [];
    const retrySettlers: Array<(response: Response) => void> = [];
    let manualRetry = false;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      requests.push(request);
      if (request.method === 'POST') {
        return Response.json({ error: 'person_merge_revision_conflict', message: 'Reload profiles' }, { status: 409 });
      }
      if (!manualRetry || retrySettlers.length >= 2) return Response.json({ error: 'unavailable' }, { status: 503 });
      return new Promise<Response>((resolve) => {
        retrySettlers.push(resolve);
      });
    });
    const onClose = vi.fn();
    renderModal(fetchFn, { onClose });

    await selectSurvivor();
    vi.useFakeTimers();
    await fireEvent.click(screen.getByRole('button', { name: 'Merge into selected survivor' }));
    await vi.advanceTimersByTimeAsync(3500);
    expect(screen.getByRole('button', { name: 'Retry profile reload' })).toHaveProperty('disabled', false);

    manualRetry = true;
    await fireEvent.click(screen.getByRole('button', { name: 'Retry profile reload' }));
    await vi.advanceTimersByTimeAsync(0);
    expect(retrySettlers).toHaveLength(2);
    expect(screen.queryByRole('button', { name: 'Retry profile reload' })).toBeNull();

    for (const settle of retrySettlers) {
      settle(Response.json({ error: 'unavailable', message: 'Reload unavailable' }, { status: 503 }));
    }
    await vi.advanceTimersByTimeAsync(3500);
    expect(screen.getByRole('button', { name: 'Retry profile reload' })).toHaveProperty('disabled', false);
    expect(screen.getByRole('alert').textContent).toContain("couldn't be reloaded");
    expect(screen.getByRole('radio', { name: 'Synthetic One (Person 7)' })).toHaveProperty('disabled', true);
    expect(screen.getByRole('checkbox', { name: /I understand this consolidates both profiles/i })).toHaveProperty(
      'disabled',
      true,
    );
    expect(requests.filter((request) => request.method === 'POST')).toHaveLength(1);
  });

  it('uses only fresh tags after an explicit reload, fresh survivor selection, and reconfirmation', async () => {
    const requests: Request[] = [];
    const refreshedSeven = person(7, 5, [70], 'Synthetic One Updated');
    const refreshedNine = person(9, 3, [90], 'Synthetic Two Updated');
    const getCounts = new Map<number, number>();
    let mergeAttempts = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      requests.push(request);
      const path = new URL(request.url).pathname;
      if (request.method === 'POST') {
        mergeAttempts += 1;
        if (mergeAttempts === 1) {
          return Response.json(
            { error: 'person_merge_revision_conflict', message: 'Reload profiles' },
            { status: 409 },
          );
        }
        return Response.json(mergeResult(person(7, 6, [70], 'Synthetic One Updated')), {
          headers: { ETag: '"person-7-r6"' },
        });
      }
      const requestedID = Number(path.split('/').at(-1));
      const count = (getCounts.get(requestedID) ?? 0) + 1;
      getCounts.set(requestedID, count);
      if (count <= 4) return Response.json({ error: 'unavailable', message: 'Reload unavailable' }, { status: 503 });
      const refreshed = requestedID === 7 ? refreshedSeven : refreshedNine;
      return Response.json(refreshed, { headers: { ETag: `"person-${requestedID}-r${refreshed.revision}"` } });
    });
    vi.spyOn(requestID, 'createBrowserRequestID')
      .mockReturnValueOnce('123-456-789-12')
      .mockReturnValueOnce('234-567-890-23');
    const { onSuccess } = renderModal(fetchFn);

    await selectSurvivor();
    vi.useFakeTimers();
    await fireEvent.click(screen.getByRole('button', { name: 'Merge into selected survivor' }));
    await vi.advanceTimersByTimeAsync(3500);
    expect(screen.getByRole('button', { name: 'Retry profile reload' })).toHaveProperty('disabled', false);
    vi.useRealTimers();
    await fireEvent.click(screen.getByRole('button', { name: 'Retry profile reload' }));

    expect(await screen.findByText('Synthetic One Updated (Person 7)')).toBeDefined();
    expect(screen.getByText('Synthetic Two Updated (Person 9)')).toBeDefined();
    expect(requests.filter((request) => request.method === 'POST')).toHaveLength(1);
    expect(screen.queryByRole('button', { name: 'Retry profile reload' })).toBeNull();
    const refreshedSurvivor = screen.getByRole('radio', { name: 'Synthetic One Updated (Person 7)' });
    const refreshedAbsorbed = screen.getByRole('radio', { name: 'Synthetic Two Updated (Person 9)' });
    await waitFor(() => expect(refreshedSurvivor).toHaveProperty('disabled', false));
    expect(refreshedSurvivor.getAttribute('aria-checked')).toBe('false');
    expect(refreshedAbsorbed.getAttribute('aria-checked')).toBe('false');
    const confirmation = screen.getByRole('checkbox', { name: /I understand this consolidates both profiles/i });
    expect(confirmation).toHaveProperty('checked', false);
    expect(confirmation).toHaveProperty('disabled', true);

    await fireEvent.click(confirmation);
    await fireEvent.click(screen.getByRole('button', { name: 'Merge into selected survivor' }));
    expect(requests.filter((request) => request.method === 'POST')).toHaveLength(1);

    await fireEvent.click(refreshedSurvivor);
    await fireEvent.click(confirmation);
    await fireEvent.click(screen.getByRole('button', { name: 'Merge into selected survivor' }));

    await waitFor(() => expect(onSuccess).toHaveBeenCalledOnce());
    const posts = requests.filter((request) => request.method === 'POST');
    expect(posts).toHaveLength(2);
    expect(posts[1]!.headers.get('If-Match')).toBe('"person-7-r5", "person-9-r3"');
    expect(posts[1]!.headers.get('Idempotency-Key')).toBe('234-567-890-23');
  });

  it.each([503, 408])('recovers a %s profile failure without another merge', async (status) => {
    const requests: Request[] = [];
    let sevenGets = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      requests.push(request);
      if (request.method === 'POST') {
        return Response.json({ error: 'person_merge_revision_conflict', message: 'Reload profiles' }, { status: 409 });
      }
      const id = Number(new URL(request.url).pathname.split('/').at(-1));
      if (id === 7 && ++sevenGets === 1) return Response.json({ error: 'unavailable' }, { status });
      return Response.json(person(id, 5, [id * 10], `Updated ${id}`), { headers: { ETag: `"person-${id}-r5"` } });
    });
    renderModal(fetchFn);
    await selectSurvivor();
    vi.useFakeTimers();
    await fireEvent.click(screen.getByRole('button', { name: 'Merge into selected survivor' }));
    await vi.advanceTimersByTimeAsync(500);
    expect(screen.getByText('Updated 7 (Person 7)')).toBeDefined();
    expect(screen.getByRole('checkbox')).toHaveProperty('checked', false);
    expect(screen.getByRole('button', { name: 'Merge into selected survivor' })).toHaveProperty('disabled', true);
    expect(requests.filter((request) => request.method === 'GET')).toHaveLength(3);
    expect(requests.filter((request) => request.method === 'POST')).toHaveLength(1);
  });

  it.each([['1', 1000], ['5', 5000], ['Wed, 21 Oct 2015 07:28:05 GMT', 5000]])('waits for Retry-After %s', async (header, delay) => {
    let sevenGets = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      if (request.method === 'POST') {
        return Response.json({ error: 'person_merge_revision_conflict', message: 'Reload profiles' }, { status: 409 });
      }
      const id = Number(new URL(request.url).pathname.split('/').at(-1));
      if (id === 7 && ++sevenGets === 1) {
        return Response.json({ error: 'rate_limited' }, { status: 429, headers: { 'Retry-After': header } });
      }
      return Response.json(person(id, 5, [id * 10], `Updated ${id}`), { headers: { ETag: `"person-${id}-r5"` } });
    });
    renderModal(fetchFn);
    await selectSurvivor();
    vi.useFakeTimers();
    vi.setSystemTime(new Date('2015-10-21T07:28:00Z'));
    await fireEvent.click(screen.getByRole('button', { name: 'Merge into selected survivor' }));
    await vi.advanceTimersByTimeAsync(delay - 1);
    expect(sevenGets).toBe(1);
    expect(screen.getByRole('button', { name: 'Merge into selected survivor' })).toHaveProperty('disabled', true);
    await vi.advanceTimersByTimeAsync(1);
    expect(sevenGets).toBe(2);
    expect(screen.getByText('Updated 7 (Person 7)')).toBeDefined();
    expect(screen.getByRole('button', { name: 'Merge into selected survivor' })).toHaveProperty('disabled', true);
  });

  it('offers Retry when the requested wait exceeds the automatic limit', async () => {
    let gets = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      if (request.method === 'POST') {
        return Response.json({ error: 'person_merge_revision_conflict', message: 'Reload profiles' }, { status: 409 });
      }
      gets++;
      return Response.json({ error: 'rate_limited' }, { status: 429, headers: { 'Retry-After': '31' } });
    });
    renderModal(fetchFn);
    await selectSurvivor();
    await fireEvent.click(screen.getByRole('button', { name: 'Merge into selected survivor' }));
    expect((await screen.findByRole('alert')).textContent).toContain("couldn't be reloaded");
    await waitFor(() => expect(screen.getByRole('button', { name: 'Retry profile reload' })).toHaveProperty('disabled', false));
    expect(gets).toBe(2);
  });

  it.each([401, 403, 404])('stops on %s while the other profile stalls', async (status) => {
    let siblingSignal: AbortSignal | undefined;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      if (request.method === 'POST') {
        return Response.json({ error: 'person_merge_revision_conflict', message: 'Reload profiles' }, { status: 409 });
      }
      if (new URL(request.url).pathname.endsWith('/7')) return Response.json({ error: 'denied' }, { status });
      siblingSignal = request.signal;
      return new Promise<Response>((_resolve, reject) => request.signal.addEventListener('abort', () => reject(new Error('aborted'))));
    });
    renderModal(fetchFn);
    await selectSurvivor();
    await fireEvent.click(screen.getByRole('button', { name: 'Merge into selected survivor' }));
    expect((await screen.findByRole('alert')).textContent).toContain("couldn't be reloaded");
    await waitFor(() => expect(screen.getByRole('button', { name: 'Retry profile reload' })).toHaveProperty('disabled', false));
    expect(siblingSignal?.aborted).toBe(true);
  });

  it('times out a stalled profile and retries it', async () => {
    let sevenGets = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      if (request.method === 'POST') {
        return Response.json({ error: 'person_merge_revision_conflict', message: 'Reload profiles' }, { status: 409 });
      }
      const id = Number(new URL(request.url).pathname.split('/').at(-1));
      if (id === 7 && ++sevenGets === 1) {
        return new Promise<Response>((_resolve, reject) => request.signal.addEventListener('abort', () => reject(new Error('timeout'))));
      }
      return Response.json(person(id, 5, [id * 10], `Updated ${id}`), { headers: { ETag: `"person-${id}-r5"` } });
    });
    renderModal(fetchFn);
    await selectSurvivor();
    vi.useFakeTimers();
    vi.spyOn(AbortSignal, 'timeout').mockImplementation((ms) => {
      const controller = new AbortController();
      setTimeout(() => controller.abort(), ms);
      return controller.signal;
    });
    await fireEvent.click(screen.getByRole('button', { name: 'Merge into selected survivor' }));
    await vi.advanceTimersByTimeAsync(10_500);
    expect(sevenGets).toBe(2);
    expect(screen.getByText('Updated 7 (Person 7)')).toBeDefined();
  });

  it('exhausts four attempts for each profile before offering Retry', async () => {
    const ids: number[] = [];
    let batch: Array<() => void> = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      if (request.method === 'POST') {
        return Response.json({ error: 'person_merge_revision_conflict', message: 'Reload profiles' }, { status: 409 });
      }
      ids.push(Number(new URL(request.url).pathname.split('/').at(-1)));
      return new Promise<Response>((resolve) => {
        batch.push(() => resolve(Response.json({ error: 'unavailable' }, { status: 503 })));
        if (batch.length === 2) {
          const current = batch;
          batch = [];
          current.forEach((finish) => finish());
        }
      });
    });
    renderModal(fetchFn);
    await selectSurvivor();
    vi.useFakeTimers();
    await fireEvent.click(screen.getByRole('button', { name: 'Merge into selected survivor' }));
    await vi.advanceTimersByTimeAsync(3500);
    expect(ids.filter((id) => id === 7)).toHaveLength(4);
    expect(ids.filter((id) => id === 9)).toHaveLength(4);
    expect(screen.getByRole('button', { name: 'Retry profile reload' })).toHaveProperty('disabled', false);
  });

  it('closes during backoff and sends no more requests', async () => {
    let gets = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      if (request.method === 'POST') {
        return Response.json({ error: 'person_merge_revision_conflict', message: 'Reload profiles' }, { status: 409 });
      }
      gets++;
      return Response.json({ error: 'unavailable' }, { status: 503 });
    });
    const onClose = vi.fn(() => unmount());
    const { unmount } = renderModal(fetchFn, { onClose });
    await selectSurvivor();
    vi.useFakeTimers();
    await fireEvent.click(screen.getByRole('button', { name: 'Merge into selected survivor' }));
    await vi.advanceTimersByTimeAsync(0);
    expect(gets).toBe(2);
    await fireEvent.keyDown(window, { key: 'Escape' });
    expect(onClose).toHaveBeenCalledOnce();
    await vi.advanceTimersByTimeAsync(3500);
    expect(gets).toBe(2);
  });

  it('offers one non-mutating inspection action per profile', async () => {
    const fetchFn = vi.fn<typeof fetch>();
    const onOpenProfile = vi.fn();
    renderModal(fetchFn, { onOpenProfile });

    await fireEvent.click(screen.getByRole('button', { name: 'Open Synthetic One profile' }));
    await fireEvent.click(screen.getByRole('button', { name: 'Open Synthetic Two profile' }));

    expect(onOpenProfile.mock.calls).toEqual([[7], [9]]);
    expect(fetchFn).not.toHaveBeenCalled();
  });

  it('blocks dismissal and root shortcuts while merge work is pending', async () => {
    let resolveMerge: ((response: Response) => void) | undefined;
    const fetchFn = vi.fn<typeof fetch>(
      () =>
        new Promise<Response>((resolve) => {
          resolveMerge = resolve;
        }),
    );
    const onClose = vi.fn();
    const rootShortcut = vi.fn();
    const unregister = appShortcuts.register('x', rootShortcut);
    try {
      renderModal(fetchFn, { onClose });
      await waitFor(() => expect(appShortcuts.activeScope()).toBe('person-binding-conflict-modal'));
      await selectSurvivor();
      await fireEvent.click(screen.getByRole('button', { name: 'Merge into selected survivor' }));

      expect(screen.getByRole('button', { name: 'Cancel' })).toHaveProperty('disabled', true);
      expect(screen.queryByRole('button', { name: 'Close person merge' })).toBeNull();
      await fireEvent.keyDown(window, { key: 'Escape' });
      await fireEvent.pointerDown(document.querySelector('.kit-modal-overlay')!);
      appShortcuts.handleKeydown(new KeyboardEvent('keydown', { key: 'x', cancelable: true }));
      expect(onClose).not.toHaveBeenCalled();
      expect(rootShortcut).not.toHaveBeenCalled();

      resolveMerge?.(Response.json({ error: 'person_carddav_published', message: 'Unpublish first' }, { status: 409 }));
      expect((await screen.findByRole('alert')).textContent).toContain('Unpublish first');
      await fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));
      expect(onClose).toHaveBeenCalledOnce();
    } finally {
      unregister();
    }
  });
});
