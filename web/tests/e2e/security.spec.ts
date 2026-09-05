import { expect, test } from '@playwright/test';
import { installOperations, OPERATION_PRIVACY_SENTINELS } from './fixtures/operations';

const secureRow = {
  key: 'source:1:message:secure', kind: 'message', message_type: 'email', conversation_type: 'email_thread',
  title: 'Synthetic secure message', preview: 'Archived content', occurred_at: '2026-01-03T12:00:00Z',
  source_id: 1, source_identifier: 'archive@example.com', source_type: 'synthetic',
  participant_labels: ['Synthetic Person'], participant_ids: [1], attachment_count: 0,
  attachment_size: 0, has_attachments: false, deleted_from_source: false, message_count: 1,
  anchor_message_id: 42, conversation_id: 7, match: {}
};

test('operations keeps production privacy sentinels out of rendered text', async ({ page }) => {
  await installOperations(page);
  await page.goto(`/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'operations' }))}`);
  await expect(page.getByRole('main', { name: 'Operations' })).toBeVisible();
  await page.getByRole('button', { name: 'Open CardDAV sync run' }).click();
  await expect(page.getByRole('region', { name: 'Operation run detail' })).toBeVisible();

  const rendered = await page.locator('body').innerText();
  for (const sentinel of OPERATION_PRIVACY_SENTINELS) {
    expect(rendered, `rendered private sentinel: ${sentinel}`).not.toContain(sentinel);
  }
});

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
    proxiedURLs.push((route.request().postDataJSON() as { url: string }).url);
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

test('daemon API key stays host-managed and never crosses the browser settings write boundary', async ({ page }) => {
	let settingsPatch: unknown;
	const daemonKey = {
		key: 'server.api_key', group: 'server', label: 'Daemon API key',
		description: 'Key used by remote clients and browser login.', kind: 'secret',
		secret: { configured: true, source: 'environment' }, restart_required: true, read_only: true
	};
	await page.route('**/api/session', (route) => {
		return route.fulfill({
			headers: { 'Set-Cookie': 'msgvault_session=old-authority; Path=/; HttpOnly; SameSite=Strict' },
      json: { auth_mode: 'session', csrf_token: 'old-csrf', https: true, plain_http_warning: false }
    });
  });
	await page.route('**/api/v1/settings', async (route) => {
		if (route.request().method() === 'PATCH') {
			settingsPatch = route.request().postDataJSON();
			return route.fulfill({ status: 500, json: { error: 'browser_must_not_write_daemon_key' } });
		}
    return route.fulfill({
      headers: { ETag: '"settings-a"' },
      json: { settings: [daemonKey], pending_restart: false }
    });
	});
	await page.route('**/api/v1/explore', (route) => {
		return route.fulfill({ json: {
			rows: [], total_count: 0, cache_revision: 'host-managed-key', search_provenance: {}
		} });
  });

	await page.goto(`/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
	await expect(page.getByRole('main', { name: 'Everything' })).toBeVisible();
	await page.getByRole('button', { name: 'Settings', exact: true }).click();
	await expect(page.getByText('Configured')).toBeVisible();
	await expect(page.getByText('Set via config.toml on the daemon host.')).toBeVisible();
	await expect(page.getByLabel('New daemon API key')).toHaveCount(0);
	await page.getByRole('button', { name: 'Save settings' }).click();
	expect(settingsPatch).toBeUndefined();

	await page.getByRole('button', { name: 'Everything', exact: true }).click();
	await expect(page.getByRole('main', { name: 'Everything' })).toBeVisible();
});
