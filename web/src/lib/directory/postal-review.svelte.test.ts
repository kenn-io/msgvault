import { describe, expect, it, vi } from 'vitest';

import type { Assessment, Input } from '../api/generated/models';
import { postalAddressText, PostalReviewSession } from './postal-review.svelte';

const input: Input = { address_kind: 'postal', street_address: ' 12 Example St ', extended_address: 'Unit 7', country_code: 'ca', original_value: ' raw\ntext ' };
async function assessment(value = input): Promise<Assessment> {
  return { original: { ...value }, proposed: { ...value, street_address: '12 Example Street' }, status: 'validated', provider: 'google', reason_codes: ['provider_confirmed'], original_extended_parts: [], proposed_extended_parts: [] };
}
function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (error: Error) => void;
  const promise = new Promise<T>((done, fail) => { resolve = done; reject = fail; });
  return { promise, resolve, reject };
}

describe('postalAddressText', () => {
  it('shows decoded extended components without repeating the first seven ADR fields', () => {
    const address = { address_kind: 'postal', street_address: '12 Example Street', locality: 'Example City', extended_components: ';;12 Example Street;Example City;;;;Room\\;3;Floor 2' };
    expect(postalAddressText(address, ['Room;3', 'Floor 2'])).toBe('12 Example Street, Example City, Room;3, Floor 2');
  });
  it('shows a current label when no current components or free text exist', () => {
    expect(postalAddressText({ address_kind: 'postal', label: 'Mailing label', original_value: '' })).toBe('Mailing label');
  });
  it('shows country code changes even when the country name stays the same', () => {
    const original = { address_kind: 'postal', country_name: 'Canada', country_code: 'CA' };
    expect(postalAddressText(original)).toBe('Canada');
    expect(postalAddressText({ ...original, country_code: 'US' })).toContain('US');
  });
  it('avoids repeating free text already present in a component', () => {
    expect(postalAddressText({ address_kind: 'postal', free_text: '12 Example Street', street_address: '12 Example Street', locality: 'Example City' })).toBe('12 Example Street, Example City');
    expect(postalAddressText({ address_kind: 'postal', free_text: 'Use side door', street_address: '12 Example Street' })).toBe('Use side door, 12 Example Street');
    expect(postalAddressText({ address_kind: 'postal', free_text: '12 Example Street\nExample City', street_address: '12 Example Street', locality: 'Example City' })).toBe('12 Example Street, Example City');
    expect(postalAddressText({ address_kind: 'postal', free_text: '12 Example Street, Example City', street_address: '12 Example Street', locality: 'Example City', country_code: 'CA' })).toBe('12 Example Street, Example City, CA');
    expect(postalAddressText({ address_kind: 'postal', free_text: '12 Example Street\nUse side door', street_address: '12 Example Street', locality: 'Example City' })).toBe('Use side door, 12 Example Street, Example City');
  });
});

describe('PostalReviewSession', () => {
  it('ignores the response ID from an invalidated check', async () => {
    const first = deferred<Assessment>(), second = deferred<Assessment>();
    const check = vi.fn().mockImplementationOnce(() => first.promise).mockImplementationOnce(() => second.promise).mockImplementation(async (_id: number, draft: Input) => ({ ...await assessment(draft), response_id: 'third' }));
    const state = new PostalReviewSession(7, check);
    const stale = state.check(input, true);
    state.invalidate();
    const changed = { ...input, street_address: '12 Example Street' };
    const current = state.check(changed, true);
    const latest = { ...await assessment(changed), response_id: 'current-id' };
    second.resolve(latest);
    await current;
    first.resolve({ ...await assessment(input), response_id: 'stale-id' });
    await stale;
    expect(state.result).toEqual(latest);
    expect(state.pending).toBe(false);
    state.invalidate();
    const editedAgain = { ...changed, locality: 'Example City' };
    await state.check(editedAgain, true);
    expect(check).toHaveBeenLastCalledWith(7, editedAgain, true, expect.any(AbortSignal), 'current-id');
  });

  it('suppresses duplicate pending checks', async () => {
    const reply = deferred<Assessment>();
    const check = vi.fn(async (_personID: number, _input: Input, _external: boolean, _signal: AbortSignal) => reply.promise);
    const state = new PostalReviewSession(7, check);
    const pending = state.check(input, true);
    await vi.waitFor(() => expect(check).toHaveBeenCalledTimes(1));
    await state.check(input, true);
    expect(check).toHaveBeenCalledTimes(1);
    const result = await assessment(); reply.resolve(result); await pending;
    expect(state.result).toEqual(result);
    expect(state.pending).toBe(false);
  });

  it.each(['success', 'error'] as const)('invalidates a pending check with stale %s and loading', async outcome => {
    const reply = deferred<Assessment>();
    const check = vi.fn(async (_personID: number, _input: Input, _external: boolean, _signal: AbortSignal) => reply.promise);
    const state = new PostalReviewSession(7, check);
    const pending = state.check(input, true);
    await vi.waitFor(() => expect(check).toHaveBeenCalledOnce());
    state.invalidate(); state.invalidate();
    expect(check.mock.calls[0]?.[3].aborted).toBe(true);
    if (outcome === 'success') reply.resolve(await assessment()); else reply.reject(new Error('stale error'));
    await pending;
    expect(state.result).toBeNull(); expect(state.error).toBeNull(); expect(state.pending).toBe(false);
  });

  it('checks locally and externally', async () => {
    const result = await assessment();
    const unavailable = { ...result, status: 'unavailable' as const, reason_codes: ['not_verified', 'provider_request_failed'] };
    const check = vi.fn<(_personID: number, _input: Input, external: boolean, _signal: AbortSignal) => Promise<Assessment>>()
      .mockResolvedValueOnce({ ...result, status: 'normalized_unverified', provider: undefined }).mockResolvedValueOnce(unavailable).mockResolvedValueOnce(result);
    const state = new PostalReviewSession(7, check);
    await state.check(input, false);
    expect(state.result?.status).toBe('normalized_unverified');
    expect(state.error).toBeNull();
    await state.check(input, true);
    expect(state.result?.status).toBe('unavailable');
    await state.check(input, true);
    expect(state.result).toEqual(result);
    expect(state.error).toBeNull(); expect(state.pending).toBe(false);
    expect(check.mock.calls.map(call => call.slice(0, 3))).toEqual([[7, input, false], [7, input, true], [7, input, true]]);
  });

  it('rejects a mismatched echoed input', async () => {
    const result = await assessment();
    result.original = { ...input, street_address: '99 Example Lane' };
    const state = new PostalReviewSession(7, vi.fn(async () => result));
    await state.check(input, true);
    expect(state.result).toBeNull(); expect(state.error).toBeTruthy();
  });
});
