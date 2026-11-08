import { expect, test } from '@playwright/test';
import { installDirectoryReviewArchive } from './e2e/fixtures/mixed-archive';

for (const width of [1280, 390]) {
  test(`recurring person tasks keep their hierarchy and labels at ${width}px`, async ({ page }) => {
    await installDirectoryReviewArchive(page);
    await page.setViewportSize({ width, height: 900 });
    await page.route('**/api/v1/integrations/kata/status', route => route.fulfill({ json: { state: 'ready' } }));
    await page.route('**/api/v1/people/7/agenda', route => route.fulfill({ json: {
      project: 'example', person_uid: 'synthetic-7', person_uids: ['synthetic-7'], truncated: false,
      recurrence: { state: 'partial', message: 'Some recurring schedules could not be read.' },
      items: [
        { uid: 'parent', ref: 'abcd', qualified_ref: 'example#abcd', project: 'example', title: 'Review project notes', revision: '1', list: 'agenda', state: 'open', status: 'open',
          kind: 'series', membership: 'direct', open_occurrences: 1, occurrences_complete: true,
          schedules: [{ uid: 'job', summary: 'Cron: 0 9 * * 1', timezone: 'Europe/London', state: 'paused' }] },
        { uid: 'child', ref: 'efgh', qualified_ref: 'example#efgh', project: 'example', title: 'Review this week’s notes', revision: '1', list: 'agenda', state: 'open', status: 'open',
          kind: 'occurrence', membership: 'inherited', parent_uid: 'parent', occurrence: { job_uid: 'job', definition_event_uid: 'historical', scheduled_at: '2026-11-02T09:00:00Z' } },
        { uid: 'ordinary', ref: 'ijkl', project: 'example', title: 'Send the proposal', revision: '1', list: 'agenda', kind: 'task', membership: 'direct', state: 'open', status: 'open' },
      ],
    } }));
    await page.goto('/?explore=' + encodeURIComponent(JSON.stringify({ workspace: 'relationships', directoryPersonID: 7 })));
    await page.getByRole('radio', { name: 'Details', exact: true }).click();
    const tasks = page.getByRole('region', { name: 'Tasks', exact: true });
    await expect(tasks.getByText('Recurring', { exact: true })).toBeVisible();
    await expect(tasks.getByText('Cron: 0 9 * * 1 · Europe/London')).toBeVisible();
    await expect(tasks.getByText('Paused', { exact: true })).toBeVisible();
    await expect(tasks.getByRole('list', { name: 'Occurrences of Review project notes' })).toContainText('Scheduled for');
    await expect(tasks.getByText('Send the proposal')).toBeVisible();
    await expect(tasks.getByRole('button', { name: 'Unlink Review this week’s notes' })).toHaveCount(0);
    await expect(tasks.getByRole('button', { name: 'Retry recurring schedules' })).toBeVisible();
    const bounds = await tasks.evaluate(element => ({ scroll: element.scrollWidth, width: element.clientWidth }));
    expect(bounds.scroll).toBeLessThanOrEqual(bounds.width);
    await tasks.screenshot({ path: `test-results/recurring-person-tasks-${width}.png` });
    const unlink = tasks.getByRole('button', { name: 'Unlink Send the proposal' });
    await unlink.click();
    await expect(tasks.getByRole('button', { name: 'Unlink', exact: true })).toBeFocused();
    await tasks.getByRole('button', { name: 'Keep task linked' }).click();
    await expect(unlink).toBeFocused();
  });
}
