import { fireEvent, render, screen } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';

import type { DomainSummary, PersonSummary } from '../../explore/models';
import type { RelationshipRow } from '../../relationships/models';
import RelationshipList from './RelationshipList.svelte';

const when = '2026-07-19T10:00:00Z';

function relationshipRow(id: number, label: string, overrides: Partial<RelationshipRow> = {}): RelationshipRow {
  return {
    canonical_id: id,
    display_label: label,
    last_at: when,
    member_ids: [id],
    score: id,
    signals: {
      last_interaction_at: when,
      meeting_count: 0,
      meetings_together: 0,
      modalities: id,
      received_from_them: 1,
      sent_count: id * 3,
      sent_to_them: 1
    },
    ...overrides
  };
}

function person(id: number, label: string): PersonSummary {
  return {
    id, display_label: label, partial_label: false, identifiers: [],
    activity_count: 4, file_count: 0, source_counts: [], first_at: when, last_at: when, cache_revision: 'cache-rel'
  };
}

function domain(name: string): DomainSummary {
  return {
    domain: name, activity_count: 9, file_count: 1, person_count: 2,
    first_at: when, last_at: when, source_counts: [], cache_revision: 'cache-rel'
  };
}

function baseProps() {
  return {
    rows: [relationshipRow(1, 'Alice Example'), relationshipRow(2, 'Bob Example')],
    loading: false,
    error: null,
    degraded: null,
    facet: 'people' as const,
    query: '',
    showAll: false,
    onQueryChange: vi.fn(),
    onFacetChange: vi.fn(),
    onShowAllChange: vi.fn(),
    onSelect: vi.fn()
  };
}

