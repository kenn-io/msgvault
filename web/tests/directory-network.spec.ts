import { expect, test } from '@playwright/test';

import { installMixedArchive } from './e2e/fixtures/mixed-archive';

function directoryURL(personID: number): string {
  return `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'directory', directoryPersonID: personID }))}`;
}

test('Directory network shows curated connections, omits message-only contacts, and navigates durable entities', async ({ page }) => {
  await installMixedArchive(page);
  await page.route('**/api/v1/organizations/21', (route) => route.fulfill({
    headers: { ETag: '"organization-21-r2"' },
    json: {
      organization: { id: 21, revision: 2, name: 'Shared Organization', kind: 'company', created_at: '2026-01-03T12:00:00Z', updated_at: '2026-01-03T12:00:00Z' },
      names: [], contact_points: [], addresses: [], categories: [], identifiers: [], media: []
    }
  }));
  await page.route('**/api/v1/people/43*', (route) => {
    const path = new URL(route.request().url()).pathname;
    if (path === '/api/v1/people/43') return route.fulfill({ json: {
      id: 43, revision: 1, display_name: 'Curated Peer', participant_ids: [], vcard_uid: 'urn:uuid:curated-peer',
      created_at: '2026-01-03T12:00:00Z', updated_at: '2026-01-03T12:00:00Z'
    } });
    if (path.endsWith('/profile')) return route.fulfill({ json: {
      person: { id: 43, revision: 1, display_name: 'Curated Peer', participant_ids: [], vcard_uid: 'urn:uuid:curated-peer', created_at: '2026-01-03T12:00:00Z', updated_at: '2026-01-03T12:00:00Z' },
      names: [], contact_points: [], addresses: [], dates: [], categories: [], media: []
    } });
    if (path.endsWith('/attributes')) return route.fulfill({ json: { person_id: 43, attributes: [] } });
    if (path.endsWith('/contact-state')) return route.fulfill({ json: { person_id: 43, cadence_status: 'unknown', computed_at: '2026-01-03T12:00:00Z', interaction_count: 0, stale: false } });
    if (path.endsWith('/employments')) return route.fulfill({ json: { employments: [] } });
    if (path.endsWith('/relationships')) return route.fulfill({ json: { relationships: [] } });
    if (path.endsWith('/days')) return route.fulfill({ json: { person_id: 43, days: [], total_count: 0 } });
    if (path.endsWith('/files/search')) return route.fulfill({ json: { files: [], total_count: 0, cache_revision: 'synthetic', search_provenance: {} } });
    if (path.endsWith('/network')) return route.fulfill({ json: {
      root_person_id: 43, depth: 1, truncated: false,
      nodes: [{ id: 'person:43', kind: 'person', entity_id: 43, label: 'Curated Peer', hop: 0 }], edges: []
    } });
    return route.abort();
  });

  await page.setViewportSize({ width: 1280, height: 900 });
  await page.goto(directoryURL(42));
  await expect(page.getByRole('heading', { name: 'Archive Person' })).toBeVisible();
  await page.getByRole('tab', { name: 'Network' }).click();

  const list = page.getByRole('list', { name: 'Directory network connections' });
  await expect(list).toContainText('Archive Person works with Curated Peer');
  await expect(list).toContainText('Curated Peer Engineer Shared Organization');
  await expect(list).not.toContainText('Message-only Contact');
  await expect(page.locator('.person-network > .projection > svg')).toHaveAttribute('aria-hidden', 'true');

  await page.getByRole('button', { name: 'Open organization Shared Organization' }).click();
  await expect(page.getByRole('tab', { name: 'Organizations' })).toHaveAttribute('aria-selected', 'true');
  const organizationEditor = page.getByRole('dialog', { name: 'Edit Shared Organization' });
  await expect(organizationEditor).toBeVisible();
  await organizationEditor.getByRole('button', { name: 'Close organization editor' }).click();

  await page.getByRole('tab', { name: 'Network' }).click();
  await page.getByRole('button', { name: 'Open person Curated Peer' }).first().click();
  await expect(page).toHaveURL(/directoryPersonID%22%3A43/);
  await expect(page.getByRole('heading', { name: 'Curated Peer' })).toBeVisible();
});
