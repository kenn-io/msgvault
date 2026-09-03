import AxeBuilder from '@axe-core/playwright';
import { expect, test, type Page } from '@playwright/test';
import { expectKitTheme, selectKitOption, setKitTheme } from '../kit-ui';
import { assertCardDAVForbiddenMarkersAbsent, installCardDAV } from './fixtures/carddav';
import { installDirectoryReviewArchive, installMixedArchive } from './fixtures/mixed-archive';

async function assertNoViolations(page: Page, label: string) {
  const result = await new AxeBuilder({ page }).analyze();
  expect(result.violations, `${label}: ${result.violations.map((v) => `${v.id}: ${v.help}`).join('; ')}`)
    .toEqual([]);
}

for (const theme of ['light', 'dark'] as const) {
  for (const density of ['compact', 'comfortable'] as const) {
    test(`${theme}/${density} primary workspaces and representative states have no axe violations`, async ({ page }) => {
      test.slow();
      await installMixedArchive(page);
      await page.unroute('**/api/v1/participants/12/files/search');
      await page.route('**/api/v1/participants/12/files/search', async (route) => {
        const body = route.request().postDataJSON() as { mime_families?: string[] };
        const media = body.mime_families?.includes('image');
        return route.fulfill({ json: {
          files: [{
            id: media ? 8 : 7, key: media ? 'file:8' : 'file:7', entry_key: 'message:100001',
            message_id: 100001, conversation_id: 100001, occurred_at: '2026-01-03T12:00:00Z',
            source_id: 1, source_type: 'synthetic', source_identifier: 'archive@example.com',
            containing_title: 'Synthetic email', filename: media ? 'archive-photo.png' : 'archive-notes.pdf',
            mime_type: media ? 'image/png' : 'application/pdf', mime_family: media ? 'image' : 'pdf',
            size_bytes: 2048, content_state: 'metadata_only', content_available: false,
            person_provenance: {
              participant_ids: [12], roles: ['from', 'conversation_member'],
              directions: ['from_person', 'group']
            }
          }],
          total_count: 1, cache_revision: 'mixed-100k', search_provenance: {}
        } });
      });
      await page.goto('/');
      await setKitTheme(page, theme);
      await selectKitOption(page, 'Temporary density', `Density: ${density === 'compact' ? 'Compact' : 'Comfortable'}`);

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
      await page.getByRole('button', { name: 'Files 1' }).click();
      await expect(page.getByRole('grid', { name: 'Files results' }).getByText('archive-notes.pdf')).toBeVisible();
      await assertNoViolations(page, `Person files ${theme}/${density}`);
      await page.getByRole('radio', { name: 'Media' }).click();
      await expect(page.getByRole('button', { name: 'Open archive-photo.png' })).toBeVisible();
      await assertNoViolations(page, `Person media ${theme}/${density}`);
      await page.getByRole('button', { name: 'Files 1' }).click();
      await expect(relationshipTimeline).toBeVisible();
      await relationshipTimeline.focus();
      await page.keyboard.press('Enter');
      await expect(page.getByRole('complementary', { name: /Reading pane/ })).toBeVisible();
      await assertNoViolations(page, `Relationships reading pane ${theme}/${density}`);
      await page.keyboard.press('Escape');

      await page.getByRole('button', { name: 'Everything', exact: true }).click();
      const grid = page.getByRole('grid', { name: 'Everything results' });
      await expect(grid.locator('[data-row-key]').first()).toBeVisible();
      await assertNoViolations(page, `Everything ${theme}/${density}`);
      await grid.focus();
      await page.keyboard.press('Enter');
      await expect(page.getByRole('complementary', { name: /Reading pane/ })).toBeVisible();
      await assertNoViolations(page, `reading pane ${theme}/${density}`);
      await page.keyboard.press('Escape');

      await page.keyboard.press('Shift+/');
      const keyboardHelp = page.getByRole('dialog', { name: 'Keyboard shortcuts' });
      await expect(keyboardHelp).toBeVisible();
      await assertNoViolations(page, `modal ${theme}/${density}`);
      await keyboardHelp.getByRole('button', { name: 'Close' }).click();

      for (const workspace of ['Directory', 'Files', 'Saved Views', 'Sources', 'Deletions', 'Settings']) {
        await page.getByRole('button', { name: workspace, exact: true }).click();
        await expect(page.getByRole('main', { name: workspace, exact: true })).toBeVisible();
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

test('Directory network list and visualization have no axe violations', async ({ page }) => {
  await installMixedArchive(page);
  await page.goto(`/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'directory', directoryPersonID: 42 }))}`);
  await expect(page.getByRole('heading', { name: 'Archive Person' })).toBeVisible();
  await page.getByRole('tab', { name: 'Network' }).click();
  await expect(page.getByRole('list', { name: 'Directory network connections' })).toContainText('Curated Peer');
  await assertNoViolations(page, 'Directory network');
});

test('Directory profile maintenance is accessible at desktop and narrow widths', async ({ page }) => {
  await installMixedArchive(page);
  for (const viewport of [
    { label: 'desktop', width: 1280, height: 900 },
    { label: 'narrow', width: 390, height: 844 }
  ]) {
    await page.setViewportSize({ width: viewport.width, height: viewport.height });
    await page.goto(`/?explore=${encodeURIComponent(JSON.stringify({
      workspace: 'directory', directoryPersonID: 42
    }))}`);
    const maintenance = page.getByRole('region', { name: 'Profile maintenance' });
    await expect(maintenance.getByRole('switch', {
      name: 'Track this person for profile maintenance'
    })).toBeEnabled();
    const reveal = maintenance.getByRole('button', { name: 'Show sensitive eligible fields' });
    await reveal.focus();
    await page.keyboard.press('Enter');
    await expect(maintenance.getByText('Private note')).toBeVisible();
    await expect(maintenance.getByText('Sensitive', { exact: true })).toBeVisible();
    await assertNoViolations(page, `Directory profile maintenance ${viewport.label}`);
  }
});

test('Directory review, merge, split, and honest Fact gate have no axe violations', async ({ page }) => {
  test.slow();
  await installDirectoryReviewArchive(page);
  await page.goto(`/?explore=${encodeURIComponent(JSON.stringify({
    workspace: 'directory_review', reviewKind: 'identity', identityState: 'candidate'
  }))}`);

  await expect(page.getByRole('article', { name: 'Identity match 17' })).toBeVisible();
  await assertNoViolations(page, 'Directory identity review');

  await page.getByRole('article', { name: 'Identity match 17' })
    .getByRole('button', { name: 'Keep separate' }).click();
  const decision = page.getByRole('dialog', { name: 'Keep separate' });
  await expect(decision).toBeVisible();
  await assertNoViolations(page, 'Directory identity decision');
  await page.keyboard.press('Escape');

  await page.getByRole('article', { name: 'Identity match 19' })
    .getByRole('button', { name: 'Link identities' }).click();
  const conflict = page.getByRole('dialog', { name: 'Link identities' });
  await conflict.getByRole('button', { name: 'Link identities' }).click();
  await conflict.getByRole('button', { name: 'Resolve merge' }).click();
  const merge = page.getByRole('dialog', { name: 'Resolve person merge' });
  await expect(merge).toBeVisible();
  await assertNoViolations(page, 'Directory person merge');
  await merge.getByRole('radio', { name: 'Synthetic One (Person 7)' }).check();
  await merge.getByRole('checkbox', { name: /I understand this consolidates both profiles/ }).check();
  await merge.getByRole('button', { name: 'Merge into selected survivor' }).click();
  await expect(page.getByRole('heading', { name: 'Synthetic One' })).toBeVisible();

  await page.goto(`/?explore=${encodeURIComponent(JSON.stringify({
    workspace: 'directory_review', reviewKind: 'fact', identityState: 'candidate', directoryPersonID: 42
  }))}`);
  await expect(page.getByRole('region', { name: 'Fact review' })).toBeVisible();
  await expect(page.getByLabel('Fact evidence')).toBeVisible();
  await assertNoViolations(page, 'Directory Fact ledger desktop');
  await page.setViewportSize({ width: 390, height: 844 });
  await expect(page.getByRole('radiogroup', { name: 'Fact ledger section' })).toBeVisible();
  await assertNoViolations(page, 'Directory Fact ledger narrow');
  await page.setViewportSize({ width: 1280, height: 720 });

  await page.goto(`/?explore=${encodeURIComponent(JSON.stringify({
    workspace: 'directory', directoryPersonID: 7
  }))}`);
  const history = page.getByRole('table', { name: 'Person merge history' });
  await expect(history).toBeVisible();
  await history.getByRole('button', { name: 'Inspect merge 41' }).click();
  await expect(page.getByRole('heading', { name: 'Merge 41 detail' })).toBeVisible();
  await assertNoViolations(page, 'Directory merge history detail');
  await page.getByRole('button', { name: 'Split merged profile' }).click();
  const split = page.getByRole('dialog', { name: 'Split merged profile' });
  await expect(split).toBeVisible();
  const splitCancel = split.getByRole('button', { name: 'Cancel' });
  await expect(splitCancel).toBeEnabled();
  await expect(splitCancel).toHaveCSS('opacity', '1');
  await assertNoViolations(page, 'Directory split modal');
});

test('Imported relationship reviews have no axe violations at desktop and narrow widths', async ({ page }) => {
  await installDirectoryReviewArchive(page);
  for (const viewport of [
    { label: 'desktop', width: 1280, height: 900 },
    { label: 'narrow', width: 390, height: 844 }
  ]) {
    await page.setViewportSize(viewport);
    await page.goto(`/?explore=${encodeURIComponent(JSON.stringify({
      workspace: 'directory_review', reviewKind: 'relationship', relationshipReviewState: 'pending'
    }))}`);
    await expect(page.getByRole('article', { name: 'Imported relationship review 41' })).toBeVisible();
    await assertNoViolations(page, `Imported relationship reviews ${viewport.label}`);
  }
});

for (const mode of ['failed', 'malformed'] as const) {
  test(`Directory Fact claims fail closed with an accessible ${mode} target catalog`, async ({ page }) => {
    const fixture = await installDirectoryReviewArchive(page);
    if (mode === 'failed') fixture.failFactCatalog();
    else fixture.malformFactCatalog();
    await page.goto(`/?explore=${encodeURIComponent(JSON.stringify({
      workspace: 'directory_review', reviewKind: 'fact', identityState: 'candidate', directoryPersonID: 42
    }))}`);

    const fact = page.getByRole('region', { name: 'Fact review' });
    await fact.getByRole('radio', { name: 'Claims' }).click();
    await expect(fact.getByRole('status', {
      name: 'Fact value hidden until target sensitivity is verified.'
    })).toBeVisible();
    await expect(fact).not.toContainText('Allowed synthetic sensitive value.');
    await expect(fact.getByRole('button', { name: 'Reveal sensitive fact value' })).toHaveCount(0);
    await assertNoViolations(page, `Directory Fact ledger ${mode} catalog`);
  });
}

test('Directory Fact claims stay private and accessible while the target catalog is deferred', async ({ page }) => {
  const fixture = await installDirectoryReviewArchive(page);
  const releaseCatalog = fixture.deferFactCatalog();
  await page.goto(`/?explore=${encodeURIComponent(JSON.stringify({
    workspace: 'directory_review', reviewKind: 'fact', identityState: 'candidate', directoryPersonID: 42
  }))}`);

  const fact = page.getByRole('region', { name: 'Fact review' });
  await fact.getByRole('radio', { name: 'Claims' }).click();
  await expect(fact.getByRole('status', {
    name: 'Fact value hidden until target sensitivity is verified.'
  })).toBeVisible();
  await expect(fact).not.toContainText('Allowed synthetic sensitive value.');
  await assertNoViolations(page, 'Directory Fact ledger deferred catalog');

  releaseCatalog();
  await expect(fact.getByRole('button', { name: 'Reveal sensitive fact value' })).toBeVisible();
  await expect(fact).not.toContainText('Allowed synthetic sensitive value.');
});

test('CardDAV account, operations, conflicts, modal, and publication are accessible at desktop and narrow widths', async ({ page }) => {
  test.slow();
  await installMixedArchive(page);
  await installCardDAV(page, { configured: true, staleConflictOnce: false });

  for (const viewport of [
    { label: 'desktop', width: 1280, height: 900 },
    { label: 'narrow', width: 390, height: 844 }
  ]) {
    await page.setViewportSize({ width: viewport.width, height: viewport.height });
    await page.goto(`/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'settings' }))}`);
    const cardDAVCategory = page.getByRole('button', { name: /^CardDAV account/ });
    await cardDAVCategory.focus();
    await page.keyboard.press('Enter');

    await expect(page.getByLabel('CardDAV status summary')).toContainText('Configured');
    await expect(page.getByLabel('CardDAV status summary')).toContainText('Runtime available');
    await expect(page.getByLabel('CardDAV status summary')).toContainText('Credential ready');
    await expect(page.getByText('Scheduled sync enabled')).toBeVisible();
    await expect(page.getByRole('table', { name: 'CardDAV sync history' })).toContainText('Succeeded');
    await assertNoViolations(page, `CardDAV account and operations ${viewport.label}`);
    await assertCardDAVForbiddenMarkersAbsent(page);

    const conflictRow = page.getByRole('button', { name: 'Review conflict 41 in Synthetic contacts' });
    await conflictRow.focus();
    await page.keyboard.press('Enter');
    const comparison = page.getByRole('region', { name: 'CardDAV conflict 41 comparison' });
    await expect(comparison).toContainText('Present');
    await expect(comparison).toContainText('Deleted. This side is a deletion tombstone.');
    await expect(comparison).toContainText('Unavailable. No safe comparison summary is available.');
    await expect(page.getByText('Only display name, email addresses, and phone numbers are shown. Your choice applies to the whole card.')).toBeVisible();
    const baseBox = await page.getByLabel('Base card summary').boundingBox();
    const localBox = await page.getByLabel('Local card summary').boundingBox();
    expect(baseBox).not.toBeNull();
    expect(localBox).not.toBeNull();
    if (viewport.label === 'desktop') expect(Math.abs(baseBox!.y - localBox!.y)).toBeLessThan(2);
    else expect(localBox!.y).toBeGreaterThan(baseBox!.y + baseBox!.height - 2);
    await assertNoViolations(page, `CardDAV conflict detail ${viewport.label}`);
    await assertCardDAVForbiddenMarkersAbsent(page);

    const keepLocal = page.getByRole('button', { name: 'Keep local card' }).first();
    await keepLocal.focus();
    await page.keyboard.press('Enter');
    const modal = page.getByRole('dialog', { name: 'Keep local CardDAV card' });
    await expect(modal).toContainText('Choosing it deletes the whole card');
    await assertNoViolations(page, `CardDAV conflict modal ${viewport.label}`);
    await assertCardDAVForbiddenMarkersAbsent(page);
    await page.keyboard.press('Escape');
    await expect(keepLocal).toBeFocused();

    await page.goto(`/?explore=${encodeURIComponent(JSON.stringify({
      workspace: 'directory', directoryPersonID: 42
    }))}`);
    const publication = page.getByRole('region', { name: 'CardDAV publication' });
    await expect(publication).toContainText('Not published');
    await expect(publication).toContainText('Desired publication: Unpublished');
    await expect(publication).toContainText('Publication address book: Synthetic contacts.');
    await expect(publication.getByRole('switch', { name: 'Publish person to CardDAV' })).toBeEnabled();
    await assertNoViolations(page, `CardDAV publication ${viewport.label}`);
    await assertCardDAVForbiddenMarkersAbsent(page);
  }
});

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
      await expectKitTheme(page, theme);
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
      await expect(page.getByText('No items match this view')).toBeVisible();
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
