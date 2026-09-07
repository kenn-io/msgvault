import type { Page, Route } from '@playwright/test';
import { readFileSync } from 'node:fs';
import type {
  OperationLaneStatus,
  OperationLaneStatusRelatedStatus,
  OperationLaneStatusSupportedActionsItem as OperationAction,
  OperationRunsResponse,
  OperationRunDetail,
  OperationRunSummary,
  OperationRunSummaryKind as OperationKind,
  OperationRunSummaryLane as OperationLane,
  OperationStatusResponse,
} from '../../../src/lib/api/generated/models';
type OperationState = OperationRunSummary['state'];

// The Go contract test projects every checked run through the production
// validator/serializer. Browser routes only select from that public output.
const operationPublicFixture = JSON.parse(readFileSync(
  new URL('./operations-public.json', import.meta.url), 'utf8'
)) as unknown;
const operationPrivateSentinels = JSON.parse(readFileSync(
  new URL('./operations-private-sentinels.json', import.meta.url), 'utf8'
)) as Record<string, string>;
const checkedPublicFixture = operationPublicFixture as unknown as OperationRunsResponse;
const sourceRun = fixtureRun('source_sync');
const cardDAVRun = fixtureRun('carddav_sync');
const documentRun = fixtureRun('document_extraction');
const visualRun = fixtureRun('visual_embedding');
const sweepRun = fixtureRun('person_sweep');

export const OPERATION_REFERENCES = {
  source: sourceRun.id,
  carddav: cardDAVRun.id,
  document: documentRun.id,
  visual: visualRun.id,
  sweep: sweepRun.id
} as const;

export const OPERATION_PRIVACY_SENTINELS = Object.values(operationPrivateSentinels);

const firstPage = [sourceRun, cardDAVRun, documentRun, visualRun];
const secondPage = [sweepRun];

export type InstalledOperations = {
  actionRequests: OperationAction[];
  listQueries: URLSearchParams[];
  statusRequests: string[];
  conflictNextCardDAV(): void;
  driftNextPage(): void;
  failNextDocumentStatus(): void;
  failNextHistory(): void;
  rotateReferencesOnNextRefresh(): void;
  setOperationConfigured(kind: 'document_extraction' | 'visual_embedding', configured: boolean): void;
};

