import { expect, test, type Page } from '@playwright/test';

function exploreURLState() {
  return {
    schemaVersion: 2,
    workspace: 'files',
    query: '',
    searchMode: 'full_text',
    filters: [],
    groupingChain: [],
    presentation: 'table',
    sort: [{ field: 'occurred_at', direction: 'desc' }],
    fileSort: { field: 'occurred_at', direction: 'desc' },
    fileFilenameQuery: '',
    fileMIMEFamilies: [],
    columns: ['kind', 'people', 'title', 'excerpt', 'time', 'attachments'],
    columnWidths: {},
    activeRow: null,
    selectedRow: null,
    inspectorPinned: false,
    inspectorWidth: 380,
    conversationAnchor: null,
    scrollAnchor: null
  };
}

function file(filename: string, mimeType: string, mimeFamily: string) {
  return {
    id: 7,
    key: 'file:7',
    entry_key: 'message:11',
    message_id: 11,
    conversation_id: 21,
    occurred_at: '2026-07-18T12:00:00Z',
    source_id: 1,
    source_type: 'synthetic',
    source_identifier: 'archive@example.com',
    containing_title: 'Containing item',
    filename,
    mime_type: mimeType,
    mime_family: mimeFamily,
    size_bytes: 1024,
    participant_labels: ['Example Person'],
    participant_domains: ['example.com'],
    content_state: 'local_content',
    content_available: true
  };
}

async function prepare(page: Page, baseURL: string | undefined, row: ReturnType<typeof file>, bytes: Buffer) {
  if (!baseURL) throw new Error('Playwright baseURL is required');
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
    id: 7, message_id: 11, conversation_id: 21, filename: row.filename,
    mime_type: row.mime_type, size_bytes: bytes.length, content_hash: 'a'.repeat(64),
    content_state: 'local_content', content_available: true
  } }));
  let contentCookie = '';
  await page.route(`**/api/v1/files/${row.id}/content`, async (route) => {
    contentCookie = route.request().headers().cookie ?? '';
    await route.fulfill({ body: bytes, headers: {
      'Content-Type': row.mime_type,
      'Content-Disposition': `attachment; filename="${row.filename}"`,
      'X-Content-Type-Options': 'nosniff'
    } });
  });
  const state = exploreURLState();
  await page.goto(`/?explore=${encodeURIComponent(JSON.stringify(state))}`);
  return () => contentCookie;
}

test('Escape closes an authenticated image once, suspends background shortcuts, and restores exact keyboard focus', async ({ page, baseURL }) => {
  await page.addInitScript(() => {
    const create = URL.createObjectURL.bind(URL);
    const revoke = URL.revokeObjectURL.bind(URL);
    Object.assign(window, { previewURLs: { created: 0, revoked: 0 } });
    URL.createObjectURL = (blob: Blob) => {
      (window as unknown as { previewURLs: { created: number } }).previewURLs.created += 1;
      return create(blob);
    };
    URL.revokeObjectURL = (url: string) => {
      (window as unknown as { previewURLs: { revoked: number } }).previewURLs.revoked += 1;
      revoke(url);
    };
  });
  const png = Buffer.from(
    'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=',
    'base64'
  );
  const contentCookie = await prepare(page, baseURL, file('pixel.png', 'image/png', 'image'), png);

  const grid = page.getByRole('grid', { name: 'Files results' });
  await page.getByRole('searchbox', { name: 'Filter filename' }).fill('pixel');
  await grid.focus();
  await expect(grid).toHaveAttribute('aria-activedescendant', 'file-row-7');
  await page.keyboard.press('Home');
  await expect.poll(() => {
    const encoded = new URL(page.url()).searchParams.get('explore');
    return encoded ? (JSON.parse(encoded) as { activeRow?: string }).activeRow : undefined;
  }).toBe('file:7');
  const filteredURL = page.url();
  await page.keyboard.press('Enter');
  await expect(page.getByRole('img', { name: 'Preview pixel.png' })).toBeVisible();
  expect(contentCookie()).toContain('msgvault_session=browser-fixture-session');
  await page.keyboard.press('f');
  await expect(page.getByRole('button', { name: 'Filters' })).toHaveAttribute('aria-expanded', 'false');
  await page.evaluate(() => {
    (window as unknown as { previewEscapePrevented?: boolean }).previewEscapePrevented = false;
    window.addEventListener('keydown', (event) => {
      if (event.key === 'Escape') {
        (window as unknown as { previewEscapePrevented?: boolean }).previewEscapePrevented = event.defaultPrevented;
      }
    }, { once: true });
  });
  await page.keyboard.press('Escape');
  await expect(grid).toBeFocused();
  await expect(page).toHaveURL(filteredURL);
  await expect.poll(() => page.evaluate(() =>
    (window as unknown as { previewEscapePrevented?: boolean }).previewEscapePrevented
  )).toBe(true);
  await expect.poll(() => page.evaluate(() =>
    (window as unknown as { previewURLs: { created: number; revoked: number } }).previewURLs
  )).toEqual({ created: 1, revoked: 1 });
  await page.goBack();
  await expect(page).toHaveURL(filteredURL);
});

