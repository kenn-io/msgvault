import { setKitTheme } from './kit-ui';
import { expect, test } from '@playwright/test';
import { installDirectoryReviewArchive } from './e2e/fixtures/mixed-archive';
import { person } from '../src/lib/directory/person-fixtures.test-support';
import { directoryPerson, relationshipType } from '../src/lib/directory/entity-data.test-support';
import { relationship } from '../src/lib/directory/entity-fixtures.test-support';

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
    const savedRelationship = relationship(50, 1, { relationship_type_id: 1, type_slug: 'parent', source_person_id: 9, target_person_id: 7, forward_label: 'parent', reverse_label: 'child' });
    let saved = false;
    await page.route('**/api/v1/person-relationships', async route => {
      saved = true;
      await route.fulfill({ status: 201, json: savedRelationship });
    });
    await page.route('**/api/v1/people/7/relationships*', route => route.fulfill({ json: { relationships: saved ? [{
      counterpart_person_id: 9, counterpart_display_name: 'Synthetic Two', counterpart_label: 'parent', direction: 'incoming',
      relationship: savedRelationship
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

for (const width of [1440, 390]) {
  test(`Add connection's pickers keep their words and insets at ${width}px`, async ({ page }) => {
    await installDirectoryReviewArchive(page);
    await page.setViewportSize({ width, height: 900 });
    let typesFail = true;
    await page.route('**/api/v1/relationship-types', route => typesFail
      ? route.fulfill({ status: 503, json: { message: 'Synthetic catalog unavailable' } })
      : route.fulfill({ json: { relationship_types: [relationshipType({ id: 1, slug: 'friend', forward_label: 'friend', reverse_label: 'friend', is_symmetric: true, ownership: 'system' })] } }));
    await page.route('**/api/v1/people/directory*', route => new URL(route.request().url()).searchParams.get('q')
      ? route.fulfill({ status: 503, json: { message: 'Synthetic search unavailable' } })
      : route.fulfill({ json: { people: [directoryPerson(9)] } }));
    await page.goto('/?explore=' + encodeURIComponent(JSON.stringify({ workspace: 'relationships', directoryPersonID: 7 })));
    await page.getByRole('radio', { name: 'Details', exact: true }).click();
    const connections = page.getByRole('heading', { name: 'Open loops', exact: true });
    const before = (await connections.boundingBox())!.y;
    await page.getByRole('button', { name: 'Add connection…', exact: true }).click();
    const dialog = page.getByRole('dialog', { name: 'Add connection', exact: true });
    // The page behind the dialog stays where it was.
    expect((await connections.boundingBox())!.y).toBe(before);
    // On a phone an empty End date holds no line of its own: Notes is a field gap below it.
    const end = await dialog.getByRole('textbox', { name: 'End date', exact: true }).evaluate(input => input.closest('.kit-text-input')!.getBoundingClientRect().bottom);
    const notes = (await dialog.locator('label.notes').boundingBox())!;
    expect(notes.y - end).toBe(width === 390 ? 16 : 36);

    // The open search shows the closed picker's words, and a failure keeps Retry on its sentence, sits 4 from the
    // panel's top and bottom with no divider under it or frame of its own, and folds the server's words under Details.
    await dialog.getByRole('button', { name: 'Connection type: Type', exact: true }).click();
    const typeSearch = dialog.getByRole('combobox', { name: 'Connection type', exact: true });
    const notice = dialog.getByRole('alert').filter({ hasText: "Couldn't load connection types." });
    const message = (await notice.getByText("Couldn't load connection types.").boundingBox())!;
    const retry = (await notice.getByRole('button', { name: 'Retry', exact: true }).boundingBox())!;
    expect(retry.y + retry.height / 2).toBeCloseTo(message.y + message.height / 2, 0);
    await expect(notice.getByText('Details', { exact: true })).toBeVisible();
    const errorInset = (failure = notice) => failure.evaluate(element => {
      const panel = element.closest('.kit-typeahead__panel')!, box = panel.getBoundingClientRect(), border = parseFloat(getComputedStyle(panel).borderTopWidth);
      const notice = element.closest('.kit-notice')!, card = notice.getBoundingClientRect(), header = getComputedStyle(element.closest('.kit-typeahead__header')!);
      return [card.top - box.top - border, box.bottom - border - card.bottom, header.borderBottomWidth, getComputedStyle(notice).borderTopWidth];
    });
    await expect.poll(() => errorInset()).toEqual([4, 4, '0px', '0px']);
    typesFail = false;
    await notice.getByRole('button', { name: 'Retry', exact: true }).click();
    await page.getByRole('heading', { name: 'Add connection', exact: true }).click();
    await dialog.getByRole('button', { name: 'Connection type: Type', exact: true }).click();
    // The highlighted choice sits 4 from both of the panel's sides, as a menu item does.
    const friend = dialog.getByRole('treeitem', { name: 'Friend', exact: true });
    await friend.hover();
    const inset = await friend.evaluate(row => {
      const panel = row.closest('.kit-typeahead__panel')!, box = panel.getBoundingClientRect(), item = row.getBoundingClientRect(), border = parseFloat(getComputedStyle(panel).borderLeftWidth);
      return [item.left - box.left - border, box.right - border - item.right];
    });
    expect(inset).toEqual([4, 4]);
    await typeSearch.fill('NoSuchType');
    // The empty line reads as the app's others: upright Meta in the muted color.
    const empty = await dialog.locator('.kit-typeahead__empty').evaluate(element => getComputedStyle(element).fontStyle);
    expect(empty).toBe('normal');
    await typeSearch.press('Escape');

    await dialog.getByRole('button', { name: 'Person', exact: true }).click();
    await dialog.getByRole('combobox', { name: 'Person', exact: true }).fill('Sela');
    const searchFailure = dialog.getByRole('alert').filter({ hasText: "Couldn't search people." });
    await expect(searchFailure).toBeVisible();
    await expect(searchFailure.getByText('Details', { exact: true })).toBeVisible();
    await expect.poll(() => errorInset(searchFailure)).toEqual([4, 4, '0px', '0px']);
  });
}
