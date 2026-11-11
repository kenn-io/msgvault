import { afterEach, describe, expect, it, vi } from 'vitest';

import type { Assessment, Input } from '../api/generated/models';
import { postalAddressText, PostalReviewSession } from './postal-review.svelte';

afterEach(() => { vi.unstubAllGlobals(); vi.restoreAllMocks(); });

const input: Input = { address_kind: 'postal', street_address: ' 12 Example St ', extended_address: 'Unit 7', country_code: 'ca', original_value: ' raw\ntext ' };
async function assessment(value = input): Promise<Assessment> {
  return { original: { ...value }, proposed: { ...value, street_address: '12 Example Street' }, status: 'validated', provider: 'google', reason_codes: [], original_extended_parts: [], proposed_extended_parts: [] };
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
    expect(postalAddressText(original)).not.toBe(postalAddressText({ ...original, country_code: 'US' }));
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
  it('suppresses duplicate pending and displayed checks without saving anything', async () => {
    const reply = deferred<Assessment>();
    const check = vi.fn(async (_personID: number, _input: Input, _external: boolean, _signal: AbortSignal) => reply.promise);
    const state = new PostalReviewSession(7, check);
    const pending = state.check(input, true);
    await vi.waitFor(() => expect(check).toHaveBeenCalledTimes(1));
    await state.check(input, true);
    expect(check).toHaveBeenCalledTimes(1);
    const result = await assessment(); reply.resolve(result); await pending;
    expect(state.result).toEqual(result);
    await state.check(input, true);
    expect(check).toHaveBeenCalledTimes(1);
    expect(state.pending).toBe(false);
    expect(check.mock.calls[0]?.[0]).toBe(7);
  });

  it.each(['success', 'error'] as const)('invalidates A → B → A including stale %s and loading', async outcome => {
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

  it('keeps only the latest request when responses complete in reverse order', async () => {
    const first = deferred<Assessment>(), second = deferred<Assessment>();
    const check = vi.fn().mockImplementationOnce(() => first.promise).mockImplementationOnce(() => second.promise);
    const state = new PostalReviewSession(7, check);
    const a = state.check(input, true);
    await vi.waitFor(() => expect(check).toHaveBeenCalledTimes(1));
    state.invalidate();
    const changed = { ...input, street_address: '99 Example Lane' };
    const b = state.check(changed, true);
    await vi.waitFor(() => expect(check).toHaveBeenCalledTimes(2));
    const latest = await assessment(changed); second.resolve(latest); await b;
    first.resolve(await assessment()); await a;
    expect(state.result).toEqual(latest); expect(state.pending).toBe(false);
  });

  it('rejects old results after close/reopen or person switch', async () => {
    const reply = deferred<Assessment>();
    const check = vi.fn(async (_personID: number, _input: Input, _external: boolean, _signal: AbortSignal) => reply.promise);
    const old = new PostalReviewSession(7, check);
    const pending = old.check(input, true);
    await vi.waitFor(() => expect(check).toHaveBeenCalledOnce());
    old.destroy();
    const reopened = new PostalReviewSession(8, check);
    expect(reopened.editorGeneration).not.toBe(old.editorGeneration);
    reply.resolve(await assessment()); await pending;
    expect(old.result).toBeNull(); expect(reopened.result).toBeNull(); expect(reopened.pending).toBe(false);
  });

  it('checks locally and externally', async () => {
    const result = await assessment();
    const check = vi.fn(async (_personID: number, _input: Input, external: boolean, _signal: AbortSignal): Promise<Assessment> =>
      external ? result : { ...result, status: 'normalized_unverified', provider: undefined });
    const state = new PostalReviewSession(7, check);
    await state.check(input, false);
    expect(state.result?.status).toBe('normalized_unverified');
    expect(state.error).toBeNull();
    await state.check(input, true);
    expect(state.result).toEqual(result);
    expect(state.error).toBeNull(); expect(state.pending).toBe(false);
    expect(check.mock.calls.map(call => call.slice(0, 3))).toEqual([[7, input, false], [7, input, true]]);
  });

  it('rejects a mismatched echoed input', async () => {
    const result = await assessment();
    result.original = { ...input, street_address: '99 Example Lane' };
    const state = new PostalReviewSession(7, vi.fn(async () => result));
    await state.check(input, true);
    expect(state.result).toBeNull(); expect(state.error).toBeTruthy();
  });
});