test('malformed and mislabeled image bytes fail without allocating an object URL', async ({ page, baseURL }) => {
  await page.addInitScript(() => {
    Object.assign(window, { previewURLs: { created: 0 } });
    const create = URL.createObjectURL.bind(URL);
    URL.createObjectURL = (blob: Blob) => {
      (window as unknown as { previewURLs: { created: number } }).previewURLs.created += 1;
      return create(blob);
    };
  });
  await prepare(page, baseURL, file('fake.png', 'image/png', 'image'), Buffer.from('<html>not an image</html>'));
  const grid = page.getByRole('grid', { name: 'Files results' });
  await grid.focus();
  await page.keyboard.press('Enter');

  await expect(page.getByRole('alert')).toContainText('Image preview was rejected');
  expect(await page.evaluate(() =>
    (window as unknown as { previewURLs: { created: number } }).previewURLs.created
  )).toBe(0);
});

test('malformed PDF fails without retaining a canvas', async ({ page, baseURL }) => {
  await prepare(page, baseURL, file('fake.pdf', 'application/pdf', 'pdf'), Buffer.from('<html>not a pdf</html>'));
  const grid = page.getByRole('grid', { name: 'Files results' });
  await grid.focus();
  await page.keyboard.press('Enter');

  await expect(page.getByRole('alert')).toContainText('file signature is invalid');
  await expect(page.locator('.pdf-preview canvas')).toHaveCount(0);
});

test('opening a containing conversation and one Back restores the exact Files URL and active row', async ({ page, baseURL }) => {
  await page.route('**/api/v1/explore', (route) => route.fulfill({ json: {
    rows: [{
      key: 'message:11', kind: 'message', message_type: 'email', conversation_type: 'email_thread',
      title: 'Containing item', preview: 'Message preview', occurred_at: '2026-07-18T12:00:00Z',
      source_id: 1, source_identifier: 'archive@example.com', source_type: 'synthetic',
      participant_labels: ['Example Person'], participant_ids: [1], attachment_count: 1,
      attachment_size: 1024, has_attachments: true, deleted_from_source: false, message_count: 1,
      conversation_id: 21, anchor_message_id: 11, match: {}
    }],
    total_count: 1, cache_revision: 'cache-files', search_provenance: {}
  } }));
  await page.route('**/api/v1/conversations/21**', (route) => route.fulfill({ json: {
    conversation_id: 21, anchor_id: 11, has_before: false, has_after: false, total: 1,
    messages: [{
      id: 11, conversation_id: 21, subject: 'Containing item', from: 'Example Person', to: [],
      sent_at: '2026-07-18T12:00:00Z', snippet: 'Message preview', body: 'Archived body',
      body_html: '', attachments: []
    }]
  } }));
  await prepare(page, baseURL, file('fixture.pdf', 'application/pdf', 'pdf'), tinyPDF('Back fixture'));
  const grid = page.getByRole('grid', { name: 'Files results' });
  await grid.focus();
  await page.keyboard.press('Enter');
  const filesURL = page.url();

  await page.getByRole('button', { name: 'Open containing conversation' }).click();
  await expect(page.getByRole('region', { name: 'Containing conversation' })).toBeVisible();
  await page.goBack();

  await expect(page).toHaveURL(filesURL);
  await expect(page.getByRole('dialog', { name: 'View fixture.pdf' })).toBeVisible();
  await expect(grid).toHaveAttribute('aria-activedescendant', 'file-row-7');
});