export async function installOperations(page: Page): Promise<InstalledOperations> {
  const actionRequests: OperationAction[] = [];
  const listQueries: URLSearchParams[] = [];
  const statusRequests: string[] = [];
  let cardDAVConflict = false;
  let pageDrift = false;
  let documentStatusFailure = false;
  let documentConfigured = true;
  let historyFailure = false;
  let referencesRotated = false;
  let rotateOnNextRefresh = false;
  let visualConfigured = true;

  await page.route('**/api/session', sessionRoute);
  await page.route('**/api/v1/settings', (route) => route.fulfill({ json: {
    settings: [
      { key: 'web.theme', group: 'web', kind: 'string', value: { string: 'light' }, restart_required: false },
      { key: 'web.density', group: 'web', kind: 'string', value: { string: 'compact' }, restart_required: false }
    ],
    pending_restart: false
  } }));
  await page.route('**/api/v1/explore', (route) => route.fulfill({ json: {
    rows: [], total_count: 0, cache_revision: 'operations-fixture', search_provenance: {}
  } }));
  await page.route('**/api/v1/sources/status', (route) => route.fulfill({ json: { sources: [] } }));
  await page.route('**/api/v1/carddav/status', (route) => route.fulfill({ json: {
    configured: true, available: true, credential_configured: true,
    enabled: true, scheduled: false, schedule: ''
  } }));
  await page.route('**/api/v1/carddav/books', (route) => route.fulfill({ json: { books: [] } }));
  await page.route('**/api/v1/carddav/conflicts**', (route) => route.fulfill({ json: { conflicts: [] } }));
  await page.route('**/api/v1/carddav/runs**', (route) => route.fulfill({ json: { runs: [] } }));
  await page.route('**/api/v1/documents/status/current', (route) => {
    statusRequests.push('/api/v1/documents/status/current');
    if (documentStatusFailure) {
      documentStatusFailure = false;
      return fulfillOperation(route, {
        error: 'document_status_unavailable', message: 'Document status is unavailable.'
      }, 503);
    }
    return route.fulfill({ json: {
      status: {
        profile_exists: true, profile_enabled: true, exact_consent: true,
        ready_owners: 4, eligible_owners: 5, missing_owners: 1, retry_owners: 0,
        terminal_owners: 0, stored_plaintext_chunks: 12, provider_requests: 2
      }
    } });
  });
  await page.route('**/api/v1/documents/vectors/status', (route) => {
    statusRequests.push('/api/v1/documents/vectors/status');
    return route.fulfill({ json: {
      enabled: true, configured: true, status: { coverage: { ready: 7, required: 9 } }
    } });
  });
  await page.route('**/api/v1/multimodal/status', (route) => {
    statusRequests.push('/api/v1/multimodal/status');
    return route.fulfill({ json: {
      current: 8, eligible: 10, retryable: 1, terminal: 0, unavailable: 1,
      active_leases: 0, journal_lag: 2, reconciliation_complete: false
    } });
  });

  await page.route('**/api/v1/operations/status', (route) =>
    fulfillOperation(route, statusResponse(referencesRotated, documentConfigured, visualConfigured)));
  await page.route('**/api/v1/operations/runs**', async (route) => {
    const url = new URL(route.request().url());
    const detailID = decodeURIComponent(url.pathname.slice('/api/v1/operations/runs/'.length));
    if (url.pathname !== '/api/v1/operations/runs') {
      const detail = detailFor(detailID);
      return detail
        ? fulfillOperation(route, detail)
        : fulfillOperation(route, { error: 'operation_not_found', message: 'Operation run was not found.' }, 404);
    }

    listQueries.push(new URLSearchParams(url.searchParams));
    if (historyFailure) {
      historyFailure = false;
      return fulfillOperation(route, {
        error: 'operation_history_unavailable', message: 'Operation history is unavailable.'
      }, 503);
    }
    if (url.searchParams.has('cursor') && pageDrift) {
      pageDrift = false;
      return fulfillOperation(route, {
        error: 'operation_history_conflict', message: 'Operation history changed.'
      }, 409);
    }

    if (!url.searchParams.has('cursor') && rotateOnNextRefresh) {
      referencesRotated = true;
      rotateOnNextRefresh = false;
    }
    const visibleFirstPage = referencesRotated ? firstPage.map(rotatedRun) : firstPage;
    const visibleSecondPage = referencesRotated ? secondPage.map(rotatedRun) : secondPage;
    const requested = url.searchParams.has('cursor')
      ? visibleSecondPage
      : filtered(visibleFirstPage, url.searchParams);
    return fulfillOperation(route, {
      runs: requested,
      unavailable_kinds: checkedPublicFixture.unavailable_kinds,
      membership_revision: checkedPublicFixture.membership_revision,
      ...(url.searchParams.has('cursor') || requested.length === 0 ? {} : { next_cursor: 'op2.fixture.page-two' })
    } satisfies OperationRunsResponse);
  });

  await page.route('**/api/v1/carddav/sync', (route) => {
    actionRequests.push('carddav_sync');
    if (cardDAVConflict) {
      cardDAVConflict = false;
      return fulfillOperation(route, {
        error: 'operation_conflict', message: 'The operation state changed.'
      }, 409);
    }
    return fulfillOperation(route, { status: 'accepted' }, 202);
  });
  await page.route('**/api/v1/multimodal/build', (route) => {
    actionRequests.push('visual_build');
    return fulfillOperation(route, { status: 'accepted' }, 202);
  });
  await page.route('**/api/v1/multimodal/run', (route) => {
    actionRequests.push('visual_resume');
    return fulfillOperation(route, { status: 'accepted' }, 202);
  });

  return {
    actionRequests,
    listQueries,
    statusRequests,
    conflictNextCardDAV(): void { cardDAVConflict = true; },
    driftNextPage(): void { pageDrift = true; },
    failNextDocumentStatus(): void { documentStatusFailure = true; },
    failNextHistory(): void { historyFailure = true; },
    rotateReferencesOnNextRefresh(): void { rotateOnNextRefresh = true; },
    setOperationConfigured(kind, configured): void {
      if (kind === 'document_extraction') documentConfigured = configured;
      else visualConfigured = configured;
    }
  };
}

