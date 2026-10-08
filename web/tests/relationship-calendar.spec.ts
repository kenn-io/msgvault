import { expect, test, type Page } from '@playwright/test';

// Touch contexts let the scroll-dismissal flow tap a day for real, matching
// the component's touch-specific tooltip behavior.
test.use({ hasTouch: true });

const person = {
  id: 1, display_label: 'Alice Example', display_name: 'Alice Example',
  partial_label: false, identifiers: [], activity_count: 4, file_count: 0,
  source_counts: [], first_at: '2018-01-02T00:00:00Z',
  last_at: '2026-12-31T00:00:00Z', cache_revision: 'cache-calendar'
};

const baseDay = {
  sent: 0, received: 0, email: 0, chat: 0, meetings: 0, total: 0,
  modality_mask: 0, level: 'NONE'
};

async function openCalendar(page: Page, firstAt: string | Promise<string> = person.first_at) {
  await page.route('**/api/session', (route) => route.fulfill({
    json: { auth_mode: 'loopback', https: false, plain_http_warning: false }
  }));
  await page.route('**/api/v1/relationships', (route) => route.fulfill({ json: {
    rows: [{ canonical_id: 1, display_label: person.display_label, member_ids: [1], score: 1,
      last_at: person.last_at, signals: { last_interaction_at: person.last_at, meeting_count: 0,
        meetings_together: 0, modalities: 2, received_from_them: 1, sent_count: 3, sent_to_them: 1 } }],
    total_count: 1, cache_revision: 'cache-calendar', identity_revision: 1
  } }));
  await page.route('**/api/v1/participants/1', async (route) => route.fulfill({
    json: { ...person, first_at: await firstAt }
  }));
  await page.route('**/api/v1/relationships/1/timeline', (route) => route.fulfill({ json: {
    canonical_id: 1, identity_revision: 1, cache_revision: 'cache-calendar', rows: [], total_count: 0
  } }));
  await page.route('**/api/v1/relationships/1/calendar', (route) => route.fulfill({ json: {
    participant_id: 1, canonical_id: 1, year: route.request().postDataJSON().year,
    timezone: route.request().postDataJSON().timezone,
    days: [
      { ...baseDay, date: '2018-11-04', email: 1, total: 1, level: 'FIRST_QUARTILE' },
      { ...baseDay, date: '2026-01-01', email: 1, chat: 2, total: 3, level: 'FOURTH_QUARTILE' },
      { ...baseDay, date: '2026-03-08', email: 1, total: 1, level: 'FIRST_QUARTILE' },
      { ...baseDay, date: '2026-03-09' },
      { ...baseDay, date: '2026-12-31', email: 1, total: 1, level: 'FIRST_QUARTILE' }
    ],
    current: { temperature: 62, rank: 1, population: 1, raw_score: 3,
      signals: { sent_signal: 1, received_volume: 1, meeting_signal: 0, modalities: 2 } },
    annual: [], peak_temperature: 87, peak_year: 2018, scoring_timezone: 'UTC',
    score_version: 1, effective_date: '2026-12-31', cache_revision: 'cache-calendar', identity_revision: 1
  } }));
  await page.goto('/');
  await page.getByRole('grid', { name: 'Relationship results' }).getByText(person.display_label).click();
  await expect(page.getByRole('region', { name: 'Relationship activity calendar' })).toBeVisible();
}

