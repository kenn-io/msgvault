import AxeBuilder from '@axe-core/playwright';
import { expect, test, type Page } from '@playwright/test';
import { installMixedArchive } from './fixtures/mixed-archive';

async function assertNoViolations(page: Page, label: string) {
  const result = await new AxeBuilder({ page }).analyze();
  expect(result.violations, `${label}: ${result.violations.map((v) => `${v.id}: ${v.help}`).join('; ')}`)
    .toEqual([]);
}

for (const theme of ['light', 'dark'] as const) {
  for (const density of ['compact', 'comfortable'] as const) {
    test(`${theme}/${density} primary workspaces and representative states have no axe violations`, async ({ page }) => {
      await installMixedArchive(page);
      await page.goto('/');
      await page.getByLabel('Temporary theme').selectOption(theme);
      await page.getByLabel('Temporary density').selectOption(density);

      // The Relationships hub is the default landing workspace; walk its
      // three panes (list, timeline, reading pane) open one at a time so
      // each incremental layout gets its own axe pass.
      const hub = page.getByRole('main', { name: 'Relationships' });
      await expect(hub).toBeVisible();
      const relationshipList = page.getByRole('grid', { name: 'Relationship results' });
      await expect(relationshipList.getByText('Archive Person')).toBeVisible();
      await assertNoViolations(page, `Relationships list ${theme}/${density}`);
      await relationshipList.getByText('Archive Person').click();
      await expect(page.getByRole('heading', { name: 'Archive Person' })).toBeVisible();
      const relationshipTimeline = page.getByRole('grid', { name: 'Relationship activity' });
      await expect(relationshipTimeline.locator('[data-row-key]').first()).toBeVisible();
      await assertNoViolations(page, `Relationships timeline ${theme}/${density}`);
      await relationshipTimeline.focus();
      await page.keyboard.press('Enter');
      await expect(page.getByRole('complementary', { name: /Inspect/ })).toBeVisible();
      await assertNoViolations(page, `Relationships reading pane ${theme}/${density}`);
      await page.keyboard.press('Escape');

      await page.getByRole('button', { name: 'Everything', exact: true }).click();
      const grid = page.getByRole('grid', { name: 'Everything results' });
      await expect(grid.locator('[data-row-key]').first()).toBeVisible();
      await assertNoViolations(page, `Everything ${theme}/${density}`);
      await grid.focus();
      await page.keyboard.press('Enter');
      await expect(page.getByRole('complementary', { name: /Inspect/ })).toBeVisible();
      await assertNoViolations(page, `inspector ${theme}/${density}`);
      await page.keyboard.press('Escape');

      await page.keyboard.press('Shift+/');
      const keyboardHelp = page.getByRole('dialog', { name: 'Keyboard shortcuts' });
      await expect(keyboardHelp).toBeVisible();
      await assertNoViolations(page, `modal ${theme}/${density}`);
      await keyboardHelp.getByRole('button', { name: 'Close' }).click();

      for (const workspace of ['Files', 'Saved Views', 'Sources', 'Deletions', 'Settings']) {
        await page.getByRole('button', { name: workspace, exact: true }).click();
        await expect(page.getByRole('heading', { level: 1, name: workspace, exact: true })).toBeVisible();
        await assertNoViolations(page, `${workspace} ${theme}/${density}`);
        if (workspace === 'Files') {
          const files = page.getByRole('grid', { name: 'Files results' });
          await files.focus();
          await page.keyboard.press('Enter');
          const viewer = page.getByRole('dialog', { name: 'View synthetic.txt' });
          await expect(viewer).toBeVisible();
          await assertNoViolations(page, `viewer ${theme}/${density}`);
          await viewer.getByRole('button', { name: 'Close file viewer' }).click();
        }
      }
    });
  }
}

for (const theme of ['light', 'dark'] as const) {
  for (const density of ['compact', 'comfortable'] as const) {
    test(`${theme}/${density} login, loading, empty, error, and degraded states have no axe violations`, async ({ page }) => {
      let sessionRequired = true;
      let exploreMode: 'loading' | 'empty' | 'error' | 'degraded' = 'loading';
      await page.addInitScript(({ theme, density }) => {
        sessionStorage.setItem('msgvault.appearance.override', JSON.stringify({ theme, density }));
      }, { theme, density });
      await page.route('**/api/session', (route) => route.fulfill({ json: sessionRequired
        ? { auth_mode: 'required', https: true, plain_http_warning: false }
        : { auth_mode: 'loopback', https: false, plain_http_warning: false } }));
      await page.route('**/api/v1/settings', (route) => route.fulfill({ json: {
        settings: [
          { key: 'web.theme', value: { string: 'light' } },
          { key: 'web.density', value: { string: 'compact' } }
        ], pending_restart: false
      } }));
      await page.route('**/api/v1/explore', async (route) => {
        if (exploreMode === 'loading') return new Promise(() => {});
        if (exploreMode === 'error') return route.fulfill({
          status: 500, json: { error: 'internal_error', message: 'Synthetic request failure.' }
        });
        if (exploreMode === 'degraded') return route.fulfill({ status: 503, json: {
          error: 'analytical_cache_unavailable', message: 'Synthetic cache unavailable.',
          readiness: 'absent', recovery_action: 'msgvault build-cache'
        } });
        return route.fulfill({ json: {
          rows: [], total_count: 0, cache_revision: 'empty', search_provenance: {}
        } });
      });

      // This test exercises Everything's own loading/empty/error/degraded
      // states, not the Relationships hub, so it lands there explicitly
      // rather than relying on whatever the default landing workspace is.
      await page.goto(`/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
      await expect(page.locator('html')).toHaveAttribute('data-theme', theme);
      await expect(page.locator('html')).toHaveAttribute('data-density', density);
      await expect(page.getByRole('main', { name: 'Authentication' })).toBeVisible();
      await expect(page.getByLabel('API key')).toBeVisible();
      await assertNoViolations(page, `login ${theme}/${density}`);

      sessionRequired = false;
      await page.reload();
      const grid = page.getByRole('grid', { name: 'Everything results' });
      await expect(grid).toHaveAttribute('aria-busy', 'true');
      await expect(page.getByTestId('everything-skeleton').first()).toBeVisible();
      await assertNoViolations(page, `loading ${theme}/${density}`);

      exploreMode = 'empty';
      await page.reload();
      await expect(page.getByText('No items match this view.')).toBeVisible();
      await assertNoViolations(page, `empty ${theme}/${density}`);

      exploreMode = 'error';
      await page.reload();
      await expect(page.getByRole('alert')).toContainText('Synthetic request failure.');
      await assertNoViolations(page, `error ${theme}/${density}`);

      exploreMode = 'degraded';
      await page.reload();
      const degraded = page.getByRole('alert');
      await expect(degraded).toContainText('Analytical cache unavailable');
      await expect(degraded).toContainText('Synthetic cache unavailable.');
      await expect(degraded).toContainText('msgvault build-cache');
      await assertNoViolations(page, `degraded ${theme}/${density}`);
    });
  }
}
