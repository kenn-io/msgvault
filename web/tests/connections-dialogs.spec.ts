import { setKitTheme } from './kit-ui';
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
  await page.getByRole('button', { name: 'Add connection…', exact: true }).click();
  const dialog = page.getByRole('dialog', { name: 'Add connection', exact: true });
  await dialog.getByRole('button', { name: 'Person', exact: true }).click();
  const option = dialog.getByRole('option', { name: /Synthetic Two/ });
  await expect(option).toBeVisible();
  expect(await dialog.locator('.kit-typeahead__body').evaluate(element => element.clientHeight)).toBeGreaterThanOrEqual(32);
  await option.click();
  await expect(dialog.getByRole('button', { name: 'Person: Synthetic Two', exact: true })).toBeVisible();
});

for (const width of [1440, 390]) {
  test(`Connections dialogs stay contained and return focus after submission at ${width}px`, async ({ page }) => {
    await installDirectoryReviewArchive(page);
    const subjectName = width === 390 ? 'a-very-long-synthetic-person-name-without-spaces@example.test' : 'Synthetic One';
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
    const add = page.getByRole('button', { name: 'Add connection…', exact: true });
    await add.click();
    const dialog = page.getByRole('dialog', { name: 'Add connection', exact: true });
    await expect(dialog.locator('.sentence strong')).toHaveText(subjectName);
    expect(await dialog.locator('.kit-typeahead__value-text').evaluateAll(labels => labels.every(label => label.scrollWidth <= label.clientWidth))).toBe(true);
    const closedPeople = await dialog.getByRole('button', { name: 'Person', exact: true }).boundingBox();
    await dialog.getByRole('button', { name: 'Connection type: Type', exact: true }).click();
    await expect(dialog.getByRole('treeitem', { name: 'Family', exact: true })).toBeVisible();
    const panel = dialog.locator('.kit-typeahead__panel');
    const bounds = await panel.boundingBox();
    const bodyBounds = await dialog.locator('.sentence').boundingBox();
    expect(bounds!.x).toBeCloseTo(bodyBounds!.x, 0);
    expect(bounds!.width).toBeCloseTo(bodyBounds!.width, 0);
    expect(await dialog.getByRole('button', { name: 'Person', exact: true }).boundingBox()).toEqual(closedPeople);
    expect(bounds!.x).toBeGreaterThanOrEqual(0);
    expect(bounds!.x + bounds!.width).toBeLessThanOrEqual(width);
    expect(bounds!.y + bounds!.height).toBeLessThanOrEqual(900);
    await dialog.getByRole('treeitem', { name: 'Child', exact: true }).click();
    await dialog.getByRole('button', { name: 'Person', exact: true }).click();
    expect((await dialog.getByRole('combobox', { name: 'Person', exact: true }).boundingBox())!.y).toBe(closedPeople!.y);
    await expect(dialog.getByRole('option', { name: /Synthetic Two/ })).toBeVisible();
    const peopleBounds = await panel.boundingBox();
    expect(peopleBounds!.x).toBeGreaterThanOrEqual(0);
    expect(peopleBounds!.x + peopleBounds!.width).toBeLessThanOrEqual(width);
    expect(peopleBounds!.y).toBeGreaterThanOrEqual(0);
    expect(peopleBounds!.y + peopleBounds!.height).toBeLessThanOrEqual(900);
    expect(await panel.locator('.kit-typeahead__body').evaluate(element => element.scrollHeight > element.clientHeight)).toBe(true);
    await dialog.getByRole('option', { name: /Synthetic Two/ }).click();
    const geometry = await dialog.evaluate(element => {
      const dates = [...element.querySelectorAll('.date-range .kit-form-field')].map(label => label.getBoundingClientRect());
      const sections = [...element.querySelectorAll('.editor > section')].map(section => section.getBoundingClientRect());
      return { overflow: [...element.querySelectorAll('.editor, .editor section, .sentence, .date-range')].some(child => child.scrollWidth > child.clientWidth), dates: dates.map(({ x, y }) => ({ x, y })), gap: sections[1].top - sections[0].bottom };
    });
    expect(geometry.overflow).toBe(false);
    expect(geometry.gap).toBeGreaterThan(0);
    if (width === 1440) expect(geometry.dates[0].y).toBe(geometry.dates[1].y);
    const start = dialog.getByRole('textbox', { name: 'Start date' });
    const end = dialog.getByRole('textbox', { name: 'End date' });
    expect((await start.boundingBox())!.width).toBeGreaterThan(100);
    expect((await end.boundingBox())!.width).toBeGreaterThan(100);
    await dialog.getByRole('textbox', { name: 'Start date' }).press('Enter');
    await expect(dialog).toHaveCount(0);
    await expect(add).toBeFocused();
    await page.getByRole('region', { name: 'Connections', exact: true }).getByRole('button', { name: /^Actions for /  }).first().click();
    await page.getByRole('menuitem', { name: 'Edit…', exact: true }).click();
    const edit = page.getByRole('dialog', { name: 'Edit connection', exact: true });
    expect((await edit.boundingBox())!.width).toBe(Math.min(560, width - 32));
    await edit.getByRole('button', { name: 'Close' }).click();
    await expect(edit).toHaveCount(0);
  });
}

