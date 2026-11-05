import { expect, test } from '@playwright/test';
import { installMixedArchive } from './e2e/fixtures/mixed-archive';

const organization = { id: 21, revision: 1, name: 'Example Org', kind: 'company', created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z' };
const employment = { id: 11, revision: 1, person_id: 42, organization_id: 21, title: 'Engineer', is_current: true, is_primary: true, source: 'user', created_at: organization.created_at, updated_at: organization.updated_at };

test.beforeEach(async ({ page }) => {
  await installMixedArchive(page);
  await page.route('**/api/v1/organizations?*', route => route.fulfill({ json: { organizations: [organization], total: 1 } }));
  await page.route('**/api/v1/organizations/21', route => route.fulfill({ headers: { ETag: '"organization-21-r1"' }, json: route.request().method() === 'PATCH' ? organization : { organization, names: [], categories: [] } }));
  await page.route('**/api/v1/people/42/employments', route => route.fulfill({ json: { employments: [employment] } }));
  await page.route('**/api/v1/employments/11', route => route.fulfill({ headers: { ETag: '"employment-11-r1"' }, json: employment }));
  await page.goto(`/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'directory', directoryPersonID: 42 }))}`);
  await page.getByRole('radio', { name: 'Details', exact: true }).click();
});

test('keyboard End keeps errors and recovery controls opaque outside the row', async ({ page }) => {
  await page.route('**/api/v1/employments/11/end', route => route.fulfill({ status: 400, json: { message: 'Unable to end this employment' } }));
  const trigger = page.getByRole('button', { name: 'End employment', exact: true });
  await trigger.focus();
  await page.keyboard.press('Enter');
  const panel = page.getByRole('dialog', { name: 'End employment' });
  const date = panel.getByRole('textbox', { name: 'Employment end date' });
  await expect(date).toBeFocused();
  await page.mouse.move(0, 0);
  await date.press('Enter');
  await expect(panel.getByRole('alert')).toContainText('Unable to end this employment');
  await expect.poll(() => panel.evaluate(element => {
    let opacity = 1;
    for (let node: Element | null = element; node; node = node.parentElement) opacity *= Number(getComputedStyle(node).opacity);
    return opacity;
  })).toBe(1);
  await expect(panel.getByRole('button', { name: 'End', exact: true })).toBeEnabled();
  await panel.getByRole('button', { name: 'Close end employment' }).click();
  await expect(trigger).toBeFocused();
});

test('Enter submits organization and employment editors through their footers', async ({ page }) => {
  await page.getByRole('button', { name: 'Example Org', exact: true }).click();
  const editor = page.getByRole('dialog', { name: 'Edit Example Org' });
  const name = editor.getByRole('textbox', { name: 'Organization name' });
  await name.fill('Example Renamed');
  const patch = page.waitForRequest(request => request.method() === 'PATCH' && request.url().endsWith('/organizations/21'));
  await name.press('Enter');
  expect((await patch).postDataJSON()).toMatchObject({ name: 'Example Renamed' });
  await expect(editor).toHaveCount(0);
  await page.getByRole('button', { name: 'Add work' }).click();
  await page.getByRole('button', { name: 'New organization' }).click();
  await page.getByRole('textbox', { name: 'Organization name' }).fill('Example School');
  await page.route('**/api/v1/organizations', route => route.fulfill({ status: 201, headers: { ETag: '"organization-21-r1"' }, json: organization }));
  await page.route('**/api/v1/employments', route => route.fulfill({ status: 201, headers: { ETag: '"employment-11-r1"' }, json: employment }));
  const post = page.waitForRequest(request => request.method() === 'POST' && request.url().endsWith('/employments'));
  await page.getByRole('textbox', { name: 'Employment title' }).fill('Teacher');
  await page.getByRole('textbox', { name: 'Employment title' }).press('Enter');
  const body = (await post).postDataJSON();
  expect(body).toMatchObject({ organization_id: 21, title: 'Teacher' });
  expect(body).not.toHaveProperty('end_date');
  await expect(page.getByRole('dialog', { name: 'Add employment' })).toHaveCount(0);
});
