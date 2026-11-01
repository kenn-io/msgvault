<script module lang="ts">
  import type {
    PersonAddress,
    PersonCategory,
    PersonContactPoint,
    PersonDate,
    PersonName,
  } from '../../api/generated/models';

  export type FieldSection = 'names' | 'contact_points' | 'addresses' | 'dates' | 'categories';
  export type FieldRecord = PersonName | PersonContactPoint | PersonAddress | PersonDate | PersonCategory;
</script>

<script lang="ts">
  import { Button, SelectDropdown, TextInput } from '@kenn-io/kit-ui';
  import { untrack } from 'svelte';

  import type {
    PartialDate,
    PersonAddressInputRequest,
    ValueEnvelopeInput,
  } from '../../api/generated/models';
  import { isTextProfileDate, profileDateError } from '../../directory/dates';
  import type { PersonProfilePatchRequest } from '../../directory/models';
  import type { DirectoryProfileController } from '../../directory/profile-controller.svelte';

  interface Props {
    controller: DirectoryProfileController;
    section: FieldSection;
    /** The value being replaced. Absent when adding. */
    current?: FieldRecord;
    /** Preselects the contact type when adding, such as "email". */
    contactKind?: string;
    /** The kind of name when adding: "nickname" or "phonetic" for how it's said. */
    nameKind?: string;
    onDone?: () => void;
    onCancel?: () => void;
  }

  let { controller, section, current = undefined, contactKind: presetKind = undefined, nameKind: presetNameKind = undefined, onDone = () => undefined, onCancel = () => undefined }: Props = $props();

  // One editor owns one draft, so props are read once instead of rebasing a
  // draft under the user's cursor.
  const initialSection = untrack(() => section);
  const initialCurrent = untrack(() => current);
  const currentName = initialCurrent as PersonName | undefined;
  const currentContact = initialCurrent as PersonContactPoint | undefined;
  const currentAddress = initialCurrent as PersonAddress | undefined;
  const currentDate = initialCurrent as PersonDate | undefined;

  let nameKind = $state(currentName?.name_kind ?? untrack(() => presetNameKind) ?? 'formatted');
  let addressKind = $state(currentAddress?.address_kind ?? 'postal');
  let formattedName = $state(currentName?.formatted ?? initialCurrent?.original_value ?? '');
  let givenName = $state(currentName?.given_name ?? '');
  let familyName = $state(currentName?.family_name ?? '');
  let contactKind = $state(currentContact?.address_kind ?? untrack(() => presetKind) ?? 'email');
  // Which service a handle is on, such as slack, and the workspace inside it.
  let serviceSlug = $state(currentContact?.service_slug ?? '');
  let scopeKind = $state(currentContact?.scope_kind ?? '');
  let scopeValue = $state(currentContact?.scope_value ?? '');
  let contactValue = $state(initialSection === 'contact_points' ? (initialCurrent?.original_value ?? '') : '');
  let streetAddress = $state(currentAddress?.street_address ?? '');
  let locality = $state(currentAddress?.locality ?? '');
  let region = $state(currentAddress?.region ?? '');
  let postalCode = $state(currentAddress?.postal_code ?? '');
  let countryName = $state(currentAddress?.country_name ?? '');
  let dateKind = $state(currentDate?.date_kind ?? 'birthday');
  let dateValue = $state(
    formatDate(currentDate?.date) ?? currentDate?.date_text ?? (initialSection === 'dates' ? (initialCurrent?.original_value ?? '') : ''),
  );
  let dateLabel = $state(currentDate?.label ?? '');
  let categoryValue = $state(initialSection === 'categories' ? (initialCurrent?.original_value ?? '') : '');
  let submitting = $state(false);
  let validationError = $state<string | null>(null);

  const dateError = $derived(profileDateError(dateValue));
  const textDate = $derived(isTextProfileDate(dateValue));
  const ordinal = initialCurrent?.envelope.ordinal ?? 0;
  const supersede = initialCurrent ? [initialCurrent.envelope.id] : [];

  const contactKinds = [
    { value: 'email', label: 'Email' },
    { value: 'phone', label: 'Phone' },
    { value: 'username', label: 'Username' },
    { value: 'impp', label: 'Messaging address' },
    { value: 'url', label: 'Website' },
    { value: 'social', label: 'Social profile' },
    { value: 'calendar', label: 'Calendar' },
    { value: 'contact_uri', label: 'Contact link' },
    { value: 'org_directory', label: 'Directory page' },
    { value: 'language', label: 'Language' },
  ];
  // Kinds that name an account on some service, where the service and workspace matter.
  const HANDLE_KINDS = ['username', 'impp', 'social'];
  const dateKinds = [
    { value: 'birthday', label: 'Birthday' },
    { value: 'anniversary', label: 'Anniversary' },
    { value: 'custom', label: 'Other' },
  ];

  function envelope(): ValueEnvelopeInput {
    return { source: 'user', ...(initialCurrent && sameDiscriminator() ? { ordinal } : {}) };
  }

  function sameDiscriminator(): boolean {
    switch (initialSection) {
      case 'names':
        return currentName?.name_kind === nameKind;
      case 'contact_points':
        return currentContact?.address_kind === contactKind;
      case 'addresses':
        return currentAddress?.address_kind === addressKind;
      case 'dates':
        return currentDate?.date_kind === dateKind;
      case 'categories':
        return true;
    }
  }

  function dateParts(value: string): PartialDate | undefined {
    const full = /^(\d{4})(?:-(\d{2})(?:-(\d{2}))?)?$/.exec(value);
    if (full) {
      return {
        year: Number(full[1]),
        ...(full[2] ? { month: Number(full[2]) } : {}),
        ...(full[3] ? { day: Number(full[3]) } : {}),
      };
    }
    const monthDay = /^--(\d{2})-(\d{2})$/.exec(value);
    if (monthDay) return { month: Number(monthDay[1]), day: Number(monthDay[2]) };
    const month = /^--(\d{2})$/.exec(value);
    if (month) return { month: Number(month[1]) };
    const day = /^---(\d{2})$/.exec(value);
    return day ? { day: Number(day[1]) } : undefined;
  }

  function formatDate(value: PartialDate | undefined): string | undefined {
    if (!value) return undefined;
    const year = value.year?.toString().padStart(4, '0');
    const month = value.month?.toString().padStart(2, '0');
    const day = value.day?.toString().padStart(2, '0');
    if (year && month && day) return `${year}-${month}-${day}`;
    if (year && month) return `${year}-${month}`;
    if (year) return year;
    if (month && day) return `--${month}-${day}`;
    if (month) return `--${month}`;
    if (day) return `---${day}`;
    return undefined;
  }

  /** What an edit carries over from the value it replaces, so nothing the form doesn't show is lost. */
  function keep<T extends object, K extends keyof T>(record: T | undefined, keys: readonly K[]): Partial<Pick<T, K>> {
    if (!record) return {};
    const kept: Partial<Pick<T, K>> = {};
    for (const key of keys) if (record[key] !== undefined) kept[key] = record[key];
    return kept;
  }

  function addressHasValue(input: PersonAddressInputRequest): boolean {
    return [input.original_value, input.post_office_box, input.extended_address, input.street_address, input.locality, input.region, input.postal_code, input.country_name, input.extended_components, input.free_text, input.place_uri, input.geo_uri, input.label, input.country_code].some(
      (value) => typeof value === 'string' && value.trim() !== '',
    );
  }

  function patch(): PersonProfilePatchRequest {
    switch (section) {
      case 'names': {
        const formatted = nameKind === 'phonetic' ? [givenName.trim(), familyName.trim()].filter(Boolean).join(' ') : formattedName.trim();
        return {
          names: {
            add: [
              {
                ...keep(currentName, [
                  'additional_names',
                  'honorific_prefixes',
                  'honorific_suffixes',
                  'secondary_surname',
                  'generation',
                  'language',
                  'script',
                  'phonetic_system',
                  'phonetic_script',
                  'sort_as',
                ]),
                name_kind: nameKind,
                original_value: formatted,
                formatted,
                ...(givenName.trim() ? { given_name: givenName.trim() } : {}),
                ...(familyName.trim() ? { family_name: familyName.trim() } : {}),
                is_derived: false,
                envelope: envelope(),
              },
            ],
            supersede,
          },
        };
      }
      case 'contact_points':
        return {
          contact_points: {
            add: [
              {
                ...keep(currentContact, ['uri']),
                ...(serviceSlug.trim() ? { service_slug: serviceSlug.trim() } : {}),
                ...(scopeKind.trim() ? { scope_kind: scopeKind.trim() } : {}),
                ...(scopeValue.trim() ? { scope_value: scopeValue.trim() } : {}),
                address_kind: contactKind,
                original_value: contactValue.trim(),
                envelope: envelope(),
              },
            ],
            supersede,
          },
        };
      case 'addresses': {
        const street = streetAddress.trim();
        return {
          addresses: {
            add: [
              {
                ...keep(currentAddress, [
                  'post_office_box',
                  'extended_address',
                  'extended_components',
                  'free_text',
                  'label',
                  'geo_uri',
                  'timezone',
                  'country_code',
                  'place_uri',
                ]),
                address_kind: addressKind,
                ...(street
                  ? { original_value: street, street_address: street }
                  : initialCurrent?.original_value.trim()
                    ? { original_value: initialCurrent.original_value }
                    : {}),
                ...(locality.trim() ? { locality: locality.trim() } : {}),
                ...(region.trim() ? { region: region.trim() } : {}),
                ...(postalCode.trim() ? { postal_code: postalCode.trim() } : {}),
                ...(countryName.trim() ? { country_name: countryName.trim() } : {}),
                envelope: envelope(),
              },
            ],
            supersede,
          },
        };
      }
      case 'dates': {
        const value = dateValue.trim();
        const parsed = dateParts(value);
        return {
          dates: {
            add: [
              {
                ...keep(currentDate, ['calendar_scale']),
                date_kind: dateKind,
                original_value: value,
                date_text: value,
                ...(parsed ? { date: parsed } : {}),
                ...(dateLabel.trim() ? { label: dateLabel.trim() } : {}),
                envelope: envelope(),
              },
            ],
            supersede,
          },
        };
      }
      case 'categories':
        return { categories: { add: [{ original_value: categoryValue.trim(), envelope: envelope() }], supersede } };
    }
  }

  const empty = $derived.by(() => {
    switch (section) {
      case 'names':
        return nameKind === 'phonetic' ? givenName.trim() === '' && familyName.trim() === '' : formattedName.trim() === '';
      case 'contact_points':
        return contactValue.trim() === '';
      case 'addresses':
        return false;
      case 'dates':
        return dateValue.trim() === '' || Boolean(dateError);
      case 'categories':
        return categoryValue.trim() === '';
    }
  });

  async function save(event: SubmitEvent): Promise<void> {
    event.preventDefault();
    if (submitting || !controller.canWriteProfile || empty) return;
    validationError = null;
    const body = patch();
    if (initialSection === 'addresses') {
      const address = body.addresses?.add?.[0];
      if (!address || !addressHasValue(address)) {
        validationError = 'Enter at least one part of the address.';
        return;
      }
    }
    submitting = true;
    try {
      const result = await controller.patchProfile(body);
      if (result === undefined && controller.draft === null) onDone();
    } finally {
      submitting = false;
    }
  }

  async function reload(): Promise<void> {
    const result = await controller.reload();
    if (result.ok && controller.draft === null) onDone();
  }

  function conflictMessage(): string | null {
    if (!controller.conflict) return null;
    return controller.conflict.code === 'person_revision_conflict'
      ? 'This person changed elsewhere. Reload and try again.'
      : controller.conflict.message;
  }
