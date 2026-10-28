import { expect, test } from '@playwright/test';
import { selectWorkspace } from './kit-ui';

import { mockArchiveLayout, row } from './archive-layout';

test.beforeEach(async ({ page }) => {
  await mockArchiveLayout(page);
  await page.route('**/api/v1/relationships', (route) => route.fulfill({ json: {
    rows: [], total_count: 0, cache_revision: 'spacing', identity_revision: 1
  } }));
  await page.goto('/');
  await expect(page.getByRole('heading', { name: 'Relationships', exact: true })).toBeVisible();
});

test('landing header divider meets the content boundary', async ({ page }) => {
  const gap = await page.locator('.page-header').evaluate((element) => {
    const band = getComputedStyle(element, '::before');
    const divider = element.getBoundingClientRect().bottom - Number.parseFloat(band.bottom);
    return document.querySelector('.hub-body')!.getBoundingClientRect().top - divider;
  });
  expect(gap).toBe(0);
  await expect(page.getByRole('grid', { name: 'Relationship results' })).toBeVisible();
});

test('reader, selection, and dialog share inset edges', async ({ page }) => {
  await selectWorkspace(page, 'Everything');
  await page.getByText(row.title, { exact: true }).click();
  const reader = page.getByRole('complementary', { name: `Reading pane: ${row.title}` });
  await expect(reader).toBeVisible();
  const inset = await reader.locator('.pane-header').evaluate((element) => {
    const box = element.getBoundingClientRect();
    const heading = element.querySelector('.pane-heading')!.getBoundingClientRect();
    const style = getComputedStyle(element);
    return { left: heading.left - box.left, top: style.paddingTop, gap: style.columnGap };
  });
  expect(inset).toEqual({ left: 12, top: '8px', gap: '16px' });
  await page.getByRole('button', { name: 'Close reading pane' }).click();
  const grid = page.getByRole('grid', { name: 'Everything results' });
  await grid.focus();
  await page.keyboard.press('Shift+A');
  const bar = page.locator('.selection-bar');
  await expect(bar).toBeVisible();
  const selectionInset = await bar.evaluate((element) => {
    const box = element.getBoundingClientRect();
    const count = element.querySelector('.selection-count')!.getBoundingClientRect();
    const style = getComputedStyle(element);
    return { left: count.left - box.left, top: style.paddingTop, gap: style.columnGap };
  });
  expect(selectionInset).toEqual({ left: 12, top: '8px', gap: '8px' });
  await page.getByRole('button', { name: 'Clear selection', exact: true }).click();
  await page.getByRole('button', { name: 'Save view…' }).click();
  const dialog = page.getByRole('dialog', { name: 'Save view' });
  await expect(dialog).toBeVisible();
  const edges = await dialog.evaluate((element) => {
    const headerElement = element.querySelector<HTMLElement>('.kit-modal-header')!;
    const header = headerElement.getBoundingClientRect();
    const body = element.querySelector('.kit-modal-body')!.getBoundingClientRect();
    const heading = element.querySelector('.kit-modal-title')!.getBoundingClientRect();
    const input = element.querySelector('.kit-text-input')!.getBoundingClientRect();
    return { heading: heading.left - header.left - headerElement.clientLeft, field: input.left - body.left };
  });
  expect(edges).toEqual({ heading: 12, field: 12 });
  await dialog.getByRole('button', { name: 'Close', exact: true }).click();
});

test('narrow page keeps the header band inside the viewport', async ({ page }) => {
  await page.setViewportSize({ width: 600, height: 720 });
  const band = await page.locator('.page-header').evaluate((element) => {
    const box = element.getBoundingClientRect();
    const style = getComputedStyle(element, '::before');
    return { left: box.left + Number.parseFloat(style.left), right: box.right - Number.parseFloat(style.right) };
  });
  expect(band.left).toBe(0);
  expect(band.right).toBe(600);
});

