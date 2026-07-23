import { expect, test } from '@playwright/test';

const secureRow = {
  key: 'source:1:message:secure', kind: 'message', message_type: 'email', conversation_type: 'email_thread',
  title: 'Synthetic secure message', preview: 'Archived content', occurred_at: '2026-01-03T12:00:00Z',
  source_id: 1, source_identifier: 'archive@example.com', source_type: 'synthetic',
  participant_labels: ['Synthetic Person'], participant_ids: [1], attachment_count: 0,
  attachment_size: 0, has_attachments: false, deleted_from_source: false, message_count: 1,
  anchor_message_id: 42, conversation_id: 7, match: {}
};

test('sanitized archived HTML requires remote-image consent and rejects forged frame messages', async ({ page }) => {
  const remoteRequests: string[] = [];
  const proxiedURLs: Array<string | null> = [];
  await page.route('**/api/session', (route) => route.fulfill({ json: {
    auth_mode: 'session', csrf_token: 'csrf', https: true, plain_http_warning: false
  } }));
  await page.route('**/api/v1/explore', (route) => route.fulfill({ json: {
    rows: [secureRow], total_count: 1, cache_revision: 'security', search_provenance: {}
  } }));
  await page.route('**/api/v1/conversations/7**', (route) => route.fulfill({ json: {
    id: 7, anchor_id: 42, has_before: false, has_after: false, total: 1,
    messages: [{ id: 42, conversation_id: 7, subject: secureRow.title, message_type: 'email',
      from: 'alice@example.com', to: ['bob@example.com'], sent_at: secureRow.occurred_at,
      snippet: secureRow.preview, body: 'Plain body', attachments: [],
      body_html: '<script>parent.document.body.remove()</script><form action="https://collector.example/"><input autofocus></form><img src="https://images.example/remote.png" alt="Remote chart"><p>Safe body</p><p style="color:#b3261e;background:url(https://collector.example/steal);position:fixed;top:0;left:0;width:expression(alert(1))">Styled body</p>' }]
  } }));
  await page.route('https://collector.example/**', (route) => {
    remoteRequests.push(route.request().url());
    return route.abort();
  });
  await page.route('https://images.example/**', (route) => {
    remoteRequests.push(route.request().url());
    return route.abort();
  });
  await page.route('**/api/v1/content/remote-image**', (route) => {
    proxiedURLs.push(new URL(route.request().url()).searchParams.get('url'));
    return route.fulfill({ contentType: 'image/png', body: Buffer.from(
      'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=', 'base64') });
  });

  await page.goto(`/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
  const grid = page.getByRole('grid', { name: 'Everything results' });
  await grid.focus();
  await page.keyboard.press('Enter');

  // The reading pane renders the sanitized message frame directly — no
  // intermediate step, no entry gating.
  const frame = page.locator('iframe[title="Message body"]');
  await expect(frame).toHaveAttribute('sandbox', 'allow-scripts');
  const frameHandle = await frame.elementHandle();
  const contentFrame = await frameHandle?.contentFrame();
  expect(contentFrame).not.toBeNull();
  expect(await contentFrame!.evaluate(() => origin)).toBe('null');
  await expect(frame.contentFrame().locator('script')).toHaveCount(1);
  await expect(frame.contentFrame().locator('form,input')).toHaveCount(0);
  const csp = await frame.contentFrame().locator('meta[http-equiv="Content-Security-Policy"]').getAttribute('content');
  expect(csp).toContain("default-src 'none'");
  expect(csp).toContain("object-src 'none'");
  expect(remoteRequests).toEqual([]);

  // Author styling survives the allowlist, but url() smuggling, expression(),
  // and positioning that could overlay the shell are all gone.
  const styledBody = frame.contentFrame().getByText('Styled body');
  await expect(styledBody).toHaveAttribute('style', 'color: #b3261e');
  await expect(styledBody).toHaveCSS('color', 'rgb(179, 38, 30)');
  await expect(styledBody).toHaveCSS('position', 'static');

  // A forged bridge message (wrong nonce) and a spoofed one (right nonce,
  // wrong source window) are both ignored; only the frame's own message
  // with the real nonce drives the Escape hand-off to the thread.
  const nonce = await frame.contentFrame().locator('html').getAttribute('data-bridge-nonce');
  const thread = page.getByRole('region', { name: 'Conversation thread' });
  await contentFrame!.evaluate(() => parent.postMessage({
    channel: 'msgvault-archived-content', nonce: 'forged', type: 'key', key: 'Escape'
  }, '*'));
  await page.evaluate((frameNonce) => postMessage({
    channel: 'msgvault-archived-content', nonce: frameNonce, type: 'key', key: 'Escape'
  }, '*'), nonce);
  await expect(thread).not.toBeFocused();
  await contentFrame!.evaluate((frameNonce) => parent.postMessage({
    channel: 'msgvault-archived-content', nonce: frameNonce, type: 'key', key: 'Escape'
  }, '*'), nonce);
  await expect(thread).toBeFocused();

  // Remote images stay blocked behind one quiet inline notice. Consent
  // fetches through the daemon's SSRF-hardened proxy only — the browser
  // never contacts the sender host, before or after consent.
  await expect(page.getByText('1 remote image is not loaded.')).toBeVisible();
  await page.getByRole('button', { name: 'Load 1 remote image' }).click();
  await expect.poll(() => proxiedURLs).toHaveLength(1);
  expect(proxiedURLs).toEqual(['https://images.example/remote.png']);
  expect(remoteRequests).toEqual([]);
  await expect(page.getByText('1 remote image is not loaded.')).toBeHidden();
});

test('activating a replacement API key rejects the old browser authority and requires login', async ({ page }) => {
  let daemonRestarted = false;
  let settingsPatch: { body: unknown; csrf: string | undefined; cookie: string | undefined } | undefined;
  const authorizedAfterPatchCookies: Array<string | undefined> = [];
  const rejectedCookies: Array<string | undefined> = [];
  const restartBootstrapCookies: Array<string | undefined> = [];
  const daemonKey = {
    key: 'server.api_key', kind: 'secret', secret: { configured: true }, restart_required: true
  };
  await page.route('**/api/session', (route) => {
    if (daemonRestarted) {
      restartBootstrapCookies.push(route.request().headers().cookie);
      return route.fulfill({
        json: { auth_mode: 'required', https: true, plain_http_warning: false }
      });
    }
    return route.fulfill({
      headers: { 'Set-Cookie': 'msgvault_session=old-authority; Path=/; HttpOnly; SameSite=Strict' },
      json: { auth_mode: 'session', csrf_token: 'old-csrf', https: true, plain_http_warning: false }
    });
  });
  await page.route('**/api/v1/settings', async (route) => {
    if (route.request().method() === 'PATCH') {
      settingsPatch = {
        body: route.request().postDataJSON(),
        csrf: route.request().headers()['x-csrf-token'],
        cookie: route.request().headers().cookie
      };
      return route.fulfill({
        headers: { ETag: '"settings-b"' },
        json: { settings: [daemonKey], pending_restart: true }
      });
    }
    return route.fulfill({
      headers: { ETag: '"settings-a"' },
      json: { settings: [daemonKey], pending_restart: false }
    });
  });
  await page.route('**/api/v1/explore', (route) => {
    if (daemonRestarted) {
      rejectedCookies.push(route.request().headers().cookie);
      return route.fulfill({ status: 401, json: {
        error: 'unauthorized', message: 'The activated key invalidated the old session.'
      } });
    }
    if (settingsPatch) authorizedAfterPatchCookies.push(route.request().headers().cookie);
    return route.fulfill({ json: {
      rows: [], total_count: 0, cache_revision: 'before-activation', search_provenance: {}
    } });
  });

  await page.goto(`/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
  await expect(page.getByRole('main', { name: 'Everything' })).toBeVisible();
  await page.getByRole('button', { name: 'Settings', exact: true }).click();
  await page.getByLabel('New daemon API key').fill('replacement-key');
  await page.getByLabel('I understand the API key changes after restart').check();
  await page.getByRole('button', { name: 'Save settings' }).click();
  await expect(page.getByRole('status')).toContainText('pending restart');
  expect(settingsPatch).toEqual({
    body: {
      updates: [{ key: 'server.api_key', secret: { action: 'set', value: 'replacement-key' } }],
      confirm_api_key_restart: true
    },
    csrf: 'old-csrf',
    cookie: expect.stringContaining('msgvault_session=old-authority')
  });

  await page.getByRole('button', { name: 'Everything', exact: true }).click();
  await expect(page.getByRole('main', { name: 'Everything' })).toBeVisible();
  expect(authorizedAfterPatchCookies).toEqual([expect.stringContaining('msgvault_session=old-authority')]);
  expect(rejectedCookies).toEqual([]);

  daemonRestarted = true;
  await page.reload();
  await expect(page.getByRole('main', { name: 'Authentication' })).toBeVisible();
  await expect(page.getByLabel('API key')).toBeVisible();
  expect(restartBootstrapCookies).toEqual([expect.stringContaining('msgvault_session=old-authority')]);
  expect(rejectedCookies).toEqual([]);
});
