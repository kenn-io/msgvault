import { SvelteSet } from 'svelte/reactivity';

import type { APIClient } from '../api/client';
import type { components } from '../api/generated/schema';
import { encodeFactTargetRef } from './fact-target-ref';

export const FACT_LEDGER_PAGE_LIMIT = 50;
export type FactLedgerSection = 'evidence' | 'claims' | 'decisions' | 'pins';
type PagedSection = Exclude<FactLedgerSection, 'pins'>;
export type FactClaimSensitivity = 'sensitive' | 'non_sensitive' | 'unknown';

export interface FactLedgerFocusRequest {
  key: string;
  contextGeneration: number;
  nonce: number;
}

type RawEvidence = components['schemas']['PersonFactEvidence'];
type RawClaim = components['schemas']['PersonFactClaim'];
type RawDecision = components['schemas']['PersonFactDecision'];

export interface FactTargetOption {
  optionID: string;
  description: string;
  kind: string;
  valueType: string;
  cardinality: string;
  sensitive: boolean;
  readonly canonical: string;
}

export interface FactEvidenceRow {
  rowToken: string;
  sourceClass: string;
  directness: string;
  authority: string;
  identityScore: number;
  eventTime: string;
  recordedTime: string;
  supported: boolean;
  currentSupportLabel: string;
  createdAt: string;
  excerpt: string | null;
  readonly evidenceKey: string;
}

export interface FactClaimRow {
  rowToken: string;
  submittedValue: string;
  normalizedValue: string | null;
  relation: string;
  origin: string;
  validFrom: string | null;
  validUntil: string | null;
  reportedScore: number;
  createdAt: string;
  readonly targetCanonical: string | undefined;
}

export interface FactDecisionRow {
  rowToken: string;
  action: string;
  reason: string;
  score: { authority: number; confidence: number; corroboration: number; directness: number; freshness: number; sourceClass: number; total: number };
  projectionKind: string | null;
  createdAt: string;
}

export interface FactPinRow {
  rowToken: string;
  pinned: boolean;
  kind: string;
  description: string;
  readonly targetCanonical: string | undefined;
}

export interface FactEvidenceStatusRow {
  supported: boolean;
  reasonLabel: string;
  createdAt: string;
}

interface PageState<T> {
  rows: T[];
  offset: number;
  loading: boolean;
  error: string | null;
  pageError: string | null;
  retryOffset?: number;
  endReached: boolean;
}

interface SimpleState<T> { rows: T[]; loading: boolean; error: string | null }

interface Lane { abort?: AbortController; generation: number }

function pageState<T>(): PageState<T> {
  return { rows: [], offset: 0, loading: false, error: null, pageError: null, endReached: false };
}

const REASON_LABELS: Readonly<Record<string, string>> = {
  'source-deleted': 'Source deleted',
  'source-edited': 'Source edited',
  'scope-unlinked': 'Scope unlinked',
  'identity-reassigned': 'Identity reassigned',
  'source-reimported': 'Source reimported',
  'scope-relinked': 'Scope relinked'
};

export function evidenceStatusReasonLabel(reason: string): string {
  return REASON_LABELS[reason] ?? 'Support status changed';
}

export class FactLedgerController {
  active = $state(false);
  personID = $state<number | null>(null);
  selectedSection = $state<FactLedgerSection>('evidence');
  selectedTargetOption = $state('all');
  targets = $state<FactTargetOption[]>([]);
  catalogLoading = $state(false);
  catalogError = $state<string | null>(null);
  evidence = $state<PageState<FactEvidenceRow>>(pageState());
  claims = $state<PageState<FactClaimRow>>(pageState());
  decisions = $state<PageState<FactDecisionRow>>(pageState());
  pins = $state<SimpleState<FactPinRow>>({ rows: [], loading: false, error: null });
  history = $state<PageState<FactEvidenceStatusRow>>(pageState());
  historyOpen = $state(false);
  historyTriggerToken = $state<string | null>(null);
  focusRequest = $state<FactLedgerFocusRequest | null>(null);
  readonly revealedEvidence = new SvelteSet<string>();
  readonly revealedClaims = new SvelteSet<string>();

  private readonly client: APIClient;
  private disposed = false;
  private contextGeneration = 0;
  private focusNonce = 0;
  private targetByOption = new Map<string, FactTargetOption>();
  private historyEvidenceKey: string | undefined;
  private readonly lanes: Record<'catalog' | PagedSection | 'pins' | 'history', Lane> = {
    catalog: { generation: 0 }, evidence: { generation: 0 }, claims: { generation: 0 },
    decisions: { generation: 0 }, pins: { generation: 0 }, history: { generation: 0 }
  };

