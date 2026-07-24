import { expect, test, type Page } from '@playwright/test';

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

async function prepare(page: Page) {
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
  await page.route('**/api/v1/people/1', (route) => route.fulfill({ json: alice }));
  await page.route('**/api/v1/relationships/1/timeline', (route) => route.fulfill({ json: {
    canonical_id: 1, identity_revision: 1, cache_revision: 'cache-relationships',
    rows: [messageRow, chatBurstRow], total_count: 2
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
  await expect.poll(() => JSON.parse(new URL(page.url()).searchParams.get('explore') ?? '{}').relationshipTarget).toBeNull();
  await expect(list.getByRole('row', { name: /Alice Example/ })).toHaveAttribute('aria-selected', 'false');
  await expect(page.getByRole('heading', { name: 'Alice Example' })).toBeHidden();
  await expect(reading).toBeHidden();
  await expect(page.getByText('Select a person or domain', { exact: true })).toBeVisible();
});
