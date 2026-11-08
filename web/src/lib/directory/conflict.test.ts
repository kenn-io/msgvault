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

  const opened = employment();
  const names = { personID: opened.person_id, organizationName: String };
  const diff = conflict(employmentFacts({ ...opened, organization_id: 22 }, names), employmentFacts(opened, names), { 'End date': '2026' });
  expect(diff.rows.some(row => row.label === 'Organization')).toBe(false);
  expect(diff.other).toContainEqual({ label: 'Organization', saved: '22', yours: '21' });

  expect(conflict({ Notes: 'Saved' }, { Notes: 'Opening' }, { Notes: '' }).rows).toEqual([{ label: 'Notes', saved: 'Saved', yours: '' }]);

  const moved = employment(11, 2, { person_id: 8 });
  const movedDiff = conflict(employmentFacts(moved, names), employmentFacts(opened, names), { 'Job title': 'Draft title' });
  expect(movedDiff.other).toEqual([{ label: 'Person', saved: 'another person', yours: 'this person' }]);
});
