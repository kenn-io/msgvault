import { expect, test } from '@playwright/test';
import {
  CHAT_CONVERSATION_COUNT,
  RAW_CHAT_MESSAGE_COUNT,
  installMixedArchive
} from './fixtures/mixed-archive';

test('100k raw chat fragments reach Everything only as logical conversation rows', async ({ page }) => {
  const fixture = await installMixedArchive(page);
  expect(fixture.rawChatMessageCount).toBe(RAW_CHAT_MESSAGE_COUNT);
  expect(fixture.chatConversationCount).toBe(CHAT_CONVERSATION_COUNT);
  expect(fixture.logicalRows.filter((row) => row.kind === 'conversation')).toHaveLength(CHAT_CONVERSATION_COUNT);
  for (const kind of ['email', 'event', 'meeting', 'item']) {
    expect(fixture.logicalRows.some((row) => row.kind === kind)).toBe(true);
  }
  expect(fixture.firstPage.rows).toHaveLength(50);
  expect(fixture.firstPage.total_count).toBe(fixture.logicalRows.length);

  await page.goto(`/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
  const grid = page.getByRole('grid', { name: 'Everything results' });
  await expect(grid).toBeVisible();
  await expect(grid.locator('[data-row-key]').first()).toBeVisible();
  await expect(grid.getByText('Preview text for message 1 about various topics', { exact: true })).toHaveCount(0);
  const renderedLogicalChats = await grid.locator('[data-row-key*="conversation:"]').count();
  expect(renderedLogicalChats).toBeGreaterThan(0);
  expect(renderedLogicalChats).toBeLessThanOrEqual(50);
  await expect(page.getByText(`${fixture.logicalRows.length} items`)).toBeVisible();
});
