import { expect, test } from '@playwright/test';

import { installMixedArchive } from './e2e/fixtures/mixed-archive';

function directoryURL(personID: number): string {
  return `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'directory', directoryPersonID: personID }))}`;
}

test('Directory Network keeps desktop and phone section geometry and opens organizations and people', async ({ page }) => {
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

  const network = page.getByRole('region', { name: 'Network', exact: true });
  await expect(network.locator('.summary')).not.toHaveText(/Loading/);
  for (const width of [1280, 390]) {
    await page.setViewportSize({ width, height: 900 });
    await expect.poll(() => page.evaluate(() => {
      const pane = document.querySelector('section.person-page [data-scroll-pane]')!;
      return { overflow: pane.scrollWidth - pane.clientWidth, fits: pane.getBoundingClientRect().right <= window.innerWidth };
    }), { message: 'the resized pane fits before Network is measured' }).toEqual({ overflow: 0, fits: true });
    const geometry = await network.evaluate(section => {
      const header = section.querySelector('header')!.getBoundingClientRect();
      return {
        header: header.height,
        control: section.querySelector('[role="radiogroup"]')!.getBoundingClientRect().height,
        summaryGap: section.querySelector('.summary')!.getBoundingClientRect().top - header.bottom
      };
    });
    expect(geometry.header, 'a 28 section header').toBe(28);
    expect(geometry.control, 'its depth control is 28').toBe(28);
    expect(geometry.summaryGap, 'the summary sits 8 below the header').toBe(8);
  }

  await page.getByRole('radio', { name: '2 steps', exact: true }).click();
  const further = network.getByRole('region', { name: 'Network at 2 steps' });
  await expect(further).toHaveAttribute('aria-busy', 'false');
  const linkColor = await network.getByRole('button', { name: 'Shared Organization' }).evaluate(button => getComputedStyle(button).color);
  await network.getByRole('button', { name: 'Shared Organization' }).click();
  const organizationEditor = page.getByRole('dialog', { name: 'Shared Organization', exact: true });
  await expect(organizationEditor).toBeVisible();
  await expect(organizationEditor.getByRole('textbox', { name: 'Organization name' })).toHaveValue('Shared Organization');
  await organizationEditor.getByRole('button', { name: 'Close', exact: true }).click();
  await network.getByRole('radio', { name: 'Direct', exact: true }).click();
  const direct = network.getByRole('button', { name: 'Curated Peer', exact: true });
  await expect(direct).toHaveCSS('color', linkColor);
  const peer = { id: 43, revision: 1, display_name: 'Curated Peer', participant_ids: [], vcard_uid: 'urn:uuid:curated-peer', created_at: '2026-01-03T12:00:00Z', updated_at: '2026-01-03T12:00:00Z' };
  await page.route('**/api/v1/people/43', route => route.fulfill({ json: peer }));
  await page.route('**/api/v1/people/43/profile', route => route.fulfill({ json: { person: peer, names: [], contact_points: [], addresses: [], dates: [], categories: [], media: [] } }));
  await direct.click();
  await expect(page.getByRole('heading', { name: 'Curated Peer', exact: true })).toBeVisible();
});