test('calendar fills its card at medium and narrow widths without clipping edge tooltips', async ({ page }) => {
  await openCalendar(page);
  const calendar = page.getByRole('region', { name: 'Relationship activity calendar' });

  for (const width of [760, 650, 300]) {
    await calendar.evaluate((element, value) => {
      const card = element.parentElement!;
      card.style.width = `${value}px`;
      // Add the card's padding so the calendar itself has the requested width.
      card.style.width = `${value + value - element.getBoundingClientRect().width}px`;
    }, width);
    expect((await calendar.boundingBox())!.width).toBe(width);
    const graphs = calendar.locator('.calendar-graphs');
    const scroll = await graphs.evaluate((element) => ({
      horizontal: element.scrollWidth - element.clientWidth,
      vertical: getComputedStyle(element).overflowY
    }));
    expect(scroll.vertical).toBe('hidden');
    if (width === 300) expect(scroll.horizontal).toBeGreaterThan(0);
    // Cell tracks size with fractional calc() lengths; per-track layout-unit
    // snapping can leave the grid under a pixel wider than the scroll
    // container, so allow one integer pixel of no-visible-overflow drift.
    else expect(scroll.horizontal).toBeLessThanOrEqual(1);
    const panels = calendar.locator(width > 700 ? '.calendar-panel.full' : '.calendar-panel.half');
    await expect(panels.first()).toBeVisible();
    for (const panel of [panels.first(), panels.last()]) {
      const panelBox = await panel.boundingBox();
      const gridBox = await panel.locator('.weeks').boundingBox();
      expect(panelBox).not.toBeNull();
      expect(gridBox).not.toBeNull();
      expect(Math.abs(gridBox!.x + gridBox!.width - panelBox!.x - panelBox!.width)).toBeLessThanOrEqual(2);
      expect(gridBox!.x).toBeGreaterThanOrEqual(panelBox!.x);
      const firstDay = await panel.locator('.day').first().boundingBox();
      expect(firstDay!.width).toBeGreaterThanOrEqual(8);
    }

    for (const name of ['3 messages on Jan 1, 2026', '1 message on Dec 31, 2026']) {
      const cell = calendar.getByRole('button', { name }).filter({ visible: true });
      await cell.hover();
      const tooltip = calendar.getByRole('tooltip');
      await expect(tooltip).toHaveText(name);
      await expect(tooltip).toBeVisible();
      const rootBox = await calendar.boundingBox();
      const cellBox = await cell.boundingBox();
      const tipBox = await tooltip.boundingBox();
      expect(rootBox && cellBox && tipBox).toBeTruthy();
      expect(tipBox!.x).toBeGreaterThanOrEqual(rootBox!.x - 1);
      expect(tipBox!.x + tipBox!.width).toBeLessThanOrEqual(rootBox!.x + rootBox!.width + 1);
      expect(tipBox!.y + tipBox!.height).toBeLessThanOrEqual(cellBox!.y - 3);
    }
  }
});

test.describe('day selection when midnight is skipped', () => {
  test.use({ timezoneId: 'America/Sao_Paulo' });

  test('excludes the following day from the timeline request', async ({ page }) => {
    await page.clock.setFixedTime(new Date('2018-11-05T12:00:00Z'));
    await openCalendar(page);
    const calendar = page.getByRole('region', { name: 'Relationship activity calendar' });
    const timelineRequest = page.waitForRequest((request) =>
      request.url().endsWith('/api/v1/relationships/1/timeline') && request.method() === 'POST'
    );
    await calendar.getByRole('button', { name: '1 message on Nov 4, 2018' }).filter({ visible: true }).click();
    expect((await timelineRequest).postDataJSON()).toMatchObject({
      timezone: 'America/Sao_Paulo',
      filters: [
        { dimension: 'after', values: ['2018-11-04T03:00:00.000Z'] },
        { dimension: 'before', values: ['2018-11-05T02:00:00.000Z'] }
      ]
    });
  });
});

test('Escape dismisses the current day but hovering another day reopens the tooltip', async ({ page }) => {
  await openCalendar(page);
  const calendar = page.getByRole('region', { name: 'Relationship activity calendar' });
  const first = calendar.getByRole('button', { name: '3 messages on Jan 1, 2026' }).filter({ visible: true });
  const last = calendar.getByRole('button', { name: '1 message on Dec 31, 2026' }).filter({ visible: true });
  const tooltip = calendar.getByRole('tooltip');
  await first.hover();
  await expect(tooltip).toHaveText('3 messages on Jan 1, 2026');
  await page.keyboard.press('Escape');
  await expect(tooltip).toHaveCount(0);
  await last.hover();
  await expect(tooltip).toHaveText('1 message on Dec 31, 2026');
  await calendar.locator('.month-row:visible').first().hover();
  await expect(tooltip).toHaveCount(0);
});

