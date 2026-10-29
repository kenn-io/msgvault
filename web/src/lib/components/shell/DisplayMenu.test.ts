import { fireEvent, render, screen } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';

import DisplayMenu from './DisplayMenu.svelte';

function renderMenu(overrides: Record<string, unknown> = {}) {
  const props = {
    theme: 'system' as const,
    density: 'compact' as const,
    onThemeChange: vi.fn(),
    onDensityChange: vi.fn(),
    ...overrides,
  };
  render(DisplayMenu, props);
  return props;
}

const open = () => fireEvent.click(screen.getByRole('button', { name: 'Display' }));

describe('DisplayMenu', () => {
  it('sets theme and density from one menu', async () => {
    const props = renderMenu();
    await open();

    await fireEvent.click(screen.getByRole('menuitemradio', { name: 'Dark' }));
    expect(props.onThemeChange).toHaveBeenCalledWith('dark');
    await fireEvent.click(screen.getByRole('menuitemradio', { name: 'Comfortable' }));
    expect(props.onDensityChange).toHaveBeenCalledWith('comfortable');
  });

  it('shows the current theme and density as checked', async () => {
    renderMenu({ theme: 'light', density: 'comfortable' });
    await open();

    const checked = (name: string) => screen.getByRole('menuitemradio', { name }).getAttribute('aria-checked');
    expect(checked('Light')).toBe('true');
    expect(checked('System')).toBe('false');
    expect(checked('Comfortable')).toBe('true');
    expect(checked('Compact')).toBe('false');
  });

  it('leaves out columns and reader position where the page has neither', async () => {
    renderMenu();
    await open();
    expect(screen.queryByRole('menuitem', { name: /Subject/ })).toBeNull();
    expect(screen.queryByRole('menuitemradio', { name: 'Below' })).toBeNull();
  });

  it('toggles a column and keeps one visible', async () => {
    const onColumnsChange = vi.fn();
    const rendered = render(DisplayMenu, { theme: 'system', density: 'compact', columns: ['title', 'time'], onColumnsChange, onThemeChange: vi.fn(), onDensityChange: vi.fn() });
    await open();

    await fireEvent.click(screen.getByRole('menuitem', { name: /^Time/ }));
    expect(onColumnsChange).toHaveBeenLastCalledWith(['title']);
    await fireEvent.click(screen.getByRole('menuitem', { name: /^Kind/ }));
    expect(onColumnsChange).toHaveBeenLastCalledWith(['kind', 'title', 'time']);
    await rendered.rerender({ columns: ['title'] });
    onColumnsChange.mockClear();
    await fireEvent.click(screen.getByRole('menuitem', { name: /^Subject/ }));
    expect(onColumnsChange).not.toHaveBeenCalled();
  });

  it('sets the reader position', async () => {
    const onReaderPositionChange = vi.fn();
    renderMenu({ readerPosition: 'below', onReaderPositionChange });
    await open();
    expect(screen.getByRole('menuitemradio', { name: 'Below' }).getAttribute('aria-checked')).toBe('true');
    await fireEvent.click(screen.getByRole('menuitemradio', { name: 'Right' }));
    expect(onReaderPositionChange).toHaveBeenCalledWith('right');
  });
});
