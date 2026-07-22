import { expect, test } from '@playwright/test';

const row = {
  key: 'source:1:message:source-1',
  kind: 'message',
  message_type: 'email',
  conversation_type: 'email_thread',
  title: 'Archived security fixture',
  preview: 'A synthetic archived message',
  occurred_at: '2026-07-18T12:00:00Z',
  source_id: 1,
  source_identifier: 'archive@example.com',
  source_type: 'synthetic',
  participant_labels: ['Example Person'],
  participant_ids: [1],
  attachment_count: 0,
  attachment_size: 0,
  has_attachments: false,
  deleted_from_source: false,
  message_count: 1,
  anchor_message_id: 42,
  conversation_id: 7,
  match: {}
};

test('archived content has an opaque capability boundary and durable conversation history', async ({ page, baseURL }) => {
  const networkRequests: string[] = [];
  const unintendedRequests: string[] = [];
  const inlineRequests: Array<{ url: string; cookie?: string }> = [];
  const remoteReferrers: Array<string | undefined> = [];
  let releaseInitialInline!: () => void;
  const initialInlineGate = new Promise<void>((resolve) => { releaseInitialInline = resolve; });
  let delayInitialInline = true;
  let releaseRemoteImage!: () => void;
  const remoteImageGate = new Promise<void>((resolve) => { releaseRemoteImage = resolve; });
  if (!baseURL) throw new Error('Playwright baseURL is required');
  await page.context().addCookies([{
    name: 'msgvault_session', value: 'synthetic-session', url: new URL(baseURL).origin
  }]);
  await page.route('**/api/session', (route) =>
    route.fulfill({ json: { auth_mode: 'session', https: false, plain_http_warning: false } })
  );
  await page.route('**/api/v1/explore', (route) => route.fulfill({ json: {
    rows: [row], total_count: 1, cache_revision: 'cache-reader', search_provenance: {}
  } }));
  await page.route('**/api/v1/conversations/7**', (route) => route.fulfill({ json: {
    id: 7,
    anchor_id: Number(new URL(route.request().url()).searchParams.get('anchor')),
    messages: [42, 43].map((id) => ({
      id,
      conversation_id: 7,
      subject: `${row.title} ${id}`,
      message_type: 'email',
      from: 'alice@example.com',
      to: ['bob@example.com'],
      sent_at: row.occurred_at,
      snippet: row.preview,
      labels: [],
      has_attachments: false,
      size_bytes: 10,
      body: 'Plain archived body',
      body_html: [
        '<button autofocus accesskey="x">Close inspector</button>',
        '<form action="https://collector.example/submit"><input name="secret"></form>',
        '<svg><a xlink:href="https://collector.example/svg"><text>SVG text</text></a></svg>',
        '<p style="background:u/**/rl(https://collector.example/comment-css)">Safe archived words</p>',
        '<p style="background:\\75\\72\\6c(https://collector.example/escaped-css)">Escaped CSS words</p>',
        '<img src="cid:logo@example.com" alt="Inline logo">',
        '<img src="https://images.example/chart.png?token=synthetic" alt="Chart">'
      ].join(''),
      attachments: []
    })),
    has_before: false,
    has_after: false,
    total: 2
  } }));
  await page.route('**/api/v1/messages/*/inline?**', async (route) => {
    inlineRequests.push({
      url: route.request().url(),
      cookie: route.request().headers()['cookie']
    });
    if (new URL(route.request().url()).pathname.includes('/messages/43/')) {
      await route.fulfill({ status: 404, contentType: 'application/json', body: '{"error":"missing"}' });
      return;
    }
    if (delayInitialInline) {
      delayInitialInline = false;
      await initialInlineGate;
    }
    await route.fulfill({
      contentType: 'image/png',
      body: Buffer.from('iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=', 'base64')
    });
  });
  await page.route('https://collector.example/**', async (route) => {
    unintendedRequests.push(route.request().url());
    await route.abort();
  });
  await page.route('https://images.example/**', async (route) => {
    networkRequests.push(route.request().url());
    remoteReferrers.push(route.request().headers()['referer']);
    await remoteImageGate;
    await route.fulfill({
      contentType: 'image/png',
      body: Buffer.from('iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=', 'base64')
    });
  });

  const explore = encodeURIComponent(JSON.stringify({ workspace: 'everything' }));
  await page.goto(`/?feature=reader-security&explore=${explore}`);
  const grid = page.getByRole('grid', { name: 'Everything results' });
  await expect(grid.getByText(row.title)).toBeVisible();
  await grid.focus();
  const priorURL = page.url();
  await page.keyboard.press('Enter');

  // The reading pane opens straight into the thread: the anchor message is
  // expanded and its archived body renders directly, with no entry gating.
  const reading = page.getByRole('complementary', { name: `Reading pane: ${row.title}` });
  await expect(reading).toBeVisible();
  const anchorCard = page.locator('[data-message-id="42"]');
  const frame = anchorCard.locator('iframe[title="Message body"]');
  await expect(anchorCard.getByText('Preparing message…')).toBeVisible();
  await expect(frame).toHaveCount(0);
  releaseInitialInline();
  await expect(frame).toHaveCount(1);
  await expect(frame).toHaveAttribute('sandbox', 'allow-scripts');
  await expect(frame).not.toHaveAttribute('sandbox', /allow-same-origin/);
  const frameHandle = await frame.elementHandle();
  const contentFrame = await frameHandle?.contentFrame();
  expect(contentFrame).not.toBeNull();
  expect(await contentFrame!.evaluate(() => origin)).toBe('null');
  await expect(frame.contentFrame().getByText('Safe archived words')).toBeVisible();
  await expect(frame.contentFrame().getByText('Escaped CSS words')).toBeVisible();
  await expect(frame.contentFrame().getByRole('button')).toHaveCount(0);
  await expect(frame.contentFrame().locator('img[alt="Inline logo"]')).toHaveAttribute('src', /^data:image\/png;base64,/);
  await expect.poll(() => inlineRequests.length).toBe(1);
  expect(inlineRequests[0]?.url).toContain('cid=logo%40example.com');
  expect(inlineRequests[0]?.cookie).toContain('msgvault_session=synthetic-session');
  const initialCSP = await frame.contentFrame().locator('meta[http-equiv="Content-Security-Policy"]').getAttribute('content');
  expect(initialCSP).toContain('img-src data:');
  // Only shell-origin static assets may execute or style; images stay data:.
  const imgDirective = initialCSP?.split(';').find((directive) => directive.trim().startsWith('img-src'));
  expect(imgDirective?.trim()).toBe('img-src data:');
  const shellOrigin = new URL(page.url()).origin;
  expect(initialCSP).toContain(`script-src ${shellOrigin}/archived-frame.js`);
  expect(initialCSP).toContain(`style-src ${shellOrigin}/archived-frame.css`);
  expect(initialCSP).not.toContain("'unsafe-inline'");
  expect(networkRequests).toEqual([]);
  expect(unintendedRequests).toEqual([]);
  const archivedDocument = await frame.getAttribute('srcdoc');
  expect(archivedDocument).toContain(`data-bridge-origin="${shellOrigin}"`);
  expect(archivedDocument).not.toMatch(/<script>|<style>/);

  // The bridge sizes the frame to its content — the height must leave the
  // shell's compact default, and the archived document itself must not
  // scroll internally (the thread is the only scroller).
  await expect.poll(async () => Number.parseFloat(await frame.evaluate(
    (element: HTMLIFrameElement) => element.style.height
  ))).toBeGreaterThan(96);
  await expect.poll(() => frame.contentFrame().locator('html').evaluate((html) =>
    html.scrollHeight - html.clientHeight
  )).toBeLessThanOrEqual(1);

  // Re-embedding the exact document below an opaque unexpected parent cannot
  // receive bridge messages because targetOrigin remains pinned to the shell.
  await page.evaluate((srcdoc) => {
    const host = document.createElement('iframe');
    host.title = 'Unexpected archived content parent';
    host.setAttribute('sandbox', 'allow-scripts');
    host.srcdoc = '<body data-received="no"><script>addEventListener("message",()=>document.body.dataset.received="yes")<\/script><iframe title="Re-embedded archived content" sandbox="allow-scripts"></iframe></body>';
    document.body.append(host);
  }, archivedDocument);
  const outerFrame = await page.getByTitle('Unexpected archived content parent').contentFrame();
  await outerFrame.getByTitle('Re-embedded archived content').evaluate((element, srcdoc) => {
    element.setAttribute('srcdoc', srcdoc ?? '');
  }, archivedDocument);
  const unexpectedFrame = outerFrame.getByTitle('Re-embedded archived content').contentFrame();
  await unexpectedFrame.locator('body').evaluate((body) => {
    body.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }));
  });
  await expect.poll(() => outerFrame.locator('body').getAttribute('data-received')).toBe('no');

  const thread = page.getByRole('region', { name: 'Conversation thread' });
  const nonce = await frame.contentFrame().locator('html').getAttribute('data-bridge-nonce');
  expect(nonce).toBeTruthy();

  // Wrong source, wrong nonce, an extra field, and an oversized scroll delta
  // must all be ignored by the bridge.
  const priorScroll = await thread.evaluate((element) => element.scrollTop);
  await page.evaluate((frameNonce) => {
    postMessage({
      channel: 'msgvault-archived-content', nonce: frameNonce,
      type: 'key', key: 'Escape'
    }, '*');
  }, nonce);
  await contentFrame!.evaluate(() => parent.postMessage({
    channel: 'msgvault-archived-content', nonce: 'wrong', type: 'key', key: 'Escape'
  }, '*'));
  await contentFrame!.evaluate((frameNonce) => parent.postMessage({
    channel: 'msgvault-archived-content', nonce: frameNonce,
    type: 'key', key: 'Escape', extra: true
  }, '*'), nonce);
  await contentFrame!.evaluate((frameNonce) => parent.postMessage({
    channel: 'msgvault-archived-content', nonce: frameNonce,
    type: 'scroll', deltaY: 10_001
  }, '*'), nonce);
  await expect(thread).not.toBeFocused();
  expect(await thread.evaluate((element) => element.scrollTop)).toBe(priorScroll);

  // The exact expected frame message hands focus back out to the thread.
  await contentFrame!.evaluate((frameNonce) => parent.postMessage({
    channel: 'msgvault-archived-content', nonce: frameNonce,
    type: 'key', key: 'Escape'
  }, '*'), nonce);
  await expect(thread).toBeFocused();

  // Remote-image consent rebuilds the document: the old incapable frame is
  // detached before the newly-capable one loads, and the remote fetch goes
  // out without referrer or credentials leaking.
  await page.getByRole('button', { name: 'Load 1 remote image' }).click();
  await expect(page.getByText('1 remote image is not loaded.')).toHaveCount(0);
  expect(await frameHandle!.evaluate((element) => element.isConnected)).toBe(false);
  await expect.poll(() => networkRequests.length).toBe(1);
  expect(networkRequests[0]).toBe('https://images.example/chart.png?token=synthetic');
  expect(remoteReferrers).toEqual([undefined]);
  expect(unintendedRequests).toEqual([]);
  releaseRemoteImage();
  const consentedNonce = await frame.contentFrame().locator('html').getAttribute('data-bridge-nonce');
  expect(consentedNonce).toBeTruthy();
  expect(consentedNonce).not.toBe(nonce);

  // Browser Back closes the reading pane, restores the pre-open URL, and
  // returns focus to the grid.
  await page.evaluate(() => history.back());
  await expect(reading).toBeHidden();
  await expect(grid).toBeFocused();
  expect(page.url()).toBe(priorURL);

  // Reopening and expanding the peer message keeps the anchor expanded and
  // renders the peer's own frame, whose missing inline image degrades to a
  // visible placeholder.
  await page.keyboard.press('Enter');
  await expect(reading).toBeVisible();
  await reading.getByRole('button', { name: 'Expand message 43 from alice@example.com' }).click();
  const peerCard = page.locator('[data-message-id="43"]');
  await expect(peerCard).toHaveAttribute('aria-current', 'true');
  const peerFrame = peerCard.locator('iframe[title="Message body"]');
  await expect(peerFrame.contentFrame().getByText('Inline image unavailable: Inline logo')).toBeVisible();
  await expect(frame).toHaveCount(1);

  // Escape closes the pane from anywhere in it, restoring URL and grid focus.
  await page.keyboard.press('Escape');
  await expect(reading).toBeHidden();
  await expect(grid).toBeFocused();
  expect(page.url()).toBe(priorURL);
});
