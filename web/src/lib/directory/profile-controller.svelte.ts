import {
  assessPersonPostalAddress as generatedAssessPersonPostalAddress,
  clearPersonAttribute as generatedClearPersonAttribute,
  createAttributeDefinition as generatedCreateAttributeDefinition,
  deletePerson as generatedDeletePerson,
  getPersonContactState as generatedGetPersonContactState,
  getPersonProfile as generatedGetPersonProfile,
  getPersonStructuredProfile as generatedGetPersonStructuredProfile,
  listAttributeDefinitions as generatedListAttributeDefinitions,
  listPersonAttributes as generatedListPersonAttributes,
  patchPerson as generatedPatchPerson,
  patchPersonStructuredProfile as generatedPatchPersonStructuredProfile,
  setPersonAttribute as generatedSetPersonAttribute,
} from '../api/generated/api/api';
import type { APIClient } from '../api/client';
import type { Assessment, Input } from '../api/generated/models';
import type {
  AttributeDefinition,
  CreateAttributeDefinitionRequest,
  DirectoryProfileConflict,
  DirectoryProfileDraft,
  DirectoryProfileOperationBlocked,
  DirectoryProfileOperationResult,
  DirectoryPersonSummaryUpdate,
  DirectoryReadBundle,
  PatchPersonRequest,
  PersonAttributeValue,
  PersonProfilePatchRequest,
  SetPersonAttributeRequest,
} from './models';
interface DirectoryProfileControllerOptions {
  invalidateRow?: (
    personID: number,
    update?: DirectoryPersonSummaryUpdate,
    refreshDirectory?: boolean,
  ) => void | Promise<void>;
  onDetailChange?: (bundle: DirectoryReadBundle) => void;
  onDeleted?: (personID: number) => void | Promise<void>;
}
type RequestResult<T> = {
  data?: T;
  error?: unknown;
  response?: Response;
};
type DefinitionCreationCommit =
  | {
      kind: 'target';
      universalID: string;
      slug: string;
    }
  | {
      kind: 'unknown';
    };
const missingCreatedDefinitionMessage =
  'Created definition was not found with the returned identity in the refreshed person attribute registry.';
const unknownCreatedDefinitionMessage =
  'Definition creation may have succeeded, but the server did not return a usable identity. Reload Directory before creating another field.';
/**
 * Owns the mutable subset of a Directory detail bundle. It deliberately uses
 * generated endpoint shapes, while callers retain ownership of the selected
 * Directory row and URL state.
 */
