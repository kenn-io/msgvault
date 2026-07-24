import { expect, test } from '@playwright/test';

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

  await expect(page.getByRole('main', { name: 'Everything' })).toBeVisible();
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
  await page.getByRole('main', { name: 'Settings' }).getByLabel('Theme').selectOption('dark');
  await page.getByRole('button', { name: 'Save settings' }).click();

  const patch = await patchCaptured;
  expect(patch.headers['x-csrf-token']).toBe('csrf-token');
  expect(patch.headers['if-match']).toBe('"etag-a"');
  expect(patch.body).toEqual({
    updates: [{ key: 'web.theme', value: { string: 'dark' } }],
    confirm_api_key_restart: false
  });
  await expect(page.getByText('Changes are pending restart.')).toBeVisible();
});

function settingsDocument(theme: string, pendingRestart: boolean) {
  return {
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
