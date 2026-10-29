import { stubOperationStatus } from './kit-ui';
import { expect, test, type BrowserContext } from '@playwright/test';
import { expectKitTheme, selectKitOption, setKitTheme } from './kit-ui';

test('Strict session cookie returns on same-origin bootstrap after a cross-site navigation', async ({
  context,
  page,
  baseURL
}) => {
  if (!baseURL) throw new Error('Playwright baseURL is required');
  const appURL = new URL('/', baseURL).toString();
  const landingURL = `${appURL}?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`;
  const cookieName = 'msgvault_session';
  const cookieValue = 'opaque-browser-session';
  const navigationCookieName = 'navigation_control';
  const navigationCookieValue = 'lax-cookie';
  let resolveBootstrapHeaders!: (headers: Record<string, string>) => void;
  const bootstrapHeadersCaptured = new Promise<Record<string, string>>((resolve) => {
    resolveBootstrapHeaders = resolve;
  });

  await context.addCookies([
    {
      name: cookieName,
      value: cookieValue,
      url: appURL,
      httpOnly: true,
      sameSite: 'Strict'
    },
    {
      name: navigationCookieName,
      value: navigationCookieValue,
      url: appURL,
      httpOnly: true,
      sameSite: 'Lax'
    }
  ]);

  await page.route('http://cross-site.example/link', async (route) => {
    await route.fulfill({
      contentType: 'text/html',
      body: `<a href="${landingURL}">Open archive</a>`
    });
  });
  await stubOperationStatus(page);
  await page.route('**/api/v1/sources/status', route => route.fulfill({ json: { sources: [] } }));
  await page.route('**/api/session', async (route) => {
    resolveBootstrapHeaders(await route.request().allHeaders());
    await route.fulfill({
      contentType: 'application/json',
      body: JSON.stringify({
        auth_mode: 'session',
        csrf_token: 'csrf-token',
        https: false,
        plain_http_warning: true
      })
    });
  });

  await page.goto('http://cross-site.example/link');
  const documentRequestCaptured = page.waitForRequest(
    (request) => request.url() === landingURL && request.resourceType() === 'document'
  );
  await page.getByRole('link', { name: 'Open archive' }).click();
  const documentRequest = await documentRequestCaptured;
  const [documentHeaders, bootstrapHeaders] = await Promise.all([
    documentRequest.allHeaders(),
    bootstrapHeadersCaptured
  ]);
  const documentCookie = documentHeaders.cookie ?? '';
  const bootstrapCookie = bootstrapHeaders.cookie ?? '';

  await expect(page.getByRole('main', { name: 'Messages' })).toBeVisible();
  await expect(page.getByRole('form', { name: 'Log in' })).toHaveCount(0);
  expect(documentCookie).toContain(`${navigationCookieName}=${navigationCookieValue}`);
  expect(documentCookie).not.toContain(`${cookieName}=${cookieValue}`);
  expect(bootstrapCookie).toContain(`${cookieName}=${cookieValue}`);
});

test('Settings navigation sends a CSRF-protected session mutation', async ({ page }) => {
  let resolvePatch!: (request: { headers: Record<string, string>; body: unknown }) => void;
  const patchCaptured = new Promise<{ headers: Record<string, string>; body: unknown }>((resolve) => {
    resolvePatch = resolve;
  });
  await stubOperationStatus(page);
  await page.route('**/api/v1/sources/status', route => route.fulfill({ json: { sources: [] } }));
  await page.route('**/api/session', async (route) => {
    await route.fulfill({
      json: {
        auth_mode: 'session',
        csrf_token: 'csrf-token',
        https: true,
        plain_http_warning: false
      }
    });
  });
  await page.route('**/api/v1/settings', async (route) => {
    if (route.request().method() === 'PATCH') {
      resolvePatch({
        headers: await route.request().allHeaders(),
        body: route.request().postDataJSON()
      });
      await route.fulfill({
        headers: { ETag: '"etag-b"' },
        json: settingsDocument('dark', true)
      });
      return;
    }
    await route.fulfill({
      headers: { ETag: '"etag-a"' },
      json: settingsDocument('system', false)
    });
  });

  await page.goto('/');
  await page.getByRole('button', { name: 'Settings' }).click();
  await page.getByRole('button', { name: 'Advanced', exact: true }).click();
  await selectKitOption(page, 'Theme', 'Dark');
  await page.getByRole('button', { name: 'Save changes' }).click();

  await expect(page.getByRole('status').filter({ hasText: 'Saved. Restart msgvault to apply these changes.' })).toBeVisible();

  const patch = await patchCaptured;
  expect(patch.headers['x-csrf-token']).toBe('csrf-token');
  expect(patch.headers['if-match']).toBe('"etag-a"');
  expect(patch.body).toEqual({
    updates: [{ key: 'web.theme', value: { string: 'dark' } }]
  });
});