  constructor(client: APIClient) { this.client = client; }

  get initialLoading(): boolean {
    return this.catalogLoading || this.evidence.loading || this.claims.loading || this.decisions.loading || this.pins.loading;
  }

  get selectedTarget(): string | undefined {
    return this.targetByOption.get(this.selectedTargetOption)?.canonical;
  }

  applyContext(active: boolean, personID: number | null, historyRestoration = false): void {
    if (this.disposed) return;
    const usablePerson = Number.isSafeInteger(personID) && (personID ?? 0) > 0 ? personID : null;
    const changed = this.active !== active || this.personID !== usablePerson || (active && historyRestoration);
    if (!changed) return;
    this.active = active;
    this.personID = active ? usablePerson : null;
    this.resetContext();
    if (!this.active || this.personID === null) return;
    const generation = this.contextGeneration;
    void this.loadCatalog(generation);
    void this.loadPage('evidence', 0, generation);
    void this.loadPage('claims', 0, generation);
    void this.loadPage('decisions', 0, generation);
    void this.loadPins(generation);
  }

  async selectSection(section: FactLedgerSection): Promise<void> {
    if (this.disposed || this.selectedSection === section) return;
    this.selectedSection = section;
    this.clearDisclosureAndDialog();
    if (section === 'pins') {
      this.clearPins();
      if (this.active && this.personID !== null) await this.loadPins(this.contextGeneration);
      return;
    }
    this.clearPage(section);
    if (this.active && this.personID !== null) await this.loadPage(section, 0, this.contextGeneration);
  }

  async selectTarget(optionID: string): Promise<void> {
    if (optionID !== 'all' && !this.targetByOption.has(optionID)) return;
    if (this.selectedTargetOption === optionID) return;
    this.selectedTargetOption = optionID;
    this.clearDisclosureAndDialog();
    if (this.selectedSection === 'pins') return;
    this.clearPage(this.selectedSection);
    if (this.active && this.personID !== null) await this.loadPage(this.selectedSection, 0, this.contextGeneration);
  }

  hasPrevious(section: PagedSection): boolean { return this[section].offset > 0; }
  hasNext(section: PagedSection): boolean {
    const page = this[section];
    return !page.endReached && page.rows.length === FACT_LEDGER_PAGE_LIMIT;
  }

  async nextPage(section: PagedSection): Promise<void> {
    const page = this[section];
    if (page.loading || !this.hasNext(section)) return;
    this.clearDisclosureAndDialog();
    await this.loadPage(section, page.offset + FACT_LEDGER_PAGE_LIMIT, this.contextGeneration);
  }

  async previousPage(section: PagedSection): Promise<void> {
    const page = this[section];
    if (page.loading || !this.hasPrevious(section)) return;
    this.clearDisclosureAndDialog();
    await this.loadPage(section, Math.max(0, page.offset - FACT_LEDGER_PAGE_LIMIT), this.contextGeneration);
  }

  async firstPage(section: PagedSection): Promise<void> {
    const page = this[section];
    if (!this.active || this.personID === null || page.loading || page.offset === 0) return;
    this.clearDisclosureAndDialog();
    await this.loadPage(section, 0, this.contextGeneration);
  }

  async retrySection(section: FactLedgerSection): Promise<void> {
    if (!this.active || this.personID === null) return;
    if (section === 'pins') return this.loadPins(this.contextGeneration);
    const page = this[section];
    await this.loadPage(section, page.retryOffset ?? page.offset, this.contextGeneration);
  }

  async retryCatalog(): Promise<void> {
    if (this.active && this.personID !== null) await this.loadCatalog(this.contextGeneration);
  }

  revealEvidence(rowToken: string): void {
    if (this.evidence.rows.some((row) => row.rowToken === rowToken)) this.revealedEvidence.add(rowToken);
  }

  revealClaim(rowToken: string): void {
    if (this.claims.rows.some((row) => row.rowToken === rowToken)) this.revealedClaims.add(rowToken);
  }

  claimSensitivity(row: FactClaimRow): FactClaimSensitivity {
    if (!row.targetCanonical) return 'unknown';
    const target = this.targets.find((candidate) => candidate.canonical === row.targetCanonical);
    if (!target) return 'unknown';
    return target.sensitive ? 'sensitive' : 'non_sensitive';
  }

