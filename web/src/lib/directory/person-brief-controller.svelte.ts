import {
  generatePersonBrief as generatedGeneratePersonBrief,
  getPersonBrief as generatedGetPersonBrief,
  getPersonBriefEnrollment as generatedGetPersonBriefEnrollment,
  listPersonBriefVersions as generatedListPersonBriefVersions,
  rejectPersonBrief as generatedRejectPersonBrief,
  setPersonBriefEnrollment as generatedSetPersonBriefEnrollment,
} from '../api/generated/api/api';
import type { APIClient } from '../api/client';
import type { APIErrorBody } from '../api/runtime';
import type {
  PersonBrief as GeneratedPersonBrief,
  PersonBriefEnrollment as GeneratedPersonBriefEnrollment,
  PersonBriefEvidencePointer as GeneratedPersonBriefEvidencePointer,
  PersonBriefRun as GeneratedPersonBriefRun,
  PersonBriefSentence as GeneratedPersonBriefSentence,
} from '../api/generated/models';

// The version history read is bounded by the same default the daemon applies,
// stated here so the card never depends on a server-side default changing.
const versionHistoryLimit = 20;

/** Enrollment state the card renders. The recorded actor is deliberately not
 * projected: it is an audit field, not something the owner acts on. */
export type PersonBriefEnrollmentState = {
  person_id: number;
  enrolled: boolean;
  enabled_at: string | null;
};

/** One rendered sentence with the structured item it came from. `detail` is
 * null when this renderer's map cannot reach an item, which keeps the
 * paragraph readable without inventing an expansion. `evidence` is the archive
 * items this sentence itself cites, resolved from the response's
 * `evidence_ordinals`; it is empty when the daemon could not supply the join,
 * and the card falls back to the whole brief's citations. */
export type PersonBriefSentenceView = {
  key: string;
  kind: string;
  index: number;
  text: string;
  detail: string | null;
  meta: string[];
  evidence: PersonBriefEvidenceView[];
};

/** One cited archive item. The content hash (`evidence_key`), the internal
 * `source_url`, and the row ID are not projected: the card points at the
 * archive with a source ref and a date, and the ledger's evidence projection
 * withholds the same identifiers. No excerpt exists in the response at all. */
export type PersonBriefEvidenceView = {
  ordinal: number;
  source_ref: string;
  directness: string;
  event_time: string;
  supported: boolean;
};

export type PersonBriefView = {
  version: number;
  status: string;
  generated_at: string;
  rendered_text: string;
  sentences: PersonBriefSentenceView[];
  evidence: PersonBriefEvidenceView[];
  dropped_item_count: number;
  rejected_at: string | null;
  rejected_reason: string;
  superseded_at: string | null;
};

/** One row of the version history. The history list shows what changed and
 * when, so it carries neither the paragraph nor the evidence. */
export type PersonBriefVersionView = {
  version: number;
  status: string;
  generated_at: string;
  rejected_at: string | null;
  rejected_reason: string;
  superseded_at: string | null;
};

export type PersonBriefOutcome = { kind: 'confirmed' | 'error' | 'ignored' };

export type PersonBriefPending = 'enrollment' | 'reject' | 'generate';

const confirmed: PersonBriefOutcome = { kind: 'confirmed' };
const failed: PersonBriefOutcome = { kind: 'error' };
const ignored: PersonBriefOutcome = { kind: 'ignored' };

const speakerLabels: Record<string, string> = {
  person: 'Said by this person',
  owner: 'Said by you',
  other: 'Said by someone else',
};

const uncertaintyLabels: Record<string, string> = {
  stale: 'May be out of date',
  conflict: 'Conflicting information',
  attribution: 'Attribution unclear',
  ambiguous: 'Ambiguous',
};

// The sweep's closed failure vocabulary. An unrecognised value is reported as
// unstated rather than echoed, so a daemon string never becomes card copy.
const failureClassLabels: Record<string, string> = {
  policy: 'Policy limit',
  budget: 'Provider budget exhausted',
  lease_lost: 'Worker lease lost',
  rate_limited: 'Provider rate limited',
  timeout: 'Provider timed out',
  provider_http: 'Provider HTTP error',
  invalid_output: 'Invalid provider output',
  archive_gap: 'Archive gap',
  internal: 'Internal error',
};

