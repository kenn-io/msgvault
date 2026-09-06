import {
  createEmployment as generatedCreateEmployment,
  createOrganization as generatedCreateOrganization,
  createPersonRelationship as generatedCreatePersonRelationship,
  createRelationshipType as generatedCreateRelationshipType,
  deleteEmployment as generatedDeleteEmployment,
  deleteOrganization as generatedDeleteOrganization,
  deletePersonRelationship as generatedDeletePersonRelationship,
  deleteRelationshipType as generatedDeleteRelationshipType,
  endEmployment as generatedEndEmployment,
  getEmployment as generatedGetEmployment,
  getOrganization as generatedGetOrganization,
  getPersonNetwork as generatedGetPersonNetwork,
  getPersonRelationship as generatedGetPersonRelationship,
  getRelationshipType as generatedGetRelationshipType,
  listOrganizations as generatedListOrganizations,
  listPersonEmployments as generatedListPersonEmployments,
  listPersonRelationships as generatedListPersonRelationships,
  listRelationshipTypes as generatedListRelationshipTypes,
  patchEmployment as generatedPatchEmployment,
  patchOrganization as generatedPatchOrganization,
  patchPersonRelationship as generatedPatchPersonRelationship,
  patchRelationshipType as generatedPatchRelationshipType,
  putOrganizationProfile as generatedPutOrganizationProfile,
  setPrimaryEmployment as generatedSetPrimaryEmployment,
} from '../api/generated/api/api';
import { SvelteMap } from 'svelte/reactivity';
import type { APIClient } from '../api/client';
import type {
  CreatePersonRelationshipRequest,
  CreateRelationshipTypeRequest,
  DirectoryEntityCreateResource,
  DirectoryEntityMutationResult,
  DirectoryEntityResource,
  Employment,
  EmploymentBody,
  EmploymentProjectionResponse,
  EndEmploymentBody,
  Organization,
  OrganizationBody,
  OrganizationCreateBody,
  OrganizationProfile,
  OrganizationProfileBody,
  PatchPersonRelationshipRequest,
  PatchRelationshipTypeRequest,
  PersonNetwork,
  PersonRelationship,
  PersonRelationshipView,
  RelationshipType,
} from './models';
type RequestResult<T> = {
  data?: T;
  error?: unknown;
  response: Response;
};
interface DirectoryEntityControllerOptions {
  onDirectoryChange?: () => void | Promise<void>;
}
const unknownCreateMessage =
  'The create request may have succeeded, but its response was lost. Refresh this collection before submitting it again.';
const blockedCreateMessage = 'Refresh this collection before retrying the create request.';
/**
 * Owns organization, employment, relationship and network state whose
 * lifetime is exactly one selected durable person. URL and profile state stay
 * with their existing owners.
 */
