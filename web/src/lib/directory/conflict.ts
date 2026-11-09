import { sentenceCase } from '../explore/labels';
import { readablePartialDate } from './dates';
import type { Employment, Organization, PersonRelationship } from './models';

// A saved record as a conflict shows it beside the form: label to readable value, empty when unset.
const text = (value: unknown) => value === undefined || value === null ? '' : String(value).trim();

export function employmentFacts(job: Employment, names: { organizationName(id: number): string; personName(id: number): string }, openedPersonID: number): Record<string, string> {
  return {
    ...(job.person_id === openedPersonID ? {} : { Person: names.personName(job.person_id) }),
    Organization: names.organizationName(job.organization_id),
    'Job title': text(job.title), 'Role on the team': text(job.role), 'Start date': readablePartialDate(job.start_date),
    'End date': job.is_current ? 'Present' : readablePartialDate(job.end_date), 'Primary job': job.is_primary ? 'Yes' : 'No',
    Department: text(job.department), Location: text(job.location), Description: text(job.description),
    ...(job.confidence === undefined ? {} : { Confidence: `${Math.round(job.confidence * 100)}%` }),
  };
}

export function employmentConflictDetails(job: Employment, message: string): string {
  return [message, job.source_ref ? `Source reference: ${job.source_ref}` : ''].filter(Boolean).join('\n');
}

export function connectionFacts(connection: PersonRelationship): Record<string, string> {
  return { 'End date': readablePartialDate(connection.end_date), Notes: text(connection.notes) };
}

// Names, categories, and contact rows follow as the profile's own groups.
export function organizationFacts(org: Organization): Record<string, string> {
  return {
    Name: org.name, Kind: sentenceCase(org.kind), Domain: text(org.primary_domain), Description: text(org.description),
    Status: org.retired_at ? `Retired ${readablePartialDate(org.retired_at.slice(0, 10))}` : 'Active',
  };
}