/** briefFailureClassLabel names why an attempt stored no version. */
export function briefFailureClassLabel(failureClass: string): string {
  return failureClassLabels[failureClass] ?? 'Not reported';
}

export class PersonBriefController {
  personID = $state<number>();
  enrollment = $state<PersonBriefEnrollmentState>();
  brief = $state<PersonBriefView>();
  briefMissing = $state(false);
  versions = $state<PersonBriefVersionView[]>([]);
  versionsShown = $state(false);
  enrollmentLoading = $state(false);
  briefLoading = $state(false);
  versionsLoading = $state(false);
  pending = $state<PersonBriefPending | null>(null);
  enrollmentError = $state<string | null>(null);
  briefError = $state<string | null>(null);
  versionsError = $state<string | null>(null);
  actionError = $state<string | null>(null);
  runOutcome = $state<string | null>(null);
  announcement = $state<string | null>(null);

  private readonly client: APIClient;
  private disposed = false;
  private contextGeneration = 0;
  private enrollmentGeneration = 0;
  private briefGeneration = 0;
  private versionsGeneration = 0;
  private mutationGeneration = 0;
  private enrollmentAbort?: AbortController;
  private briefAbort?: AbortController;
  private versionsAbort?: AbortController;
  private mutationAbort?: AbortController;

  constructor(client: APIClient) {
    this.client = client;
  }

  get contextToken(): number {
    return this.contextGeneration;
  }

  isContextCurrent(contextToken: number): boolean {
    return !this.disposed && this.contextGeneration === contextToken;
  }

  async setPerson(personID: number): Promise<void> {
    if (this.disposed || !Number.isSafeInteger(personID) || personID <= 0) return;
    this.advanceContext();
    this.personID = personID;
    this.enrollment = undefined;
    this.brief = undefined;
    this.briefMissing = false;
    this.versions = [];
    this.versionsShown = false;
    this.pending = null;
    this.enrollmentError = null;
    this.briefError = null;
    this.versionsError = null;
    this.actionError = null;
    this.runOutcome = null;
    this.announcement = null;
    await this.loadEnrollmentAndBrief(personID);
  }

  async retryEnrollment(): Promise<void> {
    const personID = this.personID;
    if (personID === undefined || this.disposed || this.enrollmentLoading || this.pending !== null) return;
    await this.loadEnrollmentAndBrief(personID);
  }

  async retryBrief(): Promise<void> {
    const personID = this.personID;
    if (personID === undefined || this.disposed || this.briefLoading || this.pending !== null) return;
    if (!this.enrollment?.enrolled) return;
    await this.loadBrief(personID);
  }

  async setEnrolled(enrolled: boolean, track: boolean): Promise<PersonBriefOutcome> {
    const personID = this.personID;
    const current = this.enrollment;
    if (
      personID === undefined ||
      this.disposed ||
      this.pending !== null ||
      this.enrollmentLoading ||
      !current ||
      current.person_id !== personID ||
      current.enrolled === enrolled
    )
      return ignored;
    const context = this.contextGeneration;
    const mutation = ++this.mutationGeneration;
    const abort = this.beginMutation('enrollment');
    this.enrollmentError = null;
    try {
      const { data, error, response } = await generatedSetPersonBriefEnrollment(
        { id: personID },
        { enrolled, track },
        { ...this.client, signal: abort.signal },
      );
      if (!this.currentMutation(context, mutation, personID, abort.signal)) return ignored;
      const state = data ? safeEnrollment(data) : undefined;
      if (!response.ok || !state || state.person_id !== personID || state.enrolled !== enrolled) {
        this.enrollmentError = enrollmentErrorMessage(error, response.status);
        return failed;
      }
      // Reads started under the previous enrollment are no longer current.
      this.briefGeneration += 1;
      this.briefAbort?.abort();
      this.briefAbort = undefined;
      this.briefLoading = false;
      this.enrollment = state;
      this.announcement = enrolled ? 'Brief enrollment enabled.' : 'Brief enrollment disabled.';
      if (!enrolled) {
        this.brief = undefined;
        this.briefMissing = false;
        this.briefError = null;
        this.versions = [];
        this.versionsShown = false;
        this.versionsError = null;
        return confirmed;
      }
      await this.loadBrief(personID);
      return confirmed;
    } catch {
      if (!this.currentMutation(context, mutation, personID, abort.signal)) return ignored;
      this.enrollmentError = 'Unable to update brief enrollment.';
      return failed;
    } finally {
      this.endMutation(context, mutation, personID, abort);
    }
  }

