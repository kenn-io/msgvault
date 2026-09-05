import type {
  AttributeDefinition as GeneratedAttributeDefinition,
  AttributeDefinitionsResponse as GeneratedAttributeDefinitionsResponse,
  ContactState as GeneratedContactState,
  CreateAttributeDefinitionRequest as GeneratedCreateAttributeDefinitionRequest,
  CreatePersonRelationshipRequest as GeneratedCreatePersonRelationshipRequest,
  CreateRelationshipTypeRequest as GeneratedCreateRelationshipTypeRequest,
  DirectoryPersonSummary as GeneratedDirectoryPersonSummary,
  Employment as GeneratedEmployment,
  EmploymentBody as GeneratedEmploymentBody,
  EmploymentProjectionResponse as GeneratedEmploymentProjectionResponse,
  EndEmploymentBody as GeneratedEndEmploymentBody,
  NetworkEdge as GeneratedNetworkEdge,
  NetworkNode as GeneratedNetworkNode,
  Organization as GeneratedOrganization,
  OrganizationBody as GeneratedOrganizationBody,
  OrganizationCreateBody as GeneratedOrganizationCreateBody,
  OrganizationProfile as GeneratedOrganizationProfile,
  OrganizationProfileBody as GeneratedOrganizationProfileBody,
  PatchPersonRelationshipRequest as GeneratedPatchPersonRelationshipRequest,
  PatchPersonRequest as GeneratedPatchPersonRequest,
  PatchRelationshipTypeRequest as GeneratedPatchRelationshipTypeRequest,
  Person as GeneratedPerson,
  PersonAttributeValue as GeneratedPersonAttributeValue,
  PersonAttributesResponse as GeneratedPersonAttributesResponse,
  PersonDaysPage as GeneratedPersonDaysPage,
  PersonFileSearchHTTPResponse as GeneratedPersonFileSearchHTTPResponse,
  PersonNetwork as GeneratedPersonNetwork,
  PersonProfilePatchRequest as GeneratedPersonProfilePatchRequest,
  PersonRelationship as GeneratedPersonRelationship,
  PersonRelationshipView as GeneratedPersonRelationshipView,
  RelationshipType as GeneratedRelationshipType,
  SetPersonAttributeRequest as GeneratedSetPersonAttributeRequest,
  StructuredPersonProfile as GeneratedStructuredPersonProfile,
} from '../api/generated/models';

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

export type DirectoryPerson = GeneratedDirectoryPersonSummary;
export interface DirectoryPersonSummaryUpdate {
  categories?: string[];
}
export type PersonProfilePatchRequest = GeneratedPersonProfilePatchRequest;
export type PatchPersonRequest = GeneratedPatchPersonRequest;
export type SetPersonAttributeRequest = GeneratedSetPersonAttributeRequest;
export type CreateAttributeDefinitionRequest = GeneratedCreateAttributeDefinitionRequest;
export type AttributeDefinition = GeneratedAttributeDefinition;
export type PersonAttributeValue = GeneratedPersonAttributeValue;
export type Organization = GeneratedOrganization;
export type OrganizationProfile = GeneratedOrganizationProfile;
export type OrganizationProfileBody = GeneratedOrganizationProfileBody;
export type OrganizationBody = GeneratedOrganizationBody;
export type OrganizationCreateBody = GeneratedOrganizationCreateBody;
export type Employment = GeneratedEmployment;
export type EmploymentBody = GeneratedEmploymentBody;
export type EndEmploymentBody = GeneratedEndEmploymentBody;
export type EmploymentProjectionResponse = GeneratedEmploymentProjectionResponse;
export type PersonRelationship = GeneratedPersonRelationship;
export type PersonRelationshipView = GeneratedPersonRelationshipView;
export type CreatePersonRelationshipRequest = GeneratedCreatePersonRelationshipRequest;
export type PatchPersonRelationshipRequest = GeneratedPatchPersonRelationshipRequest;
export type RelationshipType = GeneratedRelationshipType;
export type CreateRelationshipTypeRequest = GeneratedCreateRelationshipTypeRequest;
export type PatchRelationshipTypeRequest = GeneratedPatchRelationshipTypeRequest;
export type PersonNetwork = GeneratedPersonNetwork;
export type NetworkNode = GeneratedNetworkNode;
export type NetworkEdge = GeneratedNetworkEdge;

export type DirectoryEntityResource =
  'organizations' | 'employments' | 'relationships' | 'relationshipTypes' | 'network';
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
  code:
    | 'person_revision_conflict'
    | 'attribute_conflict'
    | 'precondition_required'
    | 'request_failed'
    | 'mutation_in_progress';
  message: string;
  status: number;
  currentValue?: PersonAttributeValue;
  currentValueID?: number;
}

export type DirectoryProfileOperationResult =
  { ok: true } | { ok: false; code: 'mutation_in_progress' | 'reload_in_progress' | 'conflict_unresolved' };

export type DirectoryProfileOperationBlocked = Extract<DirectoryProfileOperationResult, { ok: false }>;

export interface DirectoryReadBundle {
  person?: GeneratedPerson;
  structuredProfile?: GeneratedStructuredPersonProfile;
  attributes?: GeneratedPersonAttributesResponse;
  definitions?: GeneratedAttributeDefinitionsResponse;
  contactState?: GeneratedContactState;
  activity?: GeneratedPersonDaysPage;
  files?: GeneratedPersonFileSearchHTTPResponse;
  /** ETags are retained by resource so later editing can use exact reads. */
  etags: Partial<Record<DirectoryReadSection, string>>;
  /** A failed section stays absent; the detail never fabricates an empty one. */
  errors: Partial<Record<DirectoryReadSection, string>>;
}

export type DirectoryReadSection =
  'person' | 'structuredProfile' | 'attributes' | 'contactState' | 'activity' | 'files';

export type DirectoryPromotionResult =
  { ok: true; personID: number } | { ok: false; code: 'person_binding_conflict' | 'error'; message: string };
