import { expect, test } from '@playwright/test';

import type { NetworkEdge } from '../src/lib/api/generated/models';
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
  await page.route(/\/api\/v1\/people\/43(\/|\?|$)/, (route) => {
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
    return route.fallback();
  });

  await page.setViewportSize({ width: 1280, height: 900 });
  await page.goto(directoryURL(42));
  await expect(page.getByRole('heading', { name: 'Archive Person' })).toBeVisible();
  await page.getByRole('radio', { name: 'Details', exact: true }).click();

  const list = page.getByRole('list', { name: 'Network connections' });
  await expect(list).toContainText('Archive Person · works with · Curated Peer');
  await page.getByRole('radio', { name: '2 steps', exact: true }).click();
  await expect(list).toContainText('Curated Peer · Engineer · Shared Organization');
  await expect(list).not.toContainText('Message-only Contact');
  await expect(page.locator('.person-network svg')).toHaveAttribute('aria-hidden', 'true');

  const network = page.getByRole('region', { name: 'Directory network', exact: true });
  await network.getByRole('button', { name: 'Shared Organization' }).click();
  const organizationEditor = page.getByRole('dialog', { name: 'Edit Shared Organization' });
  await expect(organizationEditor).toBeVisible();
  await organizationEditor.getByRole('button', { name: 'Close organization editor' }).click();

  await network.getByRole('button', { name: 'Curated Peer' }).click();
  await expect(page).toHaveURL(/directoryPersonID%22%3A43/);
  await expect(page.getByRole('heading', { name: 'Curated Peer' })).toBeVisible();
});

for (const scenario of ['shared organization', 'nested same-column relations', 'long employment title']) {
  test(`Directory network draws connections and keeps names reachable: ${scenario}`, async ({ page }) => {
    await installMixedArchive(page);
    const nested = scenario === 'nested same-column relations';
    const count = nested ? 4 : scenario === 'shared organization' ? 3 : 1;
    const nodes = [
      { id: 'person:42', kind: 'person', entity_id: 42, label: 'Archive Person', hop: 0 },
      ...Array.from({ length: count }, (_, index) => ({ id: `person:${43 + index}`, kind: 'person', entity_id: 43 + index, label: `Curated Peer ${index + 1}`, hop: 1 })),
      ...nested ? [] : [{ id: 'organization:21', kind: 'organization', entity_id: 21, label: 'Shared Organization', hop: 2 }]
    ];
    const edges: NetworkEdge[] = Array.from({ length: count }, (_, index) => ({
      id: `root:${index}`, kind: 'relationship', source_node_id: 'person:42', target_node_id: `person:${43 + index}`, label: 'knows'
    }));
    if (nested) {
      edges.push(
        { id: 'outer', kind: 'relationship', source_node_id: 'person:46', target_node_id: 'person:43', label: 'mentors' },
        { id: 'inner', kind: 'relationship', source_node_id: 'person:44', target_node_id: 'person:45', label: 'knows' }
      );
    } else {
      for (let index = 0; index < count; index++) edges.push({
        id: `employment:${index}`, kind: 'employment', source_node_id: `person:${43 + index}`, target_node_id: 'organization:21',
        label: scenario === 'long employment title' ? 'Senior principal engineer of distributed computing systems' : 'Engineer'
      });
    }
    await page.route('**/api/v1/people/42/network*', (route) => {
      const depth = Number(new URL(route.request().url()).searchParams.get('depth'));
      const visible = nodes.filter((node) => node.hop <= depth);
      const hops = new Map(visible.map((node) => [node.id, node.hop]));
      return route.fulfill({ json: { root_person_id: 42, depth, truncated: false, nodes: visible, edges: edges.filter((edge) => hops.has(edge.source_node_id) && hops.has(edge.target_node_id) && Math.min(hops.get(edge.source_node_id)!, hops.get(edge.target_node_id)!) < depth) } });
    });
    await page.goto(directoryURL(42));
    await page.getByRole('radio', { name: 'Details', exact: true }).click();
    await page.getByRole('radio', { name: '2 steps', exact: true }).click();
    const network = page.getByRole('region', { name: 'Directory network', exact: true });
    await expect(network.locator('path')).toHaveCount(edges.length);
    await expect(network.getByRole('region', { name: 'Network graph' })).toHaveAttribute('aria-busy', 'false');
    await page.evaluate(() => document.fonts.ready);
    const names = network.getByRole('button');
    const nameBoxes = await Promise.all((await names.all()).map((name) => name.boundingBox()));
    for (const name of nameBoxes) expect(name!.width).toBeGreaterThan(40);
    if (scenario === 'long employment title') {
      for (const name of await names.all()) {
        await name.scrollIntoViewIfNeeded();
        expect(await name.evaluate((button) => {
          const box = button.getBoundingClientRect();
          return document.elementFromPoint(box.x + box.width / 2, box.y + box.height / 2) === button;
        })).toBe(true);
      }
    }
  });
}
