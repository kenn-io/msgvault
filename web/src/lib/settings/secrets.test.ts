import { describe, expect, it } from 'vitest';

import { maskSecret, pendingSecret } from './secrets';

describe('maskSecret', () => {
  it.each([
    ['', ''],
    ['task-secret', ''],
    ['test-api-key', 'tes…key'],
    ['sk-live-0123456789abcdefx9Q', 'sk-…x9Q'],
    ['ééé-secret-ключ', 'ééé…люч'],
  ])('masks %j as %j, matching the daemon', (value, hint) => {
    expect(maskSecret(value)).toBe(hint);
  });
});


it('projects replacement, removal, and saved secret hints through the same display rules', () => {
  const saved = { configured: true, hint: 'old…key' };
  expect(pendingSecret(saved, { action: 'set', value: 'test-api-key' })).toEqual({ configured: true, hint: 'tes…key' });
  expect(pendingSecret(saved, { action: 'clear' })).toEqual({ configured: false, hint: '' });
  expect(pendingSecret(saved, undefined)).toEqual(saved);
  expect(pendingSecret(undefined, undefined)).toEqual({ configured: false, hint: '' });
});
