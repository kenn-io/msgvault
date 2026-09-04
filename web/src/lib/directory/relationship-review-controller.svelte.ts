import type { APIClient } from '../api/client';
import type { components } from '../api/generated/schema';
import type { RelationshipReviewState } from '../explore/models';

export type { RelationshipReviewState } from '../explore/models';
type GeneratedReview = components['schemas']['RelationshipReview'];
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

type RelationshipReviewCommit = (patch: { relationshipReviewState?: RelationshipReviewState }) => void;

export class RelationshipReviewController {
  active = $state(false);
  state = $state<RelationshipReviewState>('pending');
  rows = $state<RelationshipReviewRow[]>([]);
  loading = $state(false);
  error = $state<string | null>(null);
  lastSuccessfulState = $state<RelationshipReviewState | null>(null);

  private readonly client: APIClient;
  private readonly commit: RelationshipReviewCommit;
  private requestAbort: AbortController | undefined;
  private requestGeneration = 0;
  private contextGeneration = $state(0);
  private disposed = false;

  constructor(client: APIClient, commit: RelationshipReviewCommit = () => undefined) {
    this.client = client;
    this.commit = commit;
  }

  get contextToken(): number { return this.contextGeneration; }

  isContextCurrent(token: number): boolean {
    return !this.disposed && this.active && this.contextGeneration === token;
  }

  applyContext(active: boolean, state: RelationshipReviewState, historyRestoration: boolean): void {
    if (this.disposed) return;
    const changed = this.active !== active || this.state !== state;
    if (!changed && !historyRestoration) return;
    this.replaceContext(active, state);
    if (active) void this.load();
  }

  setState(state: RelationshipReviewState): void {
    if (this.disposed || !this.active || this.state === state) return;
    this.commit({ relationshipReviewState: state });
    this.replaceContext(true, state);
    void this.load();
  }

  async load(): Promise<boolean> {
    if (this.disposed || !this.active) return false;
    this.requestAbort?.abort();
    const abort = new AbortController();
    this.requestAbort = abort;
    const request = ++this.requestGeneration;
    const context = this.contextGeneration;
    const state = this.state;
    this.loading = true;
    this.error = null;
    try {
      const response = await this.client.GET('/api/v1/person-relationship-reviews', {
        params: { query: { status: state } },
        signal: abort.signal
      });
      if (!this.ownsRequest(abort, request, context, state)) return false;
      if (response.data) {
        const rows = safeRows(response.data.reviews, state);
        if (!rows) {
          this.error = 'Unable to load imported relationship reviews.';
          return false;
        }
        this.rows = rows;
        this.lastSuccessfulState = state;
        return true;
      }
      this.error = response.response.status === 400 && errorCode(response.error) === 'invalid_status'
        ? 'The imported relationship review state is invalid.'
        : 'Unable to load imported relationship reviews.';
      return false;
    } catch {
      if (!this.ownsRequest(abort, request, context, state)) return false;
      this.error = 'Unable to load imported relationship reviews.';
      return false;
    } finally {
      if (this.ownsRequest(abort, request, context, state)) {
        this.requestAbort = undefined;
        this.loading = false;
      }
    }
  }

  async retry(): Promise<void> {
    if (this.disposed || !this.active || this.loading || !this.error || this.rows.length > 0) return;
    await this.load();
  }

  destroy(): void {
    if (this.disposed) return;
    this.disposed = true;
    this.requestAbort?.abort();
    this.requestAbort = undefined;
    ++this.requestGeneration;
    ++this.contextGeneration;
    this.active = false;
    this.rows = [];
    this.loading = false;
    this.error = null;
    this.lastSuccessfulState = null;
  }

  private replaceContext(active: boolean, state: RelationshipReviewState): void {
    this.requestAbort?.abort();
    this.requestAbort = undefined;
    ++this.requestGeneration;
    ++this.contextGeneration;
    this.active = active;
    this.state = state;
    this.rows = [];
    this.loading = false;
    this.error = null;
    this.lastSuccessfulState = null;
  }

  private ownsRequest(
    abort: AbortController,
    request: number,
    context: number,
    state: RelationshipReviewState
  ): boolean {
    return !this.disposed && this.active && !abort.signal.aborted && this.requestAbort === abort &&
      this.requestGeneration === request && this.contextGeneration === context && this.state === state;
  }
}

function safeRows(
  values: GeneratedReview[] | null,
  selectedState: RelationshipReviewState
): RelationshipReviewRow[] | undefined {
  if (values === null) return [];
  const rows: RelationshipReviewRow[] = [];
  for (const value of values) {
    if (!positiveID(value.id) || !positiveID(value.person_id) ||
      typeof value.raw_related_value !== 'string' || typeof value.raw_related_type !== 'string' ||
      typeof value.value_kind !== 'string' || value.status !== selectedState ||
      typeof value.source !== 'string' || typeof value.created_at !== 'string' ||
      typeof value.updated_at !== 'string' ||
      (value.reviewed_at !== undefined && typeof value.reviewed_at !== 'string')) return undefined;
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
      ...(value.reviewed_at !== undefined ? { reviewed_at: value.reviewed_at } : {})
    });
  }
  return rows;
}

function positiveID(value: number | undefined): value is number {
  return value !== undefined && Number.isSafeInteger(value) && value > 0;
}

function errorCode(error: unknown): string | undefined {
  return typeof error === 'object' && error !== null && 'error' in error && typeof error.error === 'string'
    ? error.error
    : undefined;
}
