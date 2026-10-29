import { appShortcuts, initShortcuts } from '@kenn-io/kit-ui';
import { fireEvent, render, screen } from '@testing-library/svelte';
import { createRawSnippet } from 'svelte';
import { afterAll, beforeAll, describe, expect, it, vi } from 'vitest';

import NavigationDrawer from './NavigationDrawer.svelte';

const items = createRawSnippet(() => ({
  render: () =>
    '<nav aria-label="Primary"><button>Files</button><button aria-current="page">Messages</button></nav>'
}));

let detachShortcuts: () => void;

beforeAll(() => {
  detachShortcuts = initShortcuts();
});

afterAll(() => {
  detachShortcuts();
});

describe('NavigationDrawer', () => {
  it('focuses the current item and closes on Escape without reaching root shortcuts', async () => {
    const root = vi.fn();
    const unregister = appShortcuts.register('escape', root);
    const onclose = vi.fn();
    render(NavigationDrawer, { onclose, children: items });

    await vi.waitFor(() =>
      expect(document.activeElement).toBe(screen.getByRole('button', { name: 'Messages' }))
    );
    await fireEvent.keyDown(document.activeElement as Element, { key: 'Escape' });

    expect(onclose).toHaveBeenCalledOnce();
    expect(root).not.toHaveBeenCalled();
    unregister();
  });

  it('closes from the scrim', async () => {
    const onclose = vi.fn();
    render(NavigationDrawer, { onclose, children: items });
    await fireEvent.click(screen.getByRole('button', { name: 'Close navigation' }));
    expect(onclose).toHaveBeenCalledOnce();
  });
});
