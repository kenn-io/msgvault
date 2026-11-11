import { expect, it } from 'vitest';
import { employment } from './entity-fixtures.test-support';
import { employmentConflictDetails, employmentFacts } from './conflict';

it('formats job conflict facts', () => {
  const job = employment(11, 1, { source_ref: 'imported-card-42', department: 'Research', location: 'North office', description: 'Leads field studies' });
  const facts = employmentFacts(job, { organizationName: () => 'Synthetic Org' }, 7);
  expect(facts).not.toHaveProperty('Confidence');
  expect(facts).not.toHaveProperty('Person');
  expect(employmentFacts({ ...job, person_id: 8 }, { organizationName: () => 'Synthetic Org' }, 7).Person).toBe('Someone else');

  const confident = employmentFacts({ ...job, source: 'extraction', confidence: 0.8 }, { organizationName: () => 'Synthetic Org' }, 7);
  expect(confident.Confidence).toBe('80%');
  expect(confident.Department).toBe('Research');
  expect(confident.Location).toBe('North office');
  expect(confident.Description).toBe('Leads field studies');
});

it('formats source references in job conflict details', () => {
  const job = employment(11, 1, { source_ref: 'example-source-42' });
  expect(employmentConflictDetails(job, 'Save failed'))
    .toBe('Save failed\nSource reference: example-source-42');
});