for (const width of [1440, 390]) {
  test(`the connection sentence's two 32 pickers fill the row at ${width}px`, async ({ page }, testInfo) => {
    const errors: string[] = [];
    page.on('pageerror', error => errors.push(error.message));
    await installDirectoryReviewArchive(page);
    await page.route('**/api/v1/people/7/profile', route => route.fulfill({ json: {
      person: { ...person(7), display_name: 'Synthetic One' }, names: [], contact_points: [], addresses: [], dates: [], categories: [], media: [],
    } }));
    await page.setViewportSize({ width, height: 844 });
    await page.goto('/?explore=' + encodeURIComponent(JSON.stringify({ workspace: 'relationships', directoryPersonID: 7 })));
    await page.getByRole('radio', { name: 'Details', exact: true }).click();
    await setKitTheme(page, 'dark');
    await page.getByRole('button', { name: 'Add connection…', exact: true }).click();
    const dialog = page.getByRole('dialog', { name: 'Add connection', exact: true });
    const sentence = (await dialog.locator('.sentence').boundingBox())!;
    const type = (await dialog.getByRole('button', { name: /^Connection type:/ }).boundingBox())!;
    const people = (await dialog.getByRole('button', { name: 'Person', exact: true }).boundingBox())!;
    expect([type.height, people.height]).toEqual([32, 32]);
    const name = (await dialog.locator('.sentence strong').boundingBox())!;
    const dates = (await dialog.locator('.date-range').boundingBox())!;
    // Person ends at the dates' right edge.
    expect(people.x + people.width).toBeCloseTo(dates.x + dates.width, 0);
    if (width === 1440) {
      // One line on the date pair's columns: the name and Type end where Start date does, and Person starts where End date does.
      const [start, end] = await dialog.locator('.date-range > .field').evaluateAll(fields => fields.map(field => field.getBoundingClientRect().toJSON() as { x: number; width: number }));
      expect(people.y).toBe(type.y);
      expect(type.x + type.width).toBeCloseTo(start.x + start.width, 0);
      expect(people.x).toBeCloseTo(end.x, 0);
      expect(people.width).toBeCloseTo(end.width, 0);
      expect(type.x - (name.x + name.width)).toBeCloseTo(8, 0);
    } else {
      expect(people.width).toBeCloseTo(type.width, 0);
      // The name on its own line, then each picker full width, 12 apart like stacked fields.
      expect(type.y - (name.y + name.height)).toBe(16);
      expect(people.y - (type.y + type.height)).toBe(16);
      expect([type.x, people.x]).toEqual([sentence.x, sentence.x]);
      expect(type.width).toBeCloseTo(sentence.width, 0);
    }
    // The sentence and the dates are two groups, 16 apart, and the body never scrolls sideways.
    const geometry = await dialog.evaluate(element => {
      const [who, when] = [...element.querySelectorAll('.editor > section')].map(section => section.getBoundingClientRect());
      const content = element.querySelector('.dialog-content')!;
      return { gap: when.top - who.bottom, overflow: content.scrollWidth - content.clientWidth };
    });
    expect(geometry).toEqual({ gap: 24, overflow: 0 });
    expect(errors).toEqual([]);
    await page.screenshot({ path: testInfo.outputPath('dialog.png'), fullPage: true });
  });
}

test('a custom connection label reads as written in the sentence', async ({ page }, testInfo) => {
  const errors: string[] = [];
  page.on('pageerror', error => errors.push(error.message));
  await installDirectoryReviewArchive(page);
  await page.route('**/api/v1/relationship-types', route => route.fulfill({ json: { relationship_types: [relationshipType()] } }));
  await page.goto('/?explore=' + encodeURIComponent(JSON.stringify({ workspace: 'relationships', directoryPersonID: 7 })));
  await setKitTheme(page, 'dark');
  await page.getByRole('radio', { name: 'Details', exact: true }).click();
  await page.getByRole('button', { name: 'Add connection…', exact: true }).click();
  const dialog = page.getByRole('dialog', { name: 'Add connection', exact: true });
  await dialog.getByRole('button', { name: 'Connection type: Type', exact: true }).click();
  await dialog.getByRole('treeitem', { name: 'Mentors', exact: true }).click();
  await expect(dialog.getByRole('button', { name: 'Connection type: mentors', exact: true })).toBeVisible();
  await page.screenshot({ path: testInfo.outputPath('custom-connection-label.png'), fullPage: true });
  expect(errors).toEqual([]);
});
