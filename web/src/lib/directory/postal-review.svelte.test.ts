import { webcrypto } from 'node:crypto';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import type { Assessment, Input } from '../api/generated/models';
import { postalAddressText, postalFingerprint, PostalReviewSession } from './postal-review.svelte';

beforeEach(() => vi.stubGlobal('crypto', webcrypto));
afterEach(() => { vi.unstubAllGlobals(); vi.restoreAllMocks(); });

const input: Input = { address_kind: 'postal', street_address: ' 12 Example St ', extended_address: 'Unit 7', country_code: 'ca', original_value: ' raw\ntext ' };
async function assessment(value = input): Promise<Assessment> {
  return { original: { ...value }, proposed: { ...value, street_address: '12 Example Street' }, status: 'validated', provider: 'google', reason_codes: [], fingerprint: await postalFingerprint(value) };
}
function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (error: Error) => void;
  const promise = new Promise<T>((done, fail) => { resolve = done; reject = fail; });
  return { promise, resolve, reject };
}

describe('PostalReviewSession', () => {
  it('shows country code changes even when the country name stays the same', () => {
    const original = { address_kind: 'postal', country_name: 'Canada', country_code: 'CA' };
    expect(postalAddressText(original)).toContain('CA');
    expect(postalAddressText({ ...original, country_code: 'US' })).toContain('US');
    expect(postalAddressText(original)).not.toBe(postalAddressText({ ...original, country_code: 'US' }));
  });
  it('fingerprints every field, Unicode and whitespace, with the Go length-framed contract', async () => {
    const fingerprint = await postalFingerprint(input);
    expect(fingerprint).toBe('f7897d1e0b2a3120740b2882ec8937539e022e5ce8ccf969cfef6b253a0871a3');
    for (const [field, value] of Object.entries(input)) {
      expect(await postalFingerprint({ ...input, [field]: `${value}あ` })).not.toBe(fingerprint);
    }
    expect(await postalFingerprint({ ...input, timezone: 'Europe/Paris' })).not.toBe(fingerprint);
  });

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

  it.each([7, 8])('rejects old results after close/reopen or person switch to %s', async nextPerson => {
    const reply = deferred<Assessment>();
    const check = vi.fn(async (_personID: number, _input: Input, _external: boolean, _signal: AbortSignal) => reply.promise);
    const old = new PostalReviewSession(7, check);
    const pending = old.check(input, true);
    await vi.waitFor(() => expect(check).toHaveBeenCalledOnce());
    old.destroy();
    const reopened = new PostalReviewSession(nextPerson, check);
    expect(reopened.editorGeneration).not.toBe(old.editorGeneration);
    reply.resolve(await assessment()); await pending;
    expect(old.result).toBeNull(); expect(reopened.result).toBeNull(); expect(reopened.pending).toBe(false);
  });

  it('rejects a response with a mismatched full-input fingerprint', async () => {
    const check = vi.fn(async () => ({ ...await assessment(), fingerprint: 'wrong' }));
    const state = new PostalReviewSession(7, check);
    await state.check(input, true);
    expect(state.result).toBeNull(); expect(state.error).toBeTruthy();
  });

  it.each([['SubtleCrypto', {}], ['crypto', undefined]])('checks locally and externally without %s', async (_capability, crypto) => {
    const result = await assessment();
    vi.stubGlobal('crypto', crypto);
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

  it('still invalidates a late result without SubtleCrypto', async () => {
    const result = await assessment();
    vi.stubGlobal('crypto', {});
    const reply = deferred<Assessment>();
    const check = vi.fn(async (_personID: number, _input: Input, _external: boolean, _signal: AbortSignal) => reply.promise);
    const state = new PostalReviewSession(7, check);
    const pending = state.check(input, true);
    await vi.waitFor(() => expect(check).toHaveBeenCalledOnce());
    state.invalidate(); state.invalidate();
    reply.resolve(result); await pending;
    expect(check.mock.calls[0]?.[3].aborted).toBe(true);
    expect(state.result).toBeNull(); expect(state.error).toBeNull(); expect(state.pending).toBe(false);
  });

  it.each([true, false])('rejects a mismatched echoed input with WebCrypto available=%s', async available => {
    const result = await assessment();
    result.original = { ...input, street_address: '99 Example Lane' };
    if (!available) vi.stubGlobal('crypto', {});
    const state = new PostalReviewSession(7, vi.fn(async () => result));
    await state.check(input, true);
    expect(state.result).toBeNull(); expect(state.error).toBeTruthy();
  });
});
