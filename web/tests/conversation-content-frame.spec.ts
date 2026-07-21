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
  await page.keyboard.press('Enter');
  const priorURL = page.url();
  await page.getByRole('button', { name: 'View conversation' }).click();
  await expect(page.getByRole('heading', { name: 'Conversation' })).toBeVisible();

  const frame = page.locator('iframe[title="Message body"]');
  const enter = page.getByRole('button', { name: 'Enter archived content' });
  await expect(page.getByText('Preparing archived content…')).toBeVisible();
  await expect(enter).toBeDisabled();
  await expect(frame).toHaveCount(0);
  releaseInitialInline();
  await expect(frame).toHaveCount(1);
  await expect(enter).toBeEnabled();
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
  expect(initialCSP).not.toContain('127.0.0.1');
  expect(networkRequests).toEqual([]);
  expect(unintendedRequests).toEqual([]);
  const archivedDocument = await frame.getAttribute('srcdoc');
  const shellOrigin = new URL(page.url()).origin;
  expect(archivedDocument).toContain(`const o=${JSON.stringify(shellOrigin)}`);
  expect(archivedDocument).not.toContain("postMessage({channel:c,nonce:n,type:'key',key:e.key},'*')");

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

  await enter.focus();
  await expect(frame).toHaveJSProperty('inert', true);
  const archivedWords = frame.contentFrame().getByText('Safe archived words');
  const archivedWordsBox = await archivedWords.boundingBox();
  expect(archivedWordsBox).not.toBeNull();
  await page.mouse.click(
    archivedWordsBox!.x + archivedWordsBox!.width / 2,
    archivedWordsBox!.y + archivedWordsBox!.height / 2
  );
  await expect(enter).toBeFocused();
  const box = await frame.boundingBox();
  expect(box).not.toBeNull();
  await page.mouse.move(box!.x + box!.width / 2, box!.y + box!.height / 2);
  await page.mouse.wheel(0, 100);
  await expect(enter).toBeFocused();

  await enter.click();
  await expect(page.getByText('Archived content active')).toBeVisible();
  await expect(frame).toHaveJSProperty('inert', false);
  const nonce = await frame.contentFrame().locator('html').getAttribute('data-bridge-nonce');
  expect(nonce).toBeTruthy();

  // Wrong source, wrong nonce, and an extra field must not exit content mode.
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
  await expect(page.getByText('Archived content active')).toBeVisible();

  const shellScroll = page.locator('.frame-scroll');
  const priorScroll = await shellScroll.evaluate((element) => element.scrollTop);
  await contentFrame!.evaluate((frameNonce) => parent.postMessage({
    channel: 'msgvault-archived-content', nonce: frameNonce,
    type: 'scroll', deltaY: 10_001
  }, '*'), nonce);
  expect(await shellScroll.evaluate((element) => element.scrollTop)).toBe(priorScroll);

  // Consent while entered synchronously exits shell content scope and detaches
  // the old frame before the newly-capable document can be entered.
  await page.getByRole('button', { name: 'Load 1 remote image' }).click();
  await expect(page.getByText('Archived content active')).toHaveCount(0);
  await expect(page.getByLabel('Archived content controls')).toBeFocused();
  await expect(enter).toBeDisabled();
  expect(await frameHandle!.evaluate((element) => element.isConnected)).toBe(false);
  await expect.poll(() => networkRequests.length).toBe(1);
  expect(networkRequests[0]).toBe('https://images.example/chart.png?token=synthetic');
  expect(remoteReferrers).toEqual([undefined]);
  expect(unintendedRequests).toEqual([]);
  releaseRemoteImage();
  await expect(enter).toBeEnabled();

  await enter.click();
  await expect(page.getByText('Archived content active')).toBeVisible();
  const consentedHandle = await frame.elementHandle();
  const consentedContentFrame = await consentedHandle?.contentFrame();
  const consentedNonce = await frame.contentFrame().locator('html').getAttribute('data-bridge-nonce');
  expect(consentedNonce).toBeTruthy();

  // The exact expected replacement-frame message exits and returns focus to the shell.
  await consentedContentFrame!.evaluate((frameNonce) => parent.postMessage({
    channel: 'msgvault-archived-content', nonce: frameNonce,
    type: 'key', key: 'Escape'
  }, '*'), consentedNonce);
  await expect(page.getByText('Archived content active')).toHaveCount(0);
  await expect(page.getByRole('button', { name: 'Enter archived content' })).toBeFocused();

  await page.evaluate(() => history.back());
  const restoredAfterBrowserBack = page.getByRole('button', { name: 'View conversation' });
  await expect(restoredAfterBrowserBack).toBeVisible();
  await expect(restoredAfterBrowserBack).toBeFocused();
  expect(page.url()).toBe(priorURL);

  const reopenAtPeer = async (): Promise<void> => {
    await page.getByRole('button', { name: 'View conversation' }).click();
    await expect(page.getByRole('heading', { name: 'Conversation' })).toBeVisible();
    await page.getByRole('button', { name: /Open message 43/ }).click();
    await expect(page.getByRole('article', { name: 'Selected message 43' })).toBeVisible();
    await expect(frame.contentFrame().getByText('Inline image unavailable: Inline logo')).toBeVisible();
    await expect(page.getByRole('button', { name: 'Enter archived content' })).toBeEnabled();
  };

  await reopenAtPeer();
  await page.getByRole('button', { name: 'Back from conversation' }).click();
  const restoredAfterToolbarBack = page.getByRole('button', { name: 'View conversation' });
  await expect(restoredAfterToolbarBack).toBeVisible();
  await expect(restoredAfterToolbarBack).toBeFocused();
  expect(page.url()).toBe(priorURL);

  await reopenAtPeer();
  await page.keyboard.press('Escape');
  const restoredAfterEscape = page.getByRole('button', { name: 'View conversation' });
  await expect(restoredAfterEscape).toBeVisible();
  await expect(restoredAfterEscape).toBeFocused();
  expect(page.url()).toBe(priorURL);
});