export class DirectoryEntityController {
  organizations = $state<Organization[]>([]);
  employments = $state<Employment[]>([]);
  employmentProjection = $state<EmploymentProjectionResponse>();
  relationships = $state<PersonRelationshipView[]>([]);
  relationshipsIncludeEnded = $state(false);
  relationshipTypes = $state<RelationshipType[]>([]);
  network = $state<PersonNetwork | null>(null);
  organizationRecords = new SvelteMap<number, OrganizationProfile>();
  employmentRecords = new SvelteMap<number, Employment>();
  relationshipRecords = new SvelteMap<number, PersonRelationship>();
  relationshipTypeRecords = new SvelteMap<number, RelationshipType>();
  organizationETags = new SvelteMap<number, string>();
  employmentETags = new SvelteMap<number, string>();
  relationshipETags = new SvelteMap<number, string>();
  relationshipTypeETags = new SvelteMap<number, string>();
  loading = $state<Record<DirectoryEntityResource, boolean>>({
    organizations: false,
    employments: false,
    relationships: false,
    relationshipTypes: false,
    network: false,
  });
  errors = $state<Partial<Record<DirectoryEntityResource, string>>>({});
  createBlocked = $state<Record<DirectoryEntityCreateResource, boolean>>({
    organizations: false,
    employments: false,
    relationships: false,
    relationshipTypes: false,
  });
  private readonly client: APIClient;
  private readonly options: DirectoryEntityControllerOptions;
  private organizationAbort: AbortController | undefined;
  private employmentAbort: AbortController | undefined;
  private relationshipAbort: AbortController | undefined;
  private relationshipTypeAbort: AbortController | undefined;
  private networkAbort: AbortController | undefined;
  private readonly entityRequests = new Set<AbortController>();
  private organizationGeneration = 0;
  private employmentGeneration = 0;
  private relationshipGeneration = 0;
  private relationshipTypeGeneration = 0;
  private networkGeneration = 0;
  private disposed = false;
  constructor(
    client: APIClient,
    readonly personID: number,
    options: DirectoryEntityControllerOptions = {},
  ) {
    this.client = client;
    this.options = options;
  }
  get organizationsLoading(): boolean {
    return this.loading.organizations;
  }
  get employmentsLoading(): boolean {
    return this.loading.employments;
  }
  get relationshipsLoading(): boolean {
    return this.loading.relationships;
  }
  get relationshipTypesLoading(): boolean {
    return this.loading.relationshipTypes;
  }
  get networkLoading(): boolean {
    return this.loading.network;
  }
  /** The network stays lazy: PersonNetwork requests it when its tab opens. */
  async load(): Promise<void> {
    await Promise.all([this.refreshEmployments(), this.refreshRelationships(), this.refreshRelationshipTypes()]);
  }
  async refreshOrganizations(query = ''): Promise<void> {
    const { abort, generation } = this.beginCollection('organizations');
    try {
      const response = await generatedListOrganizations(
        { limit: 50, offset: 0, ...(query.trim() ? { q: query.trim() } : {}) },
        {
          ...this.client,
          signal: abort.signal,
        },
      );
      if (!this.owns('organizations', abort, generation)) return;
      if (response.data) {
        this.organizations = response.data.organizations ?? [];
        this.createBlocked.organizations = false;
        delete this.errors.organizations;
      } else {
        this.errors.organizations = failureMessage(response.error, response.response.status);
      }
    } catch (cause: unknown) {
      if (this.owns('organizations', abort, generation)) this.errors.organizations = failureMessage(cause, 0);
    } finally {
      if (generation === this.organizationGeneration) this.loading.organizations = false;
    }
  }
  async refreshEmployments(): Promise<void> {
    await this.loadEmployments(true);
  }
  private async loadEmployments(clearCreateBlock: boolean): Promise<boolean> {
    const { abort, generation } = this.beginCollection('employments');
    try {
      const response = await generatedListPersonEmployments({ id: this.personID }, undefined, {
        ...this.client,
        signal: abort.signal,
      });
      if (!this.owns('employments', abort, generation)) return false;
      if (response.data) {
        this.employments = response.data.employments ?? [];
        this.employmentProjection = response.data.projection;
        if (clearCreateBlock) this.createBlocked.employments = false;
        delete this.errors.employments;
        return true;
      } else {
        this.errors.employments = failureMessage(response.error, response.response.status);
        return false;
      }
    } catch (cause: unknown) {
      if (this.owns('employments', abort, generation)) this.errors.employments = failureMessage(cause, 0);
      return false;
    } finally {
      if (generation === this.employmentGeneration) this.loading.employments = false;
    }
  }
  async refreshRelationships(includeEnded = this.relationshipsIncludeEnded): Promise<void> {
    this.relationshipsIncludeEnded = includeEnded;
    await this.loadRelationships(true);
  }
  private async loadRelationships(clearCreateBlock: boolean): Promise<boolean> {
    const { abort, generation } = this.beginCollection('relationships');
    try {
      const response = await generatedListPersonRelationships(
        { id: this.personID },
        { include_ended: this.relationshipsIncludeEnded },
        {
          ...this.client,
          signal: abort.signal,
        },
      );
      if (!this.owns('relationships', abort, generation)) return false;
      if (response.data) {
        this.relationships = response.data.relationships ?? [];
        if (clearCreateBlock) this.createBlocked.relationships = false;
        delete this.errors.relationships;
        return true;
      } else {
        this.errors.relationships = failureMessage(response.error, response.response.status);
        return false;
      }
    } catch (cause: unknown) {
      if (this.owns('relationships', abort, generation)) this.errors.relationships = failureMessage(cause, 0);
      return false;
    } finally {
      if (generation === this.relationshipGeneration) this.loading.relationships = false;
    }
  }
  async refreshRelationshipTypes(): Promise<void> {
    const { abort, generation } = this.beginCollection('relationshipTypes');
    try {
      const response = await generatedListRelationshipTypes({
        ...this.client,
        signal: abort.signal,
      });
      if (!this.owns('relationshipTypes', abort, generation)) return;
      if (response.data) {
        this.relationshipTypes = response.data.relationship_types ?? [];
        this.createBlocked.relationshipTypes = false;
        delete this.errors.relationshipTypes;
      } else {
        this.errors.relationshipTypes = failureMessage(response.error, response.response.status);
      }
    } catch (cause: unknown) {
      if (this.owns('relationshipTypes', abort, generation)) this.errors.relationshipTypes = failureMessage(cause, 0);
    } finally {
      if (generation === this.relationshipTypeGeneration) this.loading.relationshipTypes = false;
    }
  }
  async loadNetwork(depth = 1, includeEnded = false): Promise<void> {
    const { abort, generation } = this.beginCollection('network');
    try {
      const response = await generatedGetPersonNetwork(
        { id: this.personID },
        { depth, include_ended: includeEnded },
        {
          ...this.client,
          signal: abort.signal,
        },
      );
      if (!this.owns('network', abort, generation)) return;
      if (response.data) {
        this.network = response.data;
        delete this.errors.network;
      } else {
        this.errors.network = failureMessage(response.error, response.response.status);
      }
    } catch (cause: unknown) {
      if (this.owns('network', abort, generation)) this.errors.network = failureMessage(cause, 0);
    } finally {
      if (generation === this.networkGeneration) this.loading.network = false;
    }
  }
  async prepareOrganizationMutation(id: number): Promise<OrganizationProfile> {
    const response = await this.getMutable((signal) =>
      generatedGetOrganization(
        { id: id },
        {
          ...this.client,
          signal,
        },
      ),
    );
    this.organizationRecords.set(id, response.data);
    this.organizationETags.set(id, response.etag);
    return response.data;
  }
  async prepareEmploymentMutation(id: number): Promise<Employment> {
    const response = await this.getMutable((signal) =>
      generatedGetEmployment(
        { id: id },
        {
          ...this.client,
          signal,
        },
      ),
    );
    this.employmentRecords.set(id, response.data);
    this.employmentETags.set(id, response.etag);
    return response.data;
  }
  async prepareRelationshipMutation(id: number): Promise<PersonRelationship> {
    const response = await this.getMutable((signal) =>
      generatedGetPersonRelationship(
        { id: id },
        {
          ...this.client,
          signal,
        },
      ),
    );
    this.relationshipRecords.set(id, response.data);
    this.relationshipETags.set(id, response.etag);
    return response.data;
  }
  async prepareRelationshipTypeMutation(id: number): Promise<RelationshipType> {
    const response = await this.getMutable((signal) =>
      generatedGetRelationshipType(
        { id: id },
        {
          ...this.client,
          signal,
        },
      ),
    );
    this.relationshipTypeRecords.set(id, response.data);
    this.relationshipTypeETags.set(id, response.etag);
    return response.data;
  }
  async createOrganization(body: OrganizationCreateBody): Promise<DirectoryEntityMutationResult<Organization>> {
    const result = await this.create(
      'organizations',
      (signal) =>
        generatedCreateOrganization(body, {
          ...this.client,
          signal,
        }),
      (entity, response) => {
        this.organizations = replaceByID(this.organizations, entity);
        this.captureETag(this.organizationETags, entity.id, response);
      },
    );
    return this.afterDirectoryMutation(result);
  }
  async updateOrganization(
    id: number,
    body: OrganizationBody,
  ): Promise<DirectoryEntityMutationResult<Organization, OrganizationProfile>> {
    const result = await this.writeExisting(
      () => this.prepareOrganizationMutation(id),
      (_current, signal) =>
        generatedPatchOrganization({ id: id }, body, {
          ...this.client,
          signal,
          headers: { 'If-Match': this.organizationETags.get(id)! },
        }),
      (entity, response) => {
        this.organizations = replaceByID(this.organizations, entity);
        const current = this.organizationRecords.get(id);
        if (current) this.organizationRecords.set(id, { ...current, organization: entity });
        this.captureETag(this.organizationETags, id, response);
      },
    );
    return this.afterDirectoryMutation(result);
  }
  async putOrganizationProfile(
    id: number,
    buildBody: (current: OrganizationProfile) => OrganizationProfileBody,
  ): Promise<DirectoryEntityMutationResult<OrganizationProfile, OrganizationProfile>> {
    const result = await this.writeExisting(
      () => this.prepareOrganizationMutation(id),
      (current, signal) =>
        generatedPutOrganizationProfile({ id: id }, buildBody(current), {
          ...this.client,
          signal,
          headers: { 'If-Match': this.organizationETags.get(id)! },
        }),
      (profile, response) => {
        this.organizationRecords.set(id, profile);
        this.organizations = replaceByID(this.organizations, profile.organization);
        this.captureETag(this.organizationETags, id, response);
      },
    );
    return this.afterDirectoryMutation(result);
  }
  async deleteOrganization(id: number): Promise<DirectoryEntityMutationResult<never, OrganizationProfile>> {
    const result = await this.deleteExisting(
      () => this.prepareOrganizationMutation(id),
      (signal) =>
        generatedDeleteOrganization(
          { id: id },
          {
            ...this.client,
            signal,
            headers: { 'If-Match': this.organizationETags.get(id)! },
          },
        ),
      () => {
        this.organizations = this.organizations.filter((item) => item.id !== id);
        this.organizationRecords.delete(id);
        this.organizationETags.delete(id);
      },
    );
    return this.afterDirectoryMutation(result);
  }
  async createEmployment(body: EmploymentBody): Promise<DirectoryEntityMutationResult<Employment>> {
    const result = await this.create(
      'employments',
      (signal) =>
        generatedCreateEmployment(body, {
          ...this.client,
          signal,
        }),
      (entity, response) => {
        this.applyEmploymentEntity(entity, response);
      },
    );
    if (result.ok) await this.loadEmployments(false);
    return this.afterDirectoryMutation(result);
  }
  async updateEmployment(
    id: number,
    buildBody: (current: Employment) => EmploymentBody,
  ): Promise<DirectoryEntityMutationResult<Employment>> {
    return this.writeEmployment(id, (current, signal) =>
      generatedPatchEmployment({ id: id }, buildBody(current), {
        ...this.client,
        signal,
        headers: { 'If-Match': this.employmentETags.get(id)! },
      }),
    );
  }
  async endEmployment(id: number, body: EndEmploymentBody): Promise<DirectoryEntityMutationResult<Employment>> {
    return this.writeEmployment(id, (_current, signal) =>
      generatedEndEmployment({ id: id }, body, {
        ...this.client,
        signal,
        headers: { 'If-Match': this.employmentETags.get(id)! },
      }),
    );
  }
  async makeEmploymentPrimary(id: number): Promise<DirectoryEntityMutationResult<Employment>> {
    return this.writeEmployment(id, (_current, signal) =>
      generatedSetPrimaryEmployment(
        { id: id },
        {
          ...this.client,
          signal,
          headers: { 'If-Match': this.employmentETags.get(id)! },
        },
      ),
    );
  }
  async deleteEmployment(id: number): Promise<DirectoryEntityMutationResult<never, Employment>> {
    const result = await this.deleteExisting(
      () => this.prepareEmploymentMutation(id),
      (signal) =>
        generatedDeleteEmployment(
          { id: id },
          {
            ...this.client,
            signal,
            headers: { 'If-Match': this.employmentETags.get(id)! },
          },
        ),
      () => {
        this.employments = this.employments.filter((item) => item.id !== id);
        this.employmentProjection = undefined;
        this.employmentRecords.delete(id);
        this.employmentETags.delete(id);
      },
    );
    if (result.ok) await this.loadEmployments(false);
    return this.afterDirectoryMutation(result);
  }
  async createRelationship(
    body: CreatePersonRelationshipRequest,
  ): Promise<DirectoryEntityMutationResult<PersonRelationship>> {
    const result = await this.create(
      'relationships',
      (signal) =>
        generatedCreatePersonRelationship(body, {
          ...this.client,
          signal,
        }),
      (entity, response) => {
        this.relationshipRecords.set(entity.id, entity);
        this.captureETag(this.relationshipETags, entity.id, response);
      },
    );
    if (result.ok) await this.loadRelationships(false);
    return result;
  }
  async updateRelationship(
    id: number,
    body: PatchPersonRelationshipRequest,
  ): Promise<DirectoryEntityMutationResult<PersonRelationship>> {
    const result = await this.writeExisting(
      () => this.prepareRelationshipMutation(id),
      (_current, signal) =>
        generatedPatchPersonRelationship({ id: id }, body, {
          ...this.client,
          signal,
          headers: { 'If-Match': this.relationshipETags.get(id)! },
        }),
      (entity, response) => {
        this.relationships = this.relationships.map((view) =>
          view.relationship.id === id ? { ...view, relationship: entity } : view,
        );
        this.relationshipRecords.set(id, entity);
        this.captureETag(this.relationshipETags, id, response);
      },
    );
    if (result.ok) await this.loadRelationships(false);
    return result;
  }
  async deleteRelationship(id: number): Promise<DirectoryEntityMutationResult<never, PersonRelationship>> {
    const result = await this.deleteExisting(
      () => this.prepareRelationshipMutation(id),
      (signal) =>
        generatedDeletePersonRelationship(
          { id: id },
          {
            ...this.client,
            signal,
            headers: { 'If-Match': this.relationshipETags.get(id)! },
          },
        ),
      () => {
        this.relationships = this.relationships.filter((view) => view.relationship.id !== id);
        this.relationshipRecords.delete(id);
        this.relationshipETags.delete(id);
      },
    );
    if (result.ok) await this.loadRelationships(false);
    return result;
  }
  async createRelationshipType(
    body: CreateRelationshipTypeRequest,
  ): Promise<DirectoryEntityMutationResult<RelationshipType>> {
    return this.create(
      'relationshipTypes',
      (signal) =>
        generatedCreateRelationshipType(body, {
          ...this.client,
          signal,
        }),
      (entity, response) => {
        this.relationshipTypes = replaceByID(this.relationshipTypes, entity);
        this.relationshipTypeRecords.set(entity.id, entity);
        this.captureETag(this.relationshipTypeETags, entity.id, response);
      },
    );
  }
  async updateRelationshipType(
    id: number,
    body: PatchRelationshipTypeRequest,
  ): Promise<DirectoryEntityMutationResult<RelationshipType>> {
    let prepared: RelationshipType;
    try {
      prepared = await this.prepareRelationshipTypeMutation(id);
    } catch (cause: unknown) {
      return { ok: false, kind: 'error', status: 0, message: failureMessage(cause, 0) };
    }
    if (prepared.ownership === 'system') {
      return { ok: false, kind: 'error', status: 403, message: 'System relationship types are read-only.' };
    }
    const result = await this.writeExisting(
      () => this.prepareRelationshipTypeMutation(id),
      (_current, signal) =>
        generatedPatchRelationshipType({ id: id }, body, {
          ...this.client,
          signal,
          headers: { 'If-Match': this.relationshipTypeETags.get(id)! },
        }),
      (entity, response) => {
        this.relationshipTypes = replaceByID(this.relationshipTypes, entity);
        this.relationshipTypeRecords.set(id, entity);
        this.captureETag(this.relationshipTypeETags, id, response);
      },
      prepared,
    );
    if (result.ok) await this.loadRelationships(false);
    return result;
  }
  async deleteRelationshipType(id: number): Promise<DirectoryEntityMutationResult<never, RelationshipType>> {
    let prepared: RelationshipType;
    try {
      prepared = await this.prepareRelationshipTypeMutation(id);
    } catch (cause: unknown) {
      return { ok: false, kind: 'error', status: 0, message: failureMessage(cause, 0) };
    }
    if (prepared.ownership === 'system') {
      return { ok: false, kind: 'error', status: 403, message: 'System relationship types are read-only.' };
    }
    if (!prepared.is_deletable) {
      return { ok: false, kind: 'error', status: 403, message: 'This relationship type cannot be deleted.' };
    }
    return this.deleteExisting(
      () => this.prepareRelationshipTypeMutation(id),
      (signal) =>
        generatedDeleteRelationshipType(
          { id: id },
          {
            ...this.client,
            signal,
            headers: { 'If-Match': this.relationshipTypeETags.get(id)! },
          },
        ),
      () => {
        this.relationshipTypes = this.relationshipTypes.filter((item) => item.id !== id);
        this.relationshipTypeRecords.delete(id);
        this.relationshipTypeETags.delete(id);
      },
      prepared,
    );
  }
  destroy(): void {
    this.disposed = true;
    this.organizationAbort?.abort();
    this.employmentAbort?.abort();
    this.relationshipAbort?.abort();
    this.relationshipTypeAbort?.abort();
    this.networkAbort?.abort();
    for (const request of this.entityRequests) request.abort();
    this.entityRequests.clear();
    ++this.organizationGeneration;
    ++this.employmentGeneration;
    ++this.relationshipGeneration;
    ++this.relationshipTypeGeneration;
    ++this.networkGeneration;
    this.loading.organizations = false;
    this.loading.employments = false;
    this.loading.relationships = false;
    this.loading.relationshipTypes = false;
    this.loading.network = false;
  }
  private async writeEmployment(
    id: number,
    write: (current: Employment, signal: AbortSignal) => Promise<RequestResult<Employment>>,
  ): Promise<DirectoryEntityMutationResult<Employment>> {
    const result = await this.writeExisting(
      () => this.prepareEmploymentMutation(id),
      write,
      (entity, response) => {
        this.applyEmploymentEntity(entity, response);
      },
    );
    if (result.ok) await this.loadEmployments(false);
    return this.afterDirectoryMutation(result);
  }
  private async afterDirectoryMutation<T, C>(
    result: DirectoryEntityMutationResult<T, C>,
  ): Promise<DirectoryEntityMutationResult<T, C>> {
    if (result.ok) await this.options.onDirectoryChange?.();
    return result;
  }
  private applyEmploymentEntity(entity: Employment, response: Response): void {
    if (entity.person_id !== this.personID) {
      this.employments = this.employments.filter((item) => item.id !== entity.id);
    } else {
      const demotedIDs = new Set(
        entity.is_primary
          ? this.employments
              .filter((item) => item.id !== entity.id && item.person_id === this.personID && item.is_primary)
              .map((item) => item.id)
          : [],
      );
      const visible = entity.is_primary
        ? this.employments.map((item) => (demotedIDs.has(item.id) ? { ...item, is_primary: false } : item))
        : this.employments;
      for (const id of demotedIDs) {
        this.employmentRecords.delete(id);
        this.employmentETags.delete(id);
      }
      this.employments = replaceByID(visible, entity);
    }
    this.employmentProjection = undefined;
    this.employmentRecords.set(entity.id, entity);
    this.captureETag(this.employmentETags, entity.id, response);
  }
  private async create<T>(
    resource: DirectoryEntityCreateResource,
    send: (signal: AbortSignal) => Promise<RequestResult<T>>,
    apply: (entity: T, response: Response) => void,
  ): Promise<DirectoryEntityMutationResult<T>> {
    if (this.createBlocked[resource]) return { ok: false, kind: 'blocked', message: blockedCreateMessage };
    const abort = this.beginEntityRequest();
    try {
      const response = await send(abort.signal);
      if (!this.isActive(abort)) return { ok: false, kind: 'error', status: 0, message: 'Request was cancelled.' };
      if (response.data !== undefined) {
        apply(response.data, response.response);
        return { ok: true, entity: response.data };
      }
      if (response.response.status >= 500) {
        return this.markCreateUnknown(resource, failureMessage(response.error, response.response.status));
      }
      return {
        ok: false,
        kind: 'error',
        status: response.response.status,
        message: failureMessage(response.error, response.response.status),
      };
    } catch (cause: unknown) {
      if (!this.isActive(abort)) return { ok: false, kind: 'error', status: 0, message: 'Request was cancelled.' };
      return this.markCreateUnknown(resource, failureMessage(cause, 0));
    } finally {
      this.entityRequests.delete(abort);
    }
  }
  private async writeExisting<T, C>(
    prepare: () => Promise<C>,
    send: (current: C, signal: AbortSignal) => Promise<RequestResult<T>>,
    apply: (entity: T, response: Response) => void,
    prepared?: C,
  ): Promise<DirectoryEntityMutationResult<T, C>> {
    let current: C;
    try {
      current = prepared ?? (await prepare());
    } catch (cause: unknown) {
      return { ok: false, kind: 'error', status: 0, message: failureMessage(cause, 0) };
    }
    const abort = this.beginEntityRequest();
    try {
      const response = await send(current, abort.signal);
      if (!this.isActive(abort)) return { ok: false, kind: 'error', status: 0, message: 'Request was cancelled.' };
      if (response.data !== undefined) {
        apply(response.data, response.response);
        return { ok: true, entity: response.data };
      }
      const status = response.response.status;
      if (status === 409 || status === 412) {
        let current: C | undefined;
        try {
          current = await prepare();
        } catch {
          /* retain the conflict even when the exact refresh fails */
        }
        return {
          ok: false,
          kind: 'conflict',
          status,
          message: failureMessage(response.error, status),
          ...(current === undefined ? {} : { current }),
        };
      }
      return { ok: false, kind: 'error', status, message: failureMessage(response.error, status) };
    } catch (cause: unknown) {
      return { ok: false, kind: 'error', status: 0, message: failureMessage(cause, 0) };
    } finally {
      this.entityRequests.delete(abort);
    }
  }
  private async deleteExisting<C>(
    prepare: () => Promise<C>,
    send: (signal: AbortSignal) => Promise<RequestResult<void>>,
    apply: () => void,
    prepared?: C,
  ): Promise<DirectoryEntityMutationResult<never, C>> {
    try {
      if (prepared === undefined) await prepare();
    } catch (cause: unknown) {
      return { ok: false, kind: 'error', status: 0, message: failureMessage(cause, 0) };
    }
    const abort = this.beginEntityRequest();
    try {
      const response = await send(abort.signal);
      if (!this.isActive(abort)) return { ok: false, kind: 'error', status: 0, message: 'Request was cancelled.' };
      if (response.response.status === 204) {
        apply();
        return { ok: true };
      }
      const status = response.response.status;
      if (status === 409 || status === 412) {
        let current: C | undefined;
        try {
          current = await prepare();
        } catch {
          /* retain the conflict even when the exact refresh fails */
        }
        return {
          ok: false,
          kind: 'conflict',
          status,
          message: failureMessage(response.error, status),
          ...(current === undefined ? {} : { current }),
        };
      }
      return { ok: false, kind: 'error', status, message: failureMessage(response.error, status) };
    } catch (cause: unknown) {
      return { ok: false, kind: 'error', status: 0, message: failureMessage(cause, 0) };
    } finally {
      this.entityRequests.delete(abort);
    }
  }
  private async getMutable<T>(send: (signal: AbortSignal) => Promise<RequestResult<T>>): Promise<{
    data: T;
    etag: string;
  }> {
    const abort = this.beginEntityRequest();
    try {
      const response = await send(abort.signal);
      if (!this.isActive(abort)) throw new Error('Request was cancelled.');
      const etag = response.response.headers.get('ETag');
      if (response.data === undefined) throw new Error(failureMessage(response.error, response.response.status));
      if (!etag) throw new Error('The response did not include an ETag.');
      return { data: response.data, etag };
    } finally {
      this.entityRequests.delete(abort);
    }
  }
  private beginEntityRequest(): AbortController {
    const abort = new AbortController();
    this.entityRequests.add(abort);
    return abort;
  }
  private isActive(abort: AbortController): boolean {
    return !this.disposed && !abort.signal.aborted;
  }
  private markCreateUnknown<T>(
    resource: DirectoryEntityCreateResource,
    detail: string,
  ): DirectoryEntityMutationResult<T> {
    this.createBlocked[resource] = true;
    this.invalidateCollection(resource);
    return { ok: false, kind: 'unknown', message: `${unknownCreateMessage} ${detail}` };
  }
  private invalidateCollection(resource: DirectoryEntityCreateResource): void {
    switch (resource) {
      case 'organizations':
        this.organizationAbort?.abort();
        ++this.organizationGeneration;
        break;
      case 'employments':
        this.employmentAbort?.abort();
        ++this.employmentGeneration;
        break;
      case 'relationships':
        this.relationshipAbort?.abort();
        ++this.relationshipGeneration;
        break;
      case 'relationshipTypes':
        this.relationshipTypeAbort?.abort();
        ++this.relationshipTypeGeneration;
        break;
    }
    this.loading[resource] = false;
  }
  private beginCollection(resource: DirectoryEntityResource): {
    abort: AbortController;
    generation: number;
  } {
    const abort = new AbortController();
    let generation: number;
    switch (resource) {
      case 'organizations':
        this.organizationAbort?.abort();
        this.organizationAbort = abort;
        generation = ++this.organizationGeneration;
        break;
      case 'employments':
        this.employmentAbort?.abort();
        this.employmentAbort = abort;
        generation = ++this.employmentGeneration;
        break;
      case 'relationships':
        this.relationshipAbort?.abort();
        this.relationshipAbort = abort;
        generation = ++this.relationshipGeneration;
        break;
      case 'relationshipTypes':
        this.relationshipTypeAbort?.abort();
        this.relationshipTypeAbort = abort;
        generation = ++this.relationshipTypeGeneration;
        break;
      case 'network':
        this.networkAbort?.abort();
        this.networkAbort = abort;
        generation = ++this.networkGeneration;
        break;
    }
    this.loading[resource] = true;
    delete this.errors[resource];
    return { abort, generation };
  }
  private owns(resource: DirectoryEntityResource, abort: AbortController, generation: number): boolean {
    if (this.disposed || abort.signal.aborted) return false;
    switch (resource) {
      case 'organizations':
        return this.organizationAbort === abort && this.organizationGeneration === generation;
      case 'employments':
        return this.employmentAbort === abort && this.employmentGeneration === generation;
      case 'relationships':
        return this.relationshipAbort === abort && this.relationshipGeneration === generation;
      case 'relationshipTypes':
        return this.relationshipTypeAbort === abort && this.relationshipTypeGeneration === generation;
      case 'network':
        return this.networkAbort === abort && this.networkGeneration === generation;
    }
  }
  private captureETag(map: SvelteMap<number, string>, id: number, response: Response): void {
    const etag = response.headers.get('ETag');
    if (etag) map.set(id, etag);
  }
}
function replaceByID<
  T extends {
    id: number;
  },
>(items: T[], replacement: T): T[] {
  return items.some((item) => item.id === replacement.id)
    ? items.map((item) => (item.id === replacement.id ? replacement : item))
    : [...items, replacement];
}
function failureMessage(error: unknown, status: number): string {
  if (typeof error === 'object' && error !== null && 'message' in error && typeof error.message === 'string')
    return error.message;
  if (error instanceof Error && error.message) return error.message;
  return status > 0 ? `Request failed (${status}).` : 'Request failed.';
}
