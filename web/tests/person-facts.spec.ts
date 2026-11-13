import { expect, test, type Page } from '@playwright/test';
import { installDirectoryReviewArchive } from './e2e/fixtures/mixed-archive';
import { stubOperationStatus } from './kit-ui';
import { openPersonFacts } from './e2e/fixtures/person-facts';

test('Facts opens field settings', async ({ page }) => {
  const facts = await openFacts(page);
  await facts.getByRole('button', { name: 'Search fields: All fields' }).click();
  await page.getByRole('option', { name: 'Manage fields…' }).click();
  await expect(page.getByRole('heading', { name: 'Fields', exact: true })).toBeVisible();
});

async function openFacts(page: Page) {
  await installDirectoryReviewArchive(page);
  await stubOperationStatus(page, ['person_sweep']);
  return openPersonFacts(page);
}

for (const width of [1280, 390]) test(`Work details wrap between items at ${width}px`, async ({ page }) => {
  await page.setViewportSize({ width, height: 900 });
  await installDirectoryReviewArchive(page);
  await stubOperationStatus(page, ['person_sweep']);
  const target = { kind: 'employment', key: 'system:employment', revision: `sha256:${'d'.repeat(64)}` };
  const job = { title: 'Engineer', role: 'Lead', department: 'Research', location: 'Boston', organization: { name: 'Acme', domain: 'acme.example' }, start_date: { year: 2020, month: 3 }, end_date: { year: 2024 } };
  await page.route('**/api/v1/person-fact-targets*', route => route.fulfill({ json: { targets: [{ ...target, description: 'Work', value_type: 'object', cardinality: 'multi', sensitive: false }] } }));
  await page.route(/\/api\/v1\/people\/42\/fact-claims(?:\?.*)?$/, route => route.fulfill({ json: { claims: [{ id: 1, claim_key: 'work-1', evidence_ids: [], target, submitted_value: JSON.stringify(job), normalized_value: JSON.stringify(job), relation: 'support', origin: 'extraction', confidence: { reported_score: 740 }, created_at: '2026-11-01T12:00:00Z' }] } }));
  const facts = await openPersonFacts(page);
  const details = facts.getByText('Lead · Research · Boston · acme.example · Started Mar 2020 · Ended 2024');
  await expect(details).toBeVisible();
  const fragments = await details.evaluate(element => {
    const text = element.textContent!;
    return ['Research', 'Boston', 'acme.example', 'Started Mar 2020', 'Ended 2024', 'Lead ·', 'Research ·', 'Boston ·', 'acme.example ·', 'Started Mar 2020 ·'].map(item => {
      const start = text.indexOf(item);
      const walker = document.createTreeWalker(element, NodeFilter.SHOW_TEXT);
      const nodes: { node: Text; start: number; end: number }[] = [];
      let offset = 0;
      while (walker.nextNode()) { const node = walker.currentNode as Text; nodes.push({ node, start: offset, end: offset + node.length }); offset += node.length; }
      const first = nodes.find(node => node.end > start)!;
      const last = nodes.find(node => node.end >= start + item.length)!;
      const range = document.createRange();
      range.setStart(first.node, start - first.start);
      range.setEnd(last.node, start + item.length - last.start);
      return Array.from(range.getClientRects()).filter(rect => rect.width > 0).map(rect => rect.top);
    });
  });
  for (const lines of fragments) expect(new Set(lines).size).toBe(1);
});


test('Saved value details stack labels above values on phone', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 900 });
  await installDirectoryReviewArchive(page);
  await stubOperationStatus(page, ['person_sweep']);
  const revision = `sha256:${'d'.repeat(64)}`;
  const definition = { id: 8, universal_id: 'city', object_type: 'person', slug: 'city', label: 'City', value_type: 'text', field_type: 'text', cardinality: 'single', display_order: 1, is_sensitive: false, is_active: true, revision: 1 };
  await page.route('**/api/v1/people/42/attributes*', route => route.fulfill({ json: { person_id: 42, attributes: [{ definition, current: [{ id: 1, person_id: 42, definition_id: 8, definition_slug: 'city', ordinal: 0, value: { type: 'text', text: 'Boston' }, source: 'extraction', source_ref: 'person-fact-decision:1', created_at: '2026-01-01', active_from: '2026-01-01' }], history: [] }] } }));
  await page.route('**/api/v1/person-fact-targets*', route => route.fulfill({ json: { targets: [{ kind: 'attribute', key: 'city', revision, slug: 'city', description: 'City', value_type: 'text', cardinality: 'single', sensitive: false }] } }));
  const facts = await openPersonFacts(page);
  await facts.getByRole('button', { name: 'Search fields: All fields' }).click();
  await page.getByRole('option', { name: 'City', exact: true }).click();
  const saved = facts.getByRole('region', { name: 'Selected saved value' });
  await saved.getByRole('button', { name: 'Details', exact: true }).click();
  const reference = saved.getByText('Reference', { exact: true });
  const positions = await reference.evaluate(label => ({ label: label.getBoundingClientRect().top, value: label.nextElementSibling!.getBoundingClientRect().top }));
  expect(positions.value).toBeGreaterThan(positions.label);
});

for (const width of [1280, 390]) test(`Details aligns with source and date at ${width}px`, async ({ page }) => {
  await page.setViewportSize({ width, height: 900 });
  const facts = await openFacts(page);
  for (const section of ['Claims', 'Decisions']) {
    await facts.getByRole('radio', { name: section }).click();
    const row = facts.getByLabel(section === 'Claims' ? 'Fact claim' : 'Historical fact decision').first();
    for (const name of ['Details', 'Hide details']) {
      const button = row.getByRole('button', { name, exact: true });
      const positions = await row.evaluate((element, buttonName) => {
        const meta = element.querySelector('.meta time')!;
        const label = Array.from(element.querySelectorAll('button')).find(button => button.textContent?.trim() === buttonName)!.querySelector('.kit-button__label-text')!;
        const range = document.createRange();
        range.selectNodeContents(meta);
        const date = range.getBoundingClientRect();
        range.selectNodeContents(label);
        const action = range.getBoundingClientRect();
        return { date: date.top, action: action.top };
      }, name);
      expect(Math.abs(positions.date - positions.action)).toBeLessThanOrEqual(1);
      await button.click();
    }
  }
});

test('Facts tabs and field picker share a right edge on phone', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 900 });
  const facts = await openFacts(page);
  const tabs = await facts.getByRole('radiogroup', { name: 'Fact ledger section' }).boundingBox();
  const picker = await facts.getByRole('button', { name: 'Search fields: All fields' }).boundingBox();
  expect(Math.abs(tabs!.x + tabs!.width - picker!.x - picker!.width)).toBeLessThanOrEqual(1);
});
