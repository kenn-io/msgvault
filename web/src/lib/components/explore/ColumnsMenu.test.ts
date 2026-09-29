import { fireEvent, render, screen } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';

import ColumnsMenu from './ColumnsMenu.svelte';

describe('ColumnsMenu', () => {
  it('toggles a column off and keeps the canonical column order', async () => {
    const onchange = vi.fn();
    render(ColumnsMenu, { columns: ['kind', 'title', 'excerpt', 'time'], onchange });

    await fireEvent.click(screen.getByRole('button', { name: 'Columns' }));
    await fireEvent.click(screen.getByRole('button', { name: 'Excerpt' }));

    expect(onchange).toHaveBeenCalledWith(['kind', 'title', 'time']);
  });

  it('adds a hidden column in canonical position', async () => {
    const onchange = vi.fn();
    render(ColumnsMenu, { columns: ['title', 'time'], onchange });

    await fireEvent.click(screen.getByRole('button', { name: 'Columns' }));
    await fireEvent.click(screen.getByRole('button', { name: 'Kind' }));

    expect(onchange).toHaveBeenCalledWith(['kind', 'title', 'time']);
  });

  it('keeps the last visible column', async () => {
    const onchange = vi.fn();
    render(ColumnsMenu, { columns: ['title'], onchange });

    await fireEvent.click(screen.getByRole('button', { name: 'Columns' }));
    await fireEvent.click(screen.getByRole('button', { name: 'Subject / title' }));

    expect(onchange).not.toHaveBeenCalled();
  });
});
