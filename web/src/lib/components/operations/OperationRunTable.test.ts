import { render, screen } from '@testing-library/svelte';
import { expect, it } from 'vitest';
import { jobRun } from '../../jobs/fixtures.test-support';
import OperationRunTable from './OperationRunTable.svelte';

it.each(['partial', 'cancelled', 'failed'] as const)('keeps trigger, duration, and counters beside a %s run error', state => {
  render(OperationRunTable, { rows: [{
    ...jobRun('document_extraction'), state, trigger: 'manual',
    started_at: '2026-10-12T12:00:00Z', finished_at: '2026-10-12T12:01:00Z',
    counters: [{ name: 'processed', value: 4, unit: 'documents' }],
    error: { code: 'upstream_failed', message: 'Upstream operation failed.' }
  }] });
  expect(screen.getByText('Manual · 1 minute · 4 documents processed · Upstream operation failed.')).toBeTruthy();
});

it('keeps cancelled run metadata when there is no error', () => {
  render(OperationRunTable, { rows: [{
    ...jobRun('document_extraction'), state: 'cancelled', trigger: 'scheduled',
    started_at: '2026-10-12T12:00:00Z', finished_at: '2026-10-12T12:01:00Z',
    counters: [{ name: 'processed', value: 4, unit: 'documents' }], error: undefined
  }] });
  expect(screen.getByText('Scheduled · 1 minute · 4 documents processed')).toBeTruthy();
});
