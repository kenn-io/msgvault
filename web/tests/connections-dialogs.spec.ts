import { expect, test } from '@playwright/test';
import { installDirectoryReviewArchive } from './e2e/fixtures/mixed-archive';
import { directoryPerson, relationshipType, person } from '../src/lib/directory/entity-fixtures.test-support';

test('saved person remains selectable at 280px', async ({ page }) => {
  await installDirectoryReviewArchive(page);
  const subjectName = 'a-very-long-synthetic-person-name-without-spaces@example.test';
  await page.route('**/api/v1/people/7/profile', route => route.fulfill({ json: {
    person: { ...person(7), display_name: subjectName }, names: [], contact_points: [], addresses: [], dates: [], categories: [], media: [],
  } }));
  await page.route('**/api/v1/people/directory*', route => route.fulfill({ json: { people: [directoryPerson(9)] } }));
  await page.setViewportSize({ width: 375, height: 280 });
  await page.goto('/?explore=' + encodeURIComponent(JSON.stringify({ workspace: 'relationships', directoryPersonID: 7 })));
  await page.getByRole('radio', { name: 'Details', exact: true }).click();
  await page.getByRole('button', { name: 'Add connection', exact: true }).click();
  const dialog = page.getByRole('dialog', { name: 'Add relationship', exact: true });
  await dialog.getByRole('button', { name: 'Search people', exact: true }).click();
  const option = dialog.getByRole('option', { name: /Synthetic Two/ });
  await expect(option).toBeVisible();
  expect(await dialog.locator('.kit-typeahead__body').evaluate(element => element.clientHeight)).toBeGreaterThanOrEqual(32);
  await option.click();
  await expect(dialog.getByRole('button', { name: 'Search people: Synthetic Two', exact: true })).toBeVisible();
});

