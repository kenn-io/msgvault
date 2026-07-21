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
  it('renders ranked rows with label, modality badge, last-interaction date, and a score-proportional bar', () => {
    render(RelationshipList, baseProps());

    const grid = screen.getByRole('grid', { name: 'Relationship results' });
    expect(screen.getByText('Alice Example')).toBeDefined();
    expect(screen.getByText('Bob Example')).toBeDefined();
    expect(screen.getByLabelText('1 modalities')).toBeDefined();
    expect(screen.getByLabelText('2 modalities')).toBeDefined();
    expect(screen.getByText('3 sent')).toBeDefined();
    expect(screen.getByText('6 sent')).toBeDefined();
    const bars = grid.querySelectorAll<HTMLElement>('.activity-bar');
    expect(bars).toHaveLength(2);
    expect(bars[0]!.style.width).toBe('50%');
    expect(bars[1]!.style.width).toBe('100%');
  });

  it('fires onFacetChange when the People/Domains toggle changes', async () => {
    const props = baseProps();
    render(RelationshipList, props);

    await fireEvent.click(screen.getByRole('radio', { name: 'Domains' }));
    expect(props.onFacetChange).toHaveBeenCalledWith('domains');
  });

  it('fires onShowAllChange from the show-all checkbox', async () => {
    const props = baseProps();
    render(RelationshipList, props);

    await fireEvent.click(screen.getByRole('checkbox', { name: 'Show all senders' }));
    expect(props.onShowAllChange).toHaveBeenCalledWith(true);
  });

  it('shows the show-all checkbox under the people facet', () => {
    render(RelationshipList, baseProps());
    expect(screen.getByRole('checkbox', { name: 'Show all senders' })).toBeDefined();
  });

  it('hides the show-all checkbox under the domains facet', () => {
    render(RelationshipList, { ...baseProps(), facet: 'domains', rows: [domain('example.com')] });
    expect(screen.queryByRole('checkbox', { name: 'Show all senders' })).toBeNull();
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

  it('renders the named degraded state with an Open Everything action instead of the grid', async () => {
    const onOpenEverything = vi.fn();
    render(RelationshipList, { ...baseProps(), degraded: 'cache_unavailable', onOpenEverything });

    expect(screen.getByText('Relationship ranking needs the analytical cache/engine')).toBeDefined();
    expect(screen.queryByRole('grid')).toBeNull();
    await fireEvent.click(screen.getByRole('button', { name: 'Open Everything' }));
    expect(onOpenEverything).toHaveBeenCalledOnce();
  });
});
