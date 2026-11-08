import { expect, it } from 'vitest';
import { conflict, employmentFacts } from './conflict';
import { employment } from './entity-fixtures.test-support';

it('compares writable values and records changes outside the draft', () => {
  const facts = { Notes: 'Opening' };
  const label = 'Notes';
  const saved = { ...facts, [label]: 'Changed elsewhere' };
  expect(conflict(saved, facts, { [label]: facts[label] })).toEqual({ rows: [{ label, saved: 'Changed elsewhere', yours: facts[label] }], other: [] });
  expect(conflict(saved, facts, {})).toEqual({ rows: [], other: [{ label, saved: 'Changed elsewhere', yours: facts[label] }] });
  expect(conflict(facts, facts, {})).toEqual({ rows: [], other: [] });
});

it('puts a changed employer in Details when End job preserves it', () => {
  const opened = employment();
  const diff = conflict(employmentFacts({ ...opened, organization_id: 22 }, String), employmentFacts(opened, String), { 'End date': '2026' });
  expect(diff.rows.some(row => row.label === 'Organization')).toBe(false);
  expect(diff.other).toContainEqual({ label: 'Organization', saved: '22', yours: '21' });
});
it('shows a cleared draft value as Yours', () => {
  expect(conflict({ Notes: 'Saved' }, { Notes: 'Opening' }, { Notes: '' }).rows).toEqual([{ label: 'Notes', saved: 'Saved', yours: '' }]);
});

it('discloses a job moved to another person before keeping the draft', () => {
  const opened = employment();
  const moved = employment(11, 2, { person_id: 8 });
  const diff = conflict(employmentFacts(moved), employmentFacts(opened), { 'Job title': 'Draft title' });
  expect(diff.other).toHaveLength(1);
});
