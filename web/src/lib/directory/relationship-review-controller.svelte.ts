import { listPersonRelationshipReviews as generatedListPersonRelationshipReviews } from '../api/generated/api/api';
import type { APIClient } from '../api/client';
import type { RelationshipReview as GeneratedRelationshipReview } from '../api/generated/models';
type GeneratedReview = GeneratedRelationshipReview;
export type RelationshipReviewRow = Pick<
  GeneratedReview,
  | 'id'
  | 'person_id'
  | 'matched_person_id'
  | 'raw_related_value'
  | 'raw_related_type'
  | 'value_kind'
  | 'status'
  | 'source'
  | 'created_at'
  | 'updated_at'
  | 'reviewed_at'
>;
export class RelationshipReviewController {
  personID = $state<number>();
  rows = $state<RelationshipReviewRow[]>([]);
  loading = $state(false);
  error = $state<string | null>(null);
  private readonly client: APIClient;
  private requestAbort: AbortController | undefined;
  private requestGeneration = 0;
  private contextGeneration = $state(0);
  private disposed = false;
  constructor(client: APIClient) {
    this.client = client;
  }
  applyPerson(personID: number): void {
    if (this.disposed || this.personID === personID) return;
    this.personID = personID;
    this.replaceContext();
    void this.load();
  }
  async load(): Promise<boolean> {
    if (this.disposed || this.personID === undefined) return false;
    this.requestAbort?.abort();
    const abort = new AbortController();
    this.requestAbort = abort;
    const request = ++this.requestGeneration;
    const context = this.contextGeneration;
    this.loading = true;
    this.error = null;
    try {
      const response = await generatedListPersonRelationshipReviews(
        { person_id: this.personID },
        {
          ...this.client,
          signal: abort.signal,
        },
      );
      if (!this.ownsRequest(abort, request, context)) return false;
      if (response.data) {
        const rows = safeRows(response.data.reviews);
        if (!rows) {
          this.error = 'Unable to load imported relationship reviews.';
          return false;
        }
        this.rows = rows;
        return true;
      }
      this.error = 'Unable to load imported relationship reviews.';
      return false;
    } catch {
      if (!this.ownsRequest(abort, request, context)) return false;
      this.error = 'Unable to load imported relationship reviews.';
      return false;
    } finally {
      if (this.ownsRequest(abort, request, context)) {
        this.requestAbort = undefined;
        this.loading = false;
      }
    }
  }
  async retry(): Promise<void> {
    if (this.disposed || this.personID === undefined || this.loading || !this.error) return;
    await this.load();
  }
  destroy(): void {
    if (this.disposed) return;
    this.disposed = true;
    this.requestAbort?.abort();
    this.requestAbort = undefined;
    ++this.requestGeneration;
    ++this.contextGeneration;
    this.rows = [];
    this.loading = false;
    this.error = null;
  }
  private replaceContext(): void {
    this.requestAbort?.abort();
    this.requestAbort = undefined;
    ++this.requestGeneration;
    ++this.contextGeneration;
    this.rows = [];
    this.loading = false;
    this.error = null;
  }
  private ownsRequest(
    abort: AbortController,
    request: number,
    context: number,
  ): boolean {
    return (
      !this.disposed &&
      !abort.signal.aborted &&
      this.requestAbort === abort &&
      this.requestGeneration === request &&
      this.contextGeneration === context
    );
  }
}
function safeRows(
  values: GeneratedReview[] | null,
): RelationshipReviewRow[] | undefined {
  if (values === null) return [];
  const rows: RelationshipReviewRow[] = [];
  for (const value of values) {
    if (
      !positiveID(value.id) ||
      !positiveID(value.person_id) ||
      typeof value.raw_related_value !== 'string' ||
      typeof value.raw_related_type !== 'string' ||
      typeof value.value_kind !== 'string' ||
      !['pending', 'accepted', 'rejected'].includes(value.status) ||
      typeof value.source !== 'string' ||
      typeof value.created_at !== 'string' ||
      typeof value.updated_at !== 'string' ||
      (value.reviewed_at !== undefined && typeof value.reviewed_at !== 'string')
    )
      return undefined;
    rows.push({
      id: value.id,
      person_id: value.person_id,
      ...(positiveID(value.matched_person_id) ? { matched_person_id: value.matched_person_id } : {}),
      raw_related_value: value.raw_related_value,
      raw_related_type: value.raw_related_type,
      value_kind: value.value_kind,
      status: value.status,
      source: value.source,
      created_at: value.created_at,
      updated_at: value.updated_at,
      ...(value.reviewed_at !== undefined ? { reviewed_at: value.reviewed_at } : {}),
    });
  }
  return rows;
}
function positiveID(value: number | undefined): value is number {
  return value !== undefined && Number.isSafeInteger(value) && value > 0;
}