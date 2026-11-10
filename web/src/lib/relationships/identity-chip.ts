import type { PersonClusterEdge, PersonClusterMember, PersonIdentifier } from '../api/generated/models';

export interface IdentityChipText {
  title: string;
  subtitle: string;
  detail: string;
}

const typeLabels: Record<string, string> = {
  email: 'Email', phone: 'Phone', beeper: 'Beeper', apple_id: 'Apple ID'
};

const sourceLabels: Record<string, string> = {
  carddav_import: 'CardDAV', vcard_import: 'vCard',
  archive_observation: 'archived contacts', user: 'user input',
  extraction: 'extracted information', enrichment: 'enriched information', system: 'the system'
};

const basisLabels: Record<string, string> = {
  stable_provider_id: 'same provider identity',
  service_scope_username: 'same username on a service',
  email: 'email', phone: 'phone', display_name: 'same display name',
  conversation_membership: 'shared conversation'
};

export function identityBasisLabel(basis: string): string {
  return basisLabels[basis] || basis.replaceAll('_', ' ');
}

function linkOriginText(edge: PersonClusterEdge): string {
  const origin = edge.link_origin;
  if (origin?.kind === 'manual') return 'linked manually';
  if (origin?.kind !== 'candidate') return '';
  const source = origin.source || '';
  const from = sourceLabels[source] || source.replaceAll('_', ' ');
  const basis = identityBasisLabel(origin.basis || '');
  return `matched from ${from || 'another source'}${basis ? ` (${basis})` : ''}`;
}

function linkOrigins(edges: PersonClusterEdge[]): string {
  return [...new Set(edges.map(linkOriginText).filter(Boolean))].sort().join(' · ');
}

export function identityChipText(
  identifier: PersonIdentifier | undefined,
  member: PersonClusterMember | undefined,
  edges: PersonClusterEdge[]
): IdentityChipText {
  const origin = linkOrigins(edges);
  if (identifier) {
    const ordinary = identifier.type === 'email' || identifier.type === 'phone';
    const title = ordinary ? identifier.display_value?.trim() || identifier.value
      : identifier.service_label || typeLabels[identifier.type] || identifier.type.replaceAll('_', ' ');
    const name = identifier.participant_display_name || member?.display_name || '';
    const scope = identifier.scope_kind && identifier.scope_value
      ? `${identifier.scope_kind}: ${identifier.scope_value}` : identifier.scope_kind || identifier.scope_value || '';
    return {
      title,
      subtitle: [name === title ? '' : name, origin].filter(Boolean).join(' · '),
      detail: [identifier.value === title ? '' : identifier.value, scope].filter(Boolean).join(' · ')
    };
  }
  const name = member?.display_name || '';
  const address = member?.email || member?.phone || '';
  const title = name || address || 'Linked profile';
  const subtitle = [name ? address : '', origin, !name && !address ? 'no stored address' : '']
    .filter(Boolean).join(' · ') || 'linked';
  return { title, subtitle, detail: '' };
}
