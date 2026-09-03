import { fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../../api/client';
import type { components } from '../../api/generated/schema';
import { RelationshipReviewController } from '../../directory/relationship-review-controller.svelte';
import RelationshipReviewQueue from './RelationshipReviewQueue.svelte';

type GeneratedReview = components['schemas']['RelationshipReview'];

function review(id = 41, status = 'pending'): GeneratedReview {
  return {
    id,
    person_id: 7,
    matched_person_id: 9,
    raw_related_value: `urn:uuid:synthetic-${id}`,
    raw_related_type: 'friend',
    value_kind: 'uri',
    status,
    source: 'vcard_import',
    vcard_identity: {},
    created_by: 'synthetic_import',
    created_at: '2026-08-01T10:00:00Z',
    updated_at: '2026-08-02T11:00:00Z'
  };
}

function deferredResponse() {
  let resolve!: (response: Response) => void;
  const promise = new Promise<Response>((settle) => { resolve = settle; });
  return { promise, resolve };
}

function requestOf(input: RequestInfo | URL): Request {
  return input instanceof Request ? input : new Request(input);
}

describe('RelationshipReviewQueue', () => {
  it('renders loading, populated, and honest read-only states without paging or decisions', async () => {
    const pending = deferredResponse();
    const controller = new RelationshipReviewController(createAPIClient(vi.fn<typeof fetch>(() => pending.promise)));
    controller.applyContext(true, 'pending', false);
    render(RelationshipReviewQueue, { controller });

    expect(screen.getByText('Loading imported relationship reviews…')).toBeDefined();
    expect(screen.getByText('Imported relationship reviews are read-only in the browser until generated decision operations are available.')).toBeDefined();
    pending.resolve(Response.json({ reviews: [review()] }));
    expect(await screen.findByRole('article', { name: 'Imported relationship review 41' })).toBeDefined();
    expect(screen.queryByRole('button', { name: /accept|reject|unsure|next|previous/i })).toBeNull();
    expect(screen.queryByRole('navigation', { name: /page/i })).toBeNull();
    controller.destroy();
  });

  it('distinguishes fixed errors from an empty queue and retries only explicitly', async () => {
    let attempts = 0;
    const controller = new RelationshipReviewController(createAPIClient(vi.fn<typeof fetch>(async () => {
      attempts += 1;
      return attempts === 1
        ? Response.json({ error: 'unavailable', message: 'forbidden-private-error' }, { status: 503 })
        : Response.json({ reviews: null });
    })));
    controller.applyContext(true, 'pending', false);
    render(RelationshipReviewQueue, { controller });

    const alert = await screen.findByRole('alert');
    expect(alert.textContent).toContain('Unable to load imported relationship reviews.');
    expect(alert.textContent).not.toContain('forbidden');
    expect(screen.queryByText('No imported relationship reviews in Pending.')).toBeNull();
    expect(attempts).toBe(1);
    await fireEvent.click(within(alert).getByRole('button', { name: 'Retry imported relationship reviews' }));

    expect(await screen.findByText('No imported relationship reviews in Pending.')).toBeDefined();
    expect(screen.queryByRole('alert')).toBeNull();
    expect(attempts).toBe(2);
    controller.destroy();
  });

  it('changes status by keyboard, commits once, replaces rows, and focuses the queue heading', async () => {
    const commit = vi.fn();
    const states: string[] = [];
    const controller = new RelationshipReviewController(createAPIClient(vi.fn<typeof fetch>(async (input) => {
      const state = new URL(requestOf(input).url).searchParams.get('status') ?? 'pending';
      states.push(state);
      return Response.json({ reviews: [review(state === 'accepted' ? 51 : 41, state)] });
    })), commit);
    controller.applyContext(true, 'pending', false);
    render(RelationshipReviewQueue, { controller });
    await screen.findByRole('article', { name: 'Imported relationship review 41' });

    const pending = screen.getByRole('radio', { name: 'Pending' });
    pending.focus();
    await fireEvent.keyDown(pending, { key: 'ArrowRight' });

    expect(await screen.findByRole('article', { name: 'Imported relationship review 51' })).toBeDefined();
    expect(states).toEqual(['pending', 'accepted']);
    expect(commit).toHaveBeenCalledOnce();
    expect(commit).toHaveBeenCalledWith({ relationshipReviewState: 'accepted' });
    await waitFor(() => expect(document.activeElement).toBe(screen.getByRole('heading', { name: 'Imported relationships' })));
    controller.destroy();
  });

  it('restores connected heading focus when the focused row disappears under a failed context', async () => {
    let attempts = 0;
    const controller = new RelationshipReviewController(createAPIClient(vi.fn<typeof fetch>(async () => {
      attempts += 1;
      return attempts === 1
        ? Response.json({ reviews: [review()] })
        : Response.json({ error: 'unavailable', message: 'forbidden-restoration-error' }, { status: 503 });
    })));
    controller.applyContext(true, 'pending', false);
    render(RelationshipReviewQueue, { controller });
    const card = await screen.findByRole('article', { name: 'Imported relationship review 41' });
    card.focus();

    controller.applyContext(true, 'pending', true);
    expect(controller.rows).toEqual([]);
    await waitFor(() => expect(screen.queryByRole('article', { name: 'Imported relationship review 41' })).toBeNull());
    await screen.findByRole('alert');
    const heading = screen.getByRole('heading', { name: 'Imported relationships' });
    await waitFor(() => expect(document.activeElement).toBe(heading));
    expect(document.activeElement?.isConnected).toBe(true);
    controller.destroy();
  });
});
