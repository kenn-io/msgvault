import { appShortcuts, initShortcuts } from '@kenn-io/kit-ui';
import { fireEvent, render, screen } from '@testing-library/svelte';
import { createRawSnippet } from 'svelte';
import { afterAll, beforeAll, describe, expect, it, vi } from 'vitest';

import NavigationDrawer from './NavigationDrawer.svelte';
import AppShell from './AppShell.svelte';
import { createAPIClient } from '../../api/client';

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

  it('closes from the scrim and returns focus to the navigation opener', async () => {
    const width = window.innerWidth;
    window.innerWidth = 480;
    const client = createAPIClient(async () => Response.json({ saved_views: [], sources: [], lanes: [], rows: [], total_count: 0, cache_revision: 'cache-1', search_provenance: {} }));
    const rendered = render(AppShell, { client, enabled: false });
    try {
      const opener = screen.getByRole('button', { name: 'Open navigation' });
      await fireEvent.click(opener);
      await screen.findByRole('dialog', { name: 'Navigation' });
      await fireEvent.click(screen.getByRole('button', { name: 'Close navigation' }));
      await vi.waitFor(() => expect(screen.queryByRole('dialog', { name: 'Navigation' })).toBeNull());
      expect(document.activeElement).toBe(opener);
    } finally {
      rendered.unmount();
      window.innerWidth = width;
    }
  });
});
