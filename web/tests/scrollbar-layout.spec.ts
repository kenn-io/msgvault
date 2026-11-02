import { expect, test } from '@playwright/test';

import { mockArchiveLayout } from './archive-layout';
import { selectWorkspace } from './kit-ui';

// Headless Chromium hides scrollbars by default, which would hide the
// scrollbar-width shift these tests guard against.
test.use({
  launchOptions: { ignoreDefaultArgs: ['--hide-scrollbars'] },
  viewport: { width: 1700, height: 700 }
});

test('Settings panel stays in place when a long category makes it scroll', async ({ page }) => {
  await mockArchiveLayout(page);
  const attachmentSections = [
    { id: 'beeper', label: 'Beeper' },
    { id: 'slack', label: 'Slack' },
    { id: 'inline', label: 'Inline' },
    { id: 'discord', label: 'Discord' },
    { id: 'teams', label: 'Teams' }
  ];
  await page.route('**/api/v1/settings', (route) => route.fulfill({
    headers: { ETag: '"synthetic-settings"' },
    json: {
      groups: [
        { id: 'server', label: 'Daemon', sections: [{ id: 'lifecycle', label: 'Lifecycle' }] },
        { id: 'attachments', label: 'Attachments', sections: attachmentSections }
      ],
      settings: [
        { key: 'server.daemon_auto_start', group: 'server', section: 'lifecycle', label: 'Automatic start', kind: 'boolean', value: { boolean: true }, restart_required: true },
        ...attachmentSections.flatMap(({ id, label }) => [
          { key: `${id}.media_max_participants`, group: 'attachments', section: id, label: `${label} participant limit`, kind: 'integer', value: { integer: 25 }, restart_required: true },
          { key: `${id}.max_media_mb`, group: 'attachments', section: id, label: `${label} maximum size`, kind: 'integer', value: { integer: 100 }, restart_required: true }
        ])
      ],
      pending_restart: false
    }
  }));
  await page.goto('/');
  await selectWorkspace(page, 'Settings');
  await page.getByRole('navigation', { name: 'Settings', exact: true }).getByRole('button', { name: 'Server', exact: true }).click();
  const scroller = page.locator('.kit-settings__scroll');
  const short = page.getByRole('heading', { name: 'Server', exact: true });
  await expect(short).toBeVisible();
  expect(await scroller.evaluate((element) => element.scrollHeight > element.clientHeight)).toBe(false);
  const shortBox = await short.boundingBox();

  await page.getByRole('navigation', { name: 'Settings', exact: true }).getByRole('button', { name: 'Sources', exact: true }).click();
  const tall = page.getByRole('heading', { name: 'Sources', exact: true });
  await expect(tall).toBeVisible();
  expect(await scroller.evaluate((element) => element.scrollHeight > element.clientHeight)).toBe(true);
  expect(await scroller.evaluate((element) => (element as HTMLElement).offsetWidth - element.clientWidth)).toBeGreaterThan(0);
  const tallBox = await tall.boundingBox();

  expect(tallBox?.x).toBe(shortBox?.x);
  expect(await page.evaluate(() => window.innerWidth - document.documentElement.clientWidth)).toBe(0);
});

function shortPagesPDF(pageCount: number): Buffer {
  const fontObject = 3 + pageCount + 1;
  const contentObject = 3 + pageCount;
  const stream = 'BT /F1 18 Tf 36 100 Td (Fixture page) Tj ET';
  const objects = [
    '<< /Type /Catalog /Pages 2 0 R >>',
    `<< /Type /Pages /Kids [${Array.from({ length: pageCount }, (_, index) => `${3 + index} 0 R`).join(' ')}] /Count ${pageCount} >>`,
    ...Array.from({ length: pageCount }, () =>
      `<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 200] /Resources << /Font << /F1 ${fontObject} 0 R >> >> /Contents ${contentObject} 0 R >>`),
    `<< /Length ${stream.length} >>\nstream\n${stream}\nendstream`,
    '<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>'
  ];
  let source = '%PDF-1.4\n';
  const offsets: number[] = [];
  objects.forEach((object, index) => {
    offsets.push(source.length);
    source += `${index + 1} 0 obj\n${object}\nendobj\n`;
  });
  const xref = source.length;
  source += `xref\n0 ${objects.length + 1}\n0000000000 65535 f \n`;
  source += offsets.map((offset) => `${String(offset).padStart(10, '0')} 00000 n \n`).join('');
  source += `trailer\n<< /Size ${objects.length + 1} /Root 1 0 R >>\nstartxref\n${xref}\n%%EOF\n`;
  return Buffer.from(source, 'ascii');
}

