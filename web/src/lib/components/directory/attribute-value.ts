import type { AttributeDefinition, AttributeValue } from '../../api/generated/models';

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
