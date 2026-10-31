import { stubOperationStatus } from './kit-ui';
import { setKitTheme } from './kit-ui';
import { expect, test, type Page } from '@playwright/test';
import { meetingActions, meetingMetrics } from '../src/lib/meetings/fixtures.test-support';
import { exploreHistoryState } from './explore-state';

const when = '2026-07-19T10:00:00Z';

const alice = {
  id: 1,
  display_label: 'Alice Example',
  display_name: 'Alice Example',
  partial_label: false,
  identifiers: [],
  activity_count: 4,
  file_count: 1,
  source_counts: [{ source_type: 'gmail', count: 3 }],
  first_at: '2025-01-01T00:00:00Z',
  last_at: when,
  cache_revision: 'cache-relationships'
};

const domainSummary = {
  domain: 'example.com',
  activity_count: 3,
  person_count: 2,
  file_count: 1,
  source_counts: [{ source_type: 'gmail', count: 3 }],
  first_at: when,
  last_at: when,
  cache_revision: 'cache-relationships'
};

const messageRow = {
  key: 'message:1',
  kind: 'email',
  occurred_at: '2026-07-18T09:00:00Z',
  preview: 'Preview text',
  source_id: 1,
  title: 'Subject line',
  has_attachments: false,
  message_count: 1
};

const chatBurstRow = {
  key: 'burst:2:70:2026-07-18',
  kind: 'chat_burst',
  occurred_at: '2026-07-18T20:00:00Z',
  first_at: '2026-07-18T08:00:00Z',
  preview: 'Latest chat message',
  source_id: 2,
  title: 'Team Chat',
  has_attachments: false,
  message_count: 6,
  anchor_message_id: 500,
  conversation_id: 70
};

const documentFile = {
  id: 7, key: 'file:7', entry_key: 'message:1', message_id: 1, conversation_id: 21,
  occurred_at: '2026-07-18T09:00:00Z', source_id: 1, source_type: 'gmail',
  source_identifier: 'archive@example.com', containing_title: 'Subject line',
  filename: 'notes.pdf', mime_type: 'application/pdf', mime_family: 'pdf', size_bytes: 2048,
  content_state: 'metadata_only', content_available: false,
  person_provenance: { participant_ids: [1], roles: ['from'], directions: ['from_person'] }
};

const imageFile = {
  ...documentFile, id: 8, key: 'file:8', filename: 'photo.png', mime_type: 'image/png',
  mime_family: 'image',
  person_provenance: {
    participant_ids: [1], roles: ['from', 'conversation_member'],
    directions: ['from_person', 'group']
  }
};

