import type { DocumentIndexStatusResponse, EmbeddingStatus, OperationLaneStatus, Status } from '../lib/api/generated/models';

export const KIND_LANES: ReadonlyArray<readonly [OperationLaneStatus['kind'], OperationLaneStatus['lane']]> = [
  ['source_sync', 'messages'],
  ['message_embedding', 'messages'],
  ['person_sweep', 'person_facts'],
  ['person_embedding', 'person_facts'],
  ['person_enrichment', 'person_facts'],
  ['carddav_sync', 'contacts'],
  ['document_extraction', 'documents'],
  ['document_embedding', 'documents'],
  ['visual_embedding', 'visual_attachments']
];

export const RELATED_STATUS: Partial<Record<OperationLaneStatus['kind'], OperationLaneStatus['related_status']>> = {
  source_sync: 'listSourceStatus',
  carddav_sync: 'getCardDAVStatus',
  document_extraction: 'getDocumentIndexStatus',
  document_embedding: 'getDocumentVectorStatus',
  visual_embedding: 'getVisualAttachmentStatus',
};

export function lane(kind: OperationLaneStatus['kind'], extra: Partial<OperationLaneStatus> = {}): OperationLaneStatus {
  return { kind, related_status: RELATED_STATUS[kind], configured: true, lane: KIND_LANES.find(([candidate]) => candidate === kind)![1], history_availability: 'available', supported_actions: [], ...extra };
}

export function lanes(...overrides: OperationLaneStatus[]): OperationLaneStatus[] {
  return KIND_LANES.map(([kind]) => overrides.find(candidate => candidate.kind === kind) ?? lane(kind, { configured: false }));
}

export const activeGeneration = { id: 1, dimension: 3, model: 'synthetic-model', state: 'active' };

export const embeddingStatus: EmbeddingStatus = {
  active_generation: null, generation: activeGeneration, current: 0, eligible: 10, pending: 10, failed: 0,
  coverage_sampled_at: '', sampled_at: '', diagnostics: null, diagnostics_stale: false,
  eta_seconds: null, messages_per_minute: null, window_batches: 0, window_seconds: 0,
  job: { holding_scheduler_slot: false, phase: '', phase_seconds: 0, queued_at: null, started_at: null, state: 'idle' },
  scheduler: { registered: false, run_after_sync: false, schedule: '', slot_held: false, slot_held_since: null, slot_holder: '' }
};

export const visualStatus: Status = {
  active_leases: 0, converged: 0, convergence_ratio: 0, convergence_total: 10, current: 0, eligible: 10,
  duplicate_cost: { at_least_once: false, detail: '', provider_idempotent: true }, formats: [],
  generation: { ...activeGeneration, consented: true, fingerprint: 'synthetic-generation', source_fence: 1 },
  journal_cursor: 0, journal_high_water: 0, journal_lag: 0, reconciliation_complete: true,
  retryable: 0, stale: 0, terminal: 0, tombstoned: 0, unavailable: 0, unknown_role: 0,
  usage: { billed_units: 0, input_bytes: 0, requests: 0, usage_available: false }
};

export const documentStatus: DocumentIndexStatusResponse = {
  status: {
    profile_exists: true, profile_enabled: true, exact_consent: true,
    ready_owners: 4, eligible_owners: 5, missing_owners: 1, retry_owners: 0,
    terminal_owners: 0, stored_plaintext_chunks: 12, provider_requests: 2,
    average_provider_latency_millis: 0, eligible_bytes: 0, eligible_occurrences: 5,
    extraction_attempts: 4, failed_attempts: 0, failures: [], failures_exhausted: false,
    ineligible_role_occurrences: 0, missing_provider_byte_reports: 0, processed_provider_units: 0,
    provider_latency_millis: 0, provider_retries: 0, reported_provider_bytes: 0,
    staging_owners: 0, successful_attempts: 4, unknown_role_occurrences: 0, verified_upload_bytes: 0,
  },
};