test('year controls keep the year in place while person details load', async ({ page }) => {
  let resolvePerson!: (firstAt: string) => void;
  const firstAt = new Promise<string>((resolve) => { resolvePerson = resolve; });
  await openCalendar(page, firstAt);
  const calendar = page.getByRole('region', { name: 'Relationship activity calendar' });
  const year = calendar.getByText('2026', { exact: true });
  const before = await year.boundingBox();
  await expect(calendar.getByRole('button', { name: 'Previous relationship year' })).toBeDisabled();
  await expect(calendar.getByRole('button', { name: 'Next relationship year' })).toBeDisabled();
  resolvePerson(person.first_at);
  await expect(calendar.getByRole('button', { name: 'Previous relationship year' })).toBeEnabled();
  expect((await year.boundingBox())!.x).toBe(before!.x);
});

test('calendar remains accessible in a narrow viewport with edge tooltips on screen', async ({ page }) => {
  await openCalendar(page);
  await page.setViewportSize({ width: 320, height: 720 });
  const calendar = page.getByRole('region', { name: 'Relationship activity calendar' });
  const rootBox = await calendar.boundingBox();
  expect(rootBox).not.toBeNull();
  expect(rootBox!.width).toBeLessThanOrEqual(320);

  const graphs = calendar.locator('.calendar-graphs');
  expect(await graphs.evaluate((element) => element.scrollWidth - element.clientWidth)).toBeGreaterThan(0);
  for (const name of ['3 messages on Jan 1, 2026', '1 message on Dec 31, 2026']) {
    const cell = calendar.getByRole('button', { name }).filter({ visible: true });
    await cell.hover();
    const tooltip = calendar.getByRole('tooltip');
    await expect(tooltip).toHaveText(name);
    const tipBox = await tooltip.boundingBox();
    const cellBox = await cell.boundingBox();
    expect(tipBox && cellBox).toBeTruthy();
    expect(tipBox!.x).toBeGreaterThanOrEqual(-1);
    expect(tipBox!.x + tipBox!.width).toBeLessThanOrEqual(321);
    expect(tipBox!.y + tipBox!.height).toBeLessThanOrEqual(cellBox!.y - 3);
    if (name.startsWith('3 messages')) {
      // Exercise the touch scroll-dismissal path end to end: a touch tap
      // leaves the tooltip pinned (touch pointerleave is ignored by design),
      // and the next strip scroll must dismiss it.
      await cell.tap();
      await expect(tooltip).toHaveText(name);
      await graphs.evaluate((element) => {
        element.scrollLeft = element.scrollWidth - element.clientWidth;
      });
      await expect(tooltip).toHaveCount(0);
    }
  }
});

test('disabled year control looks disabled while the available direction remains active', async ({ page }) => {
  await openCalendar(page);
  const previous = page.getByRole('button', { name: 'Previous relationship year' });
  const next = page.getByRole('button', { name: 'Next relationship year' });
  await expect(previous).toBeEnabled();
  await expect(next).toBeDisabled();
  const previousOpacity = Number(await previous.evaluate((element) => getComputedStyle(element).opacity));
  const nextOpacity = Number(await next.evaluate((element) => getComputedStyle(element).opacity));
  expect(nextOpacity).toBeLessThan(previousOpacity);
  expect(await next.evaluate((element) => getComputedStyle(element).cursor)).toBe('default');
});

test('a loaded single-year calendar shows the year without navigation', async ({ page }) => {
  await openCalendar(page, '2026-01-01T00:00:00Z');
  const calendar = page.getByRole('region', { name: 'Relationship activity calendar' });
  await expect(calendar.getByRole('button', { name: '3 messages on Jan 1, 2026' })).toBeVisible();
  await expect(calendar.getByText('2026', { exact: true })).toBeVisible();
  await expect(calendar.getByRole('button', { name: 'Previous relationship year' })).toHaveCount(0);
  await expect(calendar.getByRole('button', { name: 'Next relationship year' })).toHaveCount(0);
});

