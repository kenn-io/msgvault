import { expect, test, type Page } from '@playwright/test';
import { loadMixedArchive } from './fixtures/mixed-archive';

async function tabTo(page: Page, accessibleName: string, limit = 120, key: 'Tab' | 'Shift+Tab' = 'Tab') {
  const seen = new Set<string>();
  for (let index = 0; index < limit; index += 1) {
    const name = await page.evaluate(() => {
      const active = document.activeElement;
      if (!active) return '';
      if (!active.matches('button, input, select, textarea, [role]')) return '';
      return active.getAttribute('aria-label') || active.textContent?.trim() || '';
    });
    seen.add(name);
    if (name.includes(accessibleName)) return;
    await page.keyboard.press(key);
  }
  throw new Error(`Keyboard focus did not reach ${accessibleName}; saw ${[...seen].join(' | ')}`);
}

test('pointer-free archive journey preserves focus, announcements, and history', async ({ page }) => {
  const fixture = await loadMixedArchive();
  const firstServedRow = fixture.firstPage.rows[0]!;
  const terminalServedRow = fixture.firstPage.rows.at(-1)!;
  let authenticated = false;
  await page.route('**/api/session', (route) => route.fulfill({ json: authenticated
    ? { auth_mode: 'session', csrf_token: 'csrf', https: true, plain_http_warning: false }
    : { auth_mode: 'required', https: true, plain_http_warning: false } }));
  await page.route('**/api/session/login', (route) => {
    authenticated = true;
    return route.fulfill({ json: { auth_mode: 'session', csrf_token: 'csrf', https: true, plain_http_warning: false } });
  });
  await page.route('**/api/v1/settings', (route) => route.fulfill({ json: { settings: [], pending_restart: false } }));
  await page.route('**/api/v1/explore', (route) => route.fulfill({ json: fixture.firstPage }));
  await page.route('**/api/v1/explore/groups', (route) => route.fulfill({ json: {
    rows: [{ key: '2', label: 'Synthetic chat', count: 100, estimated_bytes: 6400000,
      latest_at: '2026-01-02T12:00:00Z' }], total_count: 1,
    cache_revision: 'keyboard-100k', search_provenance: {}
  } }));
  await page.route('**/api/v1/conversations/**', (route) => route.fulfill({ json: {
    id: 1, anchor_id: 1, has_before: false, has_after: false, total: 1,
    messages: [{ id: 1, conversation_id: 1, subject: 'Synthetic conversation',
      message_type: 'chat', from: 'Synthetic Participant', to: [], sent_at: '2026-01-02T12:00:00Z',
      snippet: 'Synthetic chat body', body: 'Synthetic chat body', body_html: '', attachments: [] }]
  } }));
  await page.route('**/api/v1/files/search', (route) => route.fulfill({ json: {
    files: [{ id: 7, key: 'file:7', entry_key: 'message:100001', message_id: 100001,
      conversation_id: 1, occurred_at: '2026-01-03T12:00:00Z', source_id: 1,
      source_type: 'synthetic', source_identifier: 'archive@example.com', containing_title: 'Synthetic email',
      filename: 'fixture.txt', mime_type: 'text/plain', mime_family: 'text', size_bytes: 20,
      content_state: 'unsupported', content_available: true }], total_count: 1,
    cache_revision: 'keyboard-100k', search_provenance: {}
  } }));
  await page.route('**/api/v1/explore/preflight', (route) => route.fulfill({ json: {
    count: 1, estimated_bytes: 20, cache_revision: 'keyboard-100k', search_provenance: {},
    unavailable_actions: [], action_targets: [], operation_token: 'keyboard-operation',
    expires_at: '2026-01-03T12:05:00Z'
  } }));
  await page.route('**/api/v1/deletions', (route) => route.fulfill({ json:
    route.request().method() === 'POST'
      ? { message_count: 1, account: 'archive@example.com', dry_run: true }
      : { manifests: [] }
  }));
  await page.route('**/api/v1/relationships', (route) => route.fulfill({ json: {
    rows: [
      { canonical_id: 1, display_label: 'Alpha Person', last_at: '2026-01-02T12:00:00Z', member_ids: [1], score: 2,
        signals: { last_interaction_at: '2026-01-02T12:00:00Z', meeting_count: 0, meetings_together: 0,
          modalities: 1, received_from_them: 1, sent_count: 2, sent_to_them: 1 } },
      { canonical_id: 2, display_label: 'Beta Person', last_at: '2026-01-02T12:00:00Z', member_ids: [2], score: 1,
        signals: { last_interaction_at: '2026-01-02T12:00:00Z', meeting_count: 0, meetings_together: 0,
          modalities: 1, received_from_them: 1, sent_count: 1, sent_to_them: 1 } }
    ], total_count: 2, cache_revision: 'keyboard-100k', identity_revision: 1
  } }));
  await page.route('**/api/v1/people/2', (route) => route.fulfill({ json: {
    id: 2, display_label: 'Beta Person', partial_label: false, identifiers: [],
    activity_count: 1, file_count: 0, source_counts: [], first_at: '2026-01-02T12:00:00Z',
    last_at: '2026-01-02T12:00:00Z', cache_revision: 'keyboard-100k'
  } }));
  await page.route('**/api/v1/relationships/2/timeline', (route) => route.fulfill({ json: {
    canonical_id: 2, identity_revision: 1, cache_revision: 'keyboard-100k',
    rows: [{
      key: 'message:200', kind: 'chat', occurred_at: '2026-01-02T12:00:00Z',
      preview: 'Synthetic chat body', source_id: 1, title: 'Synthetic conversation',
      has_attachments: false, message_count: 1
    }], total_count: 1
  } }));

  await page.goto('/');
  await page.keyboard.press('Tab');
  await expect(page.getByLabel('API key')).toBeFocused();
  await page.keyboard.type('synthetic-key');
  await page.keyboard.press('Tab');
  await expect(page.getByRole('button', { name: 'Log in' })).toBeFocused();
  await page.keyboard.press('Enter');

  // Relationships is the default landing workspace. j/k move the ranked
  // list's selection, Enter opens a cluster into the timeline, a second
  // Enter opens the reading pane, and Esc walks back one layer at a time.
  await expect(page.getByRole('main', { name: 'Relationships' })).toBeVisible();
  await tabTo(page, 'Relationship results');
  const relationshipList = page.getByRole('grid', { name: 'Relationship results' });
  await expect(relationshipList).toBeFocused();
  await page.keyboard.press('j');
  await page.keyboard.press('Enter');
  await expect(page.getByRole('heading', { name: 'Beta Person' })).toBeVisible();

  await tabTo(page, 'Relationship activity');
  const relationshipTimeline = page.getByRole('grid', { name: 'Relationship activity' });
  await expect(relationshipTimeline).toBeFocused();
  await page.keyboard.press('Enter');
  const relationshipReading = page.getByRole('complementary', { name: /Inspect Synthetic conversation/ });
  await expect(relationshipReading).toBeVisible();

  await page.keyboard.press('Escape');
  await expect(relationshipReading).toBeHidden();
  await expect(relationshipTimeline).toBeFocused();
  await page.keyboard.press('Escape');
  await expect(relationshipList).toBeFocused();
  await expect.poll(() =>
    JSON.parse(new URL(page.url()).searchParams.get('explore') ?? '{}').relationshipTarget
  ).toBeNull();
  await expect(page.getByRole('heading', { name: 'Beta Person' })).toBeHidden();

  await tabTo(page, 'Everything');
  await page.keyboard.press('Enter');
  await expect(page.getByRole('main', { name: 'Everything' })).toBeVisible();
  await page.keyboard.press('/');
  const search = page.getByRole('searchbox', { name: 'Search everything' });
  await expect(search).toBeFocused();
  await page.keyboard.type('synthetic');
  await tabTo(page, 'Full text');
  await expect(page.getByRole('radio', { name: 'Full text' })).toBeFocused();
  await page.keyboard.press('ArrowRight');
  await expect(page.getByRole('radio', { name: 'Semantic' })).toHaveAttribute('aria-checked', 'true');
  await page.keyboard.press('Shift+Tab');
  await page.keyboard.press('Enter');
  await expect(page.locator('[aria-live="polite"]').filter({ hasText: `${fixture.logicalRows.length} items` }).first()).toBeVisible();

  await tabTo(page, 'Everything results');
  await page.keyboard.press(process.platform === 'darwin' ? 'Meta+K' : 'Control+K');
  await page.keyboard.type('group by source');
  await page.keyboard.press('Enter');
  const grouped = page.getByRole('grid', { name: 'Everything grouped by source' });
  await expect(grouped).toBeFocused();
  await tabTo(page, 'Drill into Synthetic chat');
  await page.keyboard.press('Enter');
  await expect(page.getByRole('complementary', { name: /Inspect/ })).toBeVisible();
  await page.keyboard.press('Escape');

  const grid = page.getByRole('grid', { name: 'Everything results' });
  await expect(grid).toBeFocused();
  const renderedActiveRow = grid.locator(`[data-row-key="${firstServedRow.key}"]`);
  await expect(grid).toHaveAttribute('aria-busy', 'false');
  await expect(renderedActiveRow).toBeVisible();
  await expect(grid).toHaveAttribute('aria-activedescendant', await renderedActiveRow.getAttribute('id') ?? '');
  await page.keyboard.press('End');
  await expect.poll(() => {
    const encoded = new URL(page.url()).searchParams.get('explore');
    return JSON.parse(encoded ?? '{}').activeRow;
  }).toBe(terminalServedRow.key);
  await expect(grid.locator(`[data-row-key="${terminalServedRow.key}"]`)).toBeVisible();
  await page.keyboard.press('Enter');
  await expect(page.getByRole('complementary', { name: /Inspect/ })).toBeVisible();
  await tabTo(page, 'View conversation');
  await page.keyboard.press('Enter');
  await expect(page.getByRole('heading', { name: 'Conversation', exact: true })).toBeVisible();
  await page.keyboard.press('Escape');
  await expect(page.getByRole('heading', { name: 'Conversation', exact: true })).toBeHidden();
  await expect(page.getByRole('complementary', { name: /Inspect/ })).toBeVisible();
  await expect(grid).toHaveAttribute('aria-busy', 'false');
  await page.keyboard.press('Escape');
  await expect(grid).toBeFocused();

  await tabTo(page, 'Files');
  await expect(page.getByRole('button', { name: 'Files', exact: true })).toBeFocused();
  await page.keyboard.press('Enter');
  await expect(page.getByRole('heading', { level: 1, name: 'Files' })).toBeVisible();
  const files = page.getByRole('grid', { name: 'Files results' });
  await tabTo(page, 'Files results');
  await expect(files).toBeFocused();
  await tabTo(page, 'Everything', 120, 'Shift+Tab');
  await page.keyboard.press('Enter');
  await tabTo(page, 'Everything results');
  await expect(grid).toBeFocused();
  await page.keyboard.press('Space');
  await expect(grid.locator('[aria-selected="true"]')).toHaveCount(1);
  const everythingURL = page.url();
  const everythingActiveRow = await grid.getAttribute('aria-activedescendant');
  expect(everythingActiveRow).not.toBeNull();
  await page.keyboard.press('d');
  await expect(page.getByRole('main', { name: 'Deletions' })).toBeVisible();
  await expect(page.getByRole('dialog', { name: 'Confirm selected deletion' })).toBeVisible();
  await tabTo(page, 'Cancel');
  await page.keyboard.press('Enter');
  await tabTo(page, 'Dry run');
  await page.keyboard.press('Enter');
  await expect(page.getByRole('status').filter({ hasText: /Dry run/i })).toBeVisible();

  const deletionsURL = page.url();
  await page.goBack();
  await expect(page).toHaveURL(everythingURL);
  await expect(page.getByRole('main', { name: 'Everything' })).toBeVisible();
  await expect(grid).toBeFocused();
  await expect(grid).toHaveAttribute('aria-activedescendant', everythingActiveRow!);
  await page.goForward();
  await expect(page).toHaveURL(deletionsURL);
  await expect(page.getByRole('main', { name: 'Deletions' })).toBeVisible();
  await expect(page.getByRole('button', { name: 'Deletions', exact: true })).toBeFocused();
});
