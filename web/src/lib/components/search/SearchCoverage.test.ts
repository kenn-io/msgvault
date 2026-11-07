import { fireEvent, render, screen } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';

import SearchCoverage from './SearchCoverage.svelte';
import type { SearchCoverageValue } from '../../search/modes';

const base: SearchCoverageValue = {
  eligible_count: 12_430, embedded_count: 10_441, percentage: 84,
  cache_revision: 'cache-1', actions: [], status: 'incomplete'
};

describe('SearchCoverage', () => {
  it.each([
    ['disabled', 'Semantic search is disabled'],
    ['initializing', 'Semantic index is initializing'],
    ['stale', 'Semantic index is stale'],
    ['incomplete', 'Semantic index: 84% of 12,430 items'],
    ['unavailable', 'Semantic index is unavailable'],
    ['ready', 'Semantic index: 100% of 12,430 items']
  ] as const)('renders honest %s copy', (status, copy) => {
    render(SearchCoverage, {
      props: {
        coverage: {
          ...base,
          status,
          embedded_count: status === 'ready' ? 12_430 : base.embedded_count,
          percentage: status === 'ready' ? 100 : base.percentage
        }
      }
    });
    expect(screen.getByRole('status').textContent).toContain(copy);
  });

  it('states that semantic-only results exclude unembedded content', () => {
    render(SearchCoverage, { props: { requestedMode: 'semantic', coverage: { ...base, status: 'incomplete' } } });
    expect(screen.getByRole('status').textContent).toContain('Unembedded items cannot appear');
  });

  it('offers only daemon-supported actions', async () => {
    const onaction = vi.fn();
    render(SearchCoverage, {
      props: { coverage: { ...base, status: 'unavailable', actions: ['retry'] }, onaction }
    });

    expect(screen.queryByRole('button', { name: 'Build index' })).toBeNull();
    await fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
    expect(onaction).toHaveBeenCalledWith('retry');
  });

  it('requires explicit confirmation before requesting a full rebuild', async () => {
    const onaction = vi.fn();
    render(SearchCoverage, {
      props: { coverage: { ...base, status: 'stale', actions: ['build_index'] }, onaction }
    });

    await fireEvent.click(screen.getByRole('button', { name: 'Build index' }));
    expect(onaction).not.toHaveBeenCalled();
    expect(screen.getByText('Start a full rebuild of the semantic index?')).toBeDefined();

    await fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));
    expect(onaction).not.toHaveBeenCalled();
    expect(screen.queryByRole('button', { name: 'Confirm full rebuild' })).toBeNull();

    await fireEvent.click(screen.getByRole('button', { name: 'Build index' }));
    await fireEvent.click(screen.getByRole('button', { name: 'Confirm full rebuild' }));
    expect(onaction).toHaveBeenCalledOnce();
    expect(onaction).toHaveBeenCalledWith('build_index');
  });
});
