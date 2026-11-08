import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { expect, it } from 'vitest';
import { controllerWith, employment, pathOf, requestOf } from '../../directory/entity-fixtures.test-support';
import EmploymentEndDialog from './EmploymentEndDialog.svelte';

it.each([2024, 2028])('validates a retried end date against the saved start year %s', async year => {
  const bodies: unknown[] = [];
  const opening = employment(11, 1, { start_date: { year: 2026 } });
  const saved = employment(11, 2, { start_date: { year } });
  let reads = 0;
  const { controller } = controllerWith(async input => {
    const request = requestOf(input);
    if (pathOf(request).endsWith('/end')) {
      bodies.push(await request.json());
      return Response.json({ message: 'Changed elsewhere' }, { status: 412 });
    }
    return ++reads === 1
      ? Response.json(opening, { headers: { ETag: '"employment-11-r1"' } })
      : Response.json(saved, { headers: { ETag: '"employment-11-r2"' } });
  });
  render(EmploymentEndDialog, { controller, employment: opening, onClose: () => undefined });
  const end = screen.getByRole('textbox', { name: 'End date' });
  await fireEvent.input(end, { target: { value: '2027' } });
  await fireEvent.click(screen.getByRole('button', { name: 'End' }));
  await screen.findByRole('button', { name: 'Keep yours' });
  expect(bodies).toEqual([{ end_date: '2027' }]);
  await fireEvent.input(end, { target: { value: year === 2028 ? '2027' : '2025' } });
  expect(screen.getByRole('button', { name: 'Keep yours' })).toHaveProperty('disabled', year === 2028);
  if (year === 2024) {
    await fireEvent.click(screen.getByRole('button', { name: 'Keep yours' }));
    await waitFor(() => expect(bodies).toHaveLength(2));
    expect(bodies).toEqual([{ end_date: '2027' }, { end_date: '2025' }]);
  }
});

it('shows the latest error after retrying an end conflict', async () => {
  let writes = 0;
  const { controller } = controllerWith(async input => {
    const request = requestOf(input);
    if (pathOf(request).endsWith('/end')) return ++writes === 1
      ? Response.json({ message: 'Changed elsewhere' }, { status: 412 })
      : Response.json({ message: 'Disk full' }, { status: 500 });
    return Response.json(employment(), { headers: { ETag: '"employment-11-r2"' } });
  });
  render(EmploymentEndDialog, { controller, employment: employment(), onClose: () => undefined });
  await fireEvent.click(screen.getByRole('button', { name: 'End' }));
  await fireEvent.click(await screen.findByRole('button', { name: 'Keep yours' }));
  await screen.findByText("Couldn't end this job.");
  expect(screen.getByText('Disk full')).toBeDefined();
  expect(screen.queryByRole('button', { name: 'Keep yours' })).toBeNull();
});