async function prepare(page: Page, personFileBodies: Record<string, unknown>[] = []) {
  await stubOperationStatus(page);
  await page.route('**/api/v1/meetings/metrics', route => route.fulfill({ json: meetingMetrics() }));
  await page.route('**/api/v1/meetings/actions', route => route.fulfill({ json: meetingActions() }));
  await page.route('**/api/v1/saved-views', route => route.fulfill({ json: { saved_views: [] } }));
  await page.route('**/api/v1/sources/status', route => route.fulfill({ json: { sources: [] } }));
  await page.route('**/api/session', (route) => route.fulfill({
    json: { auth_mode: 'loopback', https: false, plain_http_warning: false }
  }));
  await page.route('**/api/v1/relationships', (route) => route.fulfill({ json: {
    rows: [{
      canonical_id: 1, display_label: 'Alice Example', last_at: when, member_ids: [1], score: 2,
      signals: {
        last_interaction_at: when, meeting_count: 0, meetings_together: 0, modalities: 2,
        received_from_them: 1, sent_count: 3, sent_to_them: 1
      }
    }],
    total_count: 1, cache_revision: 'cache-relationships', identity_revision: 1
  } }));
  await page.route('**/api/v1/participants/1', (route) => route.fulfill({ json: alice }));
  await page.route('**/api/v1/relationships/1/timeline', (route) => route.fulfill({ json: {
    canonical_id: 1, identity_revision: 1, cache_revision: 'cache-relationships',
    rows: [messageRow, chatBurstRow], total_count: 2
  } }));
  await page.route('**/api/v1/participants/*/timeline', route => route.fulfill({ json: {
    rows: [{ ...messageRow, kind: 'message', message_type: 'email', conversation_type: 'email',
      source_identifier: 'archive@example.com', source_type: 'gmail', participant_labels: ['Alice Example'], participant_ids: [1],
      attachment_count: 0, attachment_size: 0, deleted_from_source: false, anchor_message_id: 1, match: {} },
      { ...chatBurstRow, key: 'conversation:70', kind: 'conversation', message_type: 'slack', conversation_type: 'chat',
        source_identifier: 'archive@example.com', source_type: 'slack', participant_labels: ['Bob Example'], participant_ids: [2],
        attachment_count: 0, attachment_size: 0, deleted_from_source: false, match: {} }],
    total_count: 2, cache_revision: 'cache-relationships', search_provenance: {}
  } }));
  await page.route('**/api/v1/relationships/*/calendar', route => route.fulfill({ json: {
    participant_id: 1, canonical_id: 1, year: route.request().postDataJSON().year, timezone: 'UTC', days: Array.from({ length: 300 }, (_, index) => {
      const date = new Date(Date.UTC(route.request().postDataJSON().year, 0, index + 1)).toISOString().slice(0, 10);
      const count = date.endsWith('-07-18') ? 2 : index % 13 === 0 ? 3 : index % 5 === 0 ? 1 : 0;
      return { date, total: count, email: count, chat: 0, meetings: 0, sent: 0, received: count, modality_mask: count ? 1 : 0, level: count === 3 ? 'FOURTH_QUARTILE' : count ? 'FIRST_QUARTILE' : 'NONE' };
    }), annual: [],
    current: { temperature: 62 }, peak_temperature: 62, peak_year: 2026,
    scoring_timezone: 'UTC', score_version: 1, effective_date: '2026-07-19', cache_revision: 'cache-relationships', identity_revision: 1
  } }));
  await page.route('**/api/v1/participants/1/files/search', async (route) => {
    const body = route.request().postDataJSON() as Record<string, unknown>;
    personFileBodies.push(body);
    const families = body.mime_families as string[] | undefined;
    return route.fulfill({ json: {
      files: families?.includes('image') ? [imageFile] : [documentFile],
      total_count: 1, cache_revision: 'cache-relationships', search_provenance: {}
    } });
  });
  await page.route('**/api/v1/files/7', (route) => route.fulfill({ json: {
    id: 7, message_id: 1, conversation_id: 21, entry_key: 'message:1', filename: 'notes.pdf',
    mime_type: 'application/pdf', size_bytes: 2048, content_state: 'metadata_only', content_available: false
  } }));
  await page.route('**/api/v1/files/8', (route) => route.fulfill({ json: {
    id: 8, message_id: 1, conversation_id: 21, entry_key: 'message:1', filename: 'photo.png',
    mime_type: 'image/png', size_bytes: 2048, content_state: 'metadata_only', content_available: false
  } }));
  await page.route('**/api/v1/explore', (route) => route.fulfill({ json: {
    rows: [{
      ...messageRow, message_type: 'email', conversation_type: 'email_thread',
      source_identifier: 'archive@example.com', source_type: 'gmail', participant_labels: ['Alice Example'],
      participant_ids: [1], attachment_count: 1, attachment_size: 2048, deleted_from_source: false,
      conversation_id: 21, anchor_message_id: 1, match: {}
    }],
    total_count: 1, cache_revision: 'cache-relationships', search_provenance: {}
  } }));
  await page.route('**/api/v1/domains/search', (route) => route.fulfill({ json: {
    rows: [domainSummary], total_count: 1, cache_revision: 'cache-relationships-domains',
    search_provenance: {}
  } }));
  await page.route('**/api/v1/conversations/70**', (route) => route.fulfill({ json: {
    id: 70, anchor_id: 500, has_before: false, has_after: false, total: 1,
    messages: [{
      id: 500, conversation_id: 70, subject: 'Team Chat', message_type: 'chat',
      from: 'Bob Example', to: [], sent_at: '2026-07-18T20:00:00Z',
      snippet: 'Latest chat message', body: 'Latest chat message', body_html: '', attachments: []
    }]
  } }));
}