export class DirectoryProfileController {
  person = $state<DirectoryReadBundle['person']>();
  structuredProfile = $state<DirectoryReadBundle['structuredProfile']>();
  attributes = $state<DirectoryReadBundle['attributes']>();
  definitions = $state<AttributeDefinition[]>([]);
  personETag = $state<string | null>(null);
  structuredProfileETag = $state<string | null>(null);
  draft = $state<DirectoryProfileDraft | null>(null);
  conflict = $state<DirectoryProfileConflict | null>(null);
  mutationPending = $state(false);
  reloadPending = $state(false);
  createdDefinition = $state<AttributeDefinition | null>(null);
  definitionCreationCommit = $state<DefinitionCreationCommit | null>(null);
  definitionsError = $state<string | null>(null);
  attributesError = $state<string | null>(null);
  private bundle: DirectoryReadBundle;
  private readonly client: APIClient;
  private readonly personID: number;
  private readonly options: DirectoryProfileControllerOptions;
  private reloadAbort: AbortController | undefined;
  private generation = 0;
  private definitionsGeneration = 0;
  private draftGeneration = 0;
  private activeMutationGeneration: number | null = null;
  private mutationSettled = Promise.resolve();
  private settleMutation: (() => void) | undefined;
  private disposed = false;
  constructor(
    client: APIClient,
    personID: number,
    bundle: DirectoryReadBundle,
    options: DirectoryProfileControllerOptions = {},
  ) {
    this.client = client;
    this.personID = personID;
    this.bundle = bundle;
    this.options = options;
    this.person = bundle.person;
    this.structuredProfile = bundle.structuredProfile;
    this.attributes = bundle.attributes; this.attributesError = bundle.errors.attributes ?? null;
    this.definitions = bundle.definitions?.definitions ?? [];
    this.personETag = bundle.etags.person ?? null;
    this.structuredProfileETag = bundle.etags.structuredProfile ?? null;
  }
  get canWritePerson(): boolean {
    return this.personETag !== null && !this.mutationPending && !this.reloadPending && !this.hasUnresolvedConflict;
  }
  get canWriteProfile(): boolean {
    return (
      this.structuredProfileETag !== null && !this.mutationPending && !this.reloadPending && !this.hasUnresolvedConflict
    );
  }
  get hasUnresolvedConflict(): boolean {
    return (
      this.conflict?.code === 'person_revision_conflict' ||
      this.conflict?.code === 'attribute_conflict' ||
      this.conflict?.code === 'precondition_required'
    );
  }
  get canReload(): boolean {
    return !this.mutationPending && !this.reloadPending;
  }
  discardAttributeDraft(slug: string): boolean {
    if (this.draft?.kind !== 'setAttribute' && this.draft?.kind !== 'clearAttribute') return false;
    if (this.draft.slug !== slug) return false;
    this.draft = null;
    this.conflict = null;
    ++this.draftGeneration;
    return true;
  }
  discardProfileDraft(): boolean {
    if (this.mutationPending || this.reloadPending || this.draft?.kind !== 'profile') return false;
    this.draft = null;
    this.conflict = null;
    ++this.draftGeneration;
    return true;
  }
  destroy(): void {
    this.disposed = true;
    ++this.generation;
    this.reloadAbort?.abort();
    this.settleMutation?.();
  }
  async reload(resolveDraft = true): Promise<DirectoryProfileOperationResult> {
    while (this.mutationPending && !this.disposed) {
      if (resolveDraft) return { ok: false, code: 'mutation_in_progress' };
      await this.mutationSettled;
    }
    if (this.disposed) return { ok: false, code: 'reload_in_progress' };
    this.reloadAbort?.abort();
    this.reloadPending = true;
    const abort = new AbortController();
    this.reloadAbort = abort;
    const generation = ++this.generation;
    const definitionsGeneration = ++this.definitionsGeneration;
    const personETagBeforeReload = this.personETag;
    const profileETagBeforeReload = this.structuredProfileETag;
    const personRequiresFreshETag = this.requiresReload('rename') || this.requiresReload('delete');
    const profileRequiresFreshETag = this.requiresReload('profile');
    const requestOptions = { ...this.client, signal: abort.signal };
    try {
      const [person, structuredProfile, attributes, definitions, contactState] = await Promise.all([
        settle(generatedGetPersonProfile({ id: this.personID }, requestOptions)),
        settle(generatedGetPersonStructuredProfile({ id: this.personID }, requestOptions)),
        settle(generatedListPersonAttributes({ id: this.personID }, { history: true }, requestOptions)),
        settle(generatedListAttributeDefinitions({ object_type: 'person', include_hidden: true }, requestOptions)),
        settle(generatedGetPersonContactState({ id: this.personID }, requestOptions)),
      ]);
      if (this.disposed || abort.signal.aborted || generation !== this.generation)
        return { ok: false, code: 'reload_in_progress' };
      const personResponseETag = person.response?.headers.get('ETag');
      const profileResponseETag = structuredProfile.response?.headers.get('ETag');
      const personReady =
        person.data !== undefined &&
        hasETag(personResponseETag) &&
        (!personRequiresFreshETag || personResponseETag !== personETagBeforeReload);
      const structuredProfileReady =
        structuredProfile.data !== undefined &&
        hasETag(profileResponseETag) &&
        (!profileRequiresFreshETag || profileResponseETag !== profileETagBeforeReload);
      const attributesReady = attributes.data !== undefined;
      const definitionsReady = definitionsGeneration === this.definitionsGeneration && definitions.data !== undefined;
      const errors = { ...this.bundle.errors };
      if (personReady && person.data && (resolveDraft || !personRequiresFreshETag)) {
        this.person = person.data; delete errors.person;
        this.personETag = person.response?.headers.get('ETag') ?? null;
      } else if (!person.data) errors.person = failureMessage(person.error, person.response?.status ?? 0);
      if (structuredProfileReady && structuredProfile.data && (resolveDraft || !profileRequiresFreshETag)) {
        this.structuredProfile = structuredProfile.data; delete errors.structuredProfile;
        this.structuredProfileETag = structuredProfile.response?.headers.get('ETag') ?? null;
      } else if (!structuredProfile.data) errors.structuredProfile = failureMessage(structuredProfile.error, structuredProfile.response?.status ?? 0);
      if (attributes.data) {
        this.attributes = attributes.data;
        this.attributesError = null; delete errors.attributes;
      } else this.attributesError = failureMessage(attributes.error, attributes.response?.status ?? 0);
      if (definitionsGeneration === this.definitionsGeneration) {
        if (definitions.data) {
          this.definitions = definitions.data.definitions ?? [];
          this.definitionsError = null;
        } else this.definitionsError = failureMessage(definitions.error, definitions.response?.status ?? 0);
      }
      if (contactState.data) {
        this.bundle = { ...this.bundle, contactState: contactState.data };
        delete errors.contactState;
      } else errors.contactState = failureMessage(contactState.error, contactState.response?.status ?? 0);
      this.bundle = { ...this.bundle, errors };
      if (resolveDraft && (this.draft?.kind !== 'createDefinition' || definitionsGeneration === this.definitionsGeneration)) this.resolveReloadDraft(
        { person, structuredProfile, attributes, definitions },
        { personReady, structuredProfileReady, attributesReady, definitionsReady },
      );
      this.publish();
      return { ok: true };
    } finally {
      if (generation === this.generation) this.reloadPending = false;
    }
  }
  async rename(displayName: string | null): Promise<void | DirectoryProfileOperationBlocked> {
    const body: PatchPersonRequest = { display_name: displayName };
    const draftGeneration = this.prepareMutation({ kind: 'rename', body });
    if (typeof draftGeneration !== 'number') return draftGeneration;
    if (!this.personETag) return this.missingETag();
    await this.writePerson(body, draftGeneration);
  }
  async deletePerson(): Promise<void | DirectoryProfileOperationBlocked> {
    const draftGeneration = this.prepareMutation({ kind: 'delete' });
    if (typeof draftGeneration !== 'number') return draftGeneration;
    if (!this.personETag) return this.missingETag();
    this.beginMutation(draftGeneration);
    let deleted = false;
    try {
      const response = await generatedDeletePerson(
        { id: this.personID },
        {
          ...this.client,
          headers: { 'If-Match': this.personETag },
        },
      );
      if (response.response.status === 204) {
        await this.completeMutation(draftGeneration, false);
        deleted = true;
      } else {
        this.captureMutationFailure(response.error, response.response.status, draftGeneration);
      }
    } catch (cause: unknown) {
      this.captureMutationFailure(cause, 0, draftGeneration);
    } finally {
      this.finishMutation(draftGeneration);
    }
    if (deleted) await this.options.onDeleted?.(this.personID);
  }
  async assessPostalAddress(personID: number, address: Input, external: boolean, signal: AbortSignal): Promise<Assessment> {
    if (this.disposed || personID !== this.personID) throw new Error('Address editor is no longer active');
    const result = await generatedAssessPersonPostalAddress({ id: this.personID }, { address, allow_external: external }, { ...this.client, signal });
    if (!result.data) throw new Error('Address check unavailable');
    return result.data;
  }
  async patchProfile(body: PersonProfilePatchRequest, reviewedETag?: string): Promise<void | DirectoryProfileOperationBlocked> {
    const draftGeneration = this.prepareMutation({ kind: 'profile', body });
    if (typeof draftGeneration !== 'number') return draftGeneration;
    const etag = reviewedETag ?? this.structuredProfileETag;
    if (!etag) return this.missingETag();
    this.beginMutation(draftGeneration);
    try {
      const response = await generatedPatchPersonStructuredProfile({ id: this.personID }, body, {
        ...this.client,
        headers: { 'If-Match': etag },
      });
      if (response.data) {
        this.structuredProfile = response.data;
        const etag = response.response.headers.get('ETag');
        this.structuredProfileETag = etag;
        this.person = response.data.person;
        this.personETag = etag;
        this.publish();
        await this.invalidateRow(
          body.categories
            ? {
                categories: (response.data.categories ?? []).map((category) => category.original_value),
              }
            : undefined,
          body.names !== undefined || body.contact_points !== undefined,
        );
        await this.completeMutation(draftGeneration, true);
        return;
      }
      this.captureMutationFailure(response.error, response.response.status, draftGeneration);
    } catch (cause: unknown) {
      this.captureMutationFailure(cause, 0, draftGeneration);
    } finally {
      this.finishMutation(draftGeneration);
    }
  }
  async setAttribute(
    slug: string,
    request: SetPersonAttributeRequest,
  ): Promise<void | DirectoryProfileOperationBlocked> {
    const body: SetPersonAttributeRequest = { ...request, source: 'user' };
    const draftGeneration = this.prepareMutation({ kind: 'setAttribute', slug, body });
    if (typeof draftGeneration !== 'number') return draftGeneration;
    await this.writeAttribute(slug, body, draftGeneration);
  }
  async clearAttribute(
    slug: string,
    expectedValueID: number,
    ordinal?: number,
  ): Promise<void | DirectoryProfileOperationBlocked> {
    const draftGeneration = this.prepareMutation({
      kind: 'clearAttribute',
      slug,
      expectedValueID,
      ...(ordinal === undefined ? {} : { ordinal }),
    });
    if (typeof draftGeneration !== 'number') return draftGeneration;
    this.beginMutation(draftGeneration);
    try {
      const response = await generatedClearPersonAttribute(
        { id: this.personID, slug: slug },
        { expected_value_id: expectedValueID, ...(ordinal === undefined ? {} : { ordinal }) },
        this.client,
      );
      if (response.data) {
        await this.completeAttributeWrite(slug, response.data, draftGeneration);
        return;
      }
      this.captureMutationFailure(response.error, response.response.status, draftGeneration);
    } catch (cause: unknown) {
      this.captureMutationFailure(cause, 0, draftGeneration);
    } finally {
      this.finishMutation(draftGeneration);
    }
  }
  async reloadDefinitions(expectedDraftGeneration = this.draftGeneration): Promise<boolean> {
    const generation = ++this.definitionsGeneration;
    const response = await settle(generatedListAttributeDefinitions({ object_type: 'person', include_hidden: true }, this.client));
    if (this.disposed || generation !== this.definitionsGeneration || this.draftGeneration !== expectedDraftGeneration) return false;
    if (response.data) {
      this.definitions = response.data.definitions ?? [];
      this.definitionsError = null;
      if (this.definitionCreationCommit) {
        const reconciled = this.reconcileDefinitionCreation(expectedDraftGeneration);
        this.publish();
        return reconciled;
      }
      if (this.draft?.kind === 'createDefinition') {
        this.draft = null;
        this.conflict = null;
      }
      this.publish();
      return true;
    }
    this.definitionsError = failureMessage(response.error, response.response?.status ?? 0);
    if (this.draft?.kind === 'createDefinition') {
      this.captureFailure(response.error, response.response?.status ?? 0);
    }
    return false;
  }
  async retryDefinitionRefresh(): Promise<void | DirectoryProfileOperationBlocked> {
    if (this.definitionCreationCommit?.kind !== 'target') return { ok: false, code: 'conflict_unresolved' };
    if (this.reloadPending) return { ok: false, code: 'reload_in_progress' };
    if (this.mutationPending) return { ok: false, code: 'mutation_in_progress' };
    const draftGeneration = this.draftGeneration;
    this.beginMutation(draftGeneration);
    try {
      await this.reloadDefinitions(draftGeneration);
    } finally {
      this.finishMutation(draftGeneration);
    }
  }
  async createDefinition(body: CreateAttributeDefinitionRequest): Promise<void | DirectoryProfileOperationBlocked> {
    if (this.mutationPending && this.draft?.kind === 'createDefinition')
      return { ok: false, code: 'mutation_in_progress' };
    if (this.definitionCreationCommit || this.createdDefinition) return { ok: false, code: 'conflict_unresolved' };
    const draftGeneration = this.prepareMutation({ kind: 'createDefinition', body });
    if (typeof draftGeneration !== 'number') return draftGeneration;
    this.beginMutation(draftGeneration);
    this.createdDefinition = null;
    try {
      // Ownership is assigned by the authenticated server; the generated
      // request does not accept an actor or ownership field.
      const response = await generatedCreateAttributeDefinition(body, {
        ...this.client,
        onResponse: (response) => {
          if (!this.disposed && response.ok) this.definitionCreationCommit = { kind: 'unknown' };
        },
      });
      if (response.response.ok) {
        this.definitionCreationCommit = { kind: 'unknown' };
        const parsed: unknown = response.data;
        if (!isRecord(parsed)) {
          this.captureMutationFailure(new Error(unknownCreatedDefinitionMessage), 502, draftGeneration);
          return;
        }
        if (parsed.ownership !== 'user' || parsed.object_type !== 'person') {
          this.captureMutationFailure(
            new Error('Created definition was not returned as a user-owned person attribute registry entry.'),
            502,
            draftGeneration,
          );
          return;
        }
        if (!hasUsableDefinitionIdentity(parsed)) {
          this.captureMutationFailure(new Error(unknownCreatedDefinitionMessage), 502, draftGeneration);
          return;
        }
        const created = parsed as AttributeDefinition;
        this.definitionCreationCommit = {
          kind: 'target',
          universalID: created.universal_id,
          slug: created.slug,
        };
        await this.reloadDefinitions(draftGeneration);
        return;
      }
      this.captureMutationFailure(response.error, response.response.status, draftGeneration);
    } catch (cause: unknown) {
      this.captureMutationFailure(cause, 0, draftGeneration);
    } finally {
      this.finishMutation(draftGeneration);
    }
  }
  acknowledgeDefinitionCreation(): boolean {
    if (this.mutationPending || this.definitionCreationCommit || !this.createdDefinition) return false;
    this.createdDefinition = null;
    return true;
  }
  private async writePerson(body: PatchPersonRequest, draftGeneration: number): Promise<void> {
    this.beginMutation(draftGeneration);
    try {
      const response = await generatedPatchPerson({ id: this.personID }, body, {
        ...this.client,
        headers: { 'If-Match': this.personETag! },
      });
      if (response.data) {
        this.person = response.data;
        const etag = response.response.headers.get('ETag');
        this.personETag = etag;
        this.structuredProfileETag = etag;
        if (this.structuredProfile) this.structuredProfile = { ...this.structuredProfile, person: response.data };
        this.publish();
        await this.invalidateRow(undefined, true);
        await this.completeMutation(draftGeneration, true);
        return;
      }
      this.captureMutationFailure(response.error, response.response.status, draftGeneration);
    } catch (cause: unknown) {
      this.captureMutationFailure(cause, 0, draftGeneration);
    } finally {
      this.finishMutation(draftGeneration);
    }
  }
  private async writeAttribute(slug: string, body: SetPersonAttributeRequest, draftGeneration: number): Promise<void> {
    this.beginMutation(draftGeneration);
    try {
      const response = await generatedSetPersonAttribute(
        { id: this.personID, slug: slug },
        body,
        undefined,
        this.client,
      );
      if (response.data) {
        await this.completeAttributeWrite(slug, response.data, draftGeneration);
        return;
      }
      this.captureMutationFailure(response.error, response.response.status, draftGeneration);
    } catch (cause: unknown) {
      this.captureMutationFailure(cause, 0, draftGeneration);
    } finally {
      this.finishMutation(draftGeneration);
    }
  }
  private async completeAttributeWrite(slug: string, response: { value?: PersonAttributeValue; superseded?: PersonAttributeValue }, draftGeneration: number): Promise<void> {
    this.applyAttributeWrite(slug, response.value, response.superseded);
    if (slug !== 'primary_channel') await this.invalidateRow();
    await this.completeMutation(draftGeneration, true);
  }
  private prepareMutation(draft: DirectoryProfileDraft): number | DirectoryProfileOperationBlocked {
    if (this.reloadPending) return { ok: false, code: 'reload_in_progress' };
    if (this.hasUnresolvedConflict) return { ok: false, code: 'conflict_unresolved' };
    if (this.mutationPending) {
      this.draft = draft;
      ++this.draftGeneration;
      this.conflict = {
        code: 'mutation_in_progress',
        message: 'A save is already in progress. Retry this change after it finishes.',
        status: 409,
      };
      return { ok: false, code: 'mutation_in_progress' };
    }
    this.draft = draft;
    this.conflict = null;
    return ++this.draftGeneration;
  }
  private beginMutation(draftGeneration: number): void {
    this.mutationSettled = new Promise(resolve => { this.settleMutation = resolve; });
    this.mutationPending = true;
    this.activeMutationGeneration = draftGeneration;
  }
  private async completeMutation(draftGeneration: number, refresh: boolean): Promise<void> {
    if (this.draftGeneration === draftGeneration) { this.draft = null; this.conflict = null; }
    this.finishMutation(draftGeneration);
    if (refresh) await this.reload(false);
  }
  private finishMutation(draftGeneration: number): void {
    if (this.activeMutationGeneration !== draftGeneration) return;
    this.activeMutationGeneration = null;
    this.mutationPending = false;
    this.settleMutation?.();
    this.settleMutation = undefined;
  }
  private captureMutationFailure(error: unknown, status: number, draftGeneration: number): void {
    if (this.draftGeneration === draftGeneration) this.captureFailure(error, status);
  }
  private resolveReloadDraft(
    results: {
      person: RequestResult<unknown>;
      structuredProfile: RequestResult<unknown>;
      attributes: RequestResult<unknown>;
      definitions: RequestResult<unknown>;
    },
    ready: {
      personReady: boolean;
      structuredProfileReady: boolean;
      attributesReady: boolean;
      definitionsReady: boolean;
    },
  ): void {
    if (!this.draft) return;
    if (this.draft.kind === 'createDefinition' && this.definitionCreationCommit) {
      if (this.definitionCreationCommit.kind === 'unknown') {
        this.definitionsError = unknownCreatedDefinitionMessage;
        this.captureFailure(new Error(unknownCreatedDefinitionMessage), 502);
        return;
      }
      if (ready.definitionsReady) {
        this.reconcileDefinitionCreation(this.draftGeneration);
        return;
      }
      this.captureFailure(results.definitions.error, results.definitions.response?.status ?? 0);
      return;
    }
    const target =
      this.draft.kind === 'rename' || this.draft.kind === 'delete'
        ? { result: results.person, ready: ready.personReady }
        : this.draft.kind === 'profile'
          ? { result: results.structuredProfile, ready: ready.structuredProfileReady }
          : this.draft.kind === 'setAttribute' || this.draft.kind === 'clearAttribute'
            ? { result: results.attributes, ready: ready.attributesReady }
            : { result: results.definitions, ready: ready.definitionsReady };
    if (target.ready) {
      this.draft = null;
      this.conflict = null;
      return;
    }
    if (target.result.data !== undefined) {
      this.conflict = {
        code: 'precondition_required',
        message: 'Reload response did not include a fresh ETag.',
        status: 428,
      };
      return;
    }
    this.captureFailure(target.result.error, target.result.response?.status ?? 0);
  }
  private reconcileDefinitionCreation(expectedDraftGeneration?: number): boolean {
    const commit = this.definitionCreationCommit;
    if (!commit) return true;
    if (commit.kind === 'unknown') {
      this.definitionsError = unknownCreatedDefinitionMessage;
      if (
        this.draft?.kind === 'createDefinition' &&
        (expectedDraftGeneration === undefined || this.draftGeneration === expectedDraftGeneration)
      ) {
        this.captureFailure(new Error(unknownCreatedDefinitionMessage), 502);
      }
      return false;
    }
    const refreshed = this.definitions.find(
      (definition) =>
        definition.universal_id === commit.universalID &&
        definition.slug === commit.slug &&
        definition.ownership === 'user' &&
        definition.object_type === 'person',
    );
    if (!refreshed) {
      this.definitionsError = missingCreatedDefinitionMessage;
      if (
        this.draft?.kind === 'createDefinition' &&
        (expectedDraftGeneration === undefined || this.draftGeneration === expectedDraftGeneration)
      ) {
        this.captureFailure(new Error(missingCreatedDefinitionMessage), 502);
      }
      return false;
    }
    this.createdDefinition = refreshed;
    this.definitionCreationCommit = null;
    if (
      this.draft?.kind === 'createDefinition' &&
      (expectedDraftGeneration === undefined || this.draftGeneration === expectedDraftGeneration)
    ) {
      this.draft = null;
      this.conflict = null;
    }
    return true;
  }
  private applyAttributeWrite(slug: string, value?: PersonAttributeValue, superseded?: PersonAttributeValue): void {
    if (!this.attributes) return;
    const attributes = this.attributes.attributes ?? [];
    let matched = false;
    const updated = attributes.map((group) => {
      if (group.definition.slug !== slug) return group;
      matched = true;
      return {
        ...group,
        current: value
          ? [...(group.current ?? []).filter((item) => item.id !== value.id && item.id !== superseded?.id), value]
          : (group.current ?? []).filter((item) => item.id !== superseded?.id),
        history: superseded
          ? [...(group.history ?? []).filter((item) => item.id !== superseded.id), superseded]
          : group.history,
      };
    });
    if (!matched) {
      const definition = this.definitions.find(
        (candidate) =>
          candidate.slug === slug &&
          (value === undefined || candidate.id === value.definition_id) &&
          (superseded === undefined || candidate.id === superseded.definition_id),
      );
      if (definition) {
        updated.push({
          definition,
          current: value ? [value] : [],
          history: superseded ? [superseded] : [],
        });
      }
    }
    this.attributes = {
      ...this.attributes,
      attributes: updated,
    };
    this.publish();
  }
  private async invalidateRow(update?: DirectoryPersonSummaryUpdate, refreshDirectory = false): Promise<void> {
    await this.options.invalidateRow?.(this.personID, update, refreshDirectory);
  }
  private missingETag(): void {
    this.conflict = { code: 'precondition_required', message: 'Reload before saving this person.', status: 428 };
  }
  private captureFailure(error: unknown, status: number): void {
    const details = errorDetails(error);
    const responseCode = typeof details.error === 'string' ? details.error : '';
    const attributeConflict =
      responseCode === 'attribute_value_conflict' ||
      (status === 409 && ('current_value_id' in details || 'current_value' in details));
    this.conflict = {
      code:
        responseCode === 'precondition_required'
          ? 'precondition_required'
          : responseCode === 'person_revision_conflict'
            ? 'person_revision_conflict'
            : attributeConflict
              ? 'attribute_conflict'
              : 'request_failed',
      message: typeof details.message === 'string' ? details.message : failureMessage(error, status),
      status,
      ...(isPersonAttributeValue(details.current_value) ? { currentValue: details.current_value } : {}),
      ...(typeof details.current_value_id === 'number' ? { currentValueID: details.current_value_id } : {}),
    };
  }
  private requiresReload(kind: DirectoryProfileDraft['kind']): boolean {
    return (
      this.draft?.kind === kind &&
      (this.conflict?.code === 'person_revision_conflict' || this.conflict?.code === 'precondition_required')
    );
  }
  private publish(): void {
    const etags = { ...this.bundle.etags };
    if (this.personETag === null) delete etags.person;
    else etags.person = this.personETag;
    if (this.structuredProfileETag === null) delete etags.structuredProfile;
    else etags.structuredProfile = this.structuredProfileETag;
    this.bundle = {
      ...this.bundle,
      ...(this.person === undefined ? {} : { person: this.person }),
      ...(this.structuredProfile === undefined ? {} : { structuredProfile: this.structuredProfile }),
      ...(this.attributes === undefined ? {} : { attributes: this.attributes }),
      definitions: { definitions: this.definitions },
      etags,
    };
    this.options.onDetailChange?.(this.bundle);
  }
}
async function settle<T>(
  request: Promise<{
    data?: T;
    error?: unknown;
    response: Response;
  }>,
): Promise<RequestResult<T>> {
  try {
    return await request;
  } catch (error: unknown) {
    return { error };
  }
}
function errorDetails(error: unknown): Record<string, unknown> {
  return typeof error === 'object' && error !== null ? (error as Record<string, unknown>) : {};
}
function failureMessage(error: unknown, status: number): string {
  const details = errorDetails(error);
  if (typeof details.message === 'string') return details.message;
  if (error instanceof Error && error.message) return error.message;
  return status ? `Request failed (${status})` : 'Request failed';
}
function hasETag(value: string | null | undefined): value is string {
  return typeof value === 'string' && /^"[^\"]+"$/.test(value);
}
function isPersonAttributeValue(value: unknown): value is PersonAttributeValue {
  return (
    typeof value === 'object' &&
    value !== null &&
    typeof (
      value as {
        id?: unknown;
      }
    ).id === 'number'
  );
}
function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null;
}
function hasUsableDefinitionIdentity(definition: Record<string, unknown>): definition is Record<string, unknown> & {
  universal_id: string;
  slug: string;
} {
  return (
    typeof definition.universal_id === 'string' &&
    definition.universal_id.trim() !== '' &&
    typeof definition.slug === 'string' &&
    definition.slug.trim() !== ''
  );
}