for (const width of [1440, 375]) {
  test(`Connections dialogs stay contained and return focus after submission at ${width}px`, async ({ page }) => {
    await installDirectoryReviewArchive(page);
    const subjectName = width === 375 ? 'a-very-long-synthetic-person-name-without-spaces@example.test' : 'Synthetic One';
    await page.route('**/api/v1/people/7', route => route.fulfill({ json: { ...person(7), display_name: subjectName } }));
    await page.route('**/api/v1/people/7/profile', route => route.fulfill({ json: {
      person: { ...person(7), display_name: subjectName }, names: [], contact_points: [], addresses: [], dates: [], categories: [], media: [],
    } }));
    await page.setViewportSize({ width, height: 900 });
    const types = [
      relationshipType({ id: 1, slug: 'parent', forward_label: 'parent', reverse_label: 'child', ownership: 'system', inverse_type_id: 2 }),
      relationshipType({ id: 2, slug: 'child', forward_label: 'child', reverse_label: 'parent', is_canonical: false, ownership: 'system', inverse_type_id: 1 }),
      ...Array.from({ length: 24 }, (_, i) => relationshipType({ id: i + 3, slug: `custom-${i}`, forward_label: `supports a long custom connection ${i}`, reverse_label: `receives support ${i}` }))
    ];
    await page.route('**/api/v1/relationship-types', route => route.fulfill({ json: { relationship_types: types } }));
    const savedPeople = [
      directoryPerson(9),
      ...Array.from({ length: 19 }, (_, index) => directoryPerson(index + 10, { display_name: `Synthetic Peer ${index}` })),
    ];
    await page.route('**/api/v1/people/directory*', route => {
      const query = new URL(route.request().url()).searchParams.get('q')?.toLowerCase() ?? '';
      return route.fulfill({ json: { people: savedPeople.filter(person => person.display_name?.toLowerCase().includes(query)) } });
    });
    let saved = false;
    await page.route('**/api/v1/person-relationships', async route => {
      saved = true;
      await route.fulfill({ status: 201, json: { id: 50, revision: 1 } });
    });
    await page.route('**/api/v1/people/7/relationships*', route => route.fulfill({ json: { relationships: saved ? [{
      counterpart_person_id: 9, counterpart_display_name: 'Synthetic Two', counterpart_label: 'parent', direction: 'incoming',
      relationship: { id: 50, revision: 1, type_slug: 'parent', source_person_id: 9, target_person_id: 7, forward_label: 'parent', reverse_label: 'child', is_symmetric: false, source: 'user' }
    }] : [] } }));
    await page.goto('/?explore=' + encodeURIComponent(JSON.stringify({ workspace: 'relationships', directoryPersonID: 7 })));
    await page.getByRole('radio', { name: 'Details', exact: true }).click();
    const add = page.getByRole('button', { name: 'Add connection', exact: true });
    await add.click();
    const dialog = page.getByRole('dialog', { name: 'Add relationship', exact: true });
    await expect(dialog.locator('.sentence strong')).toHaveText(subjectName);
    expect(await dialog.locator('.kit-typeahead__value-text').evaluateAll(labels => labels.every(label => label.scrollWidth <= label.clientWidth))).toBe(true);
    const closedPeople = await dialog.getByRole('button', { name: 'Search people', exact: true }).boundingBox();
    await dialog.getByRole('button', { name: 'Connection type: Choose connection', exact: true }).click();
    await expect(dialog.getByRole('treeitem', { name: 'Family', exact: true })).toBeVisible();
    const panel = dialog.locator('.kit-typeahead__panel');
    const bounds = await panel.boundingBox();
    const bodyBounds = await dialog.locator('.sentence').boundingBox();
    expect(bounds!.x).toBeCloseTo(bodyBounds!.x, 0);
    expect(bounds!.width).toBeCloseTo(bodyBounds!.width, 0);
    expect(await dialog.getByRole('button', { name: 'Search people', exact: true }).boundingBox()).toEqual(closedPeople);
    expect(bounds!.x).toBeGreaterThanOrEqual(0);
    expect(bounds!.x + bounds!.width).toBeLessThanOrEqual(width);
    expect(bounds!.y + bounds!.height).toBeLessThanOrEqual(900);
    await dialog.getByRole('treeitem', { name: 'Child', exact: true }).click();
    await dialog.getByRole('button', { name: 'Search people', exact: true }).click();
    expect((await dialog.getByRole('combobox', { name: 'Search people', exact: true }).boundingBox())!.y).toBe(closedPeople!.y);
    await expect(dialog.getByRole('option', { name: /Synthetic Two/ })).toBeVisible();
    const peopleBounds = await panel.boundingBox();
    expect(peopleBounds!.x).toBeGreaterThanOrEqual(0);
    expect(peopleBounds!.x + peopleBounds!.width).toBeLessThanOrEqual(width);
    expect(peopleBounds!.y).toBeGreaterThanOrEqual(0);
    expect(peopleBounds!.y + peopleBounds!.height).toBeLessThanOrEqual(900);
    expect(await panel.locator('.kit-typeahead__body').evaluate(element => element.scrollHeight > element.clientHeight)).toBe(true);
    await dialog.getByRole('option', { name: /Synthetic Two/ }).click();
    const geometry = await dialog.evaluate(element => {
      const dates = [...element.querySelectorAll('.dates label')].map(label => label.getBoundingClientRect());
      const sections = [...element.querySelectorAll('.editor > section')].map(section => section.getBoundingClientRect());
      return { overflow: [...element.querySelectorAll('.editor, .editor section, .sentence, .dates')].some(child => child.scrollWidth > child.clientWidth), dates: dates.map(({ x, y }) => ({ x, y })), gap: sections[1].top - sections[0].bottom };
    });
    expect(geometry.overflow).toBe(false);
    expect(geometry.gap).toBeGreaterThan(0);
    if (width === 375) expect(geometry.dates[0].x).toBe(geometry.dates[1].x);
    else expect(geometry.dates[0].y).toBe(geometry.dates[1].y);
    const start = dialog.getByRole('textbox', { name: 'Relationship start date' });
    if (width === 375) expect((await dialog.locator('.dates label').first().boundingBox())!.width).toBe((await dialog.locator('.dates').boundingBox())!.width);
    const end = dialog.getByRole('textbox', { name: 'Relationship end date' });
    if (width === 1440) expect((await start.boundingBox())!.y).toBe((await end.boundingBox())!.y);
    await dialog.getByRole('textbox', { name: 'Relationship notes' }).press('Enter');
    await expect(dialog).toHaveCount(0);
    await expect(add).toBeFocused();
    await page.getByRole('button', { name: 'Edit relationship', exact: true }).click();
    const edit = page.getByRole('dialog', { name: 'Edit relationship', exact: true });
    expect((await edit.boundingBox())!.width).toBe(Math.min(440, width - 32));
    await edit.getByRole('button', { name: 'Close relationship editor' }).click();
    await expect(edit).toHaveCount(0);
  });
}
