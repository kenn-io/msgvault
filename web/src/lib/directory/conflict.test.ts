import { expect, it } from 'vitest';
import { employment } from './entity-fixtures.test-support';
import { employmentConflictDetails, employmentFacts } from './conflict';

it('formats optional confidence for job conflicts', () => {
  const job = employment(11, 1, { source_ref: 'imported-card-42', department: 'Research', location: 'North office', description: 'Leads field studies' });
  const facts = employmentFacts(job, { organizationName: () => 'Synthetic Org', personName: () => 'Synthetic Person' }, 7);
  expect(facts).not.toHaveProperty('Confidence');

  const confident = employmentFacts({ ...job, confidence: 0.8 }, { organizationName: () => 'Synthetic Org', personName: () => 'Synthetic Person' }, 7);
  expect(confident.Confidence).toBe('80%');
  expect(confident.Department).toBe('Research');
  expect(confident.Location).toBe('North office');
  expect(confident.Description).toBe('Leads field studies');
});

it('formats source references in job conflict details', () => {
  const job = employment(11, 1, { source_ref: 'imported-card-42' });
  expect(employmentConflictDetails({ ...job, source_ref: 'example-source-42' }, 'Save failed'))
    .toBe('Save failed\nSource reference: example-source-42');
});