  async reject(reason: string): Promise<PersonBriefOutcome> {
    const personID = this.personID;
    if (personID === undefined || this.disposed || this.pending !== null || !this.brief) return ignored;
    const context = this.contextGeneration;
    const mutation = ++this.mutationGeneration;
    const abort = this.beginMutation('reject');
    this.actionError = null;
    this.runOutcome = null;
    try {
      const { data, error, response } = await generatedRejectPersonBrief(
        { id: personID },
        { reason: reason.trim() },
        { ...this.client, signal: abort.signal },
      );
      if (!this.currentMutation(context, mutation, personID, abort.signal)) return ignored;
      const rejected = data ? safeBrief(data) : undefined;
      if (!response.ok || !rejected) {
        this.actionError = rejectErrorMessage(error, response.status);
        return failed;
      }
      // The daemon answers with the version it just rejected, but that version
      // is no longer current: a read of the brief now returns
      // person_brief_not_found until a new one is generated. The card projects
      // the same state rather than showing the rejected paragraph as if it
      // still stood; the rejected version remains visible in the history.
      this.brief = undefined;
      this.briefMissing = true;
      this.announcement = `Brief version ${rejected.version} rejected.`;
      this.endMutation(context, mutation, personID, abort);
      await this.refreshVersions(personID);
      return confirmed;
    } catch {
      if (!this.currentMutation(context, mutation, personID, abort.signal)) return ignored;
      this.actionError = 'Unable to reject the brief.';
      return failed;
    } finally {
      this.endMutation(context, mutation, personID, abort);
    }
  }

  async generate(): Promise<PersonBriefOutcome> {
    const personID = this.personID;
    if (personID === undefined || this.disposed || this.pending !== null) return ignored;
    const context = this.contextGeneration;
    const mutation = ++this.mutationGeneration;
    const abort = this.beginMutation('generate');
    this.actionError = null;
    this.runOutcome = null;
    try {
      const { data, error, response } = await generatedGeneratePersonBrief(
        { id: personID },
        { ...this.client, signal: abort.signal },
      );
      if (!this.currentMutation(context, mutation, personID, abort.signal)) return ignored;
      const run = data ? safeRun(data) : undefined;
      if (!response.ok || !run) {
        this.actionError = generateErrorMessage(error, response.status);
        return failed;
      }
      if (run.brief_version <= 0) {
        this.runOutcome = `No new brief version. Reason: ${briefFailureClassLabel(run.brief_failure_class)}.`;
        this.announcement = this.runOutcome;
        return confirmed;
      }
      this.runOutcome = `Brief version ${run.brief_version} generated.`;
      this.announcement = this.runOutcome;
      this.endMutation(context, mutation, personID, abort);
      await this.loadBrief(personID);
      await this.refreshVersions(personID);
      return confirmed;
    } catch {
      if (!this.currentMutation(context, mutation, personID, abort.signal)) return ignored;
      this.actionError = 'Unable to generate a brief.';
      return failed;
    } finally {
      this.endMutation(context, mutation, personID, abort);
    }
  }

  async showVersions(): Promise<void> {
    const personID = this.personID;
    if (personID === undefined || this.disposed || this.versionsLoading) return;
    this.versionsShown = true;
    await this.loadVersions(personID);
  }

  // hideVersions abandons a history read in flight. Its loading flag has to be
  // cleared here for the same reason advanceContext clears them: the aborted
  // read can no longer clear it, and showVersions refuses to run while it is
  // set, which would leave the toggle dead for this person.
  hideVersions(): void {
    if (this.disposed) return;
    this.versionsGeneration += 1;
    this.versionsAbort?.abort();
    this.versionsLoading = false;
    this.versionsShown = false;
    this.versionsError = null;
  }

