import { readablePartialDate } from '../../directory/dates';
import type { DirectoryProfileController } from '../../directory/profile-controller.svelte';
import type { AttributeDefinition, AttributeValue, PersonAttributeGroup, PersonAttributeValue } from '../../api/generated/models';

function rawValue(value: AttributeValue): string {
  switch (value.type) {
    case 'text':
      return value.text ?? '—';
    case 'integer':
      return value.integer?.toString() ?? '—';
    case 'real':
      return value.real?.toString() ?? '—';
    case 'boolean':
      return value.boolean === undefined ? '—' : value.boolean ? 'Yes' : 'No';
    case 'date':
      return value.date ?? '—';
    case 'timestamp':
      return value.timestamp ?? '—';
    case 'record_reference':
      return value.record_type === 'person' && value.record_id ? `Person ${value.record_id}` : '—';
    default:
      return value.json === undefined ? '—' : JSON.stringify(value.json);
  }
}

export function displayAttributeValue(definition: AttributeDefinition, value: AttributeValue): string {
  const display = rawValue(value);
  const canonical = value.type === 'boolean' && value.boolean !== undefined ? String(value.boolean) : display;
  const choice = definition.options?.choices?.find((candidate) => candidate.value === canonical);
  return choice?.label ?? display;
}

export function displayClaimValue(value: unknown, kind: string): string {
  if (value === null) return 'No value';
  if (kind === 'employment' && value && typeof value === 'object') {
    const job = value as { title?: string; role?: string; organization?: string | { name?: string } };
    const title = job.title || job.role, organization = typeof job.organization === 'string' ? job.organization : job.organization?.name;
    return title && organization ? `${title} at ${organization}` : title || organization || 'Job';
  }
  if (Array.isArray(value)) return value.map(item => displayClaimValue(item, kind)).join(', ');
  return rawValue(typeof value === 'boolean' ? { type: 'boolean', boolean: value } : typeof value === 'string' ? { type: 'text', text: value } : { type: 'json', json: value });
}

export function employmentClaimDetailItems(value: unknown): string[] {
  if (!value || typeof value !== 'object' || Array.isArray(value)) return [];
  const job = value as { title?: string; role?: string; department?: string; location?: string; organization?: string | { domain?: string }; start_date?: string | { year?: number; month?: number; day?: number }; end_date?: string | { year?: number; month?: number; day?: number } };
  const start = readablePartialDate(job.start_date), end = readablePartialDate(job.end_date);
  return [job.title && job.role !== job.title ? job.role : undefined, job.department, job.location, typeof job.organization === 'object' ? job.organization?.domain : undefined, start ? `Started ${start}` : undefined, end ? `Ended ${end}` : undefined].filter((item): item is string => !!item);
}

export interface JoinedAttribute { definition: AttributeDefinition; current: PersonAttributeValue[]; history: PersonAttributeValue[]; }

export function joinDefinitions(definitions: AttributeDefinition[], groups: PersonAttributeGroup[]): JoinedAttribute[] {
    const byUniversalID = new Map(groups.map((group) => [group.definition.universal_id, group]));
    const joined = new Map<string, JoinedAttribute>();
    for (const definition of definitions) {
      const group = byUniversalID.get(definition.universal_id);
      joined.set(definition.universal_id, {
        definition,
        current: [...(group?.current ?? [])],
        history: historical(group?.history ?? []),
      });
    }
    for (const group of groups) {
      if (joined.has(group.definition.universal_id)) continue;
      joined.set(group.definition.universal_id, {
        definition: group.definition,
        current: [...(group.current ?? [])],
        history: historical(group.history ?? []),
      });
    }
    return [...joined.values()].sort(
      (left, right) =>
        left.definition.display_order - right.definition.display_order ||
        left.definition.slug.localeCompare(right.definition.slug),
    );
  }

  function historical(values: PersonAttributeValue[]): PersonAttributeValue[] {
    return values.filter((value) => value.active_until !== undefined || value.superseded_at !== undefined);
  }


export function isAttributeRevealed(definition: AttributeDefinition, revealed: Record<string, boolean>): boolean {
  return !definition.is_sensitive || revealed[definition.universal_id] === true;
}

type EditorKind = 'boolean' | 'number' | 'date' | 'timestamp' | 'json' | 'choice' | 'person' | 'text' | 'unsupported';

export function editorKind(value: AttributeDefinition): EditorKind {
  if (value.options?.choices?.length) return ['text', 'integer', 'real', 'boolean', 'date', 'timestamp'].includes(value.value_type) ? 'choice' : 'unsupported';
  if (value.value_type === 'boolean') return 'boolean';
  if (value.value_type === 'integer' || value.value_type === 'real') return 'number';
  if (value.value_type === 'date') return 'date';
  if (value.value_type === 'timestamp') return 'timestamp';
  if (value.value_type === 'json') return 'json';
  if (value.value_type === 'record_reference' && value.record_target === 'person') return 'person';
  if (value.value_type === 'text') return 'text';
  return 'unsupported';
}

export function supportedAttribute(definition: AttributeDefinition): boolean {
  return editorKind(definition) !== 'unsupported';
}

export function canAddAttribute(field: JoinedAttribute, controller: DirectoryProfileController, revealed: Record<string, boolean>): boolean {
    const definition = field.definition;
    return (
      controller.attributes !== undefined && controller.attributesError === null && definition.is_active &&
      definition.api_mutable &&
      definition.ui_creatable &&
      supportedAttribute(definition) &&
      (!definition.is_sensitive || isAttributeRevealed(definition, revealed)) &&
      (definition.cardinality === 'multi' || field.current.length === 0) &&
      !controller.mutationPending &&
      !controller.reloadPending &&
      !controller.hasUnresolvedConflict
    );
  }

export function canEditAttribute(definition: AttributeDefinition, controller: DirectoryProfileController): boolean {
    return (
      definition.is_active &&
      definition.api_mutable &&
      definition.ui_editable &&
      supportedAttribute(definition) &&
      !controller.mutationPending &&
      !controller.reloadPending &&
      !controller.hasUnresolvedConflict
    );
  }

export function canCloseAttribute(definition: AttributeDefinition, controller: DirectoryProfileController): boolean {
    // The store deliberately permits retiring a current value from an
    // inactive definition, but derived/non-API-mutable definitions remain
    // non-retractable.
    return (
      definition.api_mutable &&
      definition.ui_editable &&
      !controller.mutationPending &&
      !controller.reloadPending &&
      !controller.hasUnresolvedConflict
    );
  }