async function installSettingsDaemon(context: BrowserContext): Promise<void> {
  const current: Record<string, string> = {
    'web.theme': 'system', 'web.density': 'compact', 'web.default_search_mode': 'full_text'
  };
  const options: Record<string, string[]> = {
    'web.theme': ['system', 'light', 'dark'],
    'web.density': ['compact', 'comfortable'],
    'web.default_search_mode': ['full_text', 'semantic', 'hybrid']
  };
  const labels: Record<string, string> = {
    'web.theme': 'Theme', 'web.density': 'Density', 'web.default_search_mode': 'Default search mode'
  };
  const document = () => ({
    groups: [
      { id: 'browser', label: 'Appearance', description: 'How the web app looks.' },
      { id: 'server', label: 'Daemon', description: 'How the daemon runs.' },
      { id: 'search', label: 'Search', description: 'Semantic search.' }
    ],
    settings: [
      ...Object.keys(current).map((key) => ({
        key, group: 'browser', label: labels[key], kind: 'string', value: { string: current[key] },
        options: options[key], restart_required: false
      })),
      { key: 'server.log_level', group: 'server', label: 'Log level', kind: 'string', value: { string: 'info' }, restart_required: true },
      { key: 'vector.enabled', group: 'search', label: 'Semantic search', kind: 'boolean', value: { boolean: false }, restart_required: true }
    ],
    pending_restart: false
  });
  await stubOperationStatus(context);
  await context.route('**/api/v1/sources/status', route => route.fulfill({ json: { sources: [] } }));
  await context.route('**/api/session', (route) => route.fulfill({ json: {
    auth_mode: 'session', csrf_token: 'csrf-token', https: true, plain_http_warning: false
  } }));
  await context.route('**/api/v1/settings', async (route) => {
    if (route.request().method() === 'PATCH') {
      const body = route.request().postDataJSON() as { updates: Array<{ key: string; value: { string: string } }> };
      for (const { key, value } of body.updates) current[key] = value.string;
    }
    await route.fulfill({ headers: { ETag: '"settings"' }, json: document() });
  });
  await context.route('**/api/v1/explore', (route) => route.fulfill({ json: {
    rows: [], total_count: 0, cache_revision: 'settings', search_provenance: {}
  } }));
}

test('Settings keeps its category across reload and Back', async ({ context, page }) => {
  await installSettingsDaemon(context);
  await page.goto('/?workspace=settings');
  const settings = page.getByRole('main', { name: 'Settings' });
  await settings.getByRole('button', { name: 'Search & AI', exact: true }).click();
  await expect(settings.getByRole('heading', { level: 2, name: 'Search & AI' })).toBeVisible();
  await expect.poll(() => JSON.parse(new URL(page.url()).searchParams.get('explore') ?? '{}').settingsCategory).toBe('search');
  await page.reload();
  await expect(settings.getByRole('heading', { level: 2, name: 'Search & AI' })).toBeVisible();
  await settings.getByRole('button', { name: 'Advanced', exact: true }).click();
  await expect(settings.getByRole('heading', { level: 2, name: 'Advanced' })).toBeVisible();
  await page.goBack();
  await expect(settings.getByRole('heading', { level: 2, name: 'Search & AI' })).toBeVisible();
});

test('saved appearance reaches this tab, and the default mode only new tabs', async ({ context, page }) => {
  await installSettingsDaemon(context);
  await page.goto('/?workspace=settings');
  await page.getByRole('button', { name: 'Advanced', exact: true }).click();
  await selectKitOption(page, 'Theme', 'Dark');
  await selectKitOption(page, 'Search opens in', 'Best match');
  const before = page.url();
  await page.getByRole('button', { name: 'Save changes' }).click();
  await expectKitTheme(page, 'dark');
  expect(page.url()).toBe(before);
  await expect(page.getByRole('radio', { name: 'Full text' })).toBeChecked();
  expect(await page.evaluate(() => localStorage.getItem('msgvault-search-mode'))).toBe('hybrid');

  const next = await context.newPage();
  await next.goto('/?workspace=everything');
  await expect(next.getByRole('radio', { name: 'Hybrid' })).toBeChecked();
});

test('a Display menu theme override wins over a saved theme', async ({ context, page }) => {
  await installSettingsDaemon(context);
  await page.goto('/?workspace=settings');
  await setKitTheme(page, 'light');
  await page.getByRole('button', { name: 'Advanced', exact: true }).click();
  await selectKitOption(page, 'Theme', 'Dark');
  await page.getByRole('button', { name: 'Save changes' }).click();
  await expect(page.getByRole('button', { name: 'Save changes' })).toHaveCount(0);
  await expectKitTheme(page, 'light');
});

function settingsDocument(theme: string, pendingRestart: boolean) {
  return {
    groups: [{ id: 'browser', label: 'Appearance', description: 'How the web app looks.' }],
    settings: [
      {
        key: 'web.theme',
        group: 'browser',
        kind: 'string',
        value: { string: theme },
        options: ['system', 'light', 'dark'],
        restart_required: true
      }
    ],
    pending_restart: pendingRestart
  };
}
