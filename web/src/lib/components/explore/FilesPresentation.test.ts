import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';

import type { ExploreFileFact } from '../../explore/models';
import FilesPresentation from './FilesPresentation.svelte';

function file(index: number): ExploreFileFact {
  return {
    id: index,
    key: `message:${index}:file:${index}`,
    entry_key: `message:${index}`,
    message_id: index,
    conversation_id: index + 1000,
    occurred_at: '2026-07-18T12:00:00Z',
    source_id: 1,
    source_identifier: 'archive@example.com',
    title: `Containing item ${index}`,
    filename: `file-${index}.pdf`,
    mime_type: 'application/pdf',
    size: 2048
  };
}

describe('FilesPresentation', () => {
  it('virtualizes a large loaded slice and implements bounded table navigation', async () => {
    const files = Array.from({ length: 1_000 }, (_, index) => file(index + 1));
    const onActiveKey = vi.fn();
    const onLoadMore = vi.fn();
    render(FilesPresentation, {
      files, hasMore: true, totalCount: 100_000, onActiveKey, onLoadMore
    });
    const grid = screen.getByRole('grid', { name: 'Files in current context' });
    grid.focus();

    await waitFor(() => expect(screen.getAllByRole('row').length).toBeLessThan(80));
    await fireEvent.keyDown(grid, { key: 'PageDown' });
    expect(onActiveKey).toHaveBeenLastCalledWith('message:10:file:10');
    await fireEvent.keyDown(grid, { key: 'PageUp' });
    expect(onActiveKey).toHaveBeenLastCalledWith('message:1:file:1');
    await fireEvent.keyDown(grid, { key: 'End' });
    expect(onActiveKey).toHaveBeenLastCalledWith('message:1000:file:1000');
    expect(onLoadMore).not.toHaveBeenCalled();
  });

  it('loads at most one next page when keyboard movement crosses the loaded boundary', async () => {
    let files = Array.from({ length: 600 }, (_, index) => file(index + 1));
    const onActiveKey = vi.fn();
    let rendered: ReturnType<typeof render>;
    const onLoadMore = vi.fn(async () => {
      files = Array.from({ length: 700 }, (_, index) => file(index + 1));
      await rendered.rerender({
        files, hasMore: true, totalCount: 10_000,
        focusedKey: 'message:600:file:600', onActiveKey, onLoadMore
      });
      return { status: 'advanced' };
    });
    rendered = render(FilesPresentation, {
      files, hasMore: true, totalCount: 10_000,
      focusedKey: 'message:600:file:600', onActiveKey, onLoadMore
    });
    const grid = screen.getByRole('grid', { name: 'Files in current context' });
    grid.focus();

    await fireEvent.keyDown(grid, { key: 'ArrowDown' });

    expect(onLoadMore).toHaveBeenCalledTimes(1);
    await waitFor(() => expect(onActiveKey).toHaveBeenLastCalledWith('message:601:file:601'));
    expect(screen.getAllByRole('row').length).toBeLessThan(80);
  });

  it('keeps loaded files visible and exposes an accessible paging retry', async () => {
    const files = Array.from({ length: 600 }, (_, index) => file(index + 1));
    const onLoadMore = vi.fn(async () => ({ status: 'advanced' }));
    render(FilesPresentation, {
      files, hasMore: true, totalCount: 10_000,
      error: 'The next file page could not be loaded.', onLoadMore
    });

    expect(screen.getByText('file-1.pdf')).toBeDefined();
    await fireEvent.click(screen.getByRole('button', { name: 'Retry loading more files' }));
    expect(onLoadMore).toHaveBeenCalledTimes(1);
  });

  it('restores a keyed scroll anchor and reports user scroll authority', async () => {
    const onScrollAnchor = vi.fn();
    const files = Array.from({ length: 200 }, (_, index) => file(index + 1));
    const rendered = render(FilesPresentation, {
      files,
      focusedKey: 'message:150:file:150',
      scrollAnchor: { key: 'message:140:file:140', offset: 5 },
      restoring: true,
      onScrollAnchor
    });
    const grid = screen.getByRole('grid', { name: 'Files in current context' }) as HTMLDivElement;
    await waitFor(() => expect(grid.scrollTop).toBe(139 * 36 + 5));
    expect(grid.getAttribute('aria-activedescendant')).toContain('message-3a-150');

    await rendered.rerender({
      files, focusedKey: 'message:150:file:150',
      scrollAnchor: { key: 'message:140:file:140', offset: 5 },
      restoring: false, onScrollAnchor
    });
    grid.scrollTop = 72;
    await fireEvent.scroll(grid);
    expect(onScrollAnchor).toHaveBeenLastCalledWith('message:3:file:3', 0);
  });

  it('opens the attachment while keeping the containing item a separate action', async () => {
    const onOpenFile = vi.fn();
    const onOpenItem = vi.fn();
    const selected = file(7);
    render(FilesPresentation, { files: [selected], onOpenFile, onOpenItem });
    const grid = screen.getByRole('grid', { name: 'Files in current context' });
    grid.focus();

    await fireEvent.keyDown(grid, { key: 'Enter' });
    expect(onOpenFile).toHaveBeenCalledWith(selected);
    expect(onOpenItem).not.toHaveBeenCalled();

    await fireEvent.click(screen.getByRole('button', { name: 'Open containing item 7' }));
    expect(onOpenItem).toHaveBeenCalledWith('message:7');
  });
});
