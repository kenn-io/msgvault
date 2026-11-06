import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
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

  it.each([
    [50000, [], 'All 50,000 selected', 'All 50,000 matching items selected'],
    [50, ['message:1'], 'All 50 selected, except 1', 'All 50 matching items selected, except 1'],
    [undefined, [], 'All matching selected', 'All matching items selected'],
  ] as const)('labels predicate selection with total %s and exclusions %s', (totalCount, exclusions, label, announced) => {
    const selection = new ExploreSelectionState();
    selection.selectAllMatching({
      mode: 'all_matching',
      predicate: { query: 'synthetic', search_mode: 'full_text' },
      exclusions: [...exclusions],
      cacheRevision: 'cache-1',
      searchProvenance: { lexical_index_revision: 'fts-1' },
      predicateFingerprint: predicateFingerprint({ query: 'synthetic', search_mode: 'full_text' }),
      resultGeneration: 1,
    });
    render(SelectionBar, { selection, totalCount });

    expect(screen.getByRole('status').textContent).toBe(announced);
    expect(screen.getByText(label, { selector: '.selection-count' })).toBeDefined();
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

    await fireEvent.click(screen.getByRole('button', { name: 'Select all 50 matching' }));

    expect(selection.mode).toBe('all_matching');
    expect(selection.snapshot()).toMatchObject({ cacheRevision: 'cache-1' });
  });

  it('shows count and estimated size together while announcing only the count', () => {
    const selection = new ExploreSelectionState();
    selection.selectVisible(['message:1', 'message:2']);
    render(SelectionBar, { selection, preflight: { ...preflight(), estimated_bytes: 128 * 1024 * 1024 } });

    expect(screen.getByText('2 selected', { selector: '.selection-count' }).textContent).toBe('2 selected · 128.0 MB');
    expect(screen.getByRole('status').textContent).toBe('2 selected');
  });

  it('explains disabled actions on hover and keyboard focus', async () => {
    const selection = new ExploreSelectionState();
    selection.selectVisible(['message:1', 'message:2']);
    render(SelectionBar, {
      selection, onExport: vi.fn(), onReviewDeletion: vi.fn(),
      preflight: preflight([
        { action: 'export', reason: 'browser_export_requires_single_message' },
        { action: 'stage_deletion', reason: 'selection_contains_items_that_cannot_be_deleted_from_source' },
      ]),
    });
    for (const [name, reason, trigger] of [
      ['Export selection', 'Export works for one message at a time.', fireEvent.mouseEnter],
      ['Review deletion…', 'None of the selected items can be deleted from their source.', fireEvent.focusIn],
    ] as const) {
      const control = screen.getByRole('button', { name }) as HTMLButtonElement;
      expect(control.disabled).toBe(true);
      expect(screen.queryByText(reason)).toBeNull();
      const wrapper = control.closest('.kit-tooltip-trigger')!;
      await trigger(wrapper);
      expect((await screen.findByRole('tooltip')).textContent).toBe(reason);
    }
  });

  it('runs export and deletion review for the selected items', async () => {
    const selection = new ExploreSelectionState();
    selection.selectVisible(['message:1', 'message:2']);
    const onExport = vi.fn();
    const onReviewDeletion = vi.fn();
    render(SelectionBar, { selection, preflight: preflight(), onExport, onReviewDeletion });

    await fireEvent.click(screen.getByRole('button', { name: 'Export selection' }));
    expect(onExport).toHaveBeenCalledOnce();
    await fireEvent.click(screen.getByRole('button', { name: 'Review deletion…' }));
    expect(onReviewDeletion).toHaveBeenCalledExactlyOnceWith('explicit');
  });

  it('renders nothing while nothing is selected', () => {
    const selection = new ExploreSelectionState();
    render(SelectionBar, { selection, totalCount: 8 });

    expect(screen.getByRole('status').textContent).toBe('');
    expect(screen.queryByRole('button', { name: 'Clear selection' })).toBeNull();
    expect(screen.queryByText('No items selected')).toBeNull();
    expect(screen.queryByRole('button', { name: 'Review deletion…' })).toBeNull();
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
    render(SelectionBar, { selection, totalCount: 50, allMatching, onReviewDeletion, preflight: preflight() });

    expect(screen.getByRole('status').textContent).toContain('3 selected');
    expect((screen.getByRole('button', { name: 'Review deletion…' }) as HTMLButtonElement).disabled).toBe(false);
    await fireEvent.click(screen.getByRole('button', { name: 'Review deletion…' }));
    expect(onReviewDeletion).toHaveBeenLastCalledWith('explicit');

    await fireEvent.click(screen.getByRole('button', { name: 'Select all 50 matching' }));
    await fireEvent.click(screen.getByRole('button', { name: 'Review deletion…' }));
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
      const view = render(SelectionBar, {
        selection,
        totalCount: 2,
        client: createAPIClient(vi.fn<typeof fetch>()),
        meetingSelection,
        canExportMeetings,
      });
      return { selection, view };
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

    it('claims Escape and returns focus to the meeting button', async () => {
      const selection = new ExploreSelectionState();
      selection.selectVisible(['message:1']);
      let request: Request | undefined;
      render(SelectionBar, {
        selection, meetingSelection, canExportMeetings: true,
        client: createAPIClient(input => {
          request = input as Request;
          return new Promise<Response>(() => {});
        }),
      });
      const trigger = screen.getByRole('button', { name: 'Meeting context…' });
      await fireEvent.click(trigger);
      await fireEvent.click(screen.getByRole('button', { name: 'Export meeting context' }));
      await waitFor(() => expect(request).toBeDefined());
      const escape = new KeyboardEvent('keydown', { key: 'Escape', bubbles: true, cancelable: true });
      document.dispatchEvent(escape);
      await tick();
      expect(escape.defaultPrevented).toBe(true);
      expect(screen.queryByRole('dialog', { name: 'Meeting context' })).toBeNull();
      expect(document.activeElement).toBe(trigger);
      expect(request!.signal.aborted).toBe(true);
    });

    it('hides on focus leaving', async () => {
      renderBar(true);
      const trigger = screen.getByRole('button', { name: 'Meeting context…' });
      trigger.focus();
      await fireEvent.click(trigger);
      const outside = document.createElement('button');
      document.body.append(outside);
      try {
        outside.focus();
        await tick();
        expect(screen.queryByRole('dialog', { name: 'Meeting context' })).toBeNull();
      } finally { outside.remove(); }
    });

    it('closes the meeting context controls when the bar hides or eligibility changes', async () => {
      const { selection, view } = renderBar(true);
      for (const cause of ['clear', 'ineligible']) {
        await fireEvent.click(screen.getByRole('button', { name: 'Meeting context…' }));
        if (cause === 'clear') {
          await fireEvent.click(screen.getByRole('button', { name: 'Clear selection' }));
        } else {
          await view.rerender({ canExportMeetings: false });
        }
        expect(screen.queryByRole('dialog', { name: 'Meeting context' })).toBeNull();
        const escape = new KeyboardEvent('keydown', { key: 'Escape', bubbles: true, cancelable: true });
        document.dispatchEvent(escape);
        expect(escape.defaultPrevented).toBe(false);

        selection.selectVisible(['message:1']);
        await view.rerender({ canExportMeetings: true });
        expect(screen.getByRole('button', { name: 'Meeting context…' }).getAttribute('aria-expanded')).toBe('false');
        expect(screen.queryByRole('button', { name: 'Export meeting context' })).toBeNull();
      }
    });
  });
});