  async retryVersions(): Promise<void> {
    const personID = this.personID;
    if (personID === undefined || this.disposed || this.versionsLoading) return;
    await this.loadVersions(personID);
  }

  destroy(): void {
    if (this.disposed) return;
    this.disposed = true;
    this.advanceContext();
  }

  // advanceContext abandons every in-flight lane. Each load clears its own
  // loading flag in a `finally` guarded by `current*`, which is false for
  // exactly the loads this supersedes, so nothing else would ever clear them
  // and the card would stay busy for the next person.
  private advanceContext(): void {
    this.contextGeneration += 1;
    this.enrollmentGeneration += 1;
    this.briefGeneration += 1;
    this.versionsGeneration += 1;
    this.mutationGeneration += 1;
    this.enrollmentAbort?.abort();
    this.briefAbort?.abort();
    this.versionsAbort?.abort();
    this.mutationAbort?.abort();
    this.enrollmentLoading = false;
    this.briefLoading = false;
    this.versionsLoading = false;
  }

  private beginMutation(action: PersonBriefPending): AbortController {
    this.mutationAbort?.abort();
    const abort = new AbortController();
    this.mutationAbort = abort;
    this.pending = action;
    this.announcement = null;
    return abort;
  }

  private endMutation(
    context: number,
    mutation: number,
    personID: number,
    abort: AbortController,
  ): void {
    if (!this.currentMutation(context, mutation, personID)) return;
    if (this.mutationAbort === abort) this.mutationAbort = undefined;
    this.pending = null;
  }

  private async loadEnrollmentAndBrief(personID: number): Promise<void> {
    if (!(await this.loadEnrollment(personID))) return;
    if (!this.enrollment?.enrolled) return;
    await this.loadBrief(personID);
  }

  private async refreshVersions(personID: number): Promise<void> {
    if (!this.versionsShown) return;
    await this.loadVersions(personID);
  }

  private async loadEnrollment(personID: number): Promise<boolean> {
    const context = this.contextGeneration;
    const request = ++this.enrollmentGeneration;
    this.enrollmentAbort?.abort();
    const abort = new AbortController();
    this.enrollmentAbort = abort;
    this.enrollmentLoading = true;
    this.enrollmentError = null;
    try {
      const { data, response } = await generatedGetPersonBriefEnrollment(
        { id: personID },
        { ...this.client, signal: abort.signal },
      );
      if (!this.currentEnrollment(context, request, personID, abort.signal)) return false;
      const state = response.ok && data ? safeEnrollment(data) : undefined;
      if (!state || state.person_id !== personID) {
        this.enrollment = undefined;
        this.enrollmentError = 'Unable to load brief enrollment.';
        return false;
      }
      this.enrollment = state;
      return true;
    } catch {
      if (!this.currentEnrollment(context, request, personID, abort.signal)) return false;
      this.enrollment = undefined;
      this.enrollmentError = 'Unable to load brief enrollment.';
      return false;
    } finally {
      if (this.currentEnrollment(context, request, personID)) {
        if (this.enrollmentAbort === abort) this.enrollmentAbort = undefined;
        this.enrollmentLoading = false;
      }
    }
  }

  private async loadBrief(personID: number): Promise<boolean> {
    const context = this.contextGeneration;
    const request = ++this.briefGeneration;
    this.briefAbort?.abort();
    const abort = new AbortController();
    this.briefAbort = abort;
    this.briefLoading = true;
    this.briefError = null;
    try {
      const { data, error, response } = await generatedGetPersonBrief(
        { id: personID },
        { ...this.client, signal: abort.signal },
      );
      if (!this.currentBrief(context, request, personID, abort.signal)) return false;
      if (response.status === 404 && error?.error === 'person_brief_not_found') {
        this.brief = undefined;
        this.briefMissing = true;
        return true;
      }
      const view = response.ok && data ? safeBrief(data) : undefined;
      if (!view) {
        this.brief = undefined;
        this.briefMissing = false;
        this.briefError = 'Unable to load the brief.';
        return false;
      }
      this.brief = view;
      this.briefMissing = false;
      return true;
    } catch {
      if (!this.currentBrief(context, request, personID, abort.signal)) return false;
      this.brief = undefined;
      this.briefMissing = false;
      this.briefError = 'Unable to load the brief.';
      return false;
    } finally {
      if (this.currentBrief(context, request, personID)) {
        if (this.briefAbort === abort) this.briefAbort = undefined;
        this.briefLoading = false;
      }
    }
  }

