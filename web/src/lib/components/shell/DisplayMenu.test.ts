import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';

import DisplayMenu from './DisplayMenu.svelte';

describe('DisplayMenu', () => {
  it('sets and clears the per-tab density override', async () => {
    const onDensityChange = vi.fn();
    render(DisplayMenu, {
      density: 'daemon',
      themeOverridden: false,
      onDensityChange,
      onUseDaemonTheme: vi.fn(),
    });
    await fireEvent.click(screen.getByRole('button', { name: 'Display' }));

    expect(screen.getByRole('group', { name: 'Temporary density' })).toBeTruthy();
    await fireEvent.click(screen.getByRole('menuitemradio', { name: 'Comfortable' }));
    expect(onDensityChange).toHaveBeenCalledWith('comfortable');
    await fireEvent.click(screen.getByRole('menuitemradio', { name: 'Auto' }));
    expect(onDensityChange).toHaveBeenCalledWith('daemon');
    expect(screen.queryByRole('menuitem', { name: 'Use daemon theme' })).toBeNull();
  });

  it('shows the active density override as checked', async () => {
    render(DisplayMenu, {
      density: 'compact',
      themeOverridden: false,
      onDensityChange: vi.fn(),
      onUseDaemonTheme: vi.fn(),
    });
    await fireEvent.click(screen.getByRole('button', { name: 'Display' }));

    const checked = (name: string) =>
      screen.getByRole('menuitemradio', { name }).getAttribute('aria-checked');
    expect(checked('Compact')).toBe('true');
    expect(checked('Auto')).toBe('false');
    expect(checked('Comfortable')).toBe('false');
  });

  it('offers the daemon theme only while a theme override is active', async () => {
    const onUseDaemonTheme = vi.fn();
    render(DisplayMenu, {
      density: 'compact',
      themeOverridden: true,
      onDensityChange: vi.fn(),
      onUseDaemonTheme,
    });
    await fireEvent.click(screen.getByRole('button', { name: 'Display' }));
    await fireEvent.click(screen.getByRole('menuitem', { name: 'Use daemon theme' }));
    expect(onUseDaemonTheme).toHaveBeenCalledOnce();
  });

  it('closes on Escape and returns focus to its trigger', async () => {
    render(DisplayMenu, {
      density: 'compact',
      themeOverridden: false,
      onDensityChange: vi.fn(),
      onUseDaemonTheme: vi.fn(),
    });
    const trigger = screen.getByRole('button', { name: 'Display' });
    await fireEvent.click(trigger);
    const item = await screen.findByRole('menuitemradio', { name: 'Comfortable' });
    await waitFor(() => expect(document.activeElement).not.toBe(document.body));

    await fireEvent.keyDown(item, { key: 'Escape' });

    await waitFor(() => expect(screen.queryByRole('menu', { name: 'Display' })).toBeNull());
    expect(document.activeElement).toBe(trigger);
  });
});
