import { beforeEach, describe, expect, it, vi } from 'vitest';

const KEY = 'msgvault.reading-pane.position';

async function load() {
  vi.resetModules();
  return (await import('./reader-position.svelte')).readerPosition;
}

describe('readerPosition', () => {
  beforeEach(() => localStorage.clear());

  it('opens below the list until the reader moves', async () => {
    expect((await load()).value).toBe('below');
  });

  it('remembers a move to the right across page loads', async () => {
    const first = await load();
    first.set('right');
    expect(first.value).toBe('right');
    expect(localStorage.getItem(KEY)).toBe('right');
    expect((await load()).value).toBe('right');
  });

  it('treats any other value as below', async () => {
    const position = await load();
    position.set('right');
    position.set('sideways');
    expect(position.value).toBe('below');
  });
});
