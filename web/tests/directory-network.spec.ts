import { expect, test } from '@playwright/test';

import type { NetworkEdge } from '../src/lib/api/generated/models';
import { installMixedArchive } from './e2e/fixtures/mixed-archive';

function directoryURL(personID: number): string {
  return `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'directory', directoryPersonID: personID }))}`;
}

test('Directory network lists who is further out and opens organizations', async ({ page }) => {
  await installMixedArchive(page);
  await page.route('**/api/v1/organizations/21', (route) => route.fulfill({
    headers: { ETag: '"organization-21-r2"' },
    json: {
      organization: { id: 21, revision: 2, name: 'Shared Organization', kind: 'company', created_at: '2026-01-03T12:00:00Z', updated_at: '2026-01-03T12:00:00Z' },
      names: [], contact_points: [], addresses: [], categories: [], identifiers: [], media: []
    }
  }));

  await page.setViewportSize({ width: 1280, height: 900 });
  await page.goto(directoryURL(42));
  await expect(page.getByRole('heading', { name: 'Archive Person' })).toBeVisible();
  await page.getByRole('radio', { name: 'Details', exact: true }).click();

  await page.getByRole('radio', { name: '2 steps', exact: true }).click();
  const network = page.getByRole('region', { name: 'Network', exact: true });
  const further = network.getByRole('region', { name: 'Network at 2 steps' });
  await expect(further).toHaveAttribute('aria-busy', 'false');
  const linkColor = await network.getByRole('button', { name: 'Shared Organization' }).evaluate(button => getComputedStyle(button).color);
  await network.getByRole('button', { name: 'Shared Organization' }).click();
  const organizationEditor = page.getByRole('dialog', { name: 'Shared Organization', exact: true });
  await expect(organizationEditor).toBeVisible();
  await expect(organizationEditor.getByRole('textbox', { name: 'Organization name' })).toHaveValue('Shared Organization');
  await organizationEditor.getByRole('button', { name: 'Close', exact: true }).click();
  const peer = { id: 43, revision: 1, display_name: 'Curated Peer', participant_ids: [], vcard_uid: 'urn:uuid:curated-peer', created_at: '2026-01-03T12:00:00Z', updated_at: '2026-01-03T12:00:00Z' };
  await page.route('**/api/v1/people/43', route => route.fulfill({ json: peer }));
  await page.route('**/api/v1/people/43/profile', route => route.fulfill({ json: { person: peer, names: [], contact_points: [], addresses: [], dates: [], categories: [], media: [] } }));
  await network.getByRole('radio', { name: 'Direct', exact: true }).click();
  const direct = network.getByRole('button', { name: 'Curated Peer', exact: true });
  await expect(direct).toHaveCSS('color', linkColor);
  await direct.click();
  await expect(page.getByRole('heading', { name: 'Curated Peer', exact: true })).toBeVisible();

});

test('Directory network keeps long labels and shared paths reachable', async ({ page }) => {
  await installMixedArchive(page);
  const count = 3;
  const nodes = [
    { id: 'person:42', kind: 'person', entity_id: 42, label: 'Archive Person With A Very Long Full Name Here', hop: 0 },
    ...Array.from({ length: count }, (_, index) => ({ id: `person:${43 + index}`, kind: 'person', entity_id: 43 + index, label: `Curated Peer With A Very Long Full Name ${index + 1}`, hop: 1 })),
    { id: 'organization:21', kind: 'organization', entity_id: 21, label: 'Shared Organization', hop: 2 }
  ];
  const edges: NetworkEdge[] = Array.from({ length: count }, (_, index) => ({
    id: `root:${index}`, kind: 'relationship', source_node_id: 'person:42', target_node_id: `person:${43 + index}`, label: 'knows'
  }));
  for (let index = 0; index < count; index++) edges.push({
    id: `employment:${index}`, kind: 'employment', source_node_id: `person:${43 + index}`, target_node_id: 'organization:21',
    label: 'Senior principal engineer of distributed computing systems'
  });
  await page.route('**/api/v1/people/42/network*', (route) => {
    const depth = Number(new URL(route.request().url()).searchParams.get('depth'));
    const visible = nodes.filter((node) => node.hop <= depth);
    const hops = new Map(visible.map((node) => [node.id, node.hop]));
    return route.fulfill({ json: { root_person_id: 42, depth, truncated: false, nodes: visible, edges: edges.filter((edge) => hops.has(edge.source_node_id) && hops.has(edge.target_node_id) && Math.min(hops.get(edge.source_node_id)!, hops.get(edge.target_node_id)!) < depth) } });
  });
  await page.goto(directoryURL(42));
  await page.getByRole('radio', { name: 'Details', exact: true }).click();
  await page.getByRole('radio', { name: '2 steps', exact: true }).click();
  const network = page.getByRole('region', { name: 'Network', exact: true });
  const further = network.getByRole('region', { name: 'Network at 2 steps' });
  await expect(further).toHaveAttribute('aria-busy', 'false');
  await expect(further.locator('.detail-row')).toHaveCount(1);
  const name = further.getByRole('button', { name: 'Shared Organization' });
  await name.scrollIntoViewIfNeeded();
  expect(await name.evaluate((button) => {
    const box = button.getBoundingClientRect();
    return document.elementFromPoint(box.x + box.width / 2, box.y + box.height / 2) === button;
  })).toBe(true);
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
});

test('the person page and its Details sections sit on the Layout scale', async ({ page }) => {
  await installMixedArchive(page);
  await page.setViewportSize({ width: 1440, height: 900 });
  await page.goto(directoryURL(42));
  await expect(page.getByRole('heading', { name: 'Archive Person' })).toBeVisible();
  await page.getByRole('radio', { name: 'Details', exact: true }).click();
  const network = page.getByRole('region', { name: 'Network', exact: true });
  await expect(network.locator('.summary')).not.toHaveText(/Loading/);
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
    const section = document.querySelector('section[aria-label="Network"]')!;
    const sectionHeader = box(section.querySelector('header'));
    return {
      avatar: avatar.height, textCenter: text.top + text.height / 2 - (avatar.top + avatar.height / 2),
      headerPad: avatar.top - header.top, tabsGap: tabs.top - header.bottom, tabs: tabs.height,
      gutter: style.scrollbarGutter, left: content.left - box(pane).left, right: box(pane).right - content.right,
      padding: [style.paddingTop, style.paddingBottom],
      sectionHeader: sectionHeader.height, control: box(section.querySelector('header [role="radiogroup"]')).height,
      summaryGap: box(section.querySelector('.summary')).top - sectionHeader.bottom,
      tasksAdd: Boolean(document.querySelector('section[aria-label="Tasks"] header')),
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
  expect(geometry.sectionHeader, 'a 28 section header').toBe(28);
  expect(geometry.control, 'its depth control is 28').toBe(28);
  expect(geometry.summaryGap, 'the summary sits 8 below the header').toBe(8);
  expect(geometry.tasksAdd, 'Tasks has a section header').toBe(true);
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
});
