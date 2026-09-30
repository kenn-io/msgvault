import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../../api/client';
import { defaultExploreURLState } from '../../explore/state.svelte';
import type { ExploreURLState } from '../../explore/models';
import SaveViewDialog from './SaveViewDialog.svelte';

const state: ExploreURLState = {
  ...defaultExploreURLState,
  workspace: 'everything',
  query: ' invoice ',
  searchMode: 'full_text',
  filters: [{ dimension: 'source', values: ['1'] }],
  groupingChain: ['domain'],
  presentation: 'table',
  sort: [{ field: 'occurred_at', direction: 'desc' }],
  columns: ['kind', 'title'],
  inspectorPinned: true
};

function savedView() {
  return {
    id: 7, name: 'Invoices', description: 'Quarterly review', canonical_state: {},
    schema_version: 1, revision: 1,
    created_at: '2026-07-19T10:00:00Z', updated_at: '2026-07-19T10:00:00Z'
  };
}

function renderDialog(fetchFn: typeof fetch, overrides: Partial<ExploreURLState> = {}) {
  const onSaved = vi.fn();
  const onclose = vi.fn();
  render(SaveViewDialog, {
    client: createAPIClient(fetchFn), state: { ...state, ...overrides }, onSaved, onclose
  });
  return { onSaved, onclose };
}

describe('SaveViewDialog', () => {
  it('puts focus in Name when it opens', () => {
    renderDialog(vi.fn<typeof fetch>());
    expect(document.activeElement).toBe(screen.getByLabelText('Name'));
  });

  it('submits once when Enter is pressed in a named Name field', async () => {
    const fetchFn = vi.fn<typeof fetch>(async () => Response.json(savedView(), { status: 201 }));
    const { onSaved } = renderDialog(fetchFn);
    const name = screen.getByLabelText('Name') as HTMLInputElement;
    await fireEvent.input(name, { target: { value: 'Invoices' } });
    // jsdom has no implicit Enter submission; submitting the Name field's form is its effect.
    await fireEvent.submit(name.form!);
    await waitFor(() => expect(onSaved).toHaveBeenCalledOnce());
    expect(fetchFn).toHaveBeenCalledOnce();
  });

  it('keeps Save disabled until a name is typed', async () => {
    renderDialog(vi.fn<typeof fetch>());
    expect(screen.getByRole('dialog', { name: 'Save view' })).toBeDefined();
    const save = screen.getByRole('button', { name: 'Save' }) as HTMLButtonElement;
    expect(save.disabled).toBe(true);
    await fireEvent.input(screen.getByLabelText('Name'), { target: { value: '   ' } });
    expect(save.disabled).toBe(true);
    await fireEvent.input(screen.getByLabelText('Name'), { target: { value: 'Invoices' } });
    expect(save.disabled).toBe(false);
  });

  it.each(['full_text', 'semantic', 'hybrid'] as const)(
    'posts the canonical %s state without session details and reports the saved view',
    async (searchMode) => {
      const requests: Request[] = [];
      const fetchFn = vi.fn<typeof fetch>(async (input) => {
        requests.push(input instanceof Request ? input : new Request(input));
        return Response.json(savedView(), { status: 201 });
      });
      const { onSaved } = renderDialog(fetchFn, { searchMode, activeRow: 'message:9' });

      await fireEvent.input(screen.getByLabelText('Name'), { target: { value: ' Invoices ' } });
      await fireEvent.input(
        screen.getByLabelText('Description'), { target: { value: 'Quarterly review' } }
      );
      await fireEvent.click(screen.getByRole('button', { name: 'Save' }));

      await waitFor(() =>
        expect(onSaved).toHaveBeenCalledWith(expect.objectContaining({ id: 7, name: 'Invoices' }))
      );
      expect(requests[0]!.method).toBe('POST');
      const body = await requests[0]!.clone().json();
      expect(body).toEqual({
        name: 'Invoices', description: 'Quarterly review', schema_version: 1,
        canonical_state: {
          query: 'invoice', search_mode: searchMode,
          filters: [{ field: 'source', operator: 'in', values: ['1'] }],
          grouping: ['domain'], presentation: 'table',
          sort: [{ field: 'occurred_at', direction: 'desc' }],
          columns: ['kind', 'title']
        }
      });
      expect(JSON.stringify(body)).not.toContain('inspector_pinned');
      expect(JSON.stringify(body)).not.toContain('message:9');
    }
  );

  it.each([
    ['semantic', ''], ['semantic', ' \t\n '], ['hybrid', ''], ['hybrid', ' \t\n ']
  ] as const)(
    'saves a filter-only %s view with query %j without a query or mode',
    async (searchMode, query) => {
      const requests: Request[] = [];
      const fetchFn = vi.fn<typeof fetch>(async (input) => {
        requests.push(input instanceof Request ? input : new Request(input));
        return Response.json(savedView(), { status: 201 });
      });
      const { onSaved } = renderDialog(fetchFn, { query, searchMode });
      await fireEvent.input(screen.getByLabelText('Name'), { target: { value: 'Invoices' } });
      await fireEvent.click(screen.getByRole('button', { name: 'Save' }));
      await waitFor(() => expect(onSaved).toHaveBeenCalledOnce());
      const { canonical_state: saved } = await requests[0]!.clone().json();
      expect(saved).not.toHaveProperty('query');
      expect(saved).not.toHaveProperty('search_mode');
      expect(saved.filters).toEqual([{ field: 'source', operator: 'in', values: ['1'] }]);
    }
  );

  it('shows an API error in the dialog and keeps it open', async () => {
    const fetchFn = vi.fn<typeof fetch>(async () =>
      Response.json({ message: 'Name already used' }, { status: 409 })
    );
    const { onSaved, onclose } = renderDialog(fetchFn);
    await fireEvent.input(screen.getByLabelText('Name'), { target: { value: 'Invoices' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Save' }));
    expect((await screen.findByRole('alert')).textContent).toContain('Name already used');
    expect(onSaved).not.toHaveBeenCalled();
    expect(onclose).not.toHaveBeenCalled();
    expect(screen.getByRole('dialog', { name: 'Save view' })).toBeDefined();
  });

  it('notes what a Files view leaves out only in Files', () => {
    const note = 'Filename, type, and file sort aren’t saved with the view.';
    renderDialog(vi.fn<typeof fetch>());
    expect(screen.queryByText(note)).toBeNull();
    cleanup();
    renderDialog(vi.fn<typeof fetch>(), { workspace: 'files', presentation: 'files' });
    expect(screen.getByText(note)).toBeDefined();
  });
});
