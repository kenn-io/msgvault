import { render, screen, within } from '@testing-library/svelte';
import { describe, expect, it } from 'vitest';
import ChangeList from './ChangeList.svelte';

describe('ChangeList', () => {
  it('heads Saved and Yours once and lists each differing field under them', () => {
    render(ChangeList, {
      changes: [{ label: 'Title', saved: 'Garden designer', yours: 'Garden design lead' }, { label: 'Role', saved: '', yours: 'Lead' }],
      provenance: { text: 'Saved at 10:05 PM', title: 'Saved Nov 5, 2026, 10:05 PM' },
    });
    const table = screen.getByRole('table', { name: 'What changed' });
    expect(within(table).getAllByRole('columnheader').map((cell) => cell.textContent!.trim())).toEqual(['Field', 'Saved', 'Yours']);
    const rows = within(table).getAllByRole('row').slice(1).map((row) => [...row.querySelectorAll('[role=rowheader], [role=cell]')].map((cell) => cell.textContent));
    expect(rows).toEqual([['Title', 'Garden designer', 'Garden design lead'], ['Role', 'None', 'Lead']]);
    expect(screen.getByText('Saved at 10:05 PM').getAttribute('title')).toBe('Saved Nov 5, 2026, 10:05 PM');
    expect(screen.queryByText("Nothing you're editing changed.")).toBeNull();
  });

  it('summarizes changes off the form in one line', () => {
    render(ChangeList, { changes: [{ label: 'Name', saved: 'Lumen Grove', yours: 'Lumen Grove Co.' }], otherChanged: true });
    expect(screen.getByText('Other details also changed')).toBeDefined();
  });

  it('says so when nothing on the form differs', () => {
    render(ChangeList, { changes: [], otherChanged: true });
    expect(screen.queryByRole('table')).toBeNull();
    expect(screen.getByText("Nothing you're editing changed.")).toBeDefined();
    expect(screen.getByText('Other details also changed')).toBeDefined();
  });
});
