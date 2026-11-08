import { expect, it } from 'vitest';
import { WriteAttempt } from './write-attempt.svelte';

it('clears failures before a retry and carries the saved version into the send', async () => {
  const attempt = new WriteAttempt<{ revision: number }>();
  await attempt.run(async () => ({ ok: false, kind: 'conflict', status: 412, current: { revision: 2 }, message: 'Changed' }));
  expect(attempt.failed).toBe(true);
  expect(attempt.conflict).toEqual({ revision: 2 });
  await attempt.run(async current => {
    expect(current).toEqual({ revision: 2 });
    expect(attempt.conflict).toBeUndefined();
    expect(attempt.failed).toBe(false);
    expect(attempt.details).toBe('');
    return { ok: false, kind: 'error', status: 500, message: 'Unavailable' };
  });
  expect(attempt.details).toBe('Unavailable');
  await attempt.run(async current => {
    expect(current).toEqual({ revision: 2 });
    return { ok: true };
  });
  attempt.reset();
  expect(attempt.failed).toBe(false);
  expect(attempt.details).toBe('');
});
it.each(['unknown', 'blocked'] as const)('keeps %s outcomes out of Details', async kind => {
  const attempt = new WriteAttempt();
  await attempt.run(async () => ({ ok: false, kind, message: 'Check again' }));
  expect(attempt.failed).toBe(true);
  expect(attempt.details).toBe('');
});
it('allows one write at a time', async () => {
  const attempt = new WriteAttempt();
  let finish!: () => void;
  const pending = attempt.run(async () => { await new Promise<void>(resolve => finish = resolve); return { ok: true }; });
  expect(await attempt.run(async () => { throw new Error('Second write'); })).toBe(false);
  finish();
  expect(await pending).toBe(true);
  expect(attempt.busy).toBe(false);
});
