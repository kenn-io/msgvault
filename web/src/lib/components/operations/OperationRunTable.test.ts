import { render, screen } from '@testing-library/svelte';
import { expect, it } from 'vitest';
import type { OperationRunSummary } from '../../api/generated/models';
import { jobRun } from '../../jobs/fixtures.test-support';
import OperationRunTable from './OperationRunTable.svelte';

it('keeps cancelled run metadata with and without an error', () => {
  const run: OperationRunSummary = {
    ...jobRun('document_extraction'), state: 'cancelled' as const,
    started_at: '2026-10-12T12:00:00Z', finished_at: '2026-10-12T12:01:00Z',
    counters: [{ name: 'attempted', value: 4, unit: 'documents' }, { name: 'succeeded', value: 4, unit: 'documents' }],
  };
  render(OperationRunTable, { rows: [
    { ...run, trigger: 'manual', error: { code: 'invocation_cancelled', message: 'Operation was cancelled.' } },
    { ...jobRun('source_sync'), state: 'cancelled', started_at: run.started_at, finished_at: run.finished_at,
      counters: [{ name: 'processed', value: 4, unit: 'messages' }], error: undefined },
  ] });
  expect(screen.getByText('Manual · 1 minute · 4 documents attempted · 4 succeeded · Operation was cancelled.')).toBeTruthy();
  expect(screen.getByText('— · 1 minute · 4 messages processed')).toBeTruthy();
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