  async openEvidenceHistory(rowToken: string): Promise<void> {
    const row = this.evidence.rows.find((candidate) => candidate.rowToken === rowToken);
    if (!row || !this.active || this.personID === null) return;
    this.abortLane('history');
    this.history = pageState();
    this.historyOpen = true;
    this.historyTriggerToken = rowToken;
    this.historyEvidenceKey = row.evidenceKey;
    await this.loadHistory(0, this.contextGeneration);
  }

  closeEvidenceHistory(): void {
    this.abortLane('history');
    this.historyOpen = false;
    this.historyEvidenceKey = undefined;
    this.history = pageState();
    this.requestFocus(this.historyTriggerToken ? `history-trigger:${this.historyTriggerToken}` : 'evidence-heading');
    this.historyTriggerToken = null;
  }

  async nextHistoryPage(): Promise<void> {
    if (this.history.loading || !this.historyOpen || !this.hasHistoryNext) return;
    await this.loadHistory(this.history.offset + FACT_LEDGER_PAGE_LIMIT, this.contextGeneration);
  }

  async previousHistoryPage(): Promise<void> {
    if (this.history.loading || !this.historyOpen || this.history.offset === 0) return;
    await this.loadHistory(Math.max(0, this.history.offset - FACT_LEDGER_PAGE_LIMIT), this.contextGeneration);
  }

  async firstHistoryPage(): Promise<void> {
    if (this.history.loading || !this.historyOpen || this.history.offset === 0) return;
    await this.loadHistory(0, this.contextGeneration);
  }

  async retryHistory(): Promise<void> {
    if (!this.historyOpen) return;
    await this.loadHistory(this.history.retryOffset ?? this.history.offset, this.contextGeneration);
  }

  get hasHistoryNext(): boolean { return !this.history.endReached && this.history.rows.length === FACT_LEDGER_PAGE_LIMIT; }

  ownsFocusRequest(request: FactLedgerFocusRequest): boolean {
    return !this.disposed && request === this.focusRequest && request.contextGeneration === this.contextGeneration;
  }

  consumeFocusRequest(request: FactLedgerFocusRequest): void {
    if (this.ownsFocusRequest(request)) this.focusRequest = null;
  }

  destroy(): void {
    if (this.disposed) return;
    this.disposed = true;
    this.active = false;
    this.personID = null;
    this.resetContext();
  }

  private resetContext(): void {
    ++this.contextGeneration;
    for (const name of Object.keys(this.lanes) as Array<keyof typeof this.lanes>) this.abortLane(name);
    this.selectedSection = 'evidence';
    this.selectedTargetOption = 'all';
    this.targets = [];
    this.targetByOption.clear();
    this.catalogLoading = false;
    this.catalogError = null;
    this.evidence = pageState();
    this.claims = pageState();
    this.decisions = pageState();
    this.pins = { rows: [], loading: false, error: null };
    this.history = pageState();
    this.historyOpen = false;
    this.historyTriggerToken = null;
    this.historyEvidenceKey = undefined;
    this.focusRequest = null;
    this.revealedEvidence.clear();
    this.revealedClaims.clear();
  }

  private clearDisclosureAndDialog(): void {
    this.revealedEvidence.clear();
    this.revealedClaims.clear();
    if (this.historyOpen) this.closeEvidenceHistory();
    this.focusRequest = null;
  }

  private clearPage(section: PagedSection): void {
    this.abortLane(section);
    if (section === 'evidence') this.evidence = pageState();
    else if (section === 'claims') this.claims = pageState();
    else this.decisions = pageState();
  }

  private clearPins(): void {
    this.abortLane('pins');
    this.pins = { rows: [], loading: false, error: null };
  }

  private startLane(name: keyof typeof this.lanes): { abort: AbortController; generation: number } {
    this.abortLane(name);
    const lane = this.lanes[name];
    const abort = new AbortController();
    lane.abort = abort;
    return { abort, generation: ++lane.generation };
  }

  private abortLane(name: keyof typeof this.lanes): void {
    const lane = this.lanes[name];
    lane.abort?.abort();
    lane.abort = undefined;
    ++lane.generation;
  }

  private owns(name: keyof typeof this.lanes, abort: AbortController, generation: number, context: number): boolean {
    const lane = this.lanes[name];
    return !this.disposed && context === this.contextGeneration && !abort.signal.aborted && lane.abort === abort && lane.generation === generation;
  }

