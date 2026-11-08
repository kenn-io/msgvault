import type { Change } from '../components/common/ChangeList.svelte';
import { sourceWho } from '../components/directory/history-format';
import { sentenceCase } from '../explore/labels';
import { readablePartialDate } from './dates';
import type { Employment, OrganizationProfile, PersonRelationship } from './models';

export type Facts = Record<string, string>;

export function conflict(saved: Facts, opened: Facts, yours: Facts): { rows: Change[]; other: Change[] } {
  const rows = Object.keys(yours).filter(label => saved[label] !== yours[label]).map(label => ({ label, saved: saved[label], yours: yours[label] }));
  const other = Object.keys(saved).filter(label => !(label in yours) && saved[label] !== opened[label]).map(label => ({ label, saved: saved[label], yours: opened[label] }));
  return { rows, other };
}
const text = (value: unknown) => value === undefined || value === null ? '' : String(value).trim();

export function employmentFacts(job: Partial<Pick<Employment, 'person_id' | 'organization_id' | 'title' | 'role' | 'is_current' | 'is_primary' | 'department' | 'location' | 'description' | 'confidence' | 'source' | 'source_ref'>> & { start_date?: Employment['start_date'] | string | null; end_date?: Employment['end_date'] | string | null }, names: { personID: number; organizationName(id: number): string } = { personID: 0, organizationName: () => 'this organization' }): Facts {
  return {
    Person: job.person_id === undefined ? '' : job.person_id === names.personID ? 'this person' : 'another person', Organization: job.organization_id === undefined ? '' : names.organizationName(job.organization_id),
    'Job title': text(job.title), 'Role on the team': text(job.role), 'Start date': readablePartialDate(job.start_date),
    'End date': job.is_current ? 'Present' : readablePartialDate(job.end_date), 'Primary job': job.is_primary ? 'Yes' : 'No',
    Department: text(job.department), Location: text(job.location), Description: text(job.description),
    Confidence: job.confidence === undefined ? '' : `${Math.round(job.confidence * 100)}%`, Source: sourceWho(job.source ?? ''), 'Source reference': text(job.source_ref),
  };
}

export function connectionFacts(connection: PersonRelationship | { end_date?: PersonRelationship['end_date'] | string | null; notes?: string | null }): Facts {
  return { 'End date': readablePartialDate(connection.end_date), Notes: text(connection.notes) };
}

export function organizationFacts(profile: OrganizationProfile): Facts {
  const org = profile.organization;
  return {
    Name: org.name, Kind: sentenceCase(org.kind), Domain: text(org.primary_domain),
    Aliases: (profile.names ?? []).filter(item => item.name_kind === 'alias').map(item => item.name).join(', '),
    Categories: (profile.categories ?? []).map(item => item.category).join(', '), Description: text(org.description),
    'Legal names': (profile.names ?? []).filter(item => item.name_kind === 'legal').map(item => item.name).join(', '),
    Status: org.retired_at ? `Retired ${readablePartialDate(org.retired_at.slice(0, 10))}` : 'Active',
    'Contact details': (profile.contact_points ?? []).map(item => item.uri && item.uri !== item.original_value ? `${item.original_value} · ${item.uri}` : item.original_value).join(', '),
    Addresses: (profile.addresses ?? []).map(item => item.original_value).join(', '),
    Identifiers: (profile.identifiers ?? []).map(item => item.identifier_value).join(', '),
    Media: (profile.media ?? []).map(item => [item.original_value, item.media_type, item.uri, item.content_hash].filter(Boolean).join(' · ')).join(', '),
  };
}
