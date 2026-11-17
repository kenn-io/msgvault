import { expect, type Page } from '@playwright/test';

const revision = `sha256:${'d'.repeat(64)}`;
const target = { kind: 'attribute', key: 'location', revision };

export async function installPersonFacts(page: Page): Promise<void> {
  const definition = {
    id: 8, universal_id: 'location', object_type: 'person', slug: 'location', label: 'Location',
    value_type: 'text', field_type: 'text', cardinality: 'single', display_order: 1,
    is_required: false, ownership: 'user', ui_creatable: true, ui_editable: true,
    api_mutable: true, is_searchable: true, is_sensitive: false, is_audited: true,
    is_deletable: true, history_exempt: false, is_active: true, revision: 1,
    created_at: '2026-01-01T12:00:00Z', updated_at: '2026-01-01T12:00:00Z',
  };
  const claim = {
    id: 71, claim_key: 'location-claim', generation_id: 72, evidence_ids: [51], target,
    submitted_value: '"New York"', normalized_value: '"New York"', relation: 'support',
    origin: 'extraction', valid_from: '2026-01-01T00:00:00Z', valid_until: null,
    confidence: { reported_score: 900 }, created_at: '2026-01-03T12:00:00Z',
  };
  const decisions = [
    {
      id: 81, decision_key: 'location-decision', claim_key: 'location-claim', claim_value: '"New York"',
      outcome: 'current-value-kept', target, resolver_version: 'person-fact-resolver-v1',
      action: 'retained', reason: 'insufficient-margin',
      score_maxima: { authority: 300, confidence: 100, corroboration: 150, directness: 400, freshness: 100, source_class: 500, total: 1550 },
      score: { authority: 100, confidence: 90, corroboration: 0, directness: 200, freshness: 20, source_class: 300, total: 710 },
      created_at: '2026-01-03T12:00:00Z',
    },
    {
      id: 82, decision_key: 'old-location-decision', claim_key: 'old-location-claim', claim_value: '"Portland"',
      outcome: null, target, resolver_version: 'person-fact-resolver-v1', action: 'retained',
      reason: 'legacy', score: { authority: 0, confidence: 0, corroboration: 0, directness: 0, freshness: 0, source_class: 0, total: 0 },
      created_at: '2025-01-03T12:00:00Z',
    },
  ];

  await page.route('**/api/v1/people/42/attributes*', route => route.fulfill({ json: {
    person_id: 42,
    attributes: [{
      definition,
      current: [{ id: 1, person_id: 42, definition_id: 8, definition_slug: 'location', ordinal: 0, value: { type: 'text', text: 'Boston' }, source: 'user', source_ref: 'user', created_at: '2026-01-01T12:00:00Z', active_from: '2026-01-01T12:00:00Z' }],
      history: [],
    }],
  } }));
  await page.route('**/api/v1/person-fact-targets*', route => route.fulfill({ json: { targets: [{
    ...target, description: 'Location', slug: 'location', universal_id: 'location', value_type: 'text',
    cardinality: 'single', sensitive: false, choices: null, fields: null,
  }] } }));
  await page.route(/\/api\/v1\/people\/42\/fact-summary(?:\?.*)?$/, route => route.fulfill({ json: {
    claimed_fields: 1, latest_claim_at: '2026-01-03T12:00:00Z', has_values: true, has_fact_history: true,
  } }));
  await page.route(/\/api\/v1\/people\/42\/fact-claims(?:\?.*)?$/, route => route.fulfill({ json: { claims: [claim] } }));
  await page.route(/\/api\/v1\/people\/42\/fact-decisions(?:\?.*)?$/, route => {
    const claimKey = new URL(route.request().url()).searchParams.get('claim_key');
    return route.fulfill({ json: { decisions: claimKey ? decisions.filter(row => row.claim_key === claimKey) : decisions } });
  });
  await page.route(/\/api\/v1\/people\/42\/fact-evidence(?:\?.*)?$/, route => route.fulfill({ json: { evidence: [{
    id: 51, evidence_key: 'location-evidence', authority: 'ordinary', directness: 'direct-other', identity_score: 950,
    source_class: 'archive', event_time: '2026-01-01T12:00:00Z', recorded_time: '2026-01-02T12:00:00Z',
    created_at: '2026-01-03T12:00:00Z', supported: true, excerpt: null,
  }] } }));
  await page.route(/\/api\/v1\/people\/42\/fact-pins(?:\?.*)?$/, route => route.fulfill({ json: { pins: [] } }));
}

export async function openPersonFacts(page: Page) {
  await page.goto('/?explore=' + encodeURIComponent(JSON.stringify({ workspace: 'relationships', directoryPersonID: 42 })));
  await expect(page.getByRole('radio', { name: 'Messages', exact: true })).toBeVisible();
  await page.getByRole('radio', { name: 'Details', exact: true }).click();
  const facts = page.getByRole('region', { name: 'Facts', exact: true });
  await facts.getByRole('button', { name: 'Show', exact: true }).click();
  await expect(facts.getByRole('radio', { name: 'Claims', exact: true })).toBeVisible();
  return facts;
}