  private async loadCatalog(context: number): Promise<void> {
    const { abort, generation } = this.startLane('catalog');
    this.catalogLoading = true;
    this.catalogError = null;
    try {
      const response = await this.client.GET('/api/v1/person-fact-targets', {
        params: { query: { include_sensitive: true } }, signal: abort.signal
      });
      if (!this.owns('catalog', abort, generation, context)) return;
      if (!response.data) {
        this.catalogError = 'Unable to load fact targets.';
        this.targets = [];
        return;
      }
      const targets: FactTargetOption[] = [];
      for (const raw of response.data.targets ?? []) {
        const canonical = encodeFactTargetRef(raw);
        if (!canonical || typeof raw.description !== 'string' || typeof raw.value_type !== 'string' ||
          typeof raw.cardinality !== 'string' || typeof raw.sensitive !== 'boolean') continue;
        targets.push({ optionID: `target-${targets.length}`, description: raw.description, kind: raw.kind,
          valueType: raw.value_type, cardinality: raw.cardinality, sensitive: raw.sensitive, canonical });
      }
      this.targets = targets;
      this.targetByOption = new Map(targets.map((target) => [target.optionID, target]));
      this.pins.rows = this.pins.rows.map((pin) => ({
        ...pin,
        description: targets.find((target) => target.canonical === pin.targetCanonical)?.description ?? pin.description
      }));
    } catch {
      if (this.owns('catalog', abort, generation, context)) this.catalogError = 'Unable to load fact targets.';
    } finally {
      if (this.owns('catalog', abort, generation, context)) this.catalogLoading = false;
    }
  }

  private async loadPins(context: number): Promise<void> {
    const personID = this.personID;
    if (personID === null) return;
    const { abort, generation } = this.startLane('pins');
    this.pins.loading = true;
    this.pins.error = null;
    try {
      const response = await this.client.GET('/api/v1/people/{id}/fact-pins', {
        params: { path: { id: personID } }, signal: abort.signal
      });
      if (!this.owns('pins', abort, generation, context)) return;
      if (!response.data) { this.pins.rows = []; this.pins.error = 'Unable to load fact pins.'; return; }
      this.pins.rows = (response.data.pins ?? []).map((pin, index) => {
        const canonical = encodeFactTargetRef(pin.target);
        const descriptor = this.targets.find((target) => target.canonical === canonical);
        return { rowToken: `pin-${index}`, pinned: pin.pinned, kind: pin.target.kind,
          description: descriptor?.description ?? 'Eligible fact target', targetCanonical: canonical };
      });
    } catch {
      if (this.owns('pins', abort, generation, context)) this.pins.error = 'Unable to load fact pins.';
    } finally {
      if (this.owns('pins', abort, generation, context)) this.pins.loading = false;
    }
  }

  private async loadPage(section: PagedSection, targetOffset: number, context: number): Promise<void> {
    const personID = this.personID;
    if (personID === null) return;
    const page = this[section];
    const { abort, generation } = this.startLane(section);
    page.loading = true;
    page.error = null;
    page.pageError = null;
    page.retryOffset = undefined;
    const query = { ...(this.selectedTarget ? { target: this.selectedTarget } : {}), limit: FACT_LEDGER_PAGE_LIMIT, offset: targetOffset };
    try {
      const response = section === 'evidence'
        ? await this.client.GET('/api/v1/people/{id}/fact-evidence', { params: { path: { id: personID }, query }, signal: abort.signal })
        : section === 'claims'
          ? await this.client.GET('/api/v1/people/{id}/fact-claims', { params: { path: { id: personID }, query }, signal: abort.signal })
          : await this.client.GET('/api/v1/people/{id}/fact-decisions', { params: { path: { id: personID }, query }, signal: abort.signal });
      if (!this.owns(section, abort, generation, context)) return;
      if (!response.data) { this.failPage(section, targetOffset); return; }
      const rawRows = section === 'evidence' ? response.data.evidence
        : section === 'claims' ? response.data.claims : response.data.decisions;
      const projected = section === 'evidence' ? (rawRows as RawEvidence[]).map((row, index) => projectEvidence(row, targetOffset + index))
        : section === 'claims' ? (rawRows as RawClaim[]).map((row, index) => projectClaim(row, targetOffset + index))
          : (rawRows as RawDecision[]).map((row, index) => projectDecision(row, targetOffset + index));
      if (targetOffset > page.offset && projected.length === 0 && page.rows.length > 0) {
        page.endReached = true;
        page.retryOffset = undefined;
        this.requestFocus(`${section}-next`);
        return;
      }
      page.rows = projected as never[];
      page.offset = targetOffset;
      page.endReached = projected.length < FACT_LEDGER_PAGE_LIMIT;
      this.revealedEvidence.clear();
      this.revealedClaims.clear();
    } catch {
      if (this.owns(section, abort, generation, context)) this.failPage(section, targetOffset);
    } finally {
      if (this.owns(section, abort, generation, context)) page.loading = false;
    }
  }

