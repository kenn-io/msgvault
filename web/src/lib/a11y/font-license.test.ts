import { readFileSync } from 'node:fs';
import { describe, expect, it } from 'vitest';

describe('bundled Figtree license', () => {
  it('redistributes the authoritative Fontsource notice with production assets', () => {
    const upstream = readFileSync('node_modules/@fontsource-variable/figtree/LICENSE', 'utf8');
    const distributed = readFileSync('public/licenses/Figtree-OFL.txt', 'utf8');

    expect(distributed).toBe(upstream);
    expect(distributed).toContain('SIL OPEN FONT LICENSE Version 1.1');
    expect(distributed).toContain('Copyright 2022 The Figtree Project Authors');
  });
});