describe('RelationshipList', () => {
  it('renders ranked rows as name, compact date, and one quiet signal summary — no badges or bars', () => {
    render(RelationshipList, baseProps());

    const grid = screen.getByRole('grid', { name: 'Relationship results' });
    expect(screen.getByText('Alice Example')).toBeDefined();
    expect(screen.getByText('Bob Example')).toBeDefined();
    expect(screen.getByText('3 sent')).toBeDefined();
    expect(screen.getByText('6 sent')).toBeDefined();
    expect(screen.queryByLabelText(/modalities/)).toBeNull();
    expect(grid.querySelector('.activity-bar')).toBeNull();
    expect(grid.querySelector('.modality-badge')).toBeNull();
  });

  it('joins only nonzero signals into the summary line', () => {
    const rows = [
      relationshipRow(1, 'Busy Person', {
        signals: {
          last_interaction_at: when, meeting_count: 12, meetings_together: 3, modalities: 2,
          received_from_them: 4, sent_count: 651, sent_to_them: 5
        }
      }),
      relationshipRow(2, 'Quiet Person', {
        signals: {
          last_interaction_at: when, meeting_count: 0, meetings_together: 0, modalities: 1,
          received_from_them: 1, sent_count: 0, sent_to_them: 0
        }
      })
    ];
    render(RelationshipList, { ...baseProps(), rows });

    expect(screen.getByText('651 sent · 12 meetings')).toBeDefined();
    const quietRow = screen.getByText('Quiet Person').closest('[role="row"]')!;
    expect(quietRow.querySelector('.row-summary')?.textContent).toBe('');
  });

  it('renders the last interaction as a compact relative date', () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      vi.setSystemTime(new Date('2026-07-21T10:00:00Z'));
      render(RelationshipList, baseProps());

      // when = 2026-07-19T10:00:00Z, exactly two days before the fake now.
      expect(screen.getAllByText('2d')).toHaveLength(2);
    } finally {
      vi.useRealTimers();
    }
  });

  it('fires onFacetChange when the People/Domains toggle changes', async () => {
    const props = baseProps();
    render(RelationshipList, props);

    await fireEvent.click(screen.getByRole('radio', { name: 'Domains' }));
    expect(props.onFacetChange).toHaveBeenCalledWith('domains');
  });

  it('fires onShowAllChange from the all-senders filter chip', async () => {
    const props = baseProps();
    render(RelationshipList, props);

    await fireEvent.click(screen.getByRole('button', { name: 'All senders' }));
    expect(props.onShowAllChange).toHaveBeenCalledWith(true);
  });

  it('marks the all-senders chip pressed when showAll is on, and clicking it turns it off', async () => {
    const props = { ...baseProps(), showAll: true };
    render(RelationshipList, props);

    const chip = screen.getByRole('button', { name: 'All senders' });
    expect(chip.getAttribute('aria-pressed')).toBe('true');
    await fireEvent.click(chip);
    expect(props.onShowAllChange).toHaveBeenCalledWith(false);
  });

  it('shows the all-senders chip under the people facet', () => {
    render(RelationshipList, baseProps());
    expect(screen.getByRole('button', { name: 'All senders' })).toBeDefined();
  });

  it('hides the all-senders chip under the domains facet', () => {
    render(RelationshipList, { ...baseProps(), facet: 'domains', rows: [domain('example.com')] });
    expect(screen.queryByRole('button', { name: 'All senders' })).toBeNull();
  });

  it('switches from ranked rows to search results as the query and rows props change', async () => {
    const props = baseProps();
    const view = render(RelationshipList, props);

    await fireEvent.input(screen.getByRole('searchbox', { name: 'Search people and domains' }), { target: { value: 'ali' } });
    expect(props.onQueryChange).toHaveBeenCalledWith('ali');

    await view.rerender({ ...props, query: 'ali', rows: [person(3, 'Alicia Searched')] });
    expect(screen.getByText('Alicia Searched')).toBeDefined();
    expect(screen.getByText('4 items')).toBeDefined();
    expect(screen.queryByText('Alice Example')).toBeNull();
  });

  it('renders domain rows by domain name and activity count', () => {
    render(RelationshipList, { ...baseProps(), facet: 'domains', rows: [domain('example.com')] });
    expect(screen.getByText('example.com')).toBeDefined();
    expect(screen.getByText('9 items')).toBeDefined();
  });

  it('moves the active row with j/k and opens it with Enter', async () => {
    const props = baseProps();
    render(RelationshipList, props);
    const grid = screen.getByRole('grid', { name: 'Relationship results' });
    grid.focus();

    await fireEvent.keyDown(grid, { key: 'j' });
    await fireEvent.keyDown(grid, { key: 'Enter' });
    expect(props.onSelect).toHaveBeenCalledWith('cluster:2');

    await fireEvent.keyDown(grid, { key: 'k' });
    await fireEvent.keyDown(grid, { key: 'Enter' });
    expect(props.onSelect).toHaveBeenLastCalledWith('cluster:1');
  });

  it('selects a row by clicking it', async () => {
    const props = baseProps();
    render(RelationshipList, props);

    await fireEvent.pointerDown(screen.getByText('Bob Example').closest('[role="row"]')!);
    expect(props.onSelect).toHaveBeenCalledWith('cluster:2');
  });

  it('requests the next page when scroll nears the bottom while more pages exist', async () => {
    const onLoadMore = vi.fn();
    render(RelationshipList, { ...baseProps(), hasMore: true, onLoadMore });

    await fireEvent.scroll(screen.getByRole('grid', { name: 'Relationship results' }));
    expect(onLoadMore).toHaveBeenCalled();
  });

  it('does not request more while a page is loading, showing the inline loading line instead', async () => {
    const onLoadMore = vi.fn();
    render(RelationshipList, { ...baseProps(), hasMore: true, loadingMore: true, onLoadMore });

    await fireEvent.scroll(screen.getByRole('grid', { name: 'Relationship results' }));
    expect(onLoadMore).not.toHaveBeenCalled();
    expect(screen.getByText('Loading more…')).toBeDefined();
  });

  it('does not request more once the last page has been reached', async () => {
    const onLoadMore = vi.fn();
    render(RelationshipList, { ...baseProps(), hasMore: false, onLoadMore });

    await fireEvent.scroll(screen.getByRole('grid', { name: 'Relationship results' }));
    expect(onLoadMore).not.toHaveBeenCalled();
  });

  it('keeps the active row when appended pages grow the list', async () => {
    const props = { ...baseProps(), hasMore: true };
    const view = render(RelationshipList, props);
    const grid = screen.getByRole('grid', { name: 'Relationship results' });
    grid.focus();
    await fireEvent.keyDown(grid, { key: 'j' });

    await view.rerender({
      ...props,
      rows: [relationshipRow(1, 'Alice Example'), relationshipRow(2, 'Bob Example'), relationshipRow(3, 'Cara Example')]
    });

    const activeRow = screen.getByText('Bob Example').closest('[role="row"]')!;
    expect(activeRow.classList.contains('active')).toBe(true);
    expect(screen.getByText('Cara Example')).toBeDefined();
  });

  it('announces the full result count on the grid when the total is known', () => {
    render(RelationshipList, { ...baseProps(), totalCount: 1204 });

    const grid = screen.getByRole('grid', { name: 'Relationship results' });
    expect(grid.getAttribute('aria-rowcount')).toBe('1204');
  });

  it('keeps loaded rows visible under a slim banner when a later page fails', () => {
    render(RelationshipList, { ...baseProps(), error: 'boom' });

    expect(screen.getByRole('alert').textContent).toBe('boom');
    expect(screen.getByText('Alice Example')).toBeDefined();
  });

  it('still replaces the list with the error state when nothing loaded', () => {
    render(RelationshipList, { ...baseProps(), rows: [], error: 'boom' });

    expect(screen.getByRole('alert').textContent).toBe('boom');
    expect(screen.queryByRole('grid')).toBeNull();
  });

  it('renders the named degraded state with an Open Everything action instead of the grid', async () => {
    const onOpenEverything = vi.fn();
    render(RelationshipList, { ...baseProps(), degraded: 'cache_unavailable', onOpenEverything });

    expect(screen.getByText('Relationship ranking needs the analytical cache/engine')).toBeDefined();
    expect(screen.queryByRole('grid')).toBeNull();
    await fireEvent.click(screen.getByRole('button', { name: 'Open Everything' }));
    expect(onOpenEverything).toHaveBeenCalledOnce();
  });
});
