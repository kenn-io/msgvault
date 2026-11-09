import { setKitTheme } from './kit-ui';
import { expect, test, type Locator } from '@playwright/test';
import { installMixedArchive } from './e2e/fixtures/mixed-archive';

import { organization as makeOrganization, employment as makeEmployment } from '../src/lib/directory/entity-data.test-support';

const organization = makeOrganization(21, 1, 'Example Org');
const employment = makeEmployment(11, 1, { person_id: 42, is_primary: true });

test.beforeEach(async ({ page }) => {
  await installMixedArchive(page);
  await page.route('**/api/v1/organizations?*', route => route.fulfill({ json: { organizations: [organization], total: 1 } }));
  await page.route('**/api/v1/organizations/21', route => route.fulfill({ headers: { ETag: '"organization-21-r1"' }, json: route.request().method() === 'PATCH' ? organization : { organization, names: [], categories: [], addresses: [], contact_points: [], identifiers: [], media: [] } }));
  await page.route('**/api/v1/people/42/employments', route => route.fulfill({ json: { employments: [employment] } }));
  await page.route('**/api/v1/employments/11', route => route.fulfill({ headers: { ETag: '"employment-11-r1"' }, json: employment }));
  await page.goto(`/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'directory', directoryPersonID: 42 }))}`);
  await page.getByRole('radio', { name: 'Details', exact: true }).click();
});

for (const width of [1440, 390]) {
  test(`work and organization flows stay contained at ${width}px`, async ({ page }) => {
    await page.setViewportSize({ width, height: 844 });
    await page.getByRole('button', { name: 'Example Org', exact: true }).click();
    const editor = page.getByRole('dialog', { name: 'Example Org', exact: true });
    await expect(editor.getByRole('textbox', { name: 'Organization name' })).toHaveValue('Example Org');
    const bounds = await editor.boundingBox();
    for (const name of ['Delete organization…', 'Cancel', 'Save organization']) {
      const button = await editor.getByRole('button', { name, exact: true }).boundingBox();
      expect(button!.x).toBeGreaterThanOrEqual(bounds!.x);
      expect(button!.x + button!.width).toBeLessThanOrEqual(bounds!.x + bounds!.width);
    }
    await page.route('**/api/v1/organizations/21/employments*', route => route.fulfill({json:{employments:[employment]}}));
    await editor.getByRole('button', { name: 'Delete organization…' }).click();
    const deletion = page.getByRole('dialog', { name: 'Delete Example Org?' });
    // The job blocking the delete is this person's own, so its row leads straight to it; the name is a link.
    const holder = deletion.locator('.holders').getByRole('button', { name: 'Archive Person', exact: true });
    expect(await holder.evaluate(element => getComputedStyle(element).color)).toBe(await page.evaluate(() => {
      const probe = document.body.appendChild(document.createElement('a'));
      const color = getComputedStyle(probe).color;
      probe.remove();
      return color;
    }));
    await expect(page.getByRole('dialog')).toHaveCount(1);
    const geometry = await deletion.evaluate(element => ({ width:element.clientWidth, scroll:element.scrollWidth }));
    expect(geometry.scroll).toBeLessThanOrEqual(geometry.width);
    await deletion.getByRole('button',{name:'Back to Example Org',exact:true}).click();
  });
}

test('opening Add job leaves the page behind it in place', async ({ page }) => {
  const connections = page.getByRole('heading', { name: 'Connections', exact: true });
  const before = (await connections.boundingBox())!.y;
  await page.getByRole('button', { name: 'Add job…', exact: true }).click();
  await expect(page.getByRole('dialog', { name: 'Add job', exact: true })).toBeVisible();
  expect((await connections.boundingBox())!.y).toBe(before);
});

test('a long delete title wraps on a phone instead of cutting off', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await page.route('**/api/v1/people/42/employments', route => route.fulfill({ json: { employments: [{ ...employment, title: 'Senior landscape garden designer' }] } }));
  await page.route('**/api/v1/employments/11', route => route.fulfill({ headers: { ETag: '"employment-11-r1"' }, json: { ...employment, title: 'Senior landscape garden designer' } }));
  await page.reload();
  await page.getByRole('radio', { name: 'Details', exact: true }).click();
  await page.getByRole('region', { name: 'Work', exact: true }).getByRole('button', { name: /^Actions for / }).click();
  await page.getByRole('menuitem', { name: 'Delete…', exact: true }).click();
  const title = page.getByRole('dialog', { name: 'Delete Senior landscape garden designer at Example Org?' }).getByRole('heading');
  const geometry = await title.evaluate(element => ({ clipped: element.scrollWidth > element.clientWidth, lines: Math.round(element.getBoundingClientRect().height / parseFloat(getComputedStyle(element).lineHeight)) }));
  expect(geometry.clipped).toBe(false);
  expect(geometry.lines).toBeGreaterThan(1);
});

