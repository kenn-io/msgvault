import { mkdir, readFile } from 'node:fs/promises';
import { join } from 'node:path';
import { test, expect, loginToMeetingArchive } from './fixtures/meeting-daemon';
import { selectKitOption, selectWorkspace } from '../kit-ui';

test('Settings opens on Overview, keeps edits across categories, and saves them through reload', async ({ page, daemon }, testInfo) => {
  await page.setViewportSize({ width: 1440, height: 1000 });
  await page.emulateMedia({ colorScheme: 'dark' });
  const errors: string[] = [];
  page.on('pageerror', error => errors.push(error.message));
  await loginToMeetingArchive(page, daemon);
  await selectWorkspace(page, 'Settings');
  await expect(page.getByRole('heading', { name: 'Overview', exact: true })).toBeVisible();
  await page.getByRole('button', { name: 'Set up Message search', exact: true }).click();
  await expect(page.getByRole('heading', { name: 'Search & AI', exact: true })).toBeVisible();
  await expect(page.locator('[data-setting-key="vector.enabled"]')).toBeFocused();
  await page.getByRole('button', { name: 'Server', exact: true }).click();
  await page.getByRole('textbox', { name: 'Stop when idle after', exact: true }).fill('17m');
  await page.getByRole('button', { name: 'Advanced', exact: true }).click();
  await selectKitOption(page, 'Theme', 'Dark');
  await page.getByRole('button', { name: 'Overview', exact: true }).click();
  let release!: () => void;
  let started!: () => void;
  const pending = new Promise<void>(resolve => { release = resolve; });
  const saving = new Promise<void>(resolve => { started = resolve; });
  await page.route('**/api/v1/settings', async route => {
    if (route.request().method() === 'PATCH') { started(); await pending; }
    await route.continue();
  });
  const saved = page.waitForResponse(response => response.request().method() === 'PATCH' && response.url().endsWith('/api/v1/settings'));
  await page.getByRole('button', { name: 'Save changes', exact: true }).click();
  await saving;
  await page.getByRole('button', { name: 'People lookups', exact: true }).click();
  await expect(page.getByRole('button', { name: 'Add Exa', exact: true })).toBeDisabled();
  await expect(page.getByLabel('Profile name', { exact: true })).toBeDisabled();
  await page.getByRole('button', { name: 'Contacts', exact: true }).click();
  await expect(page.getByRole('combobox', { name: /^CardDAV connection/ })).toBeDisabled();
  await page.getByRole('button', { name: 'Server', exact: true }).click();
  const idle = page.getByRole('textbox', { name: 'Stop when idle after', exact: true });
  await expect(idle).toBeDisabled();
  await expect(idle.fill('lost edit', { timeout: 250 })).rejects.toThrow();
  release();
  expect((await saved).ok()).toBe(true);
  await page.unroute('**/api/v1/settings');
  await page.getByRole('button', { name: 'Overview', exact: true }).click();
  await expect(page.getByRole('button', { name: 'Save changes', exact: true })).toHaveCount(0);
  await page.reload();
  await expect(page.getByRole('heading', { name: 'Overview', exact: true })).toBeVisible();
  await page.getByRole('button', { name: 'Server', exact: true }).click();
  await expect(page.getByRole('textbox', { name: 'Stop when idle after', exact: true })).toHaveValue('17m0s');
  await page.getByRole('button', { name: 'Advanced', exact: true }).click();
  await expect(page.getByRole('combobox', { name: /^Theme:/ })).toContainText('Dark');
  await page.goBack();
  await expect(page.getByRole('heading', { name: 'Server', exact: true })).toBeVisible();
  await page.goForward();
  await expect(page.getByRole('heading', { name: 'Advanced', exact: true })).toBeVisible();

  const captureDir = process.env.MSGVAULT_SETTINGS_CAPTURE_DIR ?? testInfo.outputPath('settings');
  await mkdir(captureDir, { recursive: true });
  for (const theme of ['dark', 'light']) {
    if (theme === 'light') {
      await page.getByRole('button', { name: 'Advanced', exact: true }).click();
      await selectKitOption(page, 'Theme', 'Light');
      await page.getByRole('button', { name: 'Save changes', exact: true }).click();
      await expect(page.getByRole('button', { name: 'Save changes', exact: true })).toHaveCount(0);
    }
  for (const width of [1440, 390]) {
    await page.setViewportSize({ width, height: width === 1440 ? 1000 : 844 });
    for (const category of ['Overview', 'Server', 'Search & AI', 'People lookups', 'Tasks', 'Contacts', 'Sources', 'Advanced']) {
      await page.getByRole('navigation', { name: 'Settings', exact: true }).getByRole('button', { name: category, exact: true }).click();
      await expect(page.getByRole('heading', { name: category, exact: true, level: 2 })).toBeVisible();
      if (category === 'Overview') await expect(page.locator('[data-feature="document_embedding"]')).toBeVisible();
      if (category === 'People lookups') await expect(page.getByRole('button', { name: 'Create profile', exact: true })).toBeVisible();
      await page.screenshot({ path: join(captureDir, `${category.toLowerCase().replaceAll(/[^a-z]+/g, '-')}-${width}-${theme}.png`) });
      expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
    }
  }
  }
  await page.getByRole('button', { name: 'Overview', exact: true }).click();
  await page.getByRole('button', { name: 'Open Sources status', exact: true }).click();
  await expect(page.getByRole('heading', { name: 'Sources', exact: true })).toBeVisible();
  await selectWorkspace(page, 'Operations');
  await page.getByRole('button', { name: 'Settings overview', exact: true }).click();
  await expect(page.getByRole('heading', { name: 'Overview', exact: true })).toBeVisible();
  expect(errors).toEqual([]);

  const baseDist = process.env.MSGVAULT_SETTINGS_BASE_DIST;
  if (baseDist) {
    await page.route('**/*', async route => {
      const path = new URL(route.request().url()).pathname;
      if (path !== '/' && !/^\/assets\/[A-Za-z0-9._-]+$/.test(path)) return route.continue();
      const file = path === '/' ? 'index.html' : path.slice(1);
      await route.fulfill({ body: await readFile(join(baseDist, file)), contentType: file.endsWith('.css') ? 'text/css' : file.endsWith('.js') ? 'text/javascript' : 'text/html' });
    });
    const beforeDir = join(captureDir, '..', 'before');
    await mkdir(beforeDir, { recursive: true });
    const categories = { overview: 'browser', server: 'server', 'search-ai': 'search', 'people-lookups': 'enrichment', tasks: 'integrations', contacts: 'carddav', sources: 'sources', advanced: 'archive' };
    for (const theme of ['dark', 'light']) {
      await page.goto(`${daemon.origin}/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'settings', settingsCategory: 'browser' }))}`);
      await selectKitOption(page, 'Theme', theme === 'dark' ? 'Dark' : 'Light');
      await page.getByRole('button', { name: 'Save changes', exact: true }).click();
      await expect(page.getByRole('button', { name: 'Save changes', exact: true })).toHaveCount(0);
    for (const width of [1440, 390]) {
      await page.setViewportSize({ width, height: width === 1440 ? 1000 : 844 });
      for (const [name, settingsCategory] of Object.entries(categories)) {
        await page.goto(`${daemon.origin}/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'settings', settingsCategory }))}`);
        await expect(page.locator('.kit-settings')).toBeVisible();
        await expect(page.getByRole('heading', { name: 'Settings', exact: true })).toBeVisible();
        await page.screenshot({ path: join(beforeDir, `${name}-${width}-${theme}.png`) });
      }
    }
    }
  }
});
