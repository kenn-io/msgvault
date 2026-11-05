import { setKitTheme, stubOperationStatus } from './kit-ui';
import { expect, test, type Page } from '@playwright/test';
import { exploreHistoryState } from './explore-state';
import { shortPagesPDF } from './pdf-fixture';

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
  await page.route('**/api/v1/saved-views', (route) => route.fulfill({ json: { saved_views: [] } }));
  await stubOperationStatus(page);
  await page.route('**/api/v1/sources/status', (route) => route.fulfill({ json: { sources: [] } }));
  await page.route('**/api/v1/files/search', (route) => route.fulfill({ json: {
    files: [row], total_count: 1, cache_revision: 'cache-files', search_provenance: {}
  } }));
  await page.route('**/api/v1/files/7', (route) => route.fulfill({ json: {
    id: 7, message_id: 11, conversation_id: 21, entry_key: row.entry_key, filename: row.filename,
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

for (const dismissal of ['close button', 'Escape', 'Back']) {
  test(`closing after stepping restores the shown row via ${dismissal}`, async ({ page, baseURL }) => {
    const first = file('01-guide.pdf', 'application/pdf', 'pdf');
    const second = { ...file('06-budget.xlsx', 'application/vnd.openxmlformats-officedocument.spreadsheetml.sheet', 'document'), id: 8, key: 'file:8' };
    await prepare(page, baseURL, first, shortPagesPDF(1));
    await page.route('**/api/v1/files/search', (route) => route.fulfill({ json: {
      files: [first, second], total_count: 2, cache_revision: 'cache-files', search_provenance: {}
    } }));
    await page.route('**/api/v1/files/8', (route) => route.fulfill({ json: {
      ...second, content_state: 'local_content', content_available: true
    } }));
    await page.reload();
    const grid = page.getByRole('grid', { name: 'Files results' });
    await page.getByRole('gridcell', { name: first.filename, exact: true }).click();
    await page.getByRole('button', { name: 'Next file', exact: true }).click();
    await expect(page.getByRole('dialog', { name: `View ${second.filename}` })).toBeVisible();
    if (dismissal === 'Back') await page.goBack();
    else if (dismissal === 'Escape') await page.keyboard.press('Escape');
    else await page.getByRole('button', { name: 'Close file viewer', exact: true }).click();
    await expect(page.getByRole('dialog')).toHaveCount(0);
    await expect(grid).toHaveAttribute('aria-busy', 'false');
    await expect(grid).toBeFocused();
    await expect(grid).toHaveAttribute('aria-activedescendant', 'file-row-8');
    await expect(grid).toHaveCSS('box-shadow', 'none');
    await page.keyboard.press('Enter');
    await expect(page.getByRole('dialog', { name: `View ${second.filename}` })).toBeVisible();
  });
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
  await page.getByRole('button', { name: 'Filter', exact: true }).click();
  await page.getByRole('searchbox', { name: 'Filter filename' }).fill('pixel');
  await page.getByRole('button', { name: 'Filter', exact: true }).click();
  await grid.focus();
  await expect(grid).toHaveAttribute('aria-activedescendant', 'file-row-7');
  await page.keyboard.press('Home');
  await expect.poll(async () => {
    const state = await exploreHistoryState(page);
    return { activeRow: state.activeRow, fileFilenameQuery: state.fileFilenameQuery };
  }).toEqual({ activeRow: 'file:7', fileFilenameQuery: 'pixel' });
  await expect(grid).toHaveAttribute('aria-busy', 'false');
  await expect(grid).toHaveAttribute('aria-activedescendant', 'file-row-7');
  const filteredURL = page.url();
  await page.keyboard.press('Enter');
  await expect(page.getByRole('img', { name: 'Preview pixel.png' })).toBeVisible();
  expect(contentCookie()).toContain('msgvault_session=browser-fixture-session');
  await page.keyboard.press('f');
  await expect(page.getByRole('button', { name: 'Filter', exact: true })).toHaveAttribute('aria-expanded', 'false');
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

async function stubContainingConversation(page: Page) {
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
}

test('opening a containing conversation and one Back restores the exact Files URL and active row', async ({ page, baseURL }) => {
  await stubContainingConversation(page);
  await prepare(page, baseURL, file('fixture.pdf', 'application/pdf', 'pdf'), shortPagesPDF(1, { height: 792, text: 'Back fixture' }));
  const grid = page.getByRole('grid', { name: 'Files results' });
  await grid.focus();
  await page.keyboard.press('Enter');
  const filesURL = page.url();

  await page.getByRole('button', { name: 'More file actions' }).click();
  await page.getByRole('menuitem', { name: 'Open conversation' }).click();
  const reading = page.getByRole('complementary', { name: 'Reading pane: Containing item' });
  await expect(reading).toBeVisible();
  await expect(reading.getByRole('region', { name: 'Conversation thread' })).toBeVisible();
  await page.goBack();

  await expect(page).toHaveURL(filesURL);
  await expect(page.getByRole('dialog', { name: 'View fixture.pdf' })).toBeVisible();
  await expect(grid).toHaveAttribute('aria-activedescendant', 'file-row-7');
});

test('a phone opens a message from Files and returns to results', async ({ page, baseURL }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await stubContainingConversation(page);
  await prepare(page, baseURL, file('fixture.pdf', 'application/pdf', 'pdf'), shortPagesPDF(1, { height: 792, text: 'Phone fixture' }));
  const files = page.getByRole('grid', { name: 'Files results' });
  await files.getByText('fixture.pdf').click();
  const viewer = page.getByRole('dialog', { name: 'View fixture.pdf' });
  await expect(viewer).toBeVisible();
  await viewer.getByRole('button', { name: 'Open message' }).click();
  const reader = page.getByRole('complementary', { name: 'Reading pane: Containing item' });
  const messages = page.getByRole('grid', { name: 'Messages results' });
  await expect(reader).toBeVisible();
  await expect(reader.getByText('Archived body', { exact: true })).toBeVisible();
  await expect(messages).toBeHidden();
  const split = (await page.locator('.results-split').boundingBox())!;
  expect((await reader.boundingBox())!.width).toBeCloseTo(split.width, 0);
  await reader.getByRole('button', { name: 'Back to results' }).click();
  await expect(messages).toBeVisible();
  await expect(messages.getByText('Containing item', { exact: true })).toBeVisible();
});

test('PDF reading preserves selection, page tracking, shortcuts, focus, and Back navigation', async ({ page, baseURL }) => {
  await prepare(page, baseURL, file('fixture.pdf', 'application/pdf', 'pdf'), shortPagesPDF(3, { height: 792, text: 'Fixture preview text' }));
  const grid = page.getByRole('grid', { name: 'Files results' });
  await grid.focus();
  await expect(grid).toHaveAttribute('aria-activedescendant', 'file-row-7');
  await expect.poll(async () => (await exploreHistoryState(page)).activeRow).toBe('file:7');
  const listURL = page.url();

  await page.keyboard.press('Enter');
  await expect(page.getByRole('dialog', { name: 'View fixture.pdf' })).toBeVisible();
  const preview = page.getByRole('region', { name: 'PDF preview fixture.pdf' });
  await expect(preview.locator('.textLayer')).toHaveCount(3);
  await preview.locator('.textLayer').first().evaluate((layer) => {
    const range = document.createRange(); range.selectNodeContents(layer); const selection = window.getSelection()!; selection.removeAllRanges(); selection.addRange(range);
  });
  expect(await page.evaluate(() => window.getSelection()?.toString())).toContain('Fixture preview text');
  await expect(page.getByText('Page 1 of 3', { exact: true })).toBeVisible();
  await page.locator('.file-viewer .preview').evaluate((element) => { element.scrollTop = (element.querySelectorAll<HTMLElement>('.pdf-page')[1]!).offsetTop - (element as HTMLElement).offsetTop; });
  await expect(page.getByText('Page 2 of 3', { exact: true })).toBeVisible();
  await expect(page.locator('iframe, embed, object')).toHaveCount(0);
  await page.keyboard.press('f');
  await expect(page.getByRole('button', { name: 'Filter', exact: true })).toHaveAttribute('aria-expanded', 'false');
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

  await page.keyboard.press('Enter');
  await expect(page.getByRole('dialog', { name: 'View fixture.pdf' })).toBeVisible();
  await expect.poll(() => {
    const encoded = new URL(page.url()).searchParams.get('explore');
    return encoded ? (JSON.parse(encoded) as { selectedRow?: string | null }).selectedRow : undefined;
  }).toBe('file:7');

  await page.goBack();

  await expect(page).toHaveURL(listURL);
  await expect(page.getByRole('dialog', { name: 'View fixture.pdf' })).toHaveCount(0);
});

for (const deviceScaleFactor of [1, 2]) {
  test.describe(`PDF reading at device scale ${deviceScaleFactor}`, () => {
    test.use({ deviceScaleFactor });
    test('Letter PDF canvas and zoom retain enough resolution', async ({ page, baseURL }) => {
      const pdf = shortPagesPDF(3, { height: 792, text: 'Fixture preview text' });
      await prepare(page, baseURL, file('fixture.pdf', 'application/pdf', 'pdf'), pdf);

      const grid = page.getByRole('grid', { name: 'Files results' });
      await grid.focus();
      await page.keyboard.press('Enter');
      const preview = page.getByRole('region', { name: 'PDF preview fixture.pdf' });
      await expect(preview.getByLabel('PDF page 1', { exact: true })).toBeVisible();
      await expect(preview.locator('.textLayer').first()).toContainText('Fixture preview text');
      const canvas = preview.locator('canvas').first();
      const fitWidth = (await canvas.boundingBox())!.width;
      await page.getByRole('button', { name: 'Zoom in', exact: true }).click();
      await expect.poll(async () => (await canvas.boundingBox())!.width).toBeGreaterThan(fitWidth);
      await page.getByRole('button', { name: 'Zoom in', exact: true }).click();
      expect(await canvas.evaluate((element) => (element as HTMLCanvasElement).width)).toBeGreaterThanOrEqual((await canvas.boundingBox())!.width);
      await page.getByRole('button', { name: 'Fit width', exact: true }).click();
      await expect.poll(async () => (await canvas.boundingBox())!.width).toBe(fitWidth);
    });
  });
}

test('PDF text overlay honors page user units at fit, zoom, and resize', async ({ page, baseURL }) => {
  await prepare(page, baseURL, file('fixture.pdf', 'application/pdf', 'pdf'), shortPagesPDF(1, { height: 792, userUnit: 2 }));
  await page.getByRole('gridcell', { name: 'fixture.pdf', exact: true }).click();
  const layer = page.locator('.textLayer');
  await expect(layer).toContainText('Fixture page');
  for (const state of ['fit', 'zoom', 'resize']) {
    if (state === 'zoom') await page.getByRole('button', { name: 'Zoom in', exact: true }).click();
    if (state === 'resize') await page.setViewportSize({ width: 800, height: 700 });
    await expect.poll(() => layer.evaluate(element => {
      const canvas = element.closest('.pdf-page')!.querySelector('canvas')!;
      const span = element.querySelector('span')!;
      // The fixture uses 18-point text on a 612-point-wide page.
      return Math.abs(parseFloat(getComputedStyle(span).fontSize) - canvas.getBoundingClientRect().width * 18 / 612);
    })).toBeLessThan(0.1);
  }
});

for (const theme of ['light', 'dark'] as const) {
  test(`PDF import preserves the chosen ${theme} color scheme against the OS preference`, async ({ page, baseURL }) => {
    await page.emulateMedia({ colorScheme: theme === 'light' ? 'dark' : 'light' });
    await prepare(page, baseURL, file('fixture.pdf', 'application/pdf', 'pdf'), shortPagesPDF(1));
    await setKitTheme(page, theme);
    await expect(page.locator('html')).toHaveCSS('color-scheme', theme);
    await page.getByRole('gridcell', { name: 'fixture.pdf', exact: true }).click();
    await expect(page.locator('.textLayer')).toContainText('Fixture page');
    await expect(page.locator('html')).toHaveCSS('color-scheme', theme);
    await page.keyboard.press('Escape');
    await expect(page.locator('html')).toHaveCSS('color-scheme', theme);
  });
}

test('Files exposes visual search after setup supplies a consented active generation', async ({ page, baseURL }) => {
  let generation = { state: 'building', consented: true };
  await page.route('**/api/v1/multimodal/status', route => route.fulfill({ json: { current: 0, generation } }));
  await prepare(page, baseURL, file('example.txt', 'text/plain', 'text'), Buffer.from('Synthetic preview'));
  await expect(page.getByRole('grid', { name: 'Files results' })).toBeVisible();
  await expect(page.getByRole('switch', { name: 'Visual search' })).toHaveCount(0);
  await page.getByRole('button', { name: 'Sources', exact: true }).click();
  generation = { state: 'active', consented: true };
  await page.getByRole('button', { name: 'Files', exact: true }).click();
  await expect(page.getByRole('switch', { name: 'Visual search' })).toBeVisible();
});