test('real PDF renders in-app and Escape closes with shortcut isolation and exact focus restoration', async ({ page, baseURL }) => {
  const pdf = tinyPDF('Fixture preview text');
  await prepare(page, baseURL, file('fixture.pdf', 'application/pdf', 'pdf'), pdf);

  const grid = page.getByRole('grid', { name: 'Files results' });
  await grid.focus();
  await page.keyboard.press('Enter');
  const preview = page.getByRole('region', { name: 'PDF preview fixture.pdf' });
  await expect(preview.getByLabel('PDF page 1', { exact: true })).toBeVisible();
  await expect(preview.locator('.pdf-text')).toContainText('Fixture preview text');
  await expect(page.locator('iframe, embed, object')).toHaveCount(0);
  await page.keyboard.press('f');
  await expect(page.getByRole('button', { name: 'Filters' })).toHaveAttribute('aria-expanded', 'false');
  await page.evaluate(() => {
    (window as unknown as { pdfEscapePrevented?: boolean }).pdfEscapePrevented = false;
    window.addEventListener('keydown', (event) => {
      if (event.key === 'Escape') {
        (window as unknown as { pdfEscapePrevented?: boolean }).pdfEscapePrevented = event.defaultPrevented;
      }
    }, { once: true });
  });

  await page.keyboard.press('Escape');

  await expect(page.getByRole('dialog', { name: 'View fixture.pdf' })).toHaveCount(0);
  await expect(grid).toBeFocused();
  await expect.poll(() => page.evaluate(() =>
    (window as unknown as { pdfEscapePrevented?: boolean }).pdfEscapePrevented
  )).toBe(true);
});

function tinyPDF(text: string): Buffer {
  const escaped = text.replace(/[()\\]/g, (character) => `\\${character}`);
  const stream = `BT /F1 18 Tf 36 100 Td (${escaped}) Tj ET`;
  const objects = [
    '<< /Type /Catalog /Pages 2 0 R >>',
    '<< /Type /Pages /Kids [3 0 R] /Count 1 >>',
    '<< /Type /Page /Parent 2 0 R /MediaBox [0 0 300 144] /Resources << /Font << /F1 5 0 R >> >> /Contents 4 0 R >>',
    `<< /Length ${Buffer.byteLength(stream)} >>\nstream\n${stream}\nendstream`,
    '<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>'
  ];
  let source = '%PDF-1.4\n';
  const offsets = [0];
  for (let index = 0; index < objects.length; index += 1) {
    offsets.push(Buffer.byteLength(source));
    source += `${index + 1} 0 obj\n${objects[index]}\nendobj\n`;
  }
  const xref = Buffer.byteLength(source);
  source += `xref\n0 ${objects.length + 1}\n0000000000 65535 f \n`;
  source += offsets.slice(1).map((offset) => `${String(offset).padStart(10, '0')} 00000 n \n`).join('');
  source += `trailer\n<< /Size ${objects.length + 1} /Root 1 0 R >>\nstartxref\n${xref}\n%%EOF\n`;
  return Buffer.from(source, 'ascii');
}
