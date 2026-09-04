import type { components } from '../api/generated/schema';

export interface DirectoryURLState {
  directoryQuery: string;
  directoryContactState: string;
  directoryCategory: string;
  directoryOrganization: string;
  directoryPrimaryChannel: string;
  directoryLastContactAfter: string;
  directoryLastContactBefore: string;
  directorySort: 'name' | 'last_contact_desc' | 'last_contact_asc';
  directoryPersonID: number | null;
}

export type DirectoryPerson = components['schemas']['DirectoryPersonSummary'];
export interface DirectoryPersonSummaryUpdate {
  categories?: string[];
}
export type PersonProfilePatchRequest = components['schemas']['PersonProfilePatchRequest'];
export type PatchPersonRequest = components['schemas']['PatchPersonRequest'];
export type SetPersonAttributeRequest = components['schemas']['SetPersonAttributeRequest'];
export type CreateAttributeDefinitionRequest = components['schemas']['CreateAttributeDefinitionRequest'];
export type AttributeDefinition = components['schemas']['AttributeDefinition'];
export type PersonAttributeValue = components['schemas']['PersonAttributeValue'];
export type Organization = components['schemas']['Organization'];
export type OrganizationProfile = components['schemas']['OrganizationProfile'];
export type OrganizationProfileBody = components['schemas']['OrganizationProfileBody'];
export type OrganizationBody = components['schemas']['OrganizationBody'];
export type OrganizationCreateBody = components['schemas']['OrganizationCreateBody'];
export type Employment = components['schemas']['Employment'];
export type EmploymentBody = components['schemas']['EmploymentBody'];
export type EndEmploymentBody = components['schemas']['EndEmploymentBody'];
export type EmploymentProjectionResponse = components['schemas']['EmploymentProjectionResponse'];
export type PersonRelationship = components['schemas']['PersonRelationship'];
export type PersonRelationshipView = components['schemas']['PersonRelationshipView'];
export type CreatePersonRelationshipRequest = components['schemas']['CreatePersonRelationshipRequest'];
export type PatchPersonRelationshipRequest = components['schemas']['PatchPersonRelationshipRequest'];
export type RelationshipType = components['schemas']['RelationshipType'];
export type CreateRelationshipTypeRequest = components['schemas']['CreateRelationshipTypeRequest'];
export type PatchRelationshipTypeRequest = components['schemas']['PatchRelationshipTypeRequest'];
export type PersonNetwork = components['schemas']['PersonNetwork'];
export type NetworkNode = components['schemas']['NetworkNode'];
export type NetworkEdge = components['schemas']['NetworkEdge'];

export type DirectoryEntityResource = 'organizations' | 'employments' | 'relationships' | 'relationshipTypes' | 'network';
export type DirectoryEntityCreateResource = Exclude<DirectoryEntityResource, 'network'>;

export type DirectoryEntityMutationResult<T, C = T> =
  | { ok: true; entity?: T }
  | { ok: false; kind: 'conflict'; status: 409 | 412; message: string; current?: C }
  | { ok: false; kind: 'unknown'; message: string }
  | { ok: false; kind: 'blocked'; message: string }
  | { ok: false; kind: 'error'; status: number; message: string };

export type DirectoryProfileDraft =
  | { kind: 'rename'; body: PatchPersonRequest }
  | { kind: 'delete' }
  | { kind: 'profile'; body: PersonProfilePatchRequest }
  | { kind: 'setAttribute'; slug: string; body: SetPersonAttributeRequest }
  | { kind: 'clearAttribute'; slug: string; expectedValueID: number; ordinal?: number }
  | { kind: 'createDefinition'; body: CreateAttributeDefinitionRequest };

export interface DirectoryProfileConflict {
  code: 'person_revision_conflict' | 'attribute_conflict' | 'precondition_required' | 'request_failed' | 'mutation_in_progress';
  message: string;
  status: number;
  currentValue?: PersonAttributeValue;
  currentValueID?: number;
}

export type DirectoryProfileOperationResult =
  | { ok: true }
  | { ok: false; code: 'mutation_in_progress' | 'reload_in_progress' | 'conflict_unresolved' };

export type DirectoryProfileOperationBlocked = Extract<DirectoryProfileOperationResult, { ok: false }>;

export interface DirectoryReadBundle {
  person?: components['schemas']['Person'];
  structuredProfile?: components['schemas']['StructuredPersonProfile'];
  attributes?: components['schemas']['PersonAttributesResponse'];
  definitions?: components['schemas']['AttributeDefinitionsResponse'];
  contactState?: components['schemas']['ContactState'];
  activity?: components['schemas']['PersonDaysPage'];
  files?: components['schemas']['PersonFileSearchHTTPResponse'];
  /** ETags are retained by resource so later editing can use exact reads. */
  etags: Partial<Record<DirectoryReadSection, string>>;
  /** A failed section stays absent; the detail never fabricates an empty one. */
  errors: Partial<Record<DirectoryReadSection, string>>;
}

export type DirectoryReadSection =
  | 'person'
  | 'structuredProfile'
  | 'attributes'
  | 'contactState'
  | 'activity'
  | 'files';

export type DirectoryPromotionResult =
  | { ok: true; personID: number }
  | { ok: false; code: 'person_binding_conflict' | 'error'; message: string };