test('legacy People URL keeps list, Messages, reader, facet, and history', async ({ page }) => {
  await prepare(page);

  // A pre-rewrite bookmark for the deleted People workspace normalizes to
  // the relationships hub instead of erroring or landing somewhere blank.
  await page.goto(`/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'people' }))}`);

  const hub = page.getByRole('main', { name: 'People' });
  await expect(hub).toBeVisible();
  const list = page.getByRole('grid', { name: 'People results' });
  await expect(list.getByText('Alice Example')).toBeVisible();
  await expect(page.getByText('Select a person or domain', { exact: true })).toBeVisible();
  await expect(page.getByRole('radio', { name: 'People' })).toHaveAttribute('aria-checked', 'true');

  // Opening a person from the ranked list drives the controller and shows
  // the timeline for that cluster.
  await list.getByText('Alice Example').click();
  await expect(page.getByRole('heading', { name: 'Alice Example' })).toBeVisible();
  const timeline = page.getByRole('grid', { name: 'Messages results' });
  await expect(timeline.getByText('Subject line')).toBeVisible();
  await page.getByRole('radio', { name: 'Details', exact: true }).click();
  await expect(page.getByRole('button', { name: 'Open in Directory', exact: true })).toBeVisible();
  await expect(page.getByRole('button', { name: 'Same person…', exact: true })).toBeVisible();
  await page.getByRole('radio', { name: 'Messages', exact: true }).click();

  await timeline.getByText('Team Chat').click();
  const reading = page.getByRole('complementary', { name: /Reading pane: Team Chat/ });
  await expect(reading).toBeVisible();
  await expect(reading.getByRole('button', { name: 'Collapse message 500 from Bob Example' })).toBeVisible();
  await expect(reading.getByText('Latest chat message')).toBeVisible();

  // Toggling the facet switches the ranked list to Domains without losing
  // the open person detail underneath.
  await page.getByRole('radio', { name: 'Domains' }).click();
  await expect(page.getByRole('radio', { name: 'Domains' })).toHaveAttribute('aria-checked', 'true');
  await expect(list.getByText('example.com')).toBeVisible();

  // Browser back undoes the facet toggle first...
  await page.goBack();
  await expect(page.getByRole('radio', { name: 'People' })).toHaveAttribute('aria-checked', 'true');
  await expect(list.getByText('Alice Example')).toBeVisible();
  await expect(page.getByRole('heading', { name: 'Alice Example' })).toBeVisible();

  // ...and a second back undoes opening the person, clearing the list's
  // active selection, closing the person detail, and — since the reading
  // pane's own open/close state lives outside the URL entirely — closing
  // the conversation reading pane that was still open underneath it too.
  await page.goBack();
  await expect.poll(async () => (await exploreHistoryState(page)).relationshipTarget).toBeNull();
  await expect(list.getByRole('row', { name: /Alice Example/ })).toHaveAttribute('aria-selected', 'false');
  await expect(page.getByRole('heading', { name: 'Alice Example' })).toBeHidden();
  await expect(reading).toBeHidden();
  await expect(page.getByText('Select a person or domain', { exact: true })).toBeVisible();
});

test('person attachment gallery preserves directions and Media state across source-message history', async ({ page }) => {
  const personFileBodies: Record<string, unknown>[] = [];
  await prepare(page, personFileBodies);
  await page.goto('/?workspace=relationships');

  const list = page.getByRole('grid', { name: 'People results' });
  await list.getByText('Alice Example').click();
  await page.getByRole('radio', { name: 'Files', exact: true }).click();
  await expect(page.getByRole('grid', { name: 'Files results' }).getByText('notes.pdf')).toBeVisible();
  expect(personFileBodies[0]).toMatchObject({
    directions: ['from_person'],
    mime_families: ['pdf', 'audio', 'text', 'document', 'archive', 'other'],
    limit: 500
  });

  await page.getByRole('checkbox', { name: 'Group conversations' }).check();
  await expect.poll(() => personFileBodies.length).toBe(2);
  expect(personFileBodies[1]).toMatchObject({ directions: ['from_person', 'group'] });

  await page.getByRole('radio', { name: 'Media' }).click();
  const card = page.getByRole('button', { name: 'Open photo.png' });
  await expect(card).toBeVisible();
  expect(personFileBodies[2]).toMatchObject({
    directions: ['from_person', 'group'], mime_families: ['image', 'video'], limit: 500
  });
  await card.click();
  const viewer = page.getByRole('dialog', { name: 'View photo.png' });
  await expect(viewer).toBeVisible();
  await viewer.getByRole('button', { name: 'Open message', exact: true }).click();
  await expect(page.getByRole('heading', { level: 1, name: 'Messages' })).toBeVisible();
  await expect(page.getByRole('complementary', { name: /Reading pane: Subject line/ })).toBeVisible();

  await page.goBack();
  await expect(page.getByRole('radio', { name: 'Media' })).toHaveAttribute('aria-checked', 'true');
  await expect(page.getByRole('checkbox', { name: 'Group conversations' })).toBeChecked();
  await expect(page.getByRole('button', { name: 'Open photo.png' })).toBeVisible();
  const restored = await exploreHistoryState(page);
  expect(restored).toMatchObject({
    workspace: 'relationships', relationshipTarget: 'cluster:1', relationshipFiles: true,
    personFilePresentation: 'media', personFileDirections: ['from_person', 'group']
  });
});

