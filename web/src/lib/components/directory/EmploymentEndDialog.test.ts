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
  await waitFor(() => expect(screen.getByRole('button', { name: 'End' })).toHaveProperty('disabled', false));
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
  await waitFor(() => expect(screen.getByRole('button', { name: 'End' })).toHaveProperty('disabled', false));
  await fireEvent.click(screen.getByRole('button', { name: 'End' }));
  await fireEvent.click(await screen.findByRole('button', { name: 'Keep yours' }));
  await screen.findByText("Couldn't end this job.");
  expect(screen.getByText('Disk full')).toBeDefined();
  expect(screen.queryByRole('button', { name: 'Keep yours' })).toBeNull();
});

it('keeps the saved start date for retry after a failed conflict resolution', async () => {
  const opening = employment(11, 1, { start_date: { year: 2022 } });
  const saved = employment(11, 2, { start_date: { year: 2020 } });
  let reads = 0;
  let writes = 0;
  const { controller } = controllerWith(async input => {
    const request = requestOf(input);
    if (pathOf(request).endsWith('/end')) {
      writes++;
      return Response.json({ message: 'Failed' }, { status: writes === 1 ? 412 : 500 });
    }
    return Response.json(++reads === 1 ? opening : saved, { headers: { ETag: reads === 1 ? '"employment-11-r1"' : '"employment-11-r2"' } });
  });
  render(EmploymentEndDialog, { controller, employment: opening, onClose: () => undefined });
  const end = screen.getByRole('textbox', { name: 'End date' });
  await fireEvent.input(end, { target: { value: '2024' } });
  await waitFor(() => expect(screen.getByRole('button', { name: 'End' })).toHaveProperty('disabled', false));
  await fireEvent.click(screen.getByRole('button', { name: 'End' }));
  await screen.findByRole('button', { name: 'Keep yours' });
  await fireEvent.input(end, { target: { value: '2021' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Keep yours' }));
  await screen.findByText("Couldn't end this job.");
  expect(screen.getByRole('button', { name: 'End' })).toHaveProperty('disabled', false);
  await fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
  await waitFor(() => expect(writes).toBe(3));
});

it('sends the version opened before a concurrent edit', async () => {
  let revision = 1;
  const tags: (string | null)[] = [];
  const opening = employment();
  const { controller } = controllerWith(async input => {
    const request = requestOf(input);
    if (pathOf(request).endsWith('/end')) {
      tags.push(request.headers.get('If-Match'));
      return Response.json({ message: 'Changed' }, { status: 412 });
    }
    return Response.json(employment(11, revision), { headers: { ETag: `"employment-11-r${revision}"` } });
  });
  render(EmploymentEndDialog, { controller, employment: opening, onClose: () => undefined });
  await waitFor(() => expect(screen.getByRole('button', { name: 'End' })).toHaveProperty('disabled', false));
  revision = 2;
  await waitFor(() => expect(screen.getByRole('button', { name: 'End' })).toHaveProperty('disabled', false));
  await fireEvent.click(screen.getByRole('button', { name: 'End' }));
  await screen.findByRole('button', { name: 'Keep yours' });
  expect(tags).toEqual(['"employment-11-r1"']);
});