  private failPage(section: PagedSection, targetOffset: number): void {
    const page = this[section];
    const label = section === 'evidence' ? 'evidence' : section === 'claims' ? 'claims' : 'decisions';
    if (page.rows.length === 0 && page.offset === 0 && targetOffset === 0) page.error = `Unable to load fact ${label}.`;
    else {
      page.pageError = `Unable to load the requested ${label} page.`;
      page.retryOffset = targetOffset;
    }
  }

  private requestFocus(key: string): void {
    this.focusRequest = { key, contextGeneration: this.contextGeneration, nonce: ++this.focusNonce };
  }

  private async loadHistory(targetOffset: number, context: number): Promise<void> {
    const personID = this.personID;
    const evidenceKey = this.historyEvidenceKey;
    if (personID === null || !evidenceKey || !this.historyOpen) return;
    const page = this.history;
    const { abort, generation } = this.startLane('history');
    page.loading = true;
    page.error = null;
    page.pageError = null;
    page.retryOffset = undefined;
    try {
      const response = await this.client.GET('/api/v1/people/{id}/fact-evidence-status-events', {
        params: { path: { id: personID }, query: { evidence_key: evidenceKey, limit: FACT_LEDGER_PAGE_LIMIT, offset: targetOffset } },
        signal: abort.signal
      });
      if (!this.owns('history', abort, generation, context) || !this.historyOpen || this.historyEvidenceKey !== evidenceKey) return;
      if (!response.data) { this.failHistory(targetOffset); return; }
      const rows = response.data.events.map((event) => ({ supported: event.supported,
        reasonLabel: evidenceStatusReasonLabel(event.reason), createdAt: event.created_at }));
      if (targetOffset > page.offset && rows.length === 0 && page.rows.length > 0) { page.endReached = true; return; }
      page.rows = rows;
      page.offset = targetOffset;
      page.endReached = rows.length < FACT_LEDGER_PAGE_LIMIT;
    } catch {
      if (this.owns('history', abort, generation, context)) this.failHistory(targetOffset);
    } finally {
      if (this.owns('history', abort, generation, context)) page.loading = false;
    }
  }

  private failHistory(targetOffset: number): void {
    if (this.history.rows.length === 0 && this.history.offset === 0 && targetOffset === 0) this.history.error = 'Unable to load support history.';
    else { this.history.pageError = 'Unable to load the requested support-history page.'; this.history.retryOffset = targetOffset; }
  }
}

function projectEvidence(row: RawEvidence, index: number): FactEvidenceRow {
  return { rowToken: `evidence-${index}`, sourceClass: row.source_class, directness: row.directness,
    authority: row.authority, identityScore: row.identity_score, eventTime: row.event_time,
    recordedTime: row.recorded_time, supported: row.supported,
    currentSupportLabel: row.latest_status
      ? `${row.latest_status.supported ? 'Supported' : 'Unsupported'} — ${evidenceStatusReasonLabel(row.latest_status.reason)}`
      : 'No support status event',
    createdAt: row.created_at, excerpt: row.excerpt, evidenceKey: row.evidence_key };
}

function projectClaim(row: RawClaim, index: number): FactClaimRow {
  return { rowToken: `claim-${index}`, submittedValue: row.submitted_value, normalizedValue: row.normalized_value ?? null,
    relation: row.relation, origin: row.origin, validFrom: row.valid_from ?? null, validUntil: row.valid_until ?? null,
    reportedScore: row.confidence.reported_score, createdAt: row.created_at, targetCanonical: encodeFactTargetRef(row.target) };
}

function projectDecision(row: RawDecision, index: number): FactDecisionRow {
  return { rowToken: `decision-${index}`, action: row.action, reason: row.reason,
    score: { authority: row.score.authority, confidence: row.score.confidence, corroboration: row.score.corroboration,
      directness: row.score.directness, freshness: row.score.freshness, sourceClass: row.score.source_class, total: row.score.total },
    projectionKind: row.projection?.kind ?? null, createdAt: row.created_at };
}