test.describe('day selection', () => {
  test.use({ timezoneId: 'America/New_York' });

  test('filters the timeline by the local day and clears by keyboard or button', async ({ page }) => {
    await openCalendar(page);
    await page.route('**/api/v1/participants/1/files/search', (route) => route.fulfill({ json: {
      files: [], total_count: 0, cache_revision: 'cache-calendar', search_provenance: {}
    } }));
    await page.getByRole('radio', { name: 'Files 0' }).click();
    const requests: Array<{ filters?: unknown[]; timezone: string }> = [];
    await page.route('**/api/v1/relationships/1/timeline', async (route) => {
      const body = route.request().postDataJSON();
      requests.push(body);
      const after = body.filters?.find((filter: { dimension: string }) => filter.dimension === 'after');
      const title = after ? 'Spring planning' : 'All relationship activity';
      const rows = after?.values[0] === '2026-03-09T04:00:00.000Z' ? [] : [{
        key: 'message:10', kind: 'email', occurred_at: '2026-03-08T14:00:00Z',
        title, preview: 'A synthetic message', source_id: 1, message_count: 1, has_attachments: false
      }];
      await route.fulfill({ json: {
        canonical_id: 1, identity_revision: 1, cache_revision: 'cache-calendar', rows, total_count: rows.length
      } });
    });
    const calendar = page.getByRole('region', { name: 'Relationship activity calendar' });
    const day = calendar.getByRole('button', { name: '1 message on Mar 8, 2026' }).filter({ visible: true });
    const timeline = page.getByRole('grid', { name: 'Relationship activity' });
    await day.click();
    await expect(timeline.getByText('Spring planning')).toBeVisible();
    await expect(page.getByRole('radio', { name: 'Messages', exact: true })).toBeChecked();
    await expect(day).toHaveAttribute('aria-pressed', 'true');
    const historyLength = await page.evaluate(() => history.length);
    expect(requests.at(-1)).toMatchObject({
      timezone: 'America/New_York',
      filters: [
        { dimension: 'after', values: ['2026-03-08T05:00:00.000Z'] },
        { dimension: 'before', values: ['2026-03-09T04:00:00.000Z'] }
      ]
    });

    await day.focus();
    await day.press('Enter');
    await expect(timeline.getByText('All relationship activity')).toBeVisible();
    await expect(day).toHaveAttribute('aria-pressed', 'false');
    await day.press('Space');
    await expect(timeline.getByText('Spring planning')).toBeVisible();

    const emptyDay = calendar.getByRole('button', { name: 'No messages on Mar 9, 2026' }).filter({ visible: true });
    await emptyDay.tap();
    await expect(emptyDay).toHaveAttribute('aria-pressed', 'true');
    await expect(timeline.getByText('No activity on this day')).toBeVisible();
    await expect(timeline.getByText('Spring planning')).toHaveCount(0);
    await calendar.getByRole('button', { name: 'Clear date' }).click();
    await expect(timeline.getByText('All relationship activity')).toBeVisible();
    expect(requests.at(-1)?.filters ?? []).toEqual([]);
    await expect(emptyDay).toHaveAttribute('aria-pressed', 'false');
    expect(await page.evaluate(() => history.length)).toBe(historyLength);

    await day.click();
    await expect(timeline.getByText('Spring planning')).toBeVisible();
    await page.getByRole('radio', { name: 'Files 0' }).click();
    await expect(day).toHaveAttribute('aria-pressed', 'false');
    await expect(calendar.getByRole('button', { name: 'Clear date' })).toHaveCount(0);
    await page.getByRole('radio', { name: 'Messages', exact: true }).click();
    await expect(timeline.getByText('All relationship activity')).toBeVisible();

    await day.click();
    await expect(day).toHaveAttribute('aria-pressed', 'true');
    await calendar.getByRole('button', { name: 'Previous relationship year' }).click();
    await expect(calendar.getByText('2025', { exact: true })).toBeVisible();
    await expect(calendar.getByRole('button', { name: 'Clear date' })).toHaveCount(0);
    await expect(timeline.getByText('All relationship activity')).toBeVisible();
  });
});