  private async loadVersions(personID: number): Promise<boolean> {
    const context = this.contextGeneration;
    const request = ++this.versionsGeneration;
    this.versionsAbort?.abort();
    const abort = new AbortController();
    this.versionsAbort = abort;
    this.versionsLoading = true;
    this.versionsError = null;
    try {
      const { data, response } = await generatedListPersonBriefVersions(
        { id: personID },
        { limit: versionHistoryLimit },
        { ...this.client, signal: abort.signal },
      );
      if (!this.currentVersions(context, request, personID, abort.signal)) return false;
      const rows = response.ok && data ? safeVersions(data.versions) : undefined;
      if (!rows) {
        this.versions = [];
        this.versionsError = 'Unable to load brief version history.';
        return false;
      }
      this.versions = rows;
      return true;
    } catch {
      if (!this.currentVersions(context, request, personID, abort.signal)) return false;
      this.versions = [];
      this.versionsError = 'Unable to load brief version history.';
      return false;
    } finally {
      if (this.currentVersions(context, request, personID)) {
        if (this.versionsAbort === abort) this.versionsAbort = undefined;
        this.versionsLoading = false;
      }
    }
  }

  private current(context: number, personID: number, signal?: AbortSignal): boolean {
    return this.isContextCurrent(context) && this.personID === personID && !signal?.aborted;
  }

  private currentEnrollment(context: number, request: number, personID: number, signal?: AbortSignal): boolean {
    return this.current(context, personID, signal) && this.enrollmentGeneration === request;
  }

  private currentBrief(context: number, request: number, personID: number, signal?: AbortSignal): boolean {
    return this.current(context, personID, signal) && this.briefGeneration === request;
  }

  private currentVersions(context: number, request: number, personID: number, signal?: AbortSignal): boolean {
    return this.current(context, personID, signal) && this.versionsGeneration === request;
  }

  private currentMutation(context: number, request: number, personID: number, signal?: AbortSignal): boolean {
    return this.current(context, personID, signal) && this.mutationGeneration === request;
  }
}

function enrollmentErrorMessage(error: APIErrorBody | undefined, status: number): string {
  if (status === 409 && error?.error === 'person_brief_not_tracked') {
    return 'This person must be tracked before brief enrollment. Select “Also track this person” and enroll again.';
  }
  return 'Unable to update brief enrollment.';
}

function rejectErrorMessage(error: APIErrorBody | undefined, status: number): string {
  if (status === 404 && error?.error === 'person_brief_not_found') {
    return 'There is no current brief version to reject.';
  }
  return 'Unable to reject the brief.';
}

function generateErrorMessage(error: APIErrorBody | undefined, status: number): string {
  if (status === 409 && error?.error === 'person_brief_not_enrolled') {
    return 'Enroll this person in briefs before generating one.';
  }
  if (status === 409 && error?.error === 'person_brief_lane_disabled') {
    return 'Brief generation is turned off in the daemon configuration.';
  }
  if (status === 409 && error?.error === 'person_brief_policy_refused') {
    return 'The people inference profile does not allow sensitive content, which every brief carries.';
  }
  if (status === 409 && error?.error === 'person_brief_no_supported_lane') {
    return 'The people inference profile does not allow conversation text, the only source a brief reads in this version.';
  }
  if (status === 409 && error?.error === 'person_brief_busy') {
    return 'Another worker is sweeping this person right now; nothing was generated. Retry once it finishes.';
  }
  if (status === 409 && error?.error === 'person_brief_not_tracked') {
    return 'This person must be tracked before brief enrollment. Select “Also track this person” and enroll again.';
  }
  if (status === 503) {
    return 'Brief generation needs the daemon people sweep worker. It is unavailable.';
  }
  return 'Unable to generate a brief.';
}

