import { expect, it } from 'vitest';
import { conflict, connectionFacts, employmentFacts, organizationFacts } from './conflict';
import { employment, relationship, richOrganizationProfile } from './entity-fixtures.test-support';

const cases = [
  employmentFacts(employment()), connectionFacts(relationship()), organizationFacts(richOrganizationProfile()),
].flatMap(facts => Object.keys(facts).map(label => ({ facts, label })));
it.each(cases)('compares the writable $label and records changes outside the draft', ({ facts, label }) => {
  const saved = { ...facts, [label]: 'Changed elsewhere' };
  expect(conflict(saved, facts, { [label]: facts[label] })).toEqual({ rows: [{ label, saved: 'Changed elsewhere', yours: facts[label] }], other: [] });
  expect(conflict(saved, facts, {})).toEqual({ rows: [], other: [{ label, saved: 'Changed elsewhere', yours: facts[label] }] });
  expect(conflict(facts, facts, {})).toEqual({ rows: [], other: [] });
});
it('ignores connection status derived from an edited end date', () => {
  const opened = relationship();
  expect(conflict(connectionFacts({ ...opened, end_date: { year: 2026 } }), connectionFacts(opened), { 'End date': '2025' }).other).toEqual([]);
});
it('detects connection notes cleared elsewhere', () => {
  const opened = relationship(41, 1, { notes: 'Opening notes' });
  expect(conflict(connectionFacts(relationship(41, 2)), connectionFacts(opened), { 'End date': '2026' }).other).toEqual([{ label: 'Notes', saved: '', yours: 'Opening notes' }]);
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
