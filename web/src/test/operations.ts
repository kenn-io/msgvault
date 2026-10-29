import type { OperationLaneStatus } from '../lib/api/generated/models';

export function lane(kind: OperationLaneStatus['kind'], extra: Partial<OperationLaneStatus> = {}): OperationLaneStatus {
  const lanes = {
    carddav_sync: 'contacts', document_embedding: 'documents', document_extraction: 'documents',
    message_embedding: 'messages', person_embedding: 'person_facts', person_enrichment: 'person_facts',
    person_sweep: 'person_facts', source_sync: 'messages', visual_embedding: 'visual_attachments'
  } as const;
  return { kind, configured: true, lane: lanes[kind], history_availability: 'available', supported_actions: [], ...extra } as OperationLaneStatus;
}