function safeEnrollment(value: GeneratedPersonBriefEnrollment): PersonBriefEnrollmentState | undefined {
  if (!Number.isSafeInteger(value.person_id) || value.person_id <= 0) return undefined;
  if (typeof value.enrolled !== 'boolean') return undefined;
  if (value.enabled_at !== null && typeof value.enabled_at !== 'string') return undefined;
  return { person_id: value.person_id, enrolled: value.enrolled, enabled_at: value.enabled_at };
}

function safeRun(value: GeneratedPersonBriefRun): GeneratedPersonBriefRun | undefined {
  if (!Number.isSafeInteger(value.brief_version) || value.brief_version < 0) return undefined;
  if (typeof value.brief_failure_class !== 'string') return undefined;
  return value;
}

function safeBrief(value: GeneratedPersonBrief): PersonBriefView | undefined {
  const version = safeVersion(value);
  if (!version) return undefined;
  if (typeof value.rendered_text !== 'string') return undefined;
  if (!Number.isSafeInteger(value.dropped_item_count) || value.dropped_item_count < 0) return undefined;
  const evidence = safeEvidence(value.evidence);
  if (!evidence) return undefined;
  const sentences = safeSentences(value.sentences, value.structured, evidence);
  if (!sentences) return undefined;
  return { ...version, rendered_text: value.rendered_text, sentences, evidence, dropped_item_count: value.dropped_item_count };
}

function safeVersions(values: GeneratedPersonBrief[] | null | undefined): PersonBriefVersionView[] | undefined {
  if (values === null || values === undefined) return [];
  if (!Array.isArray(values)) return undefined;
  const rows: PersonBriefVersionView[] = [];
  for (const value of values) {
    const row = safeVersion(value);
    if (!row) return undefined;
    rows.push(row);
  }
  return rows;
}

function safeVersion(value: GeneratedPersonBrief): PersonBriefVersionView | undefined {
  if (!value || typeof value !== 'object') return undefined;
  if (!Number.isSafeInteger(value.version) || value.version < 1) return undefined;
  if (typeof value.status !== 'string' || value.status.length === 0) return undefined;
  if (typeof value.generated_at !== 'string') return undefined;
  if (typeof value.rejected_reason !== 'string') return undefined;
  if (value.rejected_at !== null && typeof value.rejected_at !== 'string') return undefined;
  if (value.superseded_at !== null && typeof value.superseded_at !== 'string') return undefined;
  return {
    version: value.version,
    status: value.status,
    generated_at: value.generated_at,
    rejected_at: value.rejected_at,
    rejected_reason: value.rejected_reason,
    superseded_at: value.superseded_at,
  };
}

function safeEvidence(
  values: GeneratedPersonBriefEvidencePointer[] | null | undefined,
): PersonBriefEvidenceView[] | undefined {
  if (values === null || values === undefined) return [];
  if (!Array.isArray(values)) return undefined;
  const pointers: PersonBriefEvidenceView[] = [];
  for (const value of values) {
    if (!value || typeof value !== 'object') return undefined;
    if (!Number.isSafeInteger(value.ordinal) || value.ordinal < 0) return undefined;
    if (typeof value.source_ref !== 'string') return undefined;
    if (typeof value.directness !== 'string') return undefined;
    if (typeof value.event_time !== 'string') return undefined;
    if (typeof value.evidence_supported !== 'boolean') return undefined;
    pointers.push({
      ordinal: value.ordinal,
      source_ref: value.source_ref,
      directness: value.directness,
      event_time: value.event_time,
      supported: value.evidence_supported,
    });
  }
  return pointers;
}

