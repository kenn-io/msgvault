import { describe, expect, it } from 'vitest';

import { maskSecret } from './secrets';

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
