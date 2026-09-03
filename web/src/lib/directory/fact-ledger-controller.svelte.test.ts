import { describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../api/client';
import { evidenceStatusReasonLabel, FACT_LEDGER_PAGE_LIMIT, FactLedgerController } from './fact-ledger-controller.svelte';

const revision = `sha256:${'a'.repeat(64)}`;

function requestOf(input: RequestInfo | URL): Request {
  return input instanceof Request ? input : new Request(input);
}

function deferredResponse() {
  let resolve!: (response: Response) => void;
  const promise = new Promise<Response>((next) => { resolve = next; });
  return { promise, resolve };
}

function payload(path: string, marker = 'safe') {
  if (path.endsWith('/person-fact-targets')) return { fingerprint: 'private-fingerprint', version: 'private-version', targets: [{
    description: 'Email address', kind: 'attribute', key: 'work:email', revision,
    value_type: 'string', cardinality: 'single', sensitive: true, choices: null, fields: null,
    slug: 'private-slug', universal_id: 'private-universal-id'
  }] };
  if (path.endsWith('/fact-evidence')) return { evidence: [{
    id: 1, evidence_key: 'private-evidence-key', authority: 'first party', directness: 'direct',
    identity_score: 91, source_class: 'message', event_time: '2026-08-01T00:00:00Z',
    recorded_time: '2026-08-02T00:00:00Z', created_at: '2026-08-03T00:00:00Z', supported: true,
    excerpt: `allowed excerpt ${marker}`, latest_status: { id: 2, evidence_key: 'private-evidence-key', generation_id: 3,
      source_version: 'private-source-version', supported: true, reason: 'source-reimported', created_at: '2026-08-03T00:00:00Z' },
    content_sha256: 'private-sha', source_ref: 'private-ref', source_url: 'private-url', source_version: 'private-version',
    span_start: 1, span_end: 2, subject_person_id: 99, subject_ref: 'private-subject'
  }] };
  if (path.endsWith('/fact-claims')) return { claims: [{
    id: 4, claim_key: 'private-claim-key', generation_id: 5, evidence_ids: [1], target: { kind: 'attribute', key: 'work:email', revision },
    submitted_value: `allowed value ${marker}`, normalized_value: `allowed normalized ${marker}`, relation: 'equals', origin: 'observed',
    valid_from: '2026-01-01T00:00:00Z', valid_until: null, confidence: { reported_score: 80 }, created_at: '2026-08-03T00:00:00Z',
    program_fingerprint: 'private-program-fingerprint', program_id: 'private-program', program_version: 'private-program-version',
    value_fingerprint: 'private-value-fingerprint'
  }] };
  if (path.endsWith('/fact-decisions')) return { decisions: [{
    id: 6, decision_key: 'private-decision-key', claim_key: 'private-claim-key', competing_claim_key: 'private-competing-key',
    resolution_id: 7, action: 'project', reason: 'highest score', projection: { kind: 'attribute', row_id: 8 },
    score: { authority: 1, confidence: 2, corroboration: 3, directness: 4, freshness: 5, source_class: 6, total: 21 },
    created_at: '2026-08-03T00:00:00Z'
  }] };
  if (path.endsWith('/fact-pins')) return { pins: [{ actor: 'private-actor', event_id: 9, pinned: true, target: { kind: 'attribute', key: 'work:email', revision } }] };
  if (path.endsWith('/fact-evidence-status-events')) return { events: [{ id: 10, evidence_key: 'private-evidence-key', generation_id: 11,
    source_version: 'private-source-version', supported: false, reason: 'source-deleted', created_at: '2026-08-04T00:00:00Z' }] };
  throw new Error(`unexpected path ${path}`);
}

describe('FactLedgerController', () => {
  it.each([
    ['source-deleted', 'Source deleted'], ['source-edited', 'Source edited'],
    ['scope-unlinked', 'Scope unlinked'], ['identity-reassigned', 'Identity reassigned'],
    ['source-reimported', 'Source reimported'], ['scope-relinked', 'Scope relinked'],
    ['private-unknown-reason', 'Support status changed']
  ])('maps support reason %s to bounded copy', (reason, expected) => {
    expect(evidenceStatusReasonLabel(reason)).toBe(expected);
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
    expect(controller.claims.rows[0]?.submittedValue).toBe('allowed value fresh');
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
    expect(controller.claims.error).toBe('Unable to load fact claims.');
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
    expect(controller.evidence.pageError).toBe('Unable to load the requested evidence page.');
    await controller.retrySection('evidence');
    expect(controller.evidence.rows).toHaveLength(50);
    expect(controller.evidence.offset).toBe(0);
    expect(controller.evidence.endReached).toBe(true);
    expect(controller.focusRequest?.key).toBe('evidence-next');
    const later = requests.filter((request) => new URL(request.url).pathname.endsWith('/fact-evidence')).slice(-2);
    for (const request of later) expect(Object.fromEntries(new URL(request.url).searchParams)).toEqual({ target, limit: '50', offset: '50' });
  });

  it.each(['evidence', 'claims', 'decisions'] as const)('loads the first %s page explicitly from a later offset', async (section) => {
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input); requests.push(request);
      return Response.json(payload(new URL(request.url).pathname));
    });
    const controller = new FactLedgerController(createAPIClient(fetchFn));
    controller.active = true;
    controller.personID = 42;
    controller[section].offset = 50;

    await controller.firstPage(section);

    const request = requests.at(-1)!;
    expect(new URL(request.url).pathname).toBe(`/api/v1/people/42/fact-${section}`);
    expect(Object.fromEntries(new URL(request.url).searchParams)).toEqual({ limit: '50', offset: '0' });
    expect(controller[section].offset).toBe(0);
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
});
