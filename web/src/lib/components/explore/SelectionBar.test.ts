import { fireEvent, render, screen } from '@testing-library/svelte';
import { describe, expect, it } from 'vitest';

import type { components } from '../../api/generated/schema';

import { ExploreSelectionState } from '../../explore/state.svelte';
import { predicateFingerprint } from '../../explore/selection';
import SelectionBar from './SelectionBar.svelte';

describe('SelectionBar', () => {
	const preflight = (unavailable_actions: components['schemas']['ExploreUnavailableAction'][] = []): components['schemas']['ExplorePreflightResponse'] => ({
		count: 2,
		estimated_bytes: 20,
		cache_revision: 'cache-1',
		search_provenance: {},
		unavailable_actions,
		action_targets: [{ action: 'export', message_id: 1, filename: 'message-1.eml' }],
		operation_token: 'token-1',
		expires_at: '2026-07-19T10:00:00Z'
	});

  it('announces explicit selection and clears it', async () => {
    const selection = new ExploreSelectionState();
    selection.selectVisible(['message:1', 'message:2']);
    render(SelectionBar, { selection, totalCount: 8 });

    expect(screen.getByRole('status').textContent).toContain('2 selected');
    await fireEvent.click(screen.getByRole('button', { name: 'Clear selection' }));
    expect(selection.count).toBe(0);
  });

  it('labels predicate selection as all matching rather than a finite URL selection', () => {
    const selection = new ExploreSelectionState();
    selection.selectAllMatching({
      mode: 'all_matching',
      predicate: { query: 'synthetic', search_mode: 'full_text' },
      exclusions: [],
      cacheRevision: 'cache-1',
      searchProvenance: { lexical_index_revision: 'fts-1' },
      predicateFingerprint: predicateFingerprint({ query: 'synthetic', search_mode: 'full_text' }),
      resultGeneration: 1
    });
    render(SelectionBar, { selection, totalCount: 50000 });

    expect(screen.getByRole('status').textContent).toContain('All 50,000 matching items selected');
  });

  it('promotes visible selection to a pinned all-matching selection', async () => {
    const selection = new ExploreSelectionState();
    selection.selectVisible(['message:1', 'message:2']);
    render(SelectionBar, {
      selection,
      totalCount: 50,
      allMatching: {
        mode: 'all_matching',
        predicate: { query: 'synthetic', search_mode: 'full_text' },
        exclusions: [],
        cacheRevision: 'cache-1',
        searchProvenance: { lexical_index_revision: 'fts-1' },
        predicateFingerprint: predicateFingerprint({ query: 'synthetic', search_mode: 'full_text' }),
        resultGeneration: 1
      }
    });

    await fireEvent.click(screen.getByRole('button', { name: 'Select all 50 matching items' }));

    expect(selection.mode).toBe('all_matching');
    expect(selection.snapshot()).toMatchObject({ cacheRevision: 'cache-1' });
  });

	it('shows executable actions only when preflight supplies a target and a handler', () => {
		const selection = new ExploreSelectionState();
		selection.selectVisible(['message:1', 'message:2']);
		render(SelectionBar, { selection, totalCount: 2, preflight: preflight(), onExport: () => undefined });

		expect(screen.getByRole('button', { name: 'Export selection' })).toBeDefined();
		expect(screen.queryByRole('button', { name: 'Open selection in source' })).toBeNull();
	});

	it('shows preflight-supplied per-action reasons without inferring support from rows', () => {
		const selection = new ExploreSelectionState();
		selection.selectVisible(['message:1', 'message:2']);
		render(SelectionBar, {
			selection,
			totalCount: 2,
			preflight: preflight([
				{ action: 'export', reason: 'selection_contains_items_without_exportable_files' },
				{ action: 'open_in_source', reason: 'selection_contains_items_that_cannot_be_opened_in_source' }
			])
		});

		expect(screen.queryByRole('button', { name: 'Export selection' })).toBeNull();
		expect(screen.queryByRole('button', { name: 'Open selection in source' })).toBeNull();
		expect(screen.getByText('Export: selection_contains_items_without_exportable_files')).toBeDefined();
		expect(screen.getByText('Open in source: selection_contains_items_that_cannot_be_opened_in_source')).toBeDefined();
	});
});
