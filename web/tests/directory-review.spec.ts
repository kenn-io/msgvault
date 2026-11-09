import { person } from '../src/lib/directory/person-fixtures.test-support';
import { emptyProfile } from '../src/lib/directory/profile-fixtures.test-support';
import { expect, test } from '@playwright/test';
import type { PersonContactPoint } from '../src/lib/api/generated/models';
import { installDirectoryReviewArchive, installMixedArchive } from './e2e/fixtures/mixed-archive';
const peopleURL = '/?explore=' + encodeURIComponent(JSON.stringify({ workspace: 'relationships' }));
const duplicatesURL = '/?explore=' + encodeURIComponent(JSON.stringify({ workspace: 'relationships', peopleDuplicates: true }));

test('Person Details aligns desktop and phone content and uses shared phone editor sizes', async ({ page }) => {
  await installDirectoryReviewArchive(page);
  const profile = emptyProfile({ ...person(7, 4, [701, 702], 'Synthetic One'), created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-02T00:00:00Z' });
  profile.names = [{ person_id: 7, name_kind: 'preferred', original_value: 'Synthetic One', formatted: 'Synthetic One', is_derived: false, envelope: { id: 1, ordinal: 0, source: 'user', created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-02T00:00:00Z', vcard: {} } }];
  await page.route('**/api/v1/people/7/profile', route => route.fulfill({ headers: { ETag: '"person-7-r4"' }, json: profile }));
  await page.setViewportSize({ width: 1440, height: 900 });
  await page.goto('/?explore=' + encodeURIComponent(JSON.stringify({ workspace: 'relationships', directoryPersonID: 7 })));
  await page.getByRole('radio', { name: 'Details', exact: true }).click();
  const geometry = await page.evaluate(() => {
    const box = (element: Element | null) => element!.getBoundingClientRect();
    const person = document.querySelector('section.person-page')!;
    const header = box(person.querySelector('header'));
    const avatar = box(person.querySelector('header .avatar'));
    const text = box(person.querySelector('header h2')!.parentElement);
    const tabs = box(person.querySelector('.tabs [role="radiogroup"]'));
    const pane = person.querySelector<HTMLElement>('[data-scroll-pane]')!;
    const style = getComputedStyle(pane);
    const content = box(pane.firstElementChild);
    return {
      avatar: avatar.height, textCenter: text.top + text.height / 2 - (avatar.top + avatar.height / 2),
      headerPad: avatar.top - header.top, tabsGap: tabs.top - header.bottom, tabs: tabs.height,
      gutter: style.scrollbarGutter, left: content.left - box(pane).left, right: box(pane).right - content.right,
      padding: [style.paddingTop, style.paddingBottom],
      addLine: box(person.querySelector('.fields .add > *')).left - box(person.querySelector('.fields .detail-row .value')).left,
    };
  });
  expect(geometry.avatar, 'a 40 avatar').toBe(40);
  expect(Math.abs(geometry.textCenter), 'the name and email center on the avatar').toBeLessThanOrEqual(0.5);
  expect(geometry.headerPad, 'the header is padded 12 above the avatar').toBe(12);
  expect(geometry.tabsGap, 'the tabs sit 8 below the header').toBe(8);
  expect(geometry.tabs, 'the tabs are a 28 control').toBe(28);
  expect(geometry.gutter, 'the Details pane reserves its bar').toBe('stable');
  expect(geometry.padding, 'Details has 16 above its sections and 32 after them').toEqual(['16px', '32px']);
  expect(geometry.left, 'content is inset 16 at the left').toBe(16);
  expect(geometry.right, 'and 16 at the right, counting the reserved bar').toBe(16);
  expect(geometry.addLine, 'the add line starts at the value column').toBe(0);

  await page.setViewportSize({ width: 390, height: 844 });
  // The shell switches to its phone layout a frame after the resize, so wait for the pane to fit before measuring.
  await expect.poll(() => page.evaluate(() => {
    const pane = document.querySelector('section.person-page [data-scroll-pane]')!;
    return pane.scrollWidth - pane.clientWidth;
  }), { message: 'nothing is wider than the pane' }).toBe(0);
  const phone = await page.evaluate(() => {
    const person = document.querySelector('section.person-page')!;
    const row = person.querySelector('.fields .detail-row:has(.actions)')!;
    const label = row.querySelector('.label')!.getBoundingClientRect(), value = row.querySelector('.value')!.getBoundingClientRect();
    const name = person.querySelector('header h2')!.getBoundingClientRect(), actions = person.querySelector('header .actions')!.getBoundingClientRect();
    const pane = person.querySelector('[data-scroll-pane]')!;
    const add = person.querySelector('.fields .add > *')!.getBoundingClientRect();
    const titleLeft = document.querySelector('.heading h1, .heading h2')!.getBoundingClientRect().left;
    return { headerInset: actions.left - titleLeft, tabsInset: person.querySelector('.tabs [role="radiogroup"]')!.getBoundingClientRect().left - titleLeft, label: label.width, sameLine: Math.abs(label.top - value.top), actionsBelow: actions.top >= name.bottom, wide: pane.scrollWidth - pane.clientWidth, addLine: add.left - value.left, titleInset: row.getBoundingClientRect().left - document.querySelector('.heading h1, .heading h2')!.getBoundingClientRect().left };
  });
  expect.soft(phone.headerInset, 'the person header aligns with the People title').toBe(0);
  expect.soft(phone.tabsInset, 'the tabs align with the People title').toBe(0);
  expect(phone.titleInset, 'Details aligns with the People title on a phone').toBe(0);
  expect(phone.label, 'the label column is 120 on a phone').toBe(120);
  expect(phone.sameLine, 'the value stays beside its label').toBeLessThanOrEqual(2);
  expect(phone.addLine, 'the add line starts at the value column on a phone too').toBe(0);
  expect(phone.actionsBelow, 'Merge with… and ⋯ wrap under the name').toBe(true);
  expect(phone.wide, 'nothing is wider than the pane').toBe(0);

  await page.getByRole('button', { name: 'Add phone', exact: true }).click();
  const form = page.getByRole('form', { name: 'Edit contact points' });
  for (const field of [form.locator('.kit-text-input').first(), form.locator('select')]) {
    expect(await field.evaluate(element => ({ height: element.getBoundingClientRect().height, font: getComputedStyle(element).fontSize }))).toEqual({ height: 32, font: '16px' });
  }
});

test('People Filter restores History on reload and To review on Back', async ({ page }) => {
  await installDirectoryReviewArchive(page);
  await page.goto(peopleURL);
  await page.getByRole('button', { name: 'Filter', exact: true }).click();
  await page.getByText('Possible duplicates', { exact: true }).click();
  await expect(page.getByRole('heading', { name: 'Possible duplicates' })).toBeVisible();
  await page.getByRole('radio', { name: 'History', exact: true }).click();
  await expect(page.getByText('Nothing decided yet')).toBeVisible();
  await page.reload();
  await expect(page.getByRole('radio', { name: 'History', exact: true })).toBeChecked();
  await page.goBack();
  await expect(page.getByRole('radio', { name: 'To review', exact: true })).toBeChecked();
  await page.getByRole('radio', { name: 'History', exact: true }).click();
  await page.getByRole('button', { name: 'Back to People' }).click();
  await page.getByRole('button', { name: 'Filter', exact: true }).click();
  await page.getByText('Possible duplicates', { exact: true }).click();
  await expect(page.getByRole('radio', { name: 'To review', exact: true })).toBeChecked();
});

async function openHistoryForLayout(page: import('@playwright/test').Page, width: number) {
  await installDirectoryReviewArchive(page);
  const errors: string[] = [];
  page.on('pageerror', error => errors.push(error.message));
  const profile = { id: 7, name: 'Synthetic One' };
  const envelope = (id: number) => ({ id, ordinal: 0, source: 'user', actor: 'user', confidence: 1, active_from: '2026-01-01T00:00:00Z', created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z', vcard: {} });
  const earlierName = 'SyntheticEarlierName'.repeat(12);
  await page.setViewportSize({ width, height: 900 });
  await page.route(`**/api/v1/people/${profile.id}/profile/history`, route => route.fulfill({ json: {
    person: { id: profile.id, revision: 4, display_name: profile.name, participant_ids: [], vcard_uid: `synthetic-${profile.id}`, created_at: '2026-01-01T00:00:00Z', updated_at: '2026-07-01T00:00:00Z' },
    names: width === 1440 ? [{ person_id: profile.id, name_kind: 'formatted', formatted: profile.name, original_value: profile.name, is_derived: false, envelope: envelope(71) }] : [{ person_id: profile.id, name_kind: 'formatted', formatted: earlierName, original_value: earlierName, is_derived: false, envelope: { ...envelope(71), active_until: '2026-06-01T00:00:00Z', superseded_at: '2026-06-02T00:00:00Z' } }],
    contact_points: width === 1440 ? [{ identity_key: '["email","","","one@example.org"]', person_id: profile.id, address_kind: 'email', original_value: 'one@example.org', normalized_value: 'one@example.org', normalization: 'email', normalization_version: 1, envelope: envelope(72) } satisfies PersonContactPoint] : [],
    addresses: [], dates: [], categories: [], media: [], observations: [],
  } }));
  await page.goto('/?explore=' + encodeURIComponent(JSON.stringify({ workspace: 'relationships', directoryPersonID: 7 })));
  await page.getByRole('button', { name: 'More actions', exact: true }).click();
  await page.getByRole('menuitem', { name: 'History', exact: true }).click();
  const dialog = page.getByRole('dialog', { name: 'History', exact: true });
  return { dialog, errors, profile, earlierName };
}

test('History follows the Layout scale', async ({ page }) => {
  const { dialog, errors, profile } = await openHistoryForLayout(page, 1440);
  const names = dialog.getByRole('heading', { name: 'Names', exact: true });
  const email = dialog.getByRole('heading', { name: 'Email and phone', exact: true });
  await expect(email).toBeVisible();
  const rect = (locator: import('@playwright/test').Locator) => locator.evaluate(el => { const r = el.getBoundingClientRect(); return { top: r.top, bottom: r.bottom, height: r.height }; });
  const nameHeading = await rect(names);
  const nameRow = await rect(dialog.locator('.detail-row', { hasText: profile.name }));
  const emailHeading = await rect(email);
  expect.soft(nameRow.top - nameHeading.bottom, 'heading to rows').toBe(8);
  expect.soft(nameRow.height, 'two-line row').toBe(54);
  expect.soft(emailHeading.top - nameRow.bottom, 'between groups').toBe(24);
  expect(errors).toEqual([]);
});

test('History fits a phone', async ({ page }) => {
  const { dialog, errors, earlierName } = await openHistoryForLayout(page, 390);
  await dialog.getByText('1 earlier value', { exact: true }).click();
  await expect(dialog.getByText(earlierName)).toBeVisible();
  expect(await dialog.locator('.profile-history').evaluate(element => element.scrollWidth <= element.clientWidth)).toBe(true);
  expect(errors).toEqual([]);
});

test('address actions align with the first text line while details opens', async ({ page }) => {
  await installMixedArchive(page);
  await page.route('**/api/v1/participants/12', route => route.fulfill({ json: {
    id: 12, display_label: 'Archive Person', partial_label: false, identifiers: [
      { type: 'email', value: 'linked@example.com', participant_id: 34, is_primary: true, provenance: 'participant_identifiers' }
    ], activity_count: 3, file_count: 1, source_counts: [], cache_revision: 'synthetic',
    cluster: { canonical_id: 12, member_ids: [12, 34], edges: [{ participant_a: 12, participant_b: 34 }] }
  } }));
  await page.goto('/?explore=' + encodeURIComponent(JSON.stringify({ workspace: 'relationships', relationshipTarget: 'cluster:12' })));
  await page.getByRole('radio', { name: 'Details', exact: true }).click();
  const identity = page.getByRole('group', { name: 'Identity linked@example.com', exact: true });
  const row = identity.locator('..').locator('..');
  const summary = identity.locator('summary');
  for (const expanded of [false, true]) {
    if (expanded) await summary.click();
    const positions = await row.evaluate(element => {
      const text = document.createRange(); text.selectNodeContents(element.querySelector('summary')!);
      const bounds = text.getBoundingClientRect();
      return { text: bounds.top + bounds.height / 2, actions: [...element.querySelectorAll('button')].map(button => { const box = button.getBoundingClientRect(); return box.top + box.height / 2; }) };
    });
    expect(positions.actions).toHaveLength(2);
    for (const center of positions.actions) expect(Math.abs(center - positions.text)).toBeLessThanOrEqual(2);
  }
});

for (const width of [1440, 390]) {
  test(`first source choices align with their heading at ${width}`, async ({ page }) => {
    await installDirectoryReviewArchive(page);
    await page.route('**/api/v1/sources/status', route => route.fulfill({ json: { sources: [] } }));
    await page.route('**/api/v1/carddav/connections', route => route.fulfill({ json: { connections: [] } }));
    await page.setViewportSize({ width, height: 1000 });
    await page.goto('/?workspace=sources');
    const heading = page.getByRole('heading', { name: 'Add your first source', exact: true });
    await expect(heading).toBeVisible();
    const label = page.getByRole('button', { name: /^Gmail Includes/ }).getByText('Gmail', { exact: true });
    const titleBounds = await heading.boundingBox();
    const labelBounds = await label.boundingBox();
    expect(Math.abs(labelBounds!.x - titleBounds!.x)).toBeLessThanOrEqual(1);
  });
}
