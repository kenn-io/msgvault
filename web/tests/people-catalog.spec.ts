import { expect, test } from '@playwright/test';
import { installMixedArchive } from './e2e/fixtures/mixed-archive';

test('phone field rows leave descriptions readable and Edit beside the name', async ({ page }) => {
  await installMixedArchive(page);
  await page.setViewportSize({ width: 390, height: 900 });
  const field = {
    id: 41, universal_id: '00000000-0000-4000-8000-000000000041', object_type: 'person', slug: 'favorite_plant', label: 'Favorite plant', description: 'Garden preferences',
    value_type: 'text', field_type: 'text', cardinality: 'single', display_order: 0, ownership: 'user', ui_creatable: true, ui_editable: true, api_mutable: true, is_active: true, is_sensitive: true, revision: 1, is_deletable: true
  };
  await page.route('**/api/v1/attribute-definitions*', route => route.fulfill({ json: { definitions: [field, { ...field, id: 42, universal_id: '00000000-0000-4000-8000-000000000042', slug: 'location', label: 'Location', description: 'Where they live', ownership: 'system', is_sensitive: false }] } }));
  await page.route('**/api/v1/person-fact-targets*', route => route.fulfill({ json: { targets: [] } }));
  await page.route('**/api/v1/organizations*', route => route.fulfill({ json: { organizations: [], total: 0, limit: 50, offset: 0 } }));
  await page.goto('/?explore=' + encodeURIComponent(JSON.stringify({ workspace: 'settings', settingsCategory: 'people_catalog' })));
  const edit = page.getByRole('button', { name: 'Edit Favorite plant', exact: true });
  await expect(edit).toBeVisible();
  const geometry = await edit.evaluate(button => {
    const row = button.closest('li')!;
    const name = row.querySelector('.name')!;
    const range = document.createRange();
    range.selectNode(name.firstChild!);
    const title = range.getBoundingClientRect();
    const description = name.querySelector('small')!.getBoundingClientRect();
    const kind = row.querySelector(':scope > small')!.getBoundingClientRect();
    const action = button.getBoundingClientRect();
    return { rowHeight: row.getBoundingClientRect().height, otherHeight: row.nextElementSibling!.getBoundingClientRect().height, descriptionWidth: description.width, descriptionTop: description.top, titleBottom: title.bottom, titleCenter: title.top + title.height / 2, actionCenter: action.top + action.height / 2, kindTop: kind.top, descriptionBottom: description.bottom, actionLeft: action.left, titleRight: title.right };
  });
  expect(Math.abs(geometry.rowHeight - geometry.otherHeight), 'Edit keeps the same row rhythm').toBeLessThanOrEqual(1);
  expect(geometry.descriptionWidth, 'the description uses the row width').toBeGreaterThanOrEqual(280);
  expect(Math.abs(geometry.titleCenter - geometry.actionCenter), 'Edit shares the name line').toBeLessThanOrEqual(2);
  expect(geometry.actionLeft).toBeGreaterThanOrEqual(geometry.titleRight);
  expect(geometry.descriptionTop).toBeGreaterThanOrEqual(geometry.titleBottom);
  expect(geometry.kindTop).toBeGreaterThanOrEqual(geometry.descriptionBottom);
});