test('Back restores the relationship message and its conversation window after following a participant', async ({ page }) => {
  await prepare(page);
  const windows: URLSearchParams[] = [];
  await page.route('**/api/v1/conversations/70**', route => {
    windows.push(new URL(route.request().url()).searchParams);
    return route.fulfill({ json: {
      id: 70, anchor_id: 500, has_before: false, has_after: false, total: 1,
      messages: [{
        id: 500, conversation_id: 70, subject: 'Team Chat', message_type: 'chat',
        from: 'Bob Example', from_participant_id: 2, to: [], sent_at: '2026-07-18T20:00:00Z',
        snippet: 'Latest chat message', body: 'Latest chat message', body_html: '', attachments: []
      }]
    } });
  });
  await page.route('**/api/v1/participants/2', route => route.fulfill({ json: {
    ...alice, id: 2, display_label: 'Bob Example', display_name: 'Bob Example'
  } }));
  await page.route('**/api/v1/relationships/2/timeline', route => route.fulfill({ json: {
    canonical_id: 2, identity_revision: 1, cache_revision: 'cache-relationships', rows: [], total_count: 0
  } }));
  await page.goto('/?workspace=relationships');
  await page.getByRole('grid', { name: 'People results' }).getByText('Alice Example').click();
  await page.getByRole('button', { name: '2 messages on Jul 18, 2026' }).filter({ visible: true }).click();
  await page.getByRole('grid', { name: 'Messages results' }).getByText('Team Chat').click();
  expect(windows.at(-1)?.get('start')).toBe('2026-07-18T00:00:00.000Z');
  expect(windows.at(-1)?.get('end')).toBe('2026-07-19T00:00:00.000Z');
  const reading = page.getByRole('complementary', { name: 'Reading pane: Team Chat' });
  await reading.getByRole('button', { name: 'Bob Example', exact: true }).click();
  await expect(page.getByRole('heading', { name: 'Bob Example' })).toBeVisible();
  await expect(reading).toBeHidden();
  await page.goBack();
  await expect(page.getByRole('heading', { name: 'Alice Example' })).toBeVisible();
  await expect(reading.getByRole('button', { name: 'Collapse message 500 from Bob Example' })).toBeVisible();
  expect(windows.at(-1)?.get('start')).toBeNull();
  expect(windows.at(-1)?.get('end')).toBeNull();
  await page.goForward();
  await expect(page.getByRole('heading', { name: 'Bob Example' })).toBeVisible();
  await expect(reading).toBeHidden();
  await page.goBack();
  await expect(reading.getByText('Latest chat message')).toBeVisible();
});

for (const width of [1440, 420]) {
  test(`person calendar, messages, and files remain usable at ${width}px`, async ({ page }) => {
    await page.setViewportSize({ width, height: 900 });
    await prepare(page);
    await page.goto(`/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'relationships', relationshipTarget: 'cluster:1' }))}`);
    await expect(page.getByRole('region', { name: 'Relationship activity calendar' })).toBeVisible();
    const messages = page.getByRole('grid', { name: 'Messages results' });
    await expect(messages.getByText('Subject line')).toBeInViewport();
    expect((await messages.boundingBox())!.height).toBeGreaterThan(100);
    await page.getByRole('radio', { name: 'Files', exact: true }).click();
    await expect(page.getByRole('grid', { name: 'Files results' }).getByText('notes.pdf')).toBeInViewport();
    expect(await page.evaluate(() => document.documentElement.scrollWidth - innerWidth)).toBeLessThanOrEqual(1);
    if (width === 420) {
      await page.getByRole('button', { name: 'Back to People' }).click();
      await expect(page.getByRole('grid', { name: 'People results' })).toBeVisible();
    }
  });
}

test('clicking a relationship label preserves keyboard navigation and one grid tab stop', async ({ page }) => {
  await prepare(page);
  await page.route('**/api/v1/relationships', route => route.fulfill({ json: {
    rows: [
      { canonical_id: 1, display_label: 'Alice Example', last_at: when, member_ids: [1], score: 2,
        signals: { last_interaction_at: when, meeting_count: 0, meetings_together: 0, modalities: 2,
          received_from_them: 1, sent_count: 3, sent_to_them: 1 } },
      { canonical_id: 2, display_label: 'Bob Example', last_at: when, member_ids: [2], score: 1,
        signals: { last_interaction_at: when, meeting_count: 0, meetings_together: 0, modalities: 1,
          received_from_them: 1, sent_count: 1, sent_to_them: 1 } }
    ], total_count: 2, cache_revision: 'cache-relationships', identity_revision: 1
  } }));
  await page.route('**/api/v1/participants/2', route => route.fulfill({ json: {
    ...alice, id: 2, display_label: 'Bob Example', display_name: 'Bob Example'
  } }));
  await page.route('**/api/v1/relationships/2/timeline', route => route.fulfill({ json: {
    canonical_id: 2, identity_revision: 1, cache_revision: 'cache-relationships', rows: [], total_count: 0
  } }));
  await page.goto('/?workspace=relationships');
  const grid = page.getByRole('grid', { name: 'People results' });
  await grid.getByText('Alice Example', { exact: true }).click();
  await expect(grid).toBeFocused();
  await page.keyboard.press('j');
  await expect(grid.getByRole('row', { name: /Bob Example/ })).toHaveClass(/focus/);
  await page.keyboard.press('ArrowUp');
  await expect(grid.getByRole('row', { name: /Alice Example/ })).toHaveClass(/focus/);
  await page.keyboard.press('ArrowDown');
  await page.keyboard.press('Enter');
  await expect(page.getByRole('heading', { name: 'Bob Example' })).toBeVisible();
  await page.keyboard.press('Tab');
  await expect.poll(() => grid.evaluate(element => element.contains(document.activeElement))).toBe(false);
});

