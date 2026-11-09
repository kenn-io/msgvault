import { expect, it } from 'vitest';
import { employment } from './entity-fixtures.test-support';
import { employmentFacts } from './conflict';

it('omits empty confidence and stored source references from job conflict facts', () => {
  const job = employment(11, 1, { source_ref: 'imported-card-42' });
  const facts = employmentFacts(job, { organizationName: () => 'Synthetic Org', personName: () => 'Synthetic Person' }, 7);
  expect(facts).not.toHaveProperty('Confidence');
  expect(facts).not.toHaveProperty('Source reference');

  const confident = employmentFacts({ ...job, confidence: 0.8 }, { organizationName: () => 'Synthetic Org', personName: () => 'Synthetic Person' }, 7);
  expect(confident.Confidence).toBe('80%');
});