test('first PDF page stays in place when a later page makes the preview scroll', async ({ page, baseURL }) => {
  if (!baseURL) throw new Error('Playwright baseURL is required');
  const bytes = shortPagesPDF(3);
  const row = {
    id: 7, key: 'file:7', entry_key: 'message:11', message_id: 11, conversation_id: 21,
    occurred_at: '2026-07-18T12:00:00Z', source_id: 1, source_type: 'synthetic',
    source_identifier: 'archive@example.com', containing_title: 'Containing item',
    filename: 'receipt.pdf', mime_type: 'application/pdf', mime_family: 'pdf', size_bytes: bytes.length,
    participant_labels: ['Example Person'], participant_domains: ['example.com'],
    content_state: 'local_content', content_available: true
  };
  await page.context().addCookies([{
    name: 'msgvault_session', value: 'browser-fixture-session', url: new URL(baseURL).origin
  }]);
  await page.route('**/api/session', (route) => route.fulfill({
    json: { auth_mode: 'session', https: false, plain_http_warning: false }
  }));
  await page.route('**/api/v1/files/search', (route) => route.fulfill({ json: {
    files: [row], total_count: 1, cache_revision: 'cache-files', search_provenance: {}
  } }));
  await page.route('**/api/v1/files/7', (route) => route.fulfill({ json: {
    id: 7, message_id: 11, conversation_id: 21, entry_key: row.entry_key, filename: row.filename,
    mime_type: row.mime_type, size_bytes: bytes.length, content_hash: 'a'.repeat(64),
    content_state: 'local_content', content_available: true
  } }));
  await page.route('**/api/v1/files/7/content', (route) => route.fulfill({ body: bytes, headers: {
    'Content-Type': row.mime_type,
    'Content-Disposition': `attachment; filename="${row.filename}"`,
    'X-Content-Type-Options': 'nosniff'
  } }));
  // Pages render one after another, so read the first page's position as each
  // one arrives. Waiting for the last page would miss the state before scrolling.
  await page.addInitScript(() => {
    const samples: Array<{ pages: number; x: number; scrolls: boolean }> = [];
    Object.assign(window, { pdfLayoutSamples: samples });
    new MutationObserver(() => {
      const pages = document.querySelectorAll('.pdf-preview .pdf-page');
      const preview = document.querySelector('.preview');
      if (!pages[0] || !preview) return;
      samples.push({
        pages: pages.length,
        x: pages[0].getBoundingClientRect().x,
        scrolls: preview.scrollHeight > preview.clientHeight
      });
    }).observe(document, { childList: true, subtree: true });
  });
  await page.goto(`/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'files', schemaVersion: 2 }))}`);
  await page.getByRole('grid', { name: 'Files results' }).focus();
  await page.keyboard.press('Home');
  await page.keyboard.press('Enter');
  const preview = page.getByRole('region', { name: 'PDF preview receipt.pdf' });
  await expect(preview.locator('.pdf-text')).toHaveCount(3);

  const samples = await page.evaluate(() =>
    (window as unknown as { pdfLayoutSamples: Array<{ pages: number; x: number; scrolls: boolean }> }).pdfLayoutSamples);
  const beforeScrolling = samples.find((sample) => sample.pages === 1);
  const afterScrolling = samples.at(-1);
  expect(beforeScrolling?.scrolls).toBe(false);
  expect(afterScrolling?.pages).toBe(3);
  expect(afterScrolling?.scrolls).toBe(true);
  expect(await page.locator('.preview').evaluate((element) => (element as HTMLElement).offsetWidth - element.clientWidth)).toBeGreaterThan(0);
  expect(afterScrolling?.x).toBe(beforeScrolling?.x);
});
