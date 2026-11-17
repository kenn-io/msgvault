import { requestOf } from './entity-fixtures.test-support';
import { describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../api/client';
import { claimValue, FACT_LEDGER_PAGE_LIMIT, FactLedgerController } from './fact-ledger-controller.svelte';

const revision = `sha256:${'a'.repeat(64)}`;

function deferredResponse() {
  let resolve!: (response: Response) => void;
  const promise = new Promise<Response>((next) => { resolve = next; });
  return { promise, resolve };
}

function payload(path: string, marker = 'safe') {
  if (path.endsWith('/person-fact-targets')) return { fingerprint: 'private-fingerprint', version: 'private-version', targets: [{
    description: 'Email address', kind: 'attribute', key: 'work:email', revision,
    value_type: 'text', cardinality: 'single', sensitive: true, choices: null, fields: null,
    slug: 'private-slug', universal_id: 'private-universal-id'
  }] };
  if (path.endsWith('/fact-evidence')) return { evidence: [{
    id: 1, evidence_key: 'private-evidence-key', authority: 'ordinary', directness: 'direct-self',
    identity_score: 950, source_class: 'archive', event_time: '2026-08-01T00:00:00Z',
    recorded_time: '2026-08-02T00:00:00Z', created_at: '2026-08-03T00:00:00Z', supported: true,
    excerpt: `allowed excerpt ${marker}`, latest_status: { id: 2, evidence_key: 'private-evidence-key', generation_id: 3,
      source_version: 'private-source-version', supported: true, reason: 'source-reimported', created_at: '2026-08-03T00:00:00Z' },
    content_sha256: 'private-sha', source_ref: 'private-ref', source_url: 'private-url', source_version: 'private-version',
    span_start: 1, span_end: 2, subject_person_id: 99, subject_ref: 'private-subject'
  }] };
  if (path.endsWith('/fact-claims')) return { claims: [{
    id: 4, claim_key: 'private-claim-key', generation_id: 5, evidence_ids: [1], target: { kind: 'attribute', key: 'work:email', revision },
    submitted_value: JSON.stringify(`allowed value ${marker}`), normalized_value: JSON.stringify(`allowed normalized ${marker}`), relation: 'support', origin: 'extraction',
    valid_from: '2026-01-01T00:00:00Z', valid_until: null, confidence: { reported_score: 80 }, created_at: '2026-08-03T00:00:00Z',
    program_fingerprint: 'private-program-fingerprint', program_id: 'private-program', program_version: 'private-program-version',
    value_fingerprint: 'private-value-fingerprint'
  }] };
  if (path.endsWith('/fact-decisions')) return { decisions: [{
    id: 6, decision_key: 'private-decision-key', claim_key: 'private-claim-key', claim_value: JSON.stringify(`allowed value ${marker}`), competing_claim_key: 'private-competing-key',
    resolution_id: 7, target: { kind: 'attribute', key: 'work:email', revision }, outcome: 'saved', resolver_version: 'person-fact-resolver-v1', action: 'applied', reason: 'applied-projection', projection: { kind: 'person_attribute', row_id: 8 },
    score: { authority: 100, confidence: 8, corroboration: 0, directness: 400, freshness: 100, source_class: 300, total: 908 },
    created_at: '2026-08-03T00:00:00Z'
  }] };
  if (path.endsWith('/fact-pins')) return { pins: [{ actor: 'private-actor', event_id: 9, pinned: true, target: { kind: 'attribute', key: 'work:email', revision } }] };
  if (path.endsWith('/fact-evidence-status-events')) return { events: [{ id: 10, evidence_key: 'private-evidence-key', generation_id: 11,
    source_version: 'private-source-version', supported: false, reason: 'source-deleted', created_at: '2026-08-04T00:00:00Z' }] };
  throw new Error(`unexpected path ${path}`);
}

describe('FactLedgerController', () => {
  it('decodes serialized claim values and keeps structured values intact', () => {
    expect(claimValue('"New York"', 'attribute')).toEqual({ parsedValue: 'New York', displayValue: 'New York' });
    expect(claimValue('9007199254740993', 'attribute').displayValue).toBe('9007199254740993');
    expect(claimValue('{"title":"Engineer","organization":{"name":"Acme"}}', 'employment')).toEqual({
      parsedValue: { title: 'Engineer', organization: { name: 'Acme' } }, displayValue: 'Engineer at Acme'
    });
  });

  it('projects a decision value when the claims page does not contain its claim', async () => {
    const fetchFn = vi.fn<typeof fetch>(async input => {
      const path = new URL(requestOf(input).url).pathname;
      return Response.json(path.endsWith('/fact-claims') ? { claims: [] } : payload(path));
    });
    const controller = new FactLedgerController(createAPIClient(fetchFn));
    controller.applyContext(true, 42);

    await vi.waitFor(() => expect(controller.initialLoading).toBe(false));

    expect(controller.claims.rows).toEqual([]);
    expect(controller.decisions.rows[0]?.displayValue).toBe('allowed value safe');
    controller.destroy();
  });

  it('stays network silent without a durable person and issues only five exact initial reads for one', async () => {
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input); requests.push(request);
      return Response.json(payload(new URL(request.url).pathname));
    });
    const controller = new FactLedgerController(createAPIClient(fetchFn));

    controller.applyContext(true, null, false);
    expect(requests).toHaveLength(0);

    controller.applyContext(true, 42, false);
    await vi.waitFor(() => expect(controller.initialLoading).toBe(false));
    expect(requests.map((request) => new URL(request.url).pathname).sort()).toEqual([
      '/api/v1/people/42/fact-claims', '/api/v1/people/42/fact-decisions',
      '/api/v1/people/42/fact-evidence', '/api/v1/people/42/fact-pins',
      '/api/v1/person-fact-targets'
    ]);
    expect(new URL(requests[0]!.url).searchParams.get('include_sensitive')).toBe('true');
    for (const request of requests.filter((item) => new URL(item.url).pathname !== '/api/v1/person-fact-targets' && !item.url.endsWith('/fact-pins'))) {
      const query = new URL(request.url).searchParams;
      expect(query.get('limit')).toBe('50');
      expect(query.get('offset')).toBe('0');
      expect(query.has('target')).toBe(false);
    }
  });

  it('shows the current support status for evidence with a deleted source', async () => {
    const fetchFn = vi.fn<typeof fetch>(async input => {
      const path = new URL(requestOf(input).url).pathname;
      if (!path.endsWith('/fact-evidence')) return Response.json(payload(path));
      const supported = payload(path).evidence![0]!;
      return Response.json({ evidence: [supported, { ...supported, id: 2, evidence_key: 'deleted', supported: false, latest_status: { ...supported.latest_status, evidence_key: 'deleted', supported: false, reason: 'source-deleted' } }] });
    });
    const controller = new FactLedgerController(createAPIClient(fetchFn));
    controller.applyContext(true, 42);
    await vi.waitFor(() => expect(controller.evidence.rows).toHaveLength(2));
    expect(controller.evidence.rows.map(row => row.currentSupportLabel)).toEqual(['Supported — Source reimported', 'Unsupported — Source deleted']);
    controller.destroy();
  });

  it('clears stale pins when a refresh fails after a successful read', async () => {
    let failPins = false;
    const fetchFn = vi.fn<typeof fetch>(async input => {
      const path = new URL(requestOf(input).url).pathname;
      if (path.endsWith('/fact-pins') && failPins) throw new Error('unavailable');
      return Response.json(payload(path));
    });
    const controller = new FactLedgerController(createAPIClient(fetchFn));
    controller.applyContext(true, 42);
    await vi.waitFor(() => expect(controller.pins.rows).toHaveLength(1));
    failPins = true;
    await controller.retrySection('pins');
    expect(controller.pins.error).toBe("Couldn't load pins.");
    expect(controller.pins.rows).toEqual([]);
    controller.destroy();
  });

  it('clears and aborts all lanes on same-person restoration and discards every late result', async () => {
    const deferred = Array.from({ length: 5 }, () => deferredResponse());
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input); requests.push(request);
      return requests.length <= 5 ? deferred[requests.length - 1]!.promise : Response.json(payload(new URL(request.url).pathname, 'fresh'));
    });
    const controller = new FactLedgerController(createAPIClient(fetchFn));
    controller.applyContext(true, 42, false);
    await vi.waitFor(() => expect(requests).toHaveLength(5));

    controller.applyContext(true, 42, true);
    expect(controller.evidence.rows).toEqual([]);
    expect(controller.claims.rows).toEqual([]);
    expect(controller.historyOpen).toBe(false);
    expect(requests.slice(0, 5).every((request) => request.signal.aborted)).toBe(true);
    await vi.waitFor(() => expect(requests).toHaveLength(10));
    for (let index = 0; index < 5; index += 1) deferred[index]!.resolve(Response.json(payload(new URL(requests[index]!.url).pathname, 'stale')));
    await vi.waitFor(() => expect(controller.initialLoading).toBe(false));
    expect(controller.evidence.rows[0]?.excerpt).toBe('allowed excerpt fresh');
    expect(controller.claims.rows[0]?.displayValue).toBe('allowed value fresh');
  });

  it('keeps independent successful lanes usable and retries only the failed lane with bounded errors', async () => {
    const paths: string[] = [];
    let claimsAttempts = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const path = new URL(requestOf(input).url).pathname; paths.push(path);
      if (path.endsWith('/fact-claims') && ++claimsAttempts === 1) {
        return Response.json({ message: 'private-error-detail' }, { status: 503 });
      }
      return Response.json(payload(path));
    });
    const controller = new FactLedgerController(createAPIClient(fetchFn));
    controller.applyContext(true, 42, false);
    await vi.waitFor(() => expect(controller.initialLoading).toBe(false));

    expect(controller.claims.rows).toEqual([]);
    expect(controller.claims.error).toBe("Couldn't load claims.");
    expect(controller.evidence.rows).toHaveLength(1);
    await controller.retrySection('claims');
    expect(controller.claims.rows).toHaveLength(1);
    expect(paths.filter((path) => path.endsWith('/fact-claims'))).toHaveLength(2);
    expect(paths.filter((path) => path.endsWith('/fact-evidence'))).toHaveLength(1);
  });

  it('retains a full current page on failure and empty end, then retries the exact filtered offset', async () => {
    const target = `attribute:work:email:${revision}`;
    const requests: Request[] = [];
    let laterAttempt = 0;
    const fifty = Array.from({ length: FACT_LEDGER_PAGE_LIMIT }, (_, index) => ({ ...payload('/fact-evidence').evidence![0], id: index + 1, evidence_key: `key-${index}` }));
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input); requests.push(request);
      const url = new URL(request.url);
      if (url.pathname.endsWith('/fact-evidence')) {
        const offset = Number(url.searchParams.get('offset'));
        if (offset === 0) return Response.json({ evidence: fifty });
        laterAttempt += 1;
        if (laterAttempt === 1) return Response.json({ message: 'private error' }, { status: 503 });
        return Response.json({ evidence: [] });
      }
      return Response.json(payload(url.pathname));
    });
    const controller = new FactLedgerController(createAPIClient(fetchFn));
    controller.applyContext(true, 42, false);
    await vi.waitFor(() => expect(controller.initialLoading).toBe(false));
    await controller.selectTarget('target-0');
    await vi.waitFor(() => expect(controller.evidence.rows).toHaveLength(50));

    await controller.nextPage('evidence');
    expect(controller.evidence.rows).toHaveLength(50);
    expect(controller.evidence.offset).toBe(0);
    expect(controller.evidence.pageError).toBe("Couldn't load that page of evidence.");
    await controller.retrySection('evidence');
    expect(controller.evidence.rows).toHaveLength(50);
    expect(controller.evidence.offset).toBe(0);
    expect(controller.evidence.endReached).toBe(true);
    expect(controller.focusRequest?.key).toBe('evidence-next');
    const later = requests.filter((request) => new URL(request.url).pathname.endsWith('/fact-evidence')).slice(-2);
    for (const request of later) expect(Object.fromEntries(new URL(request.url).searchParams)).toEqual({ target, limit: '50', offset: '50' });
  });

  it('loads evidence history only on explicit inspection and pages with the immutable key', async () => {
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input); requests.push(request);
      return Response.json(payload(new URL(request.url).pathname));
    });
    const controller = new FactLedgerController(createAPIClient(fetchFn));
    controller.applyContext(true, 42, false);
    await vi.waitFor(() => expect(controller.initialLoading).toBe(false));
    expect(requests.some((request) => request.url.includes('fact-evidence-status-events'))).toBe(false);

    await controller.openEvidenceHistory(controller.evidence.rows[0]!.rowToken);
    const request = requests.at(-1)!;
    expect(new URL(request.url).pathname).toBe('/api/v1/people/42/fact-evidence-status-events');
    expect(Object.fromEntries(new URL(request.url).searchParams)).toEqual({ evidence_key: 'private-evidence-key', limit: '50', offset: '0' });
    expect(controller.history.rows).toEqual([{ supported: false, reasonLabel: 'Source deleted', createdAt: '2026-08-04T00:00:00Z' }]);
  });

  it('closes and aborts deferred history before a replacement person can receive its result', async () => {
    const history = deferredResponse();
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input); requests.push(request);
      const path = new URL(request.url).pathname;
      if (path.endsWith('/fact-evidence-status-events')) return history.promise;
      return Response.json(payload(path));
    });
    const controller = new FactLedgerController(createAPIClient(fetchFn));
    controller.applyContext(true, 42, false);
    await vi.waitFor(() => expect(controller.initialLoading).toBe(false));

    const staleLoad = controller.openEvidenceHistory(controller.evidence.rows[0]!.rowToken);
    await vi.waitFor(() => expect(controller.history.loading).toBe(true));
    const historyRequest = requests.at(-1)!;
    controller.applyContext(true, 43, false);

    expect(historyRequest.signal.aborted).toBe(true);
    expect(controller.historyOpen).toBe(false);
    expect(controller.history.rows).toEqual([]);
    expect(controller.revealedEvidence.size).toBe(0);
    history.resolve(Response.json(payload('/fact-evidence-status-events')));
    await staleLoad;
    await vi.waitFor(() => expect(controller.initialLoading).toBe(false));
    expect(controller.personID).toBe(43);
    expect(controller.historyOpen).toBe(false);
    expect(controller.history.rows).toEqual([]);
  });

  it('retains support history on page failure and retries the exact attempted offset', async () => {
    const historyOffsets: string[] = [];
    let laterAttempt = 0;
    const events = Array.from({ length: FACT_LEDGER_PAGE_LIMIT }, (_, index) => ({
      id: index + 1, evidence_key: 'private-evidence-key', generation_id: 2,
      source_version: null, supported: true, reason: 'scope-relinked', created_at: `2026-08-${String(index + 1).padStart(2, '0')}`
    }));
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      const url = new URL(request.url);
      if (url.pathname.endsWith('/fact-evidence-status-events')) {
        historyOffsets.push(url.searchParams.get('offset') ?? '');
        if (url.searchParams.get('offset') === '0') return Response.json({ events });
        laterAttempt += 1;
        if (laterAttempt === 1) return Response.json({ message: 'private-error' }, { status: 503 });
        return Response.json({ events: [events[0]] });
      }
      return Response.json(payload(url.pathname));
    });
    const controller = new FactLedgerController(createAPIClient(fetchFn));
    controller.applyContext(true, 42, false);
    await vi.waitFor(() => expect(controller.initialLoading).toBe(false));
    await controller.openEvidenceHistory(controller.evidence.rows[0]!.rowToken);

    expect(controller.hasHistoryNext).toBe(true);
    await controller.nextHistoryPage();
    expect(controller.history.rows).toHaveLength(50);
    expect(controller.history.offset).toBe(0);
    expect(controller.history.pageError).toBe('Unable to load the requested support-history page.');
    await controller.retryHistory();
    expect(historyOffsets).toEqual(['0', '50', '50']);
    expect(controller.history.offset).toBe(50);
    expect(controller.history.rows).toHaveLength(1);
    await controller.firstHistoryPage();
    expect(historyOffsets).toEqual(['0', '50', '50', '0']);
    expect(controller.history.offset).toBe(0);
  });
  it('keeps claim_key through retry until navigation', async () => {
    const requests: URL[] = [];
    let fail = false;
    const fetchFn = vi.fn<typeof fetch>(async input => {
      const url = new URL(requestOf(input).url);
      requests.push(url);
      if (url.searchParams.has('claim_key') && fail) {
        fail = false;
        return new Response(null, { status: 503 });
      }
      const data = payload(url.pathname);
      if (url.searchParams.has('claim_key')) {
        const rows = data.decisions!;
        return Response.json({ decisions: url.searchParams.get('offset') === '0' ? Array.from({ length: 50 }, () => rows[0]) : [rows[0]] });
      }
      return Response.json(data);
    });
    const controller = new FactLedgerController(createAPIClient(fetchFn));
    controller.applyContext(true, 42);
    await vi.waitFor(() => expect(controller.initialLoading).toBe(false));
    requests.length = 0;
    const open = () => controller.showDecision('private-claim-key', `attribute:work:email:${revision}`);
    await open();
    expect(requests).toHaveLength(1);
    expect(requests[0]!.searchParams.get('claim_key')).toBe('private-claim-key');
    expect(controller.focusRequest?.key).toBe('decision:private-claim-key');
    await controller.nextPage('decisions');
    expect(controller.focusRequest).toBeNull();
    await controller.previousPage('decisions');
    fail = true;
    await controller.nextPage('decisions');
    await controller.retrySection('decisions');
    expect(requests.map(url => [url.searchParams.get('claim_key'), url.searchParams.get('offset')])).toEqual([
      ['private-claim-key', '0'], ['private-claim-key', '50'], ['private-claim-key', '0'], ['private-claim-key', '50'], ['private-claim-key', '50'],
    ]);
    expect(controller.focusRequest).toBeNull();
    await controller.showAll();
    expect(requests.at(-1)!.searchParams.has('claim_key')).toBe(false);
    await open();
    await controller.selectTarget('all');
    expect(requests.at(-1)!.searchParams.has('claim_key')).toBe(false);
    await open();
    await controller.selectSection('claims');
    expect(controller.exactFilter).toBeNull();
    controller.destroy();
  });

  it('keeps evidence_id through an exact-request retry', async () => {
    const requests: URL[] = [];
    let fail = false;
    const fetchFn = vi.fn<typeof fetch>(async input => {
      const url = new URL(requestOf(input).url);
      requests.push(url);
      if (url.searchParams.has('evidence_id') && fail) {
        fail = false;
        return new Response(null, { status: 503 });
      }
      const data = payload(url.pathname);
      if (url.searchParams.has('evidence_id')) {
        const rows = data.evidence!;
        return Response.json({ evidence: [rows[0]] });
      }
      return Response.json(data);
    });
    const controller = new FactLedgerController(createAPIClient(fetchFn));
    controller.applyContext(true, 42);
    await vi.waitFor(() => expect(controller.initialLoading).toBe(false));
    requests.length = 0;
    const open = () => controller.showEvidence(1, `attribute:work:email:${revision}`);
    await open();
    expect(requests).toHaveLength(1);
    expect(requests[0]!.searchParams.get('evidence_id')).toBe('1');
    expect(controller.focusRequest?.key).toBe('evidence:1');
    fail = true;
    await open();
    expect(controller.evidence.error).toBe("Couldn't load evidence.");
    await controller.retrySection('evidence');
    expect(requests.at(-1)!.searchParams.get('evidence_id')).toBe('1');
    expect(controller.focusRequest?.key).toBe('evidence:1');
    controller.destroy();
  });
  it('keeps a revealed claim through its decision and clears it on navigation', async () => {
    const fetchFn = vi.fn<typeof fetch>(async input => Response.json(payload(new URL(requestOf(input).url).pathname)));
    const controller = new FactLedgerController(createAPIClient(fetchFn));
    controller.applyContext(true, 42);
    await vi.waitFor(() => expect(controller.initialLoading).toBe(false));
    const key = 'private-claim-key';
    controller.revealClaim(key);
    await controller.showDecision(key, `attribute:work:email:${revision}`);
    expect(controller.revealedClaims.has(key)).toBe(true);
    await controller.selectSection('claims');
    expect(controller.revealedClaims.has(key)).toBe(false);
    controller.revealClaim(key);
    expect(controller.revealedClaims.has(key)).toBe(true);
    controller.applyContext(true, 43);
    expect(controller.revealedClaims.has(key)).toBe(false);
    controller.destroy();
  });

});
