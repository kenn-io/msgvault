import { render, screen } from '@testing-library/svelte';
import { expect, it } from 'vitest';
import type { OperationRunSummary } from '../../api/generated/models';
import { jobRun } from '../../jobs/fixtures.test-support';
import OperationRunTable from './OperationRunTable.svelte';

it('keeps cancelled run metadata with and without an error', () => {
  const run: OperationRunSummary = {
    ...jobRun('document_extraction'), state: 'cancelled' as const,
    started_at: '2026-10-12T12:00:00Z', finished_at: '2026-10-12T12:01:00Z',
    counters: [{ name: 'processed', value: 4, unit: 'documents' }],
  };
  render(OperationRunTable, { rows: [
    { ...run, trigger: 'manual', error: { code: 'upstream_failed', message: 'Upstream operation failed.' } },
    { ...run, id: `${run.id}-scheduled`, trigger: 'scheduled', error: undefined },
  ] });
  expect(screen.getByText('Manual · 1 minute · 4 documents processed · Upstream operation failed.')).toBeTruthy();
  expect(screen.getByText('Scheduled · 1 minute · 4 documents processed')).toBeTruthy();
});

it('describes the time and outcome of repeated job kinds', () => {
  const run = jobRun('source_sync');
  render(OperationRunTable, { rows: [
    { ...run, id: `${run.id}-succeeded`, state: 'succeeded', started_at: new Date(2026, 7, 30, 12).toISOString(), error: undefined },
    { ...run, started_at: new Date(2026, 7, 30, 11).toISOString() },
  ] });
  expect(screen.getByRole('button', { name: 'Open Source sync run', description: /Aug 30, 2026, 12:00 PM.*Succeeded/ })).toBeTruthy();
  expect(screen.getByRole('button', { name: 'Open Source sync run', description: /Aug 30, 2026, 11:00 AM.*Failed/ })).toBeTruthy();
});