test('clicking a relationship checkbox keeps keyboard selection in the grid', async ({ page }) => {
  await prepare(page);
  await page.route('**/api/v1/relationships', route => route.fulfill({ json: {
    rows: [
      { canonical_id: 1, display_label: 'Alice Example', last_at: when, member_ids: [1], score: 2,
        signals: { last_interaction_at: when, meeting_count: 0, meetings_together: 0, modalities: 2,
          received_from_them: 1, sent_count: 3, sent_to_them: 1 } },
      { canonical_id: 2, display_label: 'Bob Example', last_at: when, member_ids: [2], score: 1,
        signals: { last_interaction_at: when, meeting_count: 0, meetings_together: 0, modalities: 1,
          received_from_them: 1, sent_count: 1, sent_to_them: 1 } }
    ], total_count: 2, cache_revision: 'cache-relationships', identity_revision: 1
  } }));
  await page.goto('/?workspace=relationships');
  const grid = page.getByRole('grid', { name: 'People results' });
  await grid.getByRole('row', { name: /Alice Example/ }).hover();
  await grid.getByRole('checkbox', { name: 'Select Alice Example' }).click();
  await expect(grid).toBeFocused();
  await page.keyboard.press('j');
  await expect(grid.getByRole('row', { name: /Bob Example/ })).toHaveClass(/focus/);
  await page.keyboard.press('Space');
  await expect(grid.getByRole('checkbox', { name: 'Unselect Bob Example' })).toBeChecked();
});

test('relationship labels disclose on hover and identity hues retain text contrast', async ({ page }) => {
  await prepare(page);
  await page.goto('/?workspace=relationships');
  for (const theme of ['light', 'dark'] as const) {
    await setKitTheme(page, theme);
    const minimum = await page.locator('.identity-avatar').first().evaluate(element => {
      const canvas = document.createElement('canvas');
      canvas.width = canvas.height = 1;
      const context = canvas.getContext('2d')!;
      const ancestors: Element[] = [];
      for (let ancestor: Element | null = element; ancestor; ancestor = ancestor.parentElement) ancestors.unshift(ancestor);
      const luminance = (values: Uint8ClampedArray) => {
        const linear = [...values].slice(0, 3).map(value => {
          value /= 255;
          return value <= 0.04045 ? value / 12.92 : ((value + 0.055) / 1.055) ** 2.4;
        });
        return .2126 * linear[0]! + .7152 * linear[1]! + .0722 * linear[2]!;
      };
      let minimum = Infinity;
      for (let hue = 0; hue < 360; hue++) {
        (element as HTMLElement).style.setProperty('--avatar-hue', String(hue));
        context.fillStyle = 'white';
        context.fillRect(0, 0, 1, 1);
        for (const ancestor of ancestors) {
          context.fillStyle = getComputedStyle(ancestor).backgroundColor;
          context.fillRect(0, 0, 1, 1);
        }
        const background = luminance(context.getImageData(0, 0, 1, 1).data);
        context.fillStyle = getComputedStyle(element).color;
        context.fillRect(0, 0, 1, 1);
        const foreground = luminance(context.getImageData(0, 0, 1, 1).data);
        minimum = Math.min(minimum, (Math.max(background, foreground) + .05) / (Math.min(background, foreground) + .05));
      }
      return minimum;
    });
    expect(minimum, `${theme} identity initials`).toBeGreaterThanOrEqual(4.5);
  }
});