</script>

<form class="field-editor" onsubmit={save} aria-label={`Edit ${section.replace('_', ' ')}`}>
  {#if section === 'names' && !presetNameKind}<label>Name type<TextInput bind:value={nameKind} ariaLabel="Name type" block disabled={submitting} /></label>{/if}
  {#if section === 'names' && nameKind === 'nickname'}
    <label class="wide">Nickname<TextInput bind:value={formattedName} ariaLabel="Nickname" required block disabled={submitting} /></label>
  {:else if section === 'names' && nameKind === 'phonetic'}
    <div class="row">
      <label>First name, said as<TextInput bind:value={givenName} ariaLabel="First name, said as" block disabled={submitting} /></label>
      <label>Last name, said as<TextInput bind:value={familyName} ariaLabel="Last name, said as" block disabled={submitting} /></label>
    </div>
  {:else if section === 'names'}
    <label class="wide">Name<TextInput bind:value={formattedName} ariaLabel="Name" required block disabled={submitting} /></label>
    <div class="row">
      <label>First name<TextInput bind:value={givenName} ariaLabel="First name" block disabled={submitting} /></label>
      <label>Last name<TextInput bind:value={familyName} ariaLabel="Last name" block disabled={submitting} /></label>
    </div>
  {:else if section === 'contact_points'}
    <div class="row">
      <div class="kind">
        <span class="label">Type</span>
        <SelectDropdown title="Type" value={contactKind} options={contactKinds} disabled={submitting} onchange={(value) => (contactKind = value)} />
      </div>
      <label>
        {contactKinds.find((kind) => kind.value === contactKind)?.label ?? 'Value'}
        <TextInput
          bind:value={contactValue}
          type={contactKind === 'email' ? 'email' : 'text'}
          ariaLabel={contactKinds.find((kind) => kind.value === contactKind)?.label ?? 'Value'}
          required
          block
          disabled={submitting}
        />
      </label>
    </div>
    {#if HANDLE_KINDS.includes(contactKind) || serviceSlug || scopeKind || scopeValue}
      <div class="row">
        <label>Service<TextInput bind:value={serviceSlug} ariaLabel="Service" placeholder="slack" block disabled={submitting} /></label>
        <label>Scope type<TextInput bind:value={scopeKind} ariaLabel="Scope type" block disabled={submitting} /></label>
        <label>Workspace<TextInput bind:value={scopeValue} ariaLabel="Workspace" block disabled={submitting} /></label>
      </div>
    {/if}
  {:else if section === 'addresses'}
    <label>Address type<TextInput bind:value={addressKind} ariaLabel="Address type" block disabled={submitting} /></label>
    <label class="wide">Street<TextInput bind:value={streetAddress} ariaLabel="Street" block disabled={submitting} /></label>
    <div class="row">
      <label>City<TextInput bind:value={locality} ariaLabel="City" block disabled={submitting} /></label>
      <label>State<TextInput bind:value={region} ariaLabel="State" block disabled={submitting} /></label>
      <label>Postal code<TextInput bind:value={postalCode} ariaLabel="Postal code" block disabled={submitting} /></label>
    </div>
    <label class="wide">Country<TextInput bind:value={countryName} ariaLabel="Country" block disabled={submitting} /></label>
  {:else if section === 'dates'}
    <div class="row">
      <div class="kind">
        <span class="label">Type</span>
        <SelectDropdown title="Type" value={dateKind} options={dateKinds} disabled={submitting} onchange={(value) => (dateKind = value)} />
      </div>
      <label>
        Date
        <TextInput bind:value={dateValue} ariaLabel="Date" placeholder="YYYY-MM-DD" required block disabled={submitting} invalid={Boolean(dateError)} />
      </label>
      {#if dateKind === 'custom'}
        <label>Name<TextInput bind:value={dateLabel} ariaLabel="Name" block disabled={submitting} /></label>
      {/if}
    </div>
    {#if dateError}<p class="error" role="alert">{dateError}</p>{:else if textDate}<p class="hint">Saved as written</p>{/if}
  {:else}
    <label class="wide">Category<TextInput bind:value={categoryValue} ariaLabel="Category" required block disabled={submitting} /></label>
  {/if}

  {#if validationError}
    <p class="error" role="alert">{validationError}</p>
  {:else if conflictMessage()}
    <p class="error" role="alert">{conflictMessage()}</p>
  {/if}
  <div class="actions">
    <Button label="Cancel" disabled={submitting} onclick={onCancel} />
    {#if controller.conflict}
      <Button label="Reload" disabled={!controller.canReload} onclick={() => void reload()} />
    {/if}
    <Button type="submit" label="Save" tone="info" surface="solid" disabled={submitting || !controller.canWriteProfile || empty} />
  </div>
</form>

<style>
  .field-editor {
    display: grid;
    gap: var(--field-gap);
    padding: var(--pane-pad-x);
    border: 1px solid var(--border-default);
    border-radius: var(--radius-md);
    background: var(--bg-inset);
  }

  label,
  .kind {
    display: grid;
    min-width: 0;
    flex: 1;
    gap: var(--label-gap);
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
  }

  .kind {
    flex: none;
  }

  .label {
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
  }

  .row {
    display: flex;
    align-items: flex-end;
    gap: var(--field-gap);
    flex-wrap: wrap;
  }

  .actions {
    display: flex;
    justify-content: flex-end;
    gap: var(--control-gap);
  }

  .error,
  .hint {
    margin: 0;
    font-size: var(--font-size-xs);
  }

  .error {
    color: var(--text-danger);
  }

  .hint {
    color: var(--text-muted);
  }
</style>
