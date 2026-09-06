import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';

import DirectoryList from './DirectoryList.svelte';

const rows = [
  { id: 1, revision: 1, display_name: 'Alpha Fixture', contact_state: 'active', categories: [], organizations: [] },
  { id: 2, revision: 1, display_name: 'Bravo Fixture', contact_state: 'active', categories: [], organizations: [] },
  { id: 3, revision: 1, display_name: 'Charlie Fixture', contact_state: 'inactive', categories: [], organizations: [] }
];

describe('DirectoryList', () => {
  it('uses roving row focus for arrows, Home/End, and Enter/Space selection', async () => {
    const onSelect = vi.fn();
    render(DirectoryList, {
      rows, loading: false, loadingMore: false, error: null, pageError: null, pageRecovery: null,
      hasMore: false, selectedPersonID: null, onSelect, onLoadMore: vi.fn(), onReload: vi.fn()
    });

    const alpha = screen.getByRole('row', { name: /Alpha Fixture/ });
    const bravo = screen.getByRole('row', { name: /Bravo Fixture/ });
    const charlie = screen.getByRole('row', { name: /Charlie Fixture/ });
    await waitFor(() => expect(alpha.getAttribute('tabindex')).toBe('0'));
    alpha.focus();
    await fireEvent.keyDown(alpha, { key: 'ArrowDown' });
    expect(document.activeElement).toBe(bravo);
    expect(bravo.getAttribute('tabindex')).toBe('0');
    await fireEvent.keyDown(bravo, { key: 'End' });
    expect(document.activeElement).toBe(charlie);
    await fireEvent.keyDown(charlie, { key: 'Home' });
    expect(document.activeElement).toBe(alpha);
    await fireEvent.keyDown(alpha, { key: ' ' });
    expect(onSelect).toHaveBeenCalledWith(1);
    await fireEvent.keyDown(alpha, { key: 'Enter' });
    expect(onSelect).toHaveBeenCalledTimes(2);
  });

  it('offers Reload without Load more when the retained cursor needs page-one recovery', () => {
    render(DirectoryList, {
      rows, loading: false, loadingMore: false, error: null,
      pageError: 'Directory reconciliation unavailable.', pageRecovery: 'reload',
      hasMore: true, selectedPersonID: 1, onSelect: vi.fn(), onLoadMore: vi.fn(), onReload: vi.fn()
    });

    expect(screen.getByRole('button', { name: 'Reload directory' })).toBeDefined();
    expect(screen.queryByRole('button', { name: 'Load more people' })).toBeNull();
  });

  it('shows the last-contact timestamp or an explicit never-contacted state', () => {
    render(DirectoryList, {
      rows: [
        { ...rows[0]!, last_contact_at: '2026-08-20T10:00:00Z' },
        rows[1]!
      ],
      loading: false, loadingMore: false, error: null, pageError: null, pageRecovery: null,
      hasMore: false, selectedPersonID: null, onSelect: vi.fn(), onLoadMore: vi.fn(), onReload: vi.fn()
    });

    expect(screen.getByRole('row', { name: /Alpha Fixture/ }).textContent).toContain('Last contact 2026-08-20T10:00:00Z');
    expect(screen.getByRole('row', { name: /Bravo Fixture/ }).textContent).toContain('Never contacted');
  });
});