test.describe('local domain days', () => {
  test.use({ timezoneId: 'America/New_York' });
test('domain calendar retries and retains its local day across Details', async ({ page }) => {
  await prepare(page);
  await page.route('**/api/v1/domains/example.com/summary', route => route.fulfill({ json: { summary: { ...domainSummary, first_at: '2025-05-01T00:00:00Z', last_at: '2025-08-01T00:00:00Z' } } }));
  const bodies: Record<string, unknown>[] = [];
  let attempts = 0;
  await page.route('**/api/v1/domains/example.com', route => route.fulfill({ json: { ...domainSummary, first_at: '2022-01-01T12:00:00Z' } }));
  await page.route('**/api/v1/domains/example.com/timeline', route => {
    bodies.push(route.request().postDataJSON());
    return route.fulfill({ json: { rows: [
      { ...messageRow, occurred_at: when, kind: 'message', message_type: 'email', conversation_type: 'email',
        source_identifier: 'archive@example.com', source_type: 'gmail', participant_labels: ['Alice Example'], participant_ids: [1],
        attachment_count: 0, attachment_size: 0, deleted_from_source: false, anchor_message_id: 1, match: {} },
      { ...chatBurstRow, key: 'conversation:70', occurred_at: when, kind: 'conversation', message_type: 'slack', conversation_type: 'chat', message_count: 2,
        source_identifier: 'archive@example.com', source_type: 'slack', participant_labels: ['Bob Example'], participant_ids: [2],
        attachment_count: 0, attachment_size: 0, deleted_from_source: false, match: {} }
    ], total_count: 2, cache_revision: 'cache-domain', search_provenance: {} } });
  });
  await page.route('**/api/v1/explore/groups', route => {
    const body = route.request().postDataJSON();
    if (body.grouping?.[0] !== 'day') return route.fulfill({ json: { rows: [], total_count: 0, cache_revision: 'cache-domain', search_provenance: {} } });
    expect(body).toMatchObject({ timezone: 'America/New_York', sent_only: true, limit: 366, filters: [
      { dimension: 'domain', values: ['example.com'] },
      { dimension: 'after', values: [`${body.filters[1].values[0].slice(0, 4)}-01-01T05:00:00.000Z`] },
      { dimension: 'before', values: [`${Number(body.filters[1].values[0].slice(0, 4)) + 1}-01-01T05:00:00.000Z`] },
    ] });
    if (++attempts === 1) return route.fulfill({ status: 500, json: { message: 'Could not load activity.' } });
    return route.fulfill({ json: { rows: [{ key: `${body.filters[1].values[0].slice(0, 4)}-07-19`, label: 'Jul 19', count: 3, estimated_bytes: 30, latest_at: when }],
      total_count: 1, cache_revision: 'cache-domain', search_provenance: {} } });
  });
  await page.goto(`/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'relationships', filters: [{ dimension: 'source', values: ['1'] }] }))}`);
  await page.getByRole('radio', { name: 'Domains', exact: true }).click();
  await page.getByRole('grid', { name: 'People results' }).getByText('example.com', { exact: true }).click();
  const calendar = page.getByRole('region', { name: 'Relationship activity calendar' });
  await calendar.getByRole('button', { name: 'Retry' }).click();
  await expect(calendar.getByRole('heading', { name: 'Activity since Jan 2022' })).toBeVisible();
  await calendar.getByRole('button', { name: 'Previous relationship year' }).click();
  const day = calendar.getByRole('button', { name: '3 messages on Jul 19, 2025' }).filter({ visible: true });
  await day.click();
  await expect.poll(() => bodies.at(-1)).toMatchObject({ filters: [
    { dimension: 'source', values: ['1'] },
    { dimension: 'after', values: ['2025-07-19T04:00:00.000Z'] },
    { dimension: 'before', values: ['2025-07-20T04:00:00.000Z'] },
  ] });
  await expect(page.getByRole('grid', { name: 'Messages results' }).getByText('Subject line')).toBeVisible();
  await page.getByRole('radio', { name: 'Details', exact: true }).click();
  await page.getByRole('radio', { name: 'Messages', exact: true }).click();
  await expect(day).toHaveAttribute('aria-pressed', 'true');
  await page.getByRole('button', { name: 'Filter', exact: true }).click();
  await page.getByRole('button', { name: 'Clear filters', exact: true }).click();
  await page.keyboard.press('Escape');
  await expect.poll(() => bodies.at(-1)?.filters).toEqual([]);
});

});

test('saved people keep durable IDs separate from participants and open profiles with no messages', async ({ page }) => {
  await prepare(page);
  await page.route('**/api/v1/people/directory**', route => route.fulfill({ json: { people: [
    { id: 91, display_name: 'Saved Alice', organizations: ['Example Org'], categories: [], contact_state: 'active', revision: 1, carddav_bindings: [], vcard_uid: 'example-91' },
    { id: 92, display_name: 'Saved Contact', organizations: [], categories: [], contact_state: 'active', revision: 1, carddav_bindings: [], vcard_uid: 'example-92' },
  ] } }));
  await page.route('**/api/v1/people/91', route => route.fulfill({ json: { id: 91, display_name: 'Saved Alice', revision: 1, participant_ids: [1] } }));
  await page.route('**/api/v1/people/92', route => route.fulfill({ json: { id: 92, display_name: 'Saved Contact', revision: 1, participant_ids: [] } }));
  await page.goto(`/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'relationships', relationshipSaved: true }))}`);
  const list = page.getByRole('grid', { name: 'People results' });
  await list.getByText('Saved Alice', { exact: true }).click();
  await expect(page.getByRole('heading', { name: 'Alice Example' })).toBeVisible();
  expect(await exploreHistoryState(page)).toMatchObject({ directoryPersonID: 91, relationshipTarget: 'cluster:1' });
  await list.getByText('Saved Contact', { exact: true }).click();
  await expect(page.getByRole('heading', { name: 'Saved Contact' })).toBeVisible();
  expect(await exploreHistoryState(page)).toMatchObject({ directoryPersonID: 92, relationshipTarget: null });
  await page.getByRole('button', { name: 'Open in Directory', exact: true }).click();
  await expect.poll(async () => (await exploreHistoryState(page)).workspace).toBe('directory');
  expect(await exploreHistoryState(page)).toMatchObject({ directoryPersonID: 92 });
});

