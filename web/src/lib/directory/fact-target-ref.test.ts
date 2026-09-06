import { describe, expect, it } from 'vitest';

import { decodeFactTargetRef, encodeFactTargetRef } from './fact-target-ref';

const revision = `sha256:${'a'.repeat(64)}`;

describe('fact target references', () => {
  it('round-trips colon-bearing keys using the revision suffix as the boundary', () => {
    const encoded = encodeFactTargetRef({ kind: 'attribute', key: 'work:email:primary', revision });

    expect(encoded).toBe(`attribute:work:email:primary:${revision}`);
    expect(decodeFactTargetRef(encoded!)).toEqual({ kind: 'attribute', key: 'work:email:primary', revision });
  });

  it.each([
    [{ kind: 'relationship', key: 'email', revision }],
    [{ kind: 'attribute', key: '', revision }],
    [{ kind: 'attribute', key: ' email ', revision }],
    [{ kind: 'employment', key: 'role', revision: 'v1' }],
    [{ kind: 'employment', key: 'role', revision: `sha256:${'A'.repeat(64)}` }],
    [{ kind: 'employment', key: 'role', revision: ` sha256:${'a'.repeat(64)}` }]
  ])('rejects a malformed generated target %#', (target) => {
    expect(encodeFactTargetRef(target)).toBeUndefined();
  });

  it.each([
    '',
    `relationship:key:${revision}`,
    `attribute::${revision}`,
    `attribute:key:sha256:${'a'.repeat(63)}`,
    `attribute:key:sha256:${'A'.repeat(64)}`,
    ` attribute:key:${revision}`
  ])('rejects a malformed encoded target %#', (encoded) => {
    expect(decodeFactTargetRef(encoded)).toBeUndefined();
  });
});
