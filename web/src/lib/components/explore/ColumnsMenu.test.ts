import { fireEvent, render, screen } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';

import type { ExploreColumn } from '../../explore/models';
import ColumnsMenu from './ColumnsMenu.svelte';

describe('ColumnsMenu', () => {
  it('toggles a column off and announces whether each column is shown', async () => {
    const onchange = vi.fn();
    const rendered = render(ColumnsMenu, {
      columns: ['kind', 'title', 'excerpt', 'time'], onchange
    });

    await fireEvent.click(screen.getByRole('button', { name: 'Columns' }));
    const excerpt = screen.getByRole('button', { name: 'Excerpt' });
    expect(describedBy(excerpt)).toBe('Shown');
    await fireEvent.click(excerpt);

    const next: ExploreColumn[] = ['kind', 'title', 'time'];
    expect(onchange).toHaveBeenCalledWith(next);
    await rendered.rerender({ columns: next });
    expect(describedBy(screen.getByRole('button', { name: 'Excerpt' }))).toBe('Hidden');
  });

  it('adds a hidden column in canonical position', async () => {
    const onchange = vi.fn();
    render(ColumnsMenu, { columns: ['title', 'time'], onchange });

    await fireEvent.click(screen.getByRole('button', { name: 'Columns' }));
    await fireEvent.click(screen.getByRole('button', { name: 'Kind' }));

    expect(onchange).toHaveBeenCalledWith(['kind', 'title', 'time']);
  });

  it('disables the last visible column', async () => {
    const onchange = vi.fn();
    render(ColumnsMenu, { columns: ['title'], onchange });

    await fireEvent.click(screen.getByRole('button', { name: 'Columns' }));
    const title = screen.getByRole('button', { name: 'Subject / title' }) as HTMLButtonElement;
    expect(title.disabled).toBe(true);
    await fireEvent.click(title);

    expect(onchange).not.toHaveBeenCalled();
  });
});

function describedBy(element: HTMLElement): string | undefined {
  const id = element.getAttribute('aria-describedby');
  return id ? document.getElementById(id)?.textContent?.trim() : undefined;
}