test('People search and intersecting filters stay separate from identity scope across tabs and history', async ({ page }) => {
  const fileBodies: Record<string, unknown>[] = [];
  await prepare(page, fileBodies);
  const requests: Record<string, unknown>[] = [];
  let obsoleteTimeline = 0;
  await page.route('**/api/v1/relationships/1/timeline', route => { obsoleteTimeline += 1; return route.fulfill({ status: 500, json: { message: 'Obsolete timeline failed' } }); });
  await page.route('**/api/v1/participants/search', route => route.fulfill({ json: { rows: [alice], total_count: 1, cache_revision: 'cache-relationships', search_provenance: {} } }));
  await page.route('**/api/v1/participants/1/summary', route => route.fulfill({ json: { summary: alice } }));
  await page.route('**/api/v1/participants/1/timeline', async route => {
    const body = route.request().postDataJSON();
    requests.push(body);
    await route.fulfill({ json: { rows: [{ ...messageRow, title: 'Alias activity', kind: 'message', message_type: 'email', conversation_type: 'email', source_identifier: 'archive@example.com', source_type: 'gmail', participant_labels: ['Alice Alias'], participant_ids: [2], attachment_count: 0, attachment_size: 0, deleted_from_source: false, match: {} }], total_count: 1, cache_revision: 'cache-relationships', search_provenance: {} } });
  });
  await page.goto(`/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'relationships', identityQuery: 'alice', relationshipTarget: 'cluster:1', filters: [{ dimension: 'participant', values: ['2'] }] }))}`);
  await expect(page.getByRole('grid', { name: 'Messages results' }).getByText('Alias activity')).toBeVisible();
  expect(requests.at(-1)).toMatchObject({ filters: [{ dimension: 'participant', values: ['2'] }] });
  expect(requests.at(-1)).not.toHaveProperty('query');
  expect(obsoleteTimeline).toBe(0);
  await page.getByRole('button', { name: 'Previous relationship year' }).click();
  await expect(page.getByRole('region', { name: 'Relationship activity calendar' }).getByText('2025', { exact: true })).toBeVisible();
  await page.getByRole('button', { name: '2 messages on Jul 18, 2025' }).filter({ visible: true }).click();
  await page.getByRole('main', { name: 'Messages', exact: true }).getByRole('button', { name: 'Filter', exact: true }).click();
  await page.getByLabel('From', { exact: true }).fill('2026-07-01');
  await expect.poll(async () => (await exploreHistoryState(page)).filters).toContainEqual({ dimension: 'after', values: ['2026-07-01T00:00:00.000Z'] });
  await expect(page.getByRole('region', { name: 'Relationship activity calendar' }).getByText('2025', { exact: true })).toBeVisible();
  await page.keyboard.press('Escape');
  await page.getByRole('radio', { name: 'Files', exact: true }).click();
  await expect.poll(() => fileBodies.length).toBeGreaterThan(0);
  expect(fileBodies.at(-1)).toMatchObject({ predicate: { filters: [{ dimension: 'participant', values: ['2'] }, { dimension: 'after', values: ['2026-07-01T00:00:00.000Z'] }] } });
  expect((fileBodies.at(-1)?.predicate as Record<string, unknown>)).not.toHaveProperty('query');
  await page.goBack();
  await expect(page.getByRole('grid', { name: 'Messages results' }).getByText('Alias activity')).toBeVisible();
  await page.goBack();
  await expect.poll(async () => (await exploreHistoryState(page)).filters).toEqual([{ dimension: 'participant', values: ['2'] }]);
  await page.reload();
  await expect(page.getByRole('grid', { name: 'Messages results' }).getByText('Alias activity')).toBeVisible();
  expect(requests.at(-1)?.filters).toEqual([{ dimension: 'participant', values: ['2'] }]);
  await page.getByRole('searchbox', { name: 'Search people', exact: true }).fill('alice');
  await page.getByRole('button', { name: 'Search', exact: true }).click();
  await page.getByRole('button', { name: 'Messages', exact: true }).click();
  expect((await exploreHistoryState(page)).query).toBe('');
});

