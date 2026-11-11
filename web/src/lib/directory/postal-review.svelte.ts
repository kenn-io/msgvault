import type { Assessment, Input } from '../api/generated/models';

export const postalInputFields = [
  'address_kind', 'post_office_box', 'extended_address', 'street_address', 'locality',
  'region', 'postal_code', 'country_name', 'extended_components', 'free_text',
  'label', 'geo_uri', 'timezone', 'country_code', 'place_uri', 'original_value',
] as const satisfies readonly (keyof Input)[];

export function copyPostalInput(input: Partial<Input>): Input {
  const copied: Input = { address_kind: input.address_kind ?? 'postal' };
  for (const field of postalInputFields) {
    const value = input[field];
    if (value !== undefined && value !== '') copied[field] = value;
  }
  return copied;
}

export function postalInputKey(input: Input): string {
  return JSON.stringify(postalInputFields.map(field => input[field] ?? ''));
}

export type PostalAssessmentCheck = (personID: number, input: Input, external: boolean, signal: AbortSignal) => Promise<Assessment>;
export interface PostalCandidate { input: Input; sourceRef: string; personRevision: number }

let editorGeneration = 0;

/** Transient review state. Destroying or editing invalidates even A → B → A. */
export class PostalReviewSession {
  readonly editorGeneration = ++editorGeneration;
  result = $state<Assessment | null>(null);
  pending = $state(false);
  error = $state<string | null>(null);
  private live = true;
  private draftGeneration = 0;
  private requestID = 0;
  private abort?: AbortController;
  private resultKey: string | undefined;
  private resultExternal: boolean | undefined;

  constructor(readonly personID: number, private readonly assess: PostalAssessmentCheck) {}

  invalidate(): void {
    ++this.draftGeneration;
    ++this.requestID;
    this.abort?.abort();
    this.pending = false;
    this.result = null;
    this.error = null;
    this.resultKey = undefined;
  }

  destroy(): void { this.live = false; this.invalidate(); }

  async check(input: Input, external: boolean): Promise<void> {
    const key = postalInputKey(input);
    if (!this.live || this.pending || (this.result && key === this.resultKey && external === this.resultExternal)) return;
    const generation = this.draftGeneration;
    const request = ++this.requestID;
    const abort = new AbortController();
    this.abort = abort;
    const current = () => this.live && this.draftGeneration === generation && this.requestID === request;
    this.pending = true;
    this.result = null;
    this.error = null;
    try {
      const result = await this.assess(this.personID, { ...input }, external, abort.signal);
      if (!current()) return;
      if (postalInputKey(result.original) !== key) {
        throw new Error('mismatched postal assessment');
      }
      this.result = result;
      this.resultKey = key;
      this.resultExternal = external;
    } catch {
      if (current()) this.error = 'Address check unavailable.';
    } finally {
      if (current()) this.pending = false;
    }
  }
}

/** Display components without exposing a private storage envelope. */
export function postalAddressText(input: Partial<Input>, extendedParts: string[] = []): string {
  let countryCode = input.country_code;
  if (input.country_name && countryCode) {
    try {
      const name = new Intl.DisplayNames(['en'], { type: 'region' }).of(countryCode.toUpperCase());
      if (name?.toLowerCase() === input.country_name.trim().toLowerCase()) countryCode = undefined;
    } catch { /* Keep unknown codes visible. */ }
  }
  const parts = [input.post_office_box, input.extended_address, input.street_address, input.locality,
    input.region, input.postal_code, input.country_name, countryCode, ...extendedParts].filter((part): part is string => !!part);
  const components = parts.join(', ');
  const segments = (value: string) => value.split(/[,\n]+/).map(part => part.trim()).filter(Boolean);
  const structuredSegments = new Set(segments(components));
  const freeSegments = segments(input.free_text ?? '');
  const distinctFreeSegments = freeSegments.filter(part => !structuredSegments.has(part));
  const freeText = distinctFreeSegments.length === freeSegments.length ? input.free_text : distinctFreeSegments.join(', ');
  return (freeText === components ? components : [freeText, components].filter(Boolean).join(', ')) || input.label || '';
}

export function postalReason(code: string): string | undefined {
  const reasons: Record<string, string> = {
    already_adopted: 'Already saved', 'stale-target-revision': 'This suggestion is out of date',
    'evidence-unsupported': 'Evidence unavailable',
    'outside-validity': 'This suggestion is outside its valid period',
    'malformed-value': 'The address is invalid', relation_not_support: 'This evidence does not support the address',
    country_missing: 'Add a country code', country_unknown: 'Check the country code', address_incomplete: 'Add a street address or PO box',
    external_country_unsuitable: 'Enter a supported country code', external_input_unsuitable: 'Add a street address or PO box', external_input_too_long: 'The address is too long to check',
    provider_incomplete: 'The provider reports an incomplete address', provider_coarse_result: 'The provider could not confirm the full address',
    provider_unconfirmed: 'Some address components could not be confirmed', provider_component_not_confirmed: 'Some address components could not be confirmed',
    provider_inferred: 'The provider inferred some components', provider_replaced: 'The provider replaced some components', provider_corrected: 'The provider corrected some components',
    provider_unexpected: 'The provider found an unexpected component', provider_unresolved: 'The provider could not interpret part of the address',
    provider_country_changed: 'The provider changed the country', provider_country_unknown: 'The provider could not confirm the country',
    provider_not_configured: 'The provider is not configured', provider_unavailable: 'The provider is unavailable', provider_request_failed: 'The provider request failed', provider_response_invalid: 'The provider response could not be used',
    provider_disabled: 'Google checks are disabled', unit_not_confirmed: 'Unit not confirmed', po_box_not_confirmed: 'PO box not confirmed',
    'unsupported-target': 'This address target is unsupported',
    'unaligned-evidence': 'Evidence does not match this suggestion', 'identity-mismatch': 'Evidence does not match this person', 'sensitive-policy': 'Sensitive facts are disabled',
  };
  if (code === 'not_verified' || code === 'provider_confirmed' || code === 'provider_confirmation_required') return undefined;
  return reasons[code];
}

export function postalReasons(codes: string[]): string[] {
  return [...new Set(codes.filter(code => !codes.includes('external_country_unsuitable') || (code !== 'country_missing' && code !== 'country_unknown')).map(postalReason).filter((reason): reason is string => !!reason))];
}
