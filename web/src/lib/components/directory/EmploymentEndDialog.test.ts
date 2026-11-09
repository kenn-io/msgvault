import { fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import { expect, it, vi } from 'vitest';
import { controllerWith, employment, organization, collection, pathOf, requestOf } from '../../directory/entity-fixtures.test-support';
import EmploymentEndDialog from './EmploymentEndDialog.svelte';

it('keeps the End conflict valid through retry', async () => {
  const opening = employment(11, 1, { start_date: { year: 2022 } });
  const saved = employment(11, 2, { start_date: { year: 2028 } });
  let reads = 0;
  let writes = 0;
  const { controller } = controllerWith(async input => {
    const request = requestOf(input);
    if (pathOf(request).endsWith('/end')) {
      writes++;
      return Response.json({ message: writes === 1 ? 'Changed elsewhere' : 'Disk full' }, { status: writes === 1 ? 412 : 500 });
    }
    return Response.json(++reads === 1 ? opening : saved, { headers: { ETag: reads === 1 ? '"employment-11-r1"' : '"employment-11-r2"' } });
  });
  const rendered = render(EmploymentEndDialog, { controller, employment: opening, onClose: () => undefined });
  const end = screen.getByRole('textbox', { name: 'End date' });
  await fireEvent.input(end, { target: { value: '2024' } });
  await waitFor(() => expect(screen.getByRole('button', { name: 'End' })).toHaveProperty('disabled', false));
  await fireEvent.click(screen.getByRole('button', { name: 'End' }));
  await screen.findByRole('button', { name: 'Keep yours' });
  expect(screen.getByRole('button', { name: 'Keep yours' })).toHaveProperty('disabled', true);
  await fireEvent.input(end, { target: { value: '2029' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Keep yours' }));
  await screen.findByText("Couldn't end this job.");
  expect(screen.getByText('Disk full')).toBeDefined();
  expect(screen.queryByRole('button', { name: 'Keep yours' })).toBeNull();
  expect(screen.getByRole('button', { name: 'End' })).toHaveProperty('disabled', false);
  await fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
  await waitFor(() => expect(writes).toBe(3));
  rendered.unmount();
  controller.destroy();
});

it('keeps the draft and resolves the employer after an End conflict', async () => {
    let reads = 0;
    const opening = employment(11, 1, { start_date: { year: 2024, month: 3 } });
    const saved = employment(11, 2, { start_date: { year: 2024, month: 3 }, title: 'Current saved role', organization_id: 22 });
    const { controller } = controllerWith(async input => {
      const request = requestOf(input);
      if (request.method === 'POST') return Response.json({ message: 'Concurrent end', error: 'revision_conflict' }, { status: 412 });
      if (pathOf(request) === '/api/v1/organizations/22') {
        return Response.json({ organization: organization(22, 1, 'Current Employer'), names: [], categories: [], contact_points: [], addresses: [], identifiers: [], media: [] }, { headers: { ETag: '"organization-22-r1"' } });
      }
      if (pathOf(request) === '/api/v1/people/7/employments') return collection([saved]);
      reads++;
      return Response.json(reads === 1 ? opening : saved, { headers: { ETag: `"employment-11-r${reads}"` } });
    });
    controller.employments = [opening];
    const onClose = vi.fn();
    const view = render(EmploymentEndDialog, { controller, employment: opening, onClose });
    try {
      const field = await screen.findByRole('textbox', { name: 'End date' });
      await waitFor(() => expect(field).toHaveProperty('disabled', false));
      expect(screen.getByRole('dialog', { name: 'End job' })).toBeDefined();
      expect(screen.getByText('Engineer at this organization · since Mar 2024')).toBeDefined();
      await fireEvent.input(field, { target: { value: '2026' } });
      await waitFor(() => expect(screen.getByRole('button', { name: 'End' })).toHaveProperty('disabled', false));
      await fireEvent.click(screen.getByRole('button', { name: 'End' }));
      const record = await screen.findByRole('region', { name: 'Saved job' });
      expect(within(record).getByText('Organization').nextElementSibling?.textContent).toBe('Current Employer');
      expect(field).toHaveProperty('value', '2026');
      expect(reads).toBe(2);
  } finally { view.unmount(); controller.destroy(); }
});

  it('prefills the local day across UTC midnight', async () => {
    vi.stubEnv('TZ', 'America/New_York');
    vi.useFakeTimers({ toFake: ['Date'] });
    vi.setSystemTime(new Date('2026-11-05T04:30:00Z'));
    const opening = employment(11, 1, { start_date: { year: 2024, month: 3 } });
    const { controller } = controllerWith(async () => Response.json(opening, { headers: { ETag: '"employment-11-r1"' } }));
    const onClose = vi.fn();
    const view = render(EmploymentEndDialog, { controller, employment: opening, onClose });
    try {
      const field = await screen.findByRole('textbox', { name: 'End date' });
      await waitFor(() => expect(field).toHaveProperty('disabled', false));
      expect(field).toHaveProperty('value', '2026-11-04');
    } finally { vi.useRealTimers(); vi.unstubAllEnvs(); view.unmount(); controller.destroy(); }
  });