test('observed people supersede pending saved profiles and restored missing profiles show an error', async ({ page }) => {
  await prepare(page);
  await page.route('**/api/v1/people/directory**', route => route.fulfill({ json: { people: [{ id: 92, display_name: 'Saved Contact', organizations: [], categories: [], contact_state: 'active', revision: 1, carddav_bindings: [], vcard_uid: 'example-92' }] } }));
  let release!: () => void;
  const held = new Promise<void>(resolve => release = resolve);
  await page.route('**/api/v1/people/92', async route => { await held; await route.fulfill({ json: { id: 92, display_name: 'Saved Contact', revision: 1, participant_ids: [] } }); });
  await page.goto(`/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'relationships', relationshipSaved: true }))}`);
  await page.getByRole('grid', { name: 'People results' }).getByText('Saved Contact').click();
  await page.getByRole('button', { name: 'Remove Saved', exact: true }).click();
  await page.getByRole('grid', { name: 'People results' }).getByText('Alice Example').click();
  await expect(page.getByRole('heading', { name: 'Alice Example' })).toBeVisible();
  release();
  await expect(page.getByRole('heading', { name: 'Saved Contact' })).toBeHidden();
  await page.route('**/api/v1/people/999', route => route.fulfill({ status: 404, json: { message: 'Person not found' } }));
  await page.goto(`/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'relationships', directoryPersonID: 999 }))}`);
  await expect(page.getByRole('alert')).toHaveText('Saved person not found.');
  await expect(page.getByRole('heading', { name: 'Saved person', exact: true })).toBeHidden();
});

test('list filters clear hidden selections and each mode offers only supported controls', async ({ page }) => {
  await prepare(page);
  await page.route('**/api/v1/people/directory**', route => route.fulfill({ json: { people: [91, 92].map(id => ({ id, display_name: `Saved ${id}`, organizations: [], categories: [], contact_state: 'active', revision: 1, carddav_bindings: [], vcard_uid: `example-${id}` })) } }));
  await page.route('**/api/v1/relationships', route => route.fulfill({ json: { rows: [1, 2].map(id => ({ canonical_id: id, display_label: id === 1 ? 'Alice Example' : 'Bob Example', last_at: when, member_ids: [id], score: 2, signals: { last_interaction_at: when, sent_count: 3 } })), total_count: 2, cache_revision: 'cache-relationships' } }));
  await page.goto('/?workspace=relationships');
  const list = page.getByRole('grid', { name: 'People results' });
  await list.getByRole('row', { name: /Alice Example/ }).hover();
  await list.getByRole('checkbox', { name: 'Select Alice Example' }).click();
  await list.press('ArrowDown');
  await list.press('Space');
  await expect(page.getByText('2 selected', { exact: true })).toBeVisible();
  await page.getByTitle('Filter people', { exact: true }).click();
  await page.getByRole('button', { name: 'Include one-way senders', exact: true }).click();
  await expect(page.getByText('2 selected', { exact: true })).toBeHidden();
  await page.keyboard.press('Escape');
  await page.getByRole('combobox', { name: 'Sort: Closest', exact: true }).click();
  await page.getByRole('option', { name: 'Name', exact: true }).click();
  await page.getByTitle('Filter people', { exact: true }).click();
  await expect(page.getByRole('button', { name: 'Include one-way senders', exact: true })).toBeHidden();
  await page.getByRole('button', { name: 'Saved', exact: true }).click();
  await page.keyboard.press('Escape');
  await page.getByRole('combobox', { name: 'Sort: Name', exact: true }).click();
  await expect(page.getByRole('option', { name: 'Closest', exact: true })).toBeHidden();
  await page.keyboard.press('Escape');
  await list.getByRole('row', { name: /Saved 91/ }).hover();
  await list.getByRole('checkbox', { name: 'Select Saved 91' }).click();
  await expect(page.getByText('1 selected', { exact: true })).toBeVisible();
  await page.getByTitle('Filter people', { exact: true }).click();
  await page.getByRole('button', { name: 'Profile filters', exact: true }).click();
  await page.getByRole('textbox', { name: 'Category filter', exact: true }).fill('work');
  await expect(page.getByText('1 selected', { exact: true })).toBeHidden();
});

test('Saved People retains rows and reloads after a page cursor expires', async ({ page }) => {
  await prepare(page);
  let loads = 0;
  await page.route('**/api/v1/people/directory**', route => {
    if (new URL(route.request().url()).searchParams.has('cursor')) return route.fulfill({ status: 400, json: { error: 'invalid_cursor', message: 'Directory cursor is invalid' } });
    return route.fulfill({ json: { people: Array.from({ length: 40 }, (_, id) => ({ id: id + 1, display_name: loads ? `Reloaded ${id}` : `Contact ${id}`, organizations: [], categories: [], revision: 1, contact_state: 'active' })), next_cursor: loads++ ? undefined : 'page-2' } });
  });
  await page.goto(`/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'relationships', relationshipSaved: true }))}`);
  const list = page.getByRole('grid', { name: 'People results' });
  await expect(list.getByText('Contact 0', { exact: true })).toBeVisible();
  await list.evaluate(element => element.scrollTop = element.scrollHeight);
  await page.getByRole('button', { name: 'Reload', exact: true }).click();
  await expect(list.getByText('Reloaded 0', { exact: true })).toBeVisible();
});
