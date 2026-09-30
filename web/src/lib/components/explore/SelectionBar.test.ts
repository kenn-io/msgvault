import { fireEvent, render, screen } from '@testing-library/svelte';
import { tick } from 'svelte';
import { describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../../api/client';
import type {
  ExplorePreflightResponse as GeneratedExplorePreflightResponse,
  ExploreSelection as GeneratedExploreSelection,
  ExploreUnavailableAction as GeneratedExploreUnavailableAction,
} from '../../api/generated/models';
import { ExploreSelectionState } from '../../explore/state.svelte';
import { predicateFingerprint } from '../../explore/selection';
import SelectionBar from './SelectionBar.svelte';

describe('SelectionBar', () => {
  const preflight = (
    unavailable_actions: GeneratedExploreUnavailableAction[] = [],
  ): GeneratedExplorePreflightResponse => ({
    count: 2,
    deletable_count: 2,
    estimated_bytes: 20,
    cache_revision: 'cache-1',
    search_provenance: {},
    unavailable_actions,
    action_targets: [{ action: 'export', message_id: 1, filename: 'message-1.eml' }],
    operation_token: 'token-1',
    expires_at: '2026-07-19T10:00:00Z',
  });

  it('announces explicit selection and clears it', async () => {
    const selection = new ExploreSelectionState();
    selection.selectVisible(['message:1', 'message:2']);
    render(SelectionBar, { selection, totalCount: 8 });

    expect(screen.getByRole('status').textContent).toContain('2 selected');
    await fireEvent.click(screen.getByRole('button', { name: 'Clear selection' }));
    expect(selection.count).toBe(0);
  });

  it('announces the first selection through a live region that exists before it', async () => {
    const selection = new ExploreSelectionState();
    render(SelectionBar, { selection, totalCount: 8 });
    const status = screen.getByRole('status');
    expect(status.textContent).toBe('');

    selection.selectVisible(['message:1']);
    await tick();

    expect(screen.getByRole('status')).toBe(status);
    expect(status.textContent).toBe('1 selected');
  });

  it('hands focus back after Clear selection removes the bar', async () => {
    const selection = new ExploreSelectionState();
    selection.selectVisible(['message:1']);
    const onClear = vi.fn(() => expect(selection.count).toBe(0));
    render(SelectionBar, { selection, totalCount: 8, onClear });

    await fireEvent.click(screen.getByRole('button', { name: 'Clear selection' }));

    expect(onClear).toHaveBeenCalledOnce();
    expect(screen.queryByRole('button', { name: 'Clear selection' })).toBeNull();
    expect(screen.getByRole('status').textContent).toBe('');
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
      resultGeneration: 1,
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
        resultGeneration: 1,
      },
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
        { action: 'open_in_source', reason: 'selection_contains_items_that_cannot_be_opened_in_source' },
      ]),
    });

    expect(screen.queryByRole('button', { name: 'Export selection' })).toBeNull();
    expect(screen.getByText('Export unavailable: Selection contains items without exportable files.')).toBeDefined();
  });

  it('lists a disabled open-in-source item with a plain reason and no raw code', async () => {
    const selection = new ExploreSelectionState();
    selection.selectVisible(['message:1']);
    render(SelectionBar, {
      selection,
      totalCount: 2,
      preflight: preflight([{ action: 'open_in_source', reason: 'trusted_source_link_unavailable' }]),
      onOpenInSource: () => undefined,
    });

    await fireEvent.click(screen.getByRole('button', { name: 'More selection actions' }));

    const item = screen.getByRole('menuitem', {
      name: 'Open selection in source Your sources don’t provide links to open these items.'
    });
    expect(item.getAttribute('aria-disabled')).toBe('true');
    expect(screen.queryByText(/trusted_source_link_unavailable/)).toBeNull();
  });

  it('opens the selection in source from the overflow menu when preflight allows it', async () => {
    const selection = new ExploreSelectionState();
    selection.selectVisible(['message:1']);
    const onOpenInSource = vi.fn();
    render(SelectionBar, { selection, totalCount: 2, preflight: preflight(), onOpenInSource });

    await fireEvent.click(screen.getByRole('button', { name: 'More selection actions' }));
    await fireEvent.click(screen.getByRole('menuitem', { name: 'Open selection in source' }));

    expect(onOpenInSource).toHaveBeenCalledOnce();
  });

  it('renders nothing while nothing is selected', () => {
    const selection = new ExploreSelectionState();
    render(SelectionBar, { selection, totalCount: 8 });

    expect(screen.getByRole('status').textContent).toBe('');
    expect(screen.queryByRole('button', { name: 'Clear selection' })).toBeNull();
    expect(screen.queryByText('No items selected')).toBeNull();
    expect(screen.queryByRole('button', { name: 'Review for deletion…' })).toBeNull();
  });

  it('offers deletion review for an explicit and then an all-matching selection', async () => {
    const selection = new ExploreSelectionState();
    selection.selectVisible(['message:1', 'message:2', 'message:3']);
    const onReviewDeletion = vi.fn();
    const allMatching = {
      mode: 'all_matching' as const,
      predicate: { query: 'synthetic', search_mode: 'full_text' as const },
      exclusions: [],
      cacheRevision: 'cache-1',
      searchProvenance: { lexical_index_revision: 'fts-1' },
      predicateFingerprint: predicateFingerprint({ query: 'synthetic', search_mode: 'full_text' }),
      resultGeneration: 1,
    };
    render(SelectionBar, { selection, totalCount: 50, allMatching, onReviewDeletion });

    expect(screen.getByRole('status').textContent).toContain('3 selected');
    await fireEvent.click(screen.getByRole('button', { name: 'Review for deletion…' }));
    expect(onReviewDeletion).toHaveBeenLastCalledWith('explicit');

    await fireEvent.click(screen.getByRole('button', { name: 'Select all 50 matching items' }));
    await fireEvent.click(screen.getByRole('button', { name: 'Review for deletion…' }));
    expect(onReviewDeletion).toHaveBeenLastCalledWith('all_matching');
  });

  it('keeps meeting context independent from raw-export preflight eligibility', async () => {
    const selection = new ExploreSelectionState();
    selection.selectVisible(['message:7', 'message:91']);
    const meetingSelection: GeneratedExploreSelection = {
      mode: 'explicit',
      predicate: { filters: [], presentation: 'table' },
      row_keys: ['message:7', 'message:91'],
      cache_revision: 'cache-1',
      search_provenance: {},
    };
    render(SelectionBar, {
      selection,
      totalCount: 2,
      preflight: preflight([
        { action: 'export', reason: 'selection_contains_items_without_exportable_files' },
      ]),
      client: createAPIClient(async () =>
        Response.json(
          { error: 'selection_not_all_meetings', message: 'Every selected row must be a meeting' },
          { status: 400 },
        ),
      ),
      meetingSelection,
      canExportMeetings: true,
    });

    expect(screen.getByText('Export unavailable: Selection contains items without exportable files.')).toBeDefined();
    await fireEvent.click(screen.getByRole('button', { name: 'Meeting context…' }));
    await fireEvent.click(screen.getByRole('button', { name: 'Export meeting context' }));
    expect((await screen.findByRole('alert')).textContent).toContain('Select meetings only');
  });

  it('explains an explicit selection over the 100-meeting context limit before request', async () => {
    const rowKeys = Array.from({ length: 101 }, (_, index) => `message:${index + 1}`);
    const selection = new ExploreSelectionState();
    selection.selectVisible(rowKeys);
    const fetchFn = vi.fn<typeof fetch>();
    render(SelectionBar, {
      selection,
      totalCount: rowKeys.length,
      client: createAPIClient(fetchFn),
      canExportMeetings: true,
      meetingSelection: {
        mode: 'explicit',
        predicate: { filters: [], presentation: 'table' },
        row_keys: rowKeys,
        cache_revision: 'cache-1',
        search_provenance: {},
      } satisfies GeneratedExploreSelection,
    });

    await fireEvent.click(screen.getByRole('button', { name: 'Meeting context…' }));
    expect(screen.getByText('Meeting context accepts at most 100 meetings.')).toBeDefined();
    expect((screen.getByRole('button', { name: 'Export meeting context' }) as HTMLButtonElement).disabled).toBe(true);
    expect(fetchFn).not.toHaveBeenCalled();
  });

  describe('meeting context', () => {
    const meetingSelection: GeneratedExploreSelection = {
      mode: 'explicit',
      predicate: { filters: [], presentation: 'table' },
      row_keys: ['message:1'],
      cache_revision: 'cache-1',
      search_provenance: {},
    };
    const renderBar = (canExportMeetings?: boolean) => {
      const selection = new ExploreSelectionState();
      selection.selectVisible(['message:1']);
      render(SelectionBar, {
        selection,
        totalCount: 2,
        client: createAPIClient(vi.fn<typeof fetch>()),
        meetingSelection,
        canExportMeetings,
      });
      return selection;
    };

    it('hides meeting export for selections that cannot contain meetings', () => {
      renderBar(false);

      expect(screen.queryByRole('button', { name: 'Meeting context…' })).toBeNull();
      expect(screen.queryByRole('button', { name: 'Export meeting context' })).toBeNull();
    });

    it('reveals the export controls from a disclosure button for meeting selections', async () => {
      renderBar(true);

      expect(screen.queryByRole('button', { name: 'Export meeting context' })).toBeNull();
      const toggle = screen.getByRole('button', { name: 'Meeting context…' });
      expect(toggle.getAttribute('aria-expanded')).toBe('false');
      await fireEvent.click(toggle);

      expect(toggle.getAttribute('aria-expanded')).toBe('true');
      expect(screen.getByRole('button', { name: 'Export meeting context' })).toBeDefined();
    });

    it('closes the meeting context controls when the bar hides', async () => {
      const selection = renderBar(true);
      await fireEvent.click(screen.getByRole('button', { name: 'Meeting context…' }));
      await fireEvent.click(screen.getByRole('button', { name: 'Clear selection' }));

      selection.selectVisible(['message:1']);
      await tick();

      expect(screen.getByRole('button', { name: 'Meeting context…' }).getAttribute('aria-expanded')).toBe('false');
      expect(screen.queryByRole('button', { name: 'Export meeting context' })).toBeNull();
    });
  });
});