function safeSentences(
  values: GeneratedPersonBriefSentence[] | null | undefined,
  structured: unknown,
  evidence: PersonBriefEvidenceView[],
): PersonBriefSentenceView[] | undefined {
  if (values === null || values === undefined) return [];
  if (!Array.isArray(values)) return undefined;
  const byOrdinal = new Map(evidence.map((item) => [item.ordinal, item]));
  const sentences: PersonBriefSentenceView[] = [];
  const keys = new Set<string>();
  for (const value of values) {
    if (!value || typeof value !== 'object') return undefined;
    if (typeof value.kind !== 'string' || value.kind.length === 0) return undefined;
    if (!Number.isSafeInteger(value.index) || value.index < 0) return undefined;
    if (typeof value.text !== 'string') return undefined;
    // The key is what the card renders sentences by, so a duplicate would make
    // the expansion ambiguous. Nothing sensible can be shown, and Svelte's
    // keyed each would throw, so this fails closed like every other check here.
    const key = `${value.kind}:${value.index}`;
    if (keys.has(key)) return undefined;
    keys.add(key);
    const cited = safeSentenceEvidence(value.evidence_ordinals, byOrdinal);
    if (!cited) return undefined;
    const item = structuredItem(structured, value.kind, value.index);
    sentences.push({
      key,
      kind: value.kind,
      index: value.index,
      text: value.text,
      detail: item?.detail ?? null,
      meta: item?.meta ?? [],
      evidence: cited,
    });
  }
  return sentences;
}

// safeSentenceEvidence resolves one sentence's citations from the ordinals the
// daemon computed for it. An absent field is a daemon that predates the join
// key and means "no per-sentence citations"; a malformed one, or an ordinal
// naming no pointer in this same response, is an inconsistent answer and fails
// the whole projection rather than showing a citation that may be wrong.
function safeSentenceEvidence(
  ordinals: number[] | null | undefined,
  byOrdinal: Map<number, PersonBriefEvidenceView>,
): PersonBriefEvidenceView[] | undefined {
  if (ordinals === null || ordinals === undefined) return [];
  if (!Array.isArray(ordinals)) return undefined;
  const cited: PersonBriefEvidenceView[] = [];
  for (const ordinal of ordinals) {
    if (!Number.isSafeInteger(ordinal) || ordinal < 0) return undefined;
    const item = byOrdinal.get(ordinal);
    if (!item) return undefined;
    cited.push(item);
  }
  return cited;
}

function record(value: unknown): Record<string, unknown> | undefined {
  return value && typeof value === 'object' && !Array.isArray(value)
    ? (value as Record<string, unknown>)
    : undefined;
}

function text(value: unknown): string | undefined {
  return typeof value === 'string' && value.length > 0 ? value : undefined;
}

function itemAt(structured: Record<string, unknown>, key: string, index: number): Record<string, unknown> | undefined {
  const list = structured[key];
  if (!Array.isArray(list)) return undefined;
  return record(list[index]);
}

// structuredItem resolves one sentence back to the validated item the renderer
// produced it from. Only the item's own text and its bounded labels are
// projected; the packet-local citation IDs on the item are never read here,
// because the daemon already resolved them to evidence ordinals.
function structuredItem(
  structured: unknown,
  kind: string,
  index: number,
): { detail: string; meta: string[] } | undefined {
  const document = record(structured);
  if (!document) return undefined;
  if (kind === 'last_interaction') {
    if (index !== 0) return undefined;
    const summary = text(record(document.last_meaningful_interaction)?.summary);
    return summary ? { detail: summary, meta: [] } : undefined;
  }
  if (kind === 'highlight') {
    const item = itemAt(document, 'highlights', index);
    const detail = text(item?.text);
    if (!item || !detail) return undefined;
    const meta: string[] = [];
    const speaker = speakerLabels[String(item.speaker)];
    if (speaker) meta.push(speaker);
    const observed = text(item.observed_at);
    if (observed) meta.push(`Observed ${observed}`);
    return { detail, meta };
  }
  if (kind === 'follow_up') {
    const item = itemAt(document, 'follow_ups', index);
    const detail = text(item?.question);
    if (!item || !detail) return undefined;
    const why = text(item.why);
    return { detail, meta: why ? [why] : [] };
  }
  if (kind === 'appreciation') {
    const detail = text(itemAt(document, 'appreciations', index)?.text);
    return detail ? { detail, meta: [] } : undefined;
  }
  if (kind === 'uncertainty') {
    const item = itemAt(document, 'uncertainties', index);
    const detail = text(item?.text);
    if (!item || !detail) return undefined;
    const label = uncertaintyLabels[String(item.kind)];
    return { detail, meta: label ? [label] : [] };
  }
  return undefined;
}
