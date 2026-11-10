import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { afterAll, expect, it, vi } from 'vitest';
import { controllerWith, employment, organizationProfile, pathOf, requestOf } from '../../directory/entity-fixtures.test-support';
import EmploymentEndDialog from './EmploymentEndDialog.svelte';

const formatter = vi.hoisted(() => {
  const DateTimeFormat = Intl.DateTimeFormat;
  return vi.spyOn(Intl, 'DateTimeFormat').mockImplementation(function (locale, options) { return new DateTimeFormat(locale ?? 'en-US', options); });
});
afterAll(() => formatter.mockRestore());

it('keeps the End conflict valid and shows a failed retry', async () => {
  const opening = employment(11, 1, { start_date: { year: 2024, month: 3 } });
  const saved = employment(11, 2, { start_date: { year: 2028 } });
  let reads = 0;
  let writes = 0;
  const { controller } = controllerWith(async input => {
    const request = requestOf(input);
    if (pathOf(request) === '/api/v1/organizations/21') return Response.json(organizationProfile(), { headers: { ETag: '"organization-21-r1"' } });
    if (pathOf(request) === '/api/v1/people/7/employments') return Response.json({ employments: [saved] });
    if (pathOf(request).endsWith('/end')) {
      writes++;
      return Response.json({ error: writes === 1 ? 'employment_revision_conflict' : 'internal_error', message: writes === 1 ? 'Changed elsewhere' : 'Disk full' }, { status: writes === 1 ? 409 : 500 });
    }
    return Response.json(++reads === 1 ? opening : saved, { headers: { ETag: reads === 1 ? '"employment-11-r1"' : '"employment-11-r2"' } });
  });
  controller.organizationRecords.set(21, organizationProfile());
  const rendered = render(EmploymentEndDialog, { controller, employment: opening, onClose: () => undefined });
  const end = screen.getByRole('textbox', { name: 'End date' });
  expect(screen.getByText('Engineer at Synthetic Org · since Mar 2024')).toBeDefined();
  await fireEvent.input(end, { target: { value: '2026' } });
  await waitFor(() => expect(screen.getByRole('button', { name: 'End' })).toHaveProperty('disabled', false));
  await fireEvent.click(screen.getByRole('button', { name: 'End' }));
  await screen.findByRole('button', { name: 'Keep yours' });
  expect(end).toHaveProperty('value', '2026');
  expect(reads).toBe(2);
  expect(screen.getByRole('button', { name: 'Keep yours' })).toHaveProperty('disabled', true);
  await fireEvent.input(end, { target: { value: '2029' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Keep yours' }));
  await screen.findByText("Couldn't end this job.");
  expect(screen.getByText('Disk full')).toBeDefined();
  rendered.unmount();
  controller.destroy();
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

it('offers no Keep yours for a moved job', async () => {
  let reads = 0;
  const { controller } = controllerWith(async input => {
    const request = requestOf(input), path = pathOf(request);
    if (path === '/api/v1/employments/11' && request.method === 'GET') return Response.json(employment(11, ++reads, { person_id: reads === 1 ? 7 : 8 }), { headers: { ETag: `"employment-11-r${reads}"` } });
    if (path === '/api/v1/employments/11/end') return Response.json({ error: 'employment_revision_conflict', message: 'Changed' }, { status: 409 });
    if (path === '/api/v1/people/7/employments') return Response.json({ employments: [] });
    if (path === '/api/v1/organizations/21') return Response.json(organizationProfile(), { headers: { ETag: '"organization-21-r1"' } });
    throw new Error(`unexpected ${request.method} ${path}`);
  });
  render(EmploymentEndDialog, { controller, employment: employment(), onClose: vi.fn() });
  const submit = screen.getByRole('button', { name: 'End' });
  await waitFor(() => expect(submit).toHaveProperty('disabled', false));
  await fireEvent.click(submit);
  await screen.findByRole('region', { name: 'Saved job' });
  expect(screen.queryByRole('button', { name: 'Keep yours' })).toBeNull();
  expect(screen.getByRole('button', { name: 'Close' })).toBeDefined();
});