test('Enter saves organization and work drafts through the shared dialog', async ({page}) => {
  await page.getByRole('button',{name:'Example Org',exact:true}).click();
  const organizationDialog = page.getByRole('dialog',{name:'Example Org',exact:true});
  const name = organizationDialog.getByRole('textbox',{name:'Organization name',exact:true});
  await name.fill('Renamed Example');
  const patch = page.waitForRequest(request => request.method() === 'PATCH' && request.url().endsWith('/organizations/21'));
  await name.press('Enter');
  await patch;
  await expect(organizationDialog).toHaveCount(0);
  await page.route('**/api/v1/employments', route => route.fulfill({status:201,headers:{ETag:'"employment-12-r1"'},json:{...employment,id:12}}));
  await page.getByRole('button',{name:'Add job…',exact:true}).click();
  const work = page.getByRole('dialog',{name:'Add job',exact:true});
  await work.getByRole('button',{name:/^Organization:/}).click();
  await work.getByRole('option',{name:/^Example Org/}).click();
  const title = work.getByRole('textbox',{name:'Job title',exact:true});
  await title.fill('Designer');
  const post = page.waitForRequest(request => request.method() === 'POST' && request.url().endsWith('/employments'));
  await title.press('Enter');
  await post;
  await expect(work).toHaveCount(0);
});

// A text field's box is the kit's bordered wrapper around the input.
const field = (input: Locator) => input.evaluate(element => {
  const { x, y, width, height } = (element.closest('.kit-text-input') ?? element).getBoundingClientRect();
  return { x, y, width, height };
});

for (const width of [1440, 390]) {
  test(`Add job and Organization sit on the field scale at ${width}px`, async ({ page }, testInfo) => {
    const errors: string[] = [];
    page.on('pageerror', error => errors.push(error.message));
    await page.setViewportSize({ width, height: 844 });
    await setKitTheme(page, 'dark');
    await page.getByRole('button', { name: 'Example Org', exact: true }).click();
    const editor = page.getByRole('dialog', { name: 'Example Org', exact: true });
    await expect(editor.getByRole('textbox', { name: 'Organization name' })).toHaveValue('Example Org');
    // Kind is a 32 field with Body text at 400, like Domain beside it; the add links are 28 targets 12 apart.
    const kind = await editor.locator('.kit-select-dropdown__trigger').evaluate(element => ({ height: element.getBoundingClientRect().height, weight: getComputedStyle(element).fontWeight }));
    expect(kind).toEqual({ height: 32, weight: '400' });
    expect((await field(editor.getByRole('textbox', { name: 'Organization domain' }))).height).toBe(32);
    const links = await editor.locator('.add-link').evaluateAll(elements => elements.map(element => element.getBoundingClientRect()).map(({ left, right, height }) => ({ left, right, height })));
    expect(links.map(link => link.height)).toEqual([28, 28]);
    expect(links[1].left - links[0].right).toBe(12);
    await editor.getByRole('button', { name: 'Close', exact: true }).click();

    await page.getByRole('button', { name: 'Add job…', exact: true }).click();
    const add = page.getByRole('dialog', { name: 'Add job', exact: true });
    const box = (name: string) => field(add.getByRole('textbox', { name, exact: true }));
    const organization = (await add.getByRole('button', { name: /^Organization:/ }).boundingBox())!;
    const [title, role, start, end] = await Promise.all(['Job title', 'Role on the team', 'Start date', 'End date'].map(name => box(name)));
    expect(organization.height).toBe(32);
    for (const input of [title, role, start, end]) expect(input.height).toBe(32);
    // Job title | Role and Start | End are the same two equal columns, 12 apart, so the four fields line up.
    expect(start.x).toBe(title.x);
    expect(start.width).toBe(title.width);
    expect(end.width).toBe(role.width);
    expect(end.x).toBe(role.x);
    if (width === 1440) {
      expect(role.width).toBe(title.width);
      expect(role.x - (title.x + title.width)).toBe(12);
    } else {
      // On a phone the pair stacks, each field the body's full width.
      expect(role.x).toBe(title.x);
      expect(role.width).toBe(organization.width);
    }
    // Fields are 12 apart: the field above ends 12 above the next field's label.
    const titleLabel = (await add.locator('label[for="work-title"]').boundingBox())!;
    expect(titleLabel.y - (organization.y + organization.height)).toBe(16);
    // End date holds its empty hint line, level with Start's hint, and Current sits 8 below it.
    const current = (await add.locator('.date-range .current').boundingBox())!;
    const [hint, endHint] = await Promise.all([0, 1].map(async index => (await add.locator('.date-range .note').nth(index).boundingBox())!));
    expect(endHint.y - (end.y + end.height)).toBe(4);
    expect(current.y - (endHint.y + endHint.height)).toBe(8);
    if (width === 1440) expect(endHint.y).toBe(hint.y);
    await add.getByRole('checkbox', { name: 'Current', exact: true }).uncheck();
    expect((await add.locator('.date-range .current').boundingBox())!.y).toBe(current.y);
    // A focused invalid date has one outline, the ring in the error's red over the border, and its error 4 below it.
    const startDate = add.getByRole('textbox', { name: 'Start date', exact: true });
    await startDate.fill('2026-99');
    const ring = await startDate.evaluate(input => {
      const box = input.closest('.kit-text-input')!, style = getComputedStyle(box);
      const error = box.closest('.kit-form-field')!.querySelector('.kit-form-field__error')!.getBoundingClientRect();
      return { color: style.outlineColor === style.borderTopColor, offset: style.outlineOffset, gap: error.top - box.getBoundingClientRect().bottom };
    });
    expect(ring).toEqual({ color: true, offset: '-2px', gap: 4 });
    // With one organization, the open picker ends 4 below it, as it starts 4 above it.
    await add.getByRole('button', { name: /^Organization:/ }).click();
    const inset = await add.getByRole('option', { name: /^Example Org/ }).evaluate(row => {
      const panel = row.closest('.kit-typeahead__panel')!, box = panel.getBoundingClientRect(), item = row.getBoundingClientRect(), border = parseFloat(getComputedStyle(panel).borderTopWidth);
      return [item.top - box.top - border, box.bottom - border - item.bottom];
    });
    expect(inset).toEqual([4, 4]);
    await add.getByRole('combobox', { name: 'Organization', exact: true }).fill('Example School');
    await add.getByRole('option', { name: 'Create organization “Example School”', exact: true }).click();
    if (width === 1440) {
      // The new organization's Kind | Domain split where Job title | Role below them does.
      const [kind, domain] = await add.locator('.creation .form-pair > *').evaluateAll(cells => cells.map(cell => cell.getBoundingClientRect().toJSON() as { x: number; right: number }));
      const [title, role] = await add.locator('.editor > .form-pair').first().locator('> *').evaluateAll(cells => cells.map(cell => cell.getBoundingClientRect().toJSON() as { x: number; right: number }));
      expect([kind.right, domain.x, domain.right]).toEqual([title.right, role.x, role.right]);
    }
    expect(errors).toEqual([]);
    await page.screenshot({ path: testInfo.outputPath('dialog.png'), fullPage: true });
  });
}