function fixtureRun(kind: OperationKind): OperationRunSummary {
  const match = checkedPublicFixture.runs.find((candidate) => candidate.kind === kind);
  if (!match) throw new Error(`missing checked Operations fixture for ${kind}`);
  return match;
}

function detailFor(id: string): OperationRunDetail | undefined {
  const original = [...firstPage, ...secondPage].find((candidate) =>
    candidate.id === id || rotatedReference(candidate.id) === id);
  const runByID = original && id === rotatedReference(original.id) ? rotatedRun(original) : original;
  if (!runByID) return undefined;
  const related = {
    source_sync: 'listSourceStatus',
    carddav_sync: 'getCardDAVStatus',
    document_extraction: 'getDocumentIndexStatus',
    document_embedding: 'getDocumentVectorStatus',
    visual_embedding: 'getVisualAttachmentStatus'
  } as const;
  return {
    ...runByID,
    supported_actions: runByID.kind === 'carddav_sync'
      ? ['carddav_sync']
      : runByID.kind === 'visual_embedding'
        ? ['visual_resume']
        : [],
    ...(runByID.kind in related
      ? { related_status: related[runByID.kind as keyof typeof related] }
      : {})
  };
}

function statusResponse(
  rotated = false,
  documentConfigured = true,
  visualConfigured = true
): OperationStatusResponse {
  const visible = (run: OperationRunSummary) => rotated ? rotatedRun(run) : run;
  return {
    lanes: [
      lane('messages', 'source_sync', visible(sourceRun), 'listSourceStatus'),
      lane('messages', 'message_embedding'),
      lane('person_facts', 'person_sweep', visible(sweepRun)),
      lane('person_facts', 'person_embedding', undefined, undefined, [], 'unavailable'),
      lane('person_facts', 'person_enrichment'),
      lane('contacts', 'carddav_sync', visible(cardDAVRun), 'getCardDAVStatus', ['carddav_sync']),
      lane('documents', 'document_extraction', visible(documentRun), 'getDocumentIndexStatus', [], 'available', documentConfigured),
      lane('documents', 'document_embedding', undefined, 'getDocumentVectorStatus'),
      lane('visual_attachments', 'visual_embedding', visible(visualRun), 'getVisualAttachmentStatus', ['visual_resume'], 'available', visualConfigured)
    ]
  };
}

function rotatedReference(id: string): string {
  return `${id}_rotated`;
}

function rotatedRun(run: OperationRunSummary): OperationRunSummary {
  return { ...run, id: rotatedReference(run.id) };
}

function lane(
  laneName: OperationLane,
  kind: OperationKind,
  latest?: OperationRunSummary,
  related_status?: OperationLaneStatusRelatedStatus,
  supported_actions: OperationAction[] = [],
  history_availability: 'available' | 'unavailable' = 'available',
  configured = kind !== 'message_embedding'
): OperationLaneStatus {
  return {
    lane: laneName,
    kind,
    configured,
    history_availability,
    supported_actions,
    ...(latest ? { latest, latest_successful: latest.state === 'succeeded' ? latest : undefined } : {}),
    ...(latest?.state === 'running' ? { active: latest } : {}),
    ...(related_status ? { related_status } : {}),
    ...(history_availability === 'unavailable' ? { unavailable_code: 'adapter_unavailable' } : {})
  };
}

function filtered(runs: OperationRunSummary[], query: URLSearchParams): OperationRunSummary[] {
  const lane = query.get('lane') as OperationLane | null;
  const kind = query.get('kind') as OperationKind | null;
  const state = query.get('state') as OperationState | null;
  const from = query.get('started_from');
  const before = query.get('started_before');
  return runs.filter((candidate) =>
    (!lane || candidate.lane === lane) &&
    (!kind || candidate.kind === kind) &&
    (!state || candidate.state === state) &&
    (!from || candidate.started_at >= from) &&
    (!before || candidate.started_at < before)
  );
}

function fulfillOperation(
  route: Route,
  body: unknown,
  status = 200
) {
  return route.fulfill({ status, json: body });
}

function sessionRoute(route: Route) {
  return route.fulfill({ json: {
    auth_mode: 'session', csrf_token: 'synthetic-csrf', https: true, plain_http_warning: false
  } });
}