test('loaded message sender, body, and attachment edges align with the reader', async ({ page }) => {
  await page.route('**/api/v1/explore', (route) => route.fulfill({ json: {
    rows: [{ ...row, conversation_id: 11, anchor_message_id: 1 }], total_count: 1, cache_revision: 'spacing', search_provenance: {}
  } }));
  await page.route('**/api/v1/conversations/11**', (route) => route.fulfill({ json: {
    id: 11, anchor_id: 1, has_before: false, has_after: false, total: 1,
    messages: [{ id: 1, conversation_id: 11, subject: row.title, message_type: 'meeting_transcript',
      from: 'Example Person', to: [], sent_at: row.occurred_at, snippet: row.preview,
      body: 'A plain text archive message.', body_html: '',
      attachments: [{ id: 1, filename: 'Meeting-notes.txt', stored: false }] }]
  } }));
  await selectWorkspace(page, 'Everything');
  await page.getByText(row.title, { exact: true }).click();
  const reader = page.getByRole('complementary', { name: `Reading pane: ${row.title}` });
  await expect(reader.getByText('A plain text archive message.')).toBeVisible();
  await expect(reader.getByText('Meeting-notes.txt (not downloaded)')).toBeVisible();
  const insets = await reader.evaluate((element) => [
    '.pane-heading', '.card-header', '.card-body', '.attachments'
  ].map((selector) => {
    const section = element.querySelector(selector)!;
    if (selector === '.pane-heading') return section.getBoundingClientRect().left - element.getBoundingClientRect().left;
    return section.firstElementChild!.getBoundingClientRect().left - section.getBoundingClientRect().left;
  }));
  expect(insets).toEqual([12, 12, 12, 12]);
});

test('adjacent removable chips have equal heights and even visible insets', async ({ page }) => {
  for (const workspace of ['Everything', 'Files']) {
    await selectWorkspace(page, workspace);
    await page.getByRole('searchbox', { name: 'Search everything' }).fill('archive');
    if (workspace === 'Everything') {
      await page.getByRole('combobox', { name: 'Group by', exact: true }).click();
      await page.getByRole('option', { name: 'Source', exact: true }).click();
    }
    const chips = page.locator('.context-chips .chip');
    await expect(chips).toHaveCount(2);
    const geometry = await chips.evaluateAll((elements) => elements.map((element) => {
      const range = document.createRange();
      range.selectNodeContents(element.firstChild!);
      const box = element.getBoundingClientRect();
      const icon = element.querySelector('svg')!.getBoundingClientRect();
      const border = Number.parseFloat(getComputedStyle(element).borderLeftWidth);
      return { height: box.height, left: range.getBoundingClientRect().left - box.left - border,
        right: box.right - border - icon.right };
    }));
    expect(geometry).toEqual([{ height: 26, left: 8, right: 8 }, { height: 26, left: 8, right: 8 }]);
  }
});

test('linked message uses the page title text scale', async ({ page }) => {
  await page.route('**/api/v1/messages/1', (route) => route.fulfill({ json: {
    id: 1, conversation_id: 11, subject: row.title, message_type: 'email'
  } }));
  await page.route('**/api/v1/conversations/11**', (route) => route.fulfill({ json: {
    id: 11, anchor_id: 1, messages: [], has_before: false, has_after: false, total: 0
  } }));
  await page.goto('/messages/1');
  await expect(page.getByRole('heading', { level: 1, name: row.title })).toHaveCSS('font-size', '18px');
});

test('overflowing settings keep controls clear of the themed scrollbar', async ({ page }) => {
  await page.setViewportSize({ width: 1024, height: 600 });
  await page.route('**/api/v1/settings', (route) => route.fulfill({
    headers: { ETag: '"scrolling-settings"' },
    json: {
      groups: [{ id: 'browser', label: 'Appearance' }],
      settings: Array.from({ length: 30 }, (_, index) => ({
        key: `web.example_${index}`, group: 'browser', label: `Example setting ${index}`,
        kind: 'string', value: { string: 'Example value' }, restart_required: false
      }))
    }
  }));
  await selectWorkspace(page, 'Settings');
  const scroll = page.locator('.kit-settings__scroll');
  const control = page.getByRole('textbox', { name: 'Example setting 29', exact: true });
  await control.scrollIntoViewIfNeeded();
  const geometry = await scroll.evaluate((element) => {
    const box = element.getBoundingClientRect();
    const control = element.querySelectorAll('.kit-text-input');
    const card = control[control.length - 1].closest('.kit-card')!;
    return { overflow: element.scrollHeight > element.clientHeight,
      clearance: box.left + element.clientLeft + element.clientWidth - control[control.length - 1].getBoundingClientRect().right,
      inset: card.getBoundingClientRect().right - Number.parseFloat(getComputedStyle(card).borderRightWidth) - control[control.length - 1].getBoundingClientRect().right,
      scrollbar: getComputedStyle(element, '::-webkit-scrollbar').width };
  });
  expect(geometry.overflow).toBe(true);
  expect(geometry.clearance).toBeGreaterThanOrEqual(12);
  expect(geometry.inset).toBe(12);
  expect(geometry.scrollbar).toBe('8px');
  const gap = await page.locator('.page-header').evaluate((element) => {
    const band = getComputedStyle(element, '::before');
    return document.querySelector('.kit-settings')!.getBoundingClientRect().top - element.getBoundingClientRect().bottom + Number.parseFloat(band.bottom);
  });
  expect(gap).toBe(0);
});
