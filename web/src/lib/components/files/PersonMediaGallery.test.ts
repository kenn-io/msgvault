import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../../api/client';
import type { PersonFileSearchRow } from '../../explore/models';
import PersonMediaGallery from './PersonMediaGallery.svelte';

const BOUNDED_PNG = new Uint8Array([
  0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a,
  0, 0, 0, 13, 0x49, 0x48, 0x44, 0x52,
  0, 0, 0, 1, 0, 0, 0, 1, 8, 2, 0, 0, 0, 0, 0, 0, 0,
  0, 0, 0, 0, 0x49, 0x45, 0x4e, 0x44, 0, 0, 0, 0
]);

function media(overrides: Partial<PersonFileSearchRow> = {}): PersonFileSearchRow {
  return {
    id: 7, key: 'file:7', entry_key: 'message:11', message_id: 11, conversation_id: 21,
    occurred_at: '2026-07-18T12:00:00Z', source_id: 1, source_type: 'synthetic',
    source_identifier: 'archive@example.com', containing_title: 'Summer thread',
    filename: 'photo.png', mime_type: 'image/png', mime_family: 'image', size_bytes: 8,
    content_state: 'metadata_only', content_available: false,
    person_provenance: {
      participant_ids: [42], roles: ['to', 'cc', 'conversation_member'],
      directions: ['to_person', 'group']
    },
    ...overrides
  };
}

describe('PersonMediaGallery', () => {
  it('renders source, date, exact relationship metadata and opens the existing selection path', async () => {
    const onOpen = vi.fn();
    render(PersonMediaGallery, {
      client: createAPIClient(vi.fn<typeof fetch>()), rows: [media()], onOpen
    });

    expect(screen.getByText('archive@example.com')).toBeDefined();
    expect(screen.getByText('To them (to, cc) · Group conversation')).toBeDefined();
    expect(screen.getByText('Summer thread')).toBeDefined();
    expect(screen.getByText(/Jul 18, 2026/)).toBeDefined();
    await fireEvent.click(screen.getByRole('button', { name: 'Open photo.png' }));
    expect(onOpen).toHaveBeenCalledWith(expect.objectContaining({ key: 'file:7' }), expect.any(HTMLElement));
  });

  it('offers explicit bounded pagination instead of eager loading every page', async () => {
    const onLoadMore = vi.fn();
    render(PersonMediaGallery, {
      client: createAPIClient(vi.fn<typeof fetch>()), rows: [media()],
      hasMore: true, onLoadMore
    });

    await fireEvent.click(screen.getByRole('button', { name: 'Load more media' }));
    expect(onLoadMore).toHaveBeenCalledOnce();
  });

  it('separates retryable page errors from terminal cursor reloads', async () => {
    const onLoadMore = vi.fn();
    const onReload = vi.fn();
    const view = render(PersonMediaGallery, {
      client: createAPIClient(vi.fn<typeof fetch>()), rows: [media()],
      hasMore: true, pageError: 'Temporary page failure.', onLoadMore, onReload
    });

    expect(screen.getByRole('alert').textContent).toContain('Temporary page failure.');
    expect(screen.queryByRole('button', { name: 'Load more media' })).toBeNull();
    await fireEvent.click(screen.getByRole('button', { name: 'Retry loading more media' }));
    expect(onLoadMore).toHaveBeenCalledOnce();
    expect(onReload).not.toHaveBeenCalled();

    await view.rerender({ pageError: '', error: 'Results changed under this cursor.', hasMore: false });
    expect(screen.getByRole('alert').textContent).toContain('Results changed under this cursor.');
    await fireEvent.click(screen.getByRole('button', { name: 'Reload media' }));
    expect(onReload).toHaveBeenCalledOnce();
  });

  it('caps simultaneous visible thumbnail requests at four', async () => {
    const resolvers: Array<(response: Response) => void> = [];
    const fetchFn = vi.fn<typeof fetch>(() => new Promise<Response>((resolve) => resolvers.push(resolve)));
    Object.defineProperty(URL, 'createObjectURL', {
      configurable: true, value: vi.fn((_: Blob) => `blob:queued-${Math.random()}`)
    });
    Object.defineProperty(URL, 'revokeObjectURL', { configurable: true, value: vi.fn() });
    const rows = Array.from({ length: 6 }, (_, index) => media({
      id: index + 1, key: `file:${index + 1}`, filename: `photo-${index + 1}.png`,
      content_state: 'local_content', content_available: true
    }));
    render(PersonMediaGallery, { client: createAPIClient(fetchFn), rows });

    await waitFor(() => expect(fetchFn).toHaveBeenCalledTimes(4));
    expect(resolvers).toHaveLength(4);
    for (const resolve of resolvers.slice(0, 4)) resolve(new Response(
      BOUNDED_PNG.buffer as ArrayBuffer,
      { headers: { 'Content-Type': 'image/png' } }
    ));
    await waitFor(() => expect(fetchFn).toHaveBeenCalledTimes(6));
    for (const resolve of resolvers.slice(4)) resolve(new Response(
      BOUNDED_PNG.buffer as ArrayBuffer,
      { headers: { 'Content-Type': 'image/png' } }
    ));
    await waitFor(() => expect(screen.getAllByRole('img')).toHaveLength(6));
  });
});