test('an organization that fails to load shows its error 12 inside the dialog on both sides', async ({ page }) => {
  await expect(page.getByRole('button', { name: 'Example Org', exact: true })).toBeVisible();
  await page.route('**/api/v1/organizations/21', route => route.fulfill({ status: 503, json: { message: 'Synthetic organization unavailable' } }));
  await page.getByRole('button', { name: 'Example Org', exact: true }).click();
  const editor = page.getByRole('dialog', { name: 'Example Org', exact: true });
  const notice = editor.getByRole('alert').filter({ hasText: "Couldn't load this organization." });
  await expect(notice).toBeVisible();
  const gaps = await notice.evaluate(element => {
    const box = element.closest('.dialog-notice')!.getBoundingClientRect(), content = element.closest('.dialog-content')!, style = getComputedStyle(content), pane = content.getBoundingClientRect();
    return [box.top - pane.top - parseFloat(style.paddingTop), pane.bottom - box.bottom - parseFloat(style.paddingBottom)];
  });
  expect(gaps).toEqual([0, 0]);
});

for (const width of [1440, 390]) {
  test(`End job conflict stays contained at ${width}px`, async ({ page }, testInfo) => {
    await page.setViewportSize({ width, height: 844 });
    await setKitTheme(page, 'dark');
    let reads = 0;
    await page.route('**/api/v1/employments/11', route => {
      reads++;
      return route.fulfill(reads === 1
        ? { headers: { ETag: '"employment-11-r1"' }, json: employment }
        : { headers: { ETag: '"employment-11-r2"' }, json: { ...employment, revision: 2, start_date: { year: 2024 } } });
    });
    await page.route('**/api/v1/employments/11/end', route => route.fulfill({ status: 412, json: { message: 'Changed elsewhere' } }));
    await page.getByRole('region', { name: 'Work', exact: true }).getByRole('button', { name: /^Actions for / }).click();
    await page.getByRole('menuitem', { name: 'End…', exact: true }).click();
    const dialog = page.getByRole('dialog', { name: 'End job', exact: true });
    const end = dialog.getByRole('textbox', { name: 'End date', exact: true });
    await end.fill('2027');
    await dialog.getByRole('button', { name: 'End', exact: true }).click();
    await expect(dialog.getByRole('region', { name: 'Saved job', exact: true })).toBeVisible();
    expect((await dialog.boundingBox())!.width).toBe(Math.min(400, width - 32));
    expect(await dialog.evaluate(element => element.scrollWidth <= element.clientWidth)).toBe(true);
    await page.screenshot({ path: testInfo.outputPath('end-conflict.png'), fullPage: true });
  });
}
