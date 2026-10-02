import { expect, test, type Page } from '@playwright/test';
import { setKitTheme } from './kit-ui';
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
  first_at: when,
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

test('legacy People URL lands on the Relationships hub and walks list, timeline, reading pane, facet, and history', async ({ page }) => {
  await prepare(page);

  // A pre-rewrite bookmark for the deleted People workspace normalizes to
  // the relationships hub instead of erroring or landing somewhere blank.
  await page.goto(`/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'people' }))}`);

  const hub = page.getByRole('main', { name: 'Relationships' });
  await expect(hub).toBeVisible();
  const list = page.getByRole('grid', { name: 'Relationship results' });
  await expect(list.getByText('Alice Example')).toBeVisible();
  await expect(page.getByText('Select a person or domain', { exact: true })).toBeVisible();
  await expect(page.getByRole('radio', { name: 'People' })).toHaveAttribute('aria-checked', 'true');

  // Opening a person from the ranked list drives the controller and shows
  // the timeline for that cluster.
  await list.getByText('Alice Example').click();
  await expect(page.getByRole('heading', { name: 'Alice Example' })).toBeVisible();
  const timeline = page.getByRole('grid', { name: 'Relationship activity' });
  await expect(timeline.getByText('Subject line')).toBeVisible();

  // A chat-burst row opens straight into the bounded conversation window in
  // the reading pane rather than the plain entry summary: the anchor message
  // renders expanded as a card in the thread.
  await timeline.getByText('6 messages in Team Chat').click();
  const reading = page.getByRole('complementary', { name: /Reading pane: 6 messages in Team Chat/ });
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
  await page.goto('/');

  const list = page.getByRole('grid', { name: 'Relationship results' });
  await list.getByText('Alice Example').click();
  await page.getByRole('radio', { name: 'Files 1' }).click();
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
  await viewer.getByRole('button', { name: 'Open containing item' }).click();
  await expect(page.getByRole('heading', { level: 1, name: 'Everything' })).toBeVisible();
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

for (const width of [1440, 420]) {
  test(`expanded relationship overview leaves usable activity at ${width}px`, async ({ page }) => {
    await page.setViewportSize({ width, height: 900 });
    await prepare(page);
    await page.route('**/api/v1/relationships/1/calendar', route => route.fulfill({ json: {
      participant_id: 1, canonical_id: 1, year: 2026, timezone: 'UTC', days: [], annual: [],
      current: { temperature: 62, rank: 1, population: 1, raw_score: 3,
        signals: { sent_signal: 1, received_volume: 1, meeting_signal: 0, modalities: 2 } },
      peak_temperature: 62, peak_year: 2026, scoring_timezone: 'UTC', score_version: 1,
      effective_date: '2026-07-19', cache_revision: 'cache-relationships', identity_revision: 1
    } }));
    await page.route('**/api/v1/meetings/metrics', route => route.fulfill({ json: {
      totals: { meeting_count: 2, known_duration_count: 2, unknown_duration_count: 0,
        total_known_seconds: 3600, average_known_seconds: 1800 },
      duration_by_basis: [{ basis: 'provider', count: 2, total_seconds: 3600 }],
      months: [], undated_count: 0
    } }));
    await page.route('**/api/v1/meetings/actions', route => route.fulfill({ json: {
      rows: [], total_count: 0, coverage: { available: 2, meeting_count: 2, partial: 0, unavailable: 0, unsupported: 0 }
    } }));
    await page.goto(`/?explore=${encodeURIComponent(JSON.stringify({
      workspace: 'relationships', relationshipTarget: 'cluster:1'
    }))}`);
    const overview = page.locator('.meeting-overview');
    await expect(overview).toHaveAttribute('open', '');
    await expect(page.getByRole('heading', { name: '2 meetings', exact: true })).toBeVisible();
    const timeline = page.getByRole('grid', { name: 'Relationship activity' });
    await expect(timeline).toBeInViewport();
    expect((await timeline.boundingBox())!.height).toBeGreaterThanOrEqual(100);
    await timeline.getByText('6 messages in Team Chat', { exact: true }).click();
    await expect(page.getByRole('complementary', { name: /^Reading pane:/ })).toBeVisible();
    await expect(timeline).toBeInViewport();
    expect((await timeline.boundingBox())!.height).toBeGreaterThanOrEqual(100);
    await page.getByRole('radio', { name: 'Files 1', exact: true }).click();
    const files = page.getByRole('grid', { name: 'Files results' });
    await expect(files.getByText('notes.pdf', { exact: true })).toBeInViewport();
    expect((await files.boundingBox())!.height).toBeGreaterThanOrEqual(100);
    await expect(overview).toHaveAttribute('open', '');
    expect(await page.evaluate(() => document.documentElement.scrollWidth - innerWidth)).toBeLessThanOrEqual(1);
    await page.screenshot({ path: `test-results/artifacts/relationships-expanded-${width}.png` });
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
  await page.goto('/');
  const grid = page.getByRole('grid', { name: 'Relationship results' });
  await grid.getByText('Alice Example', { exact: true }).click();
  await expect(grid).toBeFocused();
  await page.keyboard.press('j');
  await expect(grid.getByRole('row', { name: /Bob Example/ })).toHaveClass(/active/);
  await page.keyboard.press('ArrowUp');
  await expect(grid.getByRole('row', { name: /Alice Example/ })).toHaveClass(/active/);
  await page.keyboard.press('ArrowDown');
  await page.keyboard.press('Enter');
  await expect(page.getByRole('heading', { name: 'Bob Example' })).toBeVisible();
  await page.keyboard.press('Tab');
  await expect.poll(() => grid.evaluate(element => element.contains(document.activeElement))).toBe(false);
});

test('relationship labels disclose on hover and identity hues retain text contrast', async ({ page }) => {
  await prepare(page);
  await page.goto('/');
  const name = page.getByRole('grid', { name: 'Relationship results' }).locator('.kit-tooltip-trigger').first();
  await name.hover();
  await expect(page.getByRole('tooltip')).toHaveText('Alice Example');
  await expect(page.getByRole('tooltip')).toBeInViewport();
  await page.keyboard.press('Escape');
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
