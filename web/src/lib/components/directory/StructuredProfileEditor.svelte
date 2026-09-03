<script lang="ts">
  import { Button, TextInput } from '@kenn-io/kit-ui';
  import { untrack } from 'svelte';

  import type { components } from '../../api/generated/schema';
  import type { PersonProfilePatchRequest } from '../../directory/models';
  import type { DirectoryProfileController } from '../../directory/profile-controller.svelte';

  export type StructuredProfileSectionName = 'names' | 'contact_points' | 'addresses' | 'dates' | 'categories' | 'media';
  export type StructuredProfileRecord =
    | components['schemas']['PersonName']
    | components['schemas']['PersonContactPoint']
    | components['schemas']['PersonAddress']
    | components['schemas']['PersonDate']
    | components['schemas']['PersonCategory']
    | components['schemas']['PersonMedia'];
  type PersonAddressInputRequest = components['schemas']['PersonAddressInputRequest'];

  interface Props {
    controller: DirectoryProfileController;
    section: StructuredProfileSectionName;
    current?: StructuredProfileRecord;
    onDone?: () => void;
    onCancel?: () => void;
  }

  let { controller, section, current = undefined, onDone = () => undefined, onCancel = () => undefined }: Props = $props();

  // One editor instance owns one explicit draft. The section replaces the
  // component when the target changes, so props are intentionally captured
  // once instead of rebasing an in-progress draft under the user's cursor.
  const initialSection = untrack(() => section);
  const initialCurrent = untrack(() => current);
  const currentName = initialCurrent as components['schemas']['PersonName'] | undefined;
  const currentContact = initialCurrent as components['schemas']['PersonContactPoint'] | undefined;
  const currentAddress = initialCurrent as components['schemas']['PersonAddress'] | undefined;
  const currentDate = initialCurrent as components['schemas']['PersonDate'] | undefined;
  const currentMedia = initialCurrent as components['schemas']['PersonMedia'] | undefined;

  let nameKind = $state(currentName?.name_kind ?? 'formatted');
  let formattedName = $state(currentName?.formatted ?? initialCurrent?.original_value ?? '');
  let givenName = $state(currentName?.given_name ?? '');
  let familyName = $state(currentName?.family_name ?? '');
  let contactKind = $state(currentContact?.address_kind ?? 'email');
  let contactValue = $state(initialSection === 'contact_points' ? initialCurrent?.original_value ?? '' : '');
  let serviceSlug = $state(currentContact?.service_slug ?? '');
  let scopeKind = $state(currentContact?.scope_kind ?? '');
  let scopeValue = $state(currentContact?.scope_value ?? '');
  let addressKind = $state(currentAddress?.address_kind ?? 'postal');
  let streetAddress = $state(currentAddress?.street_address ?? '');
  let locality = $state(currentAddress?.locality ?? '');
  let region = $state(currentAddress?.region ?? '');
  let postalCode = $state(currentAddress?.postal_code ?? '');
  let countryName = $state(currentAddress?.country_name ?? '');
  let dateKind = $state(currentDate?.date_kind ?? 'birthday');
  let dateValue = $state(formatDate(currentDate?.date) ?? currentDate?.date_text ?? (initialSection === 'dates' ? initialCurrent?.original_value ?? '' : ''));
  let dateLabel = $state(currentDate?.label ?? '');
  let categoryValue = $state(initialSection === 'categories' ? initialCurrent?.original_value ?? '' : '');
  let mediaKind = $state(currentMedia?.media_kind ?? 'photo');
  let mediaURI = $state(currentMedia?.uri ?? (initialSection === 'media' ? initialCurrent?.original_value ?? '' : ''));
  let mediaType = $state(currentMedia?.media_type ?? '');
  let mediaLabel = $state(initialSection === 'media' ? initialCurrent?.original_value ?? '' : '');
  let submitting = $state(false);
  let validationError = $state<string | null>(null);

  const ordinal = initialCurrent?.envelope.ordinal ?? 0;
  const supersede = initialCurrent ? [initialCurrent.envelope.id] : [];
  const envelope = (): components['schemas']['ValueEnvelopeInput'] => ({
    source: 'user',
    ...(initialCurrent && sameDiscriminator() ? { ordinal } : {})
  });

  function dateParts(value: string): components['schemas']['PartialDate'] | undefined {
    const yearBearing = /^(\d{4})(?:-(\d{2})(?:-(\d{2}))?)?$/.exec(value);
    if (yearBearing) return {
      year: Number(yearBearing[1]),
      ...(yearBearing[2] ? { month: Number(yearBearing[2]) } : {}),
      ...(yearBearing[3] ? { day: Number(yearBearing[3]) } : {})
    };
    const monthDay = /^--(\d{2})-(\d{2})$/.exec(value);
    if (monthDay) return { month: Number(monthDay[1]), day: Number(monthDay[2]) };
    const month = /^--(\d{2})$/.exec(value);
    if (month) return { month: Number(month[1]) };
    const day = /^---(\d{2})$/.exec(value);
    return day ? { day: Number(day[1]) } : undefined;
  }

  function formatDate(value: components['schemas']['PartialDate'] | undefined): string | undefined {
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

  function keep<T extends object, K extends keyof T>(record: T | undefined, keys: readonly K[]): Partial<Pick<T, K>> {
    if (!record) return {};
    const kept: Partial<Pick<T, K>> = {};
    for (const key of keys) {
      if (record[key] !== undefined) kept[key] = record[key];
    }
    return kept;
  }

  function sameDiscriminator(): boolean {
    if (!initialCurrent) return false;
    switch (initialSection) {
      case 'names': return currentName?.name_kind === nameKind;
      case 'contact_points': return currentContact?.address_kind === contactKind;
      case 'addresses': return currentAddress?.address_kind === addressKind;
      case 'dates': return currentDate?.date_kind === dateKind;
      case 'media': return currentMedia?.media_kind === mediaKind;
      case 'categories': return true;
    }
  }

  function addressHasValue(input: PersonAddressInputRequest): boolean {
    return [
      input.original_value, input.post_office_box, input.extended_address,
      input.street_address, input.locality, input.region, input.postal_code,
      input.country_name, input.extended_components, input.free_text,
      input.place_uri, input.geo_uri, input.label, input.country_code
    ].some((value) => typeof value === 'string' && value.trim() !== '');
  }

  function patch(): PersonProfilePatchRequest {
    switch (section) {
      case 'names': {
        const formatted = formattedName.trim();
        return { names: { add: [{
          ...keep(currentName, ['additional_names', 'honorific_prefixes', 'honorific_suffixes', 'secondary_surname', 'generation', 'language', 'script', 'phonetic_system', 'phonetic_script', 'sort_as']),
          name_kind: nameKind,
          original_value: formatted,
          formatted,
          ...(givenName.trim() ? { given_name: givenName.trim() } : {}),
          ...(familyName.trim() ? { family_name: familyName.trim() } : {}),
          is_derived: false,
          envelope: envelope()
        }], supersede } };
      }
      case 'contact_points':
        return { contact_points: { add: [{
          ...keep(currentContact, ['uri']),
          address_kind: contactKind,
          original_value: contactValue.trim(),
          ...(serviceSlug.trim() ? { service_slug: serviceSlug.trim() } : {}),
          ...(scopeKind.trim() ? { scope_kind: scopeKind.trim() } : {}),
          ...(scopeValue.trim() ? { scope_value: scopeValue.trim() } : {}),
          envelope: envelope()
        }], supersede } };
      case 'addresses': {
        const street = streetAddress.trim();
        return { addresses: { add: [{
          ...keep(currentAddress, ['post_office_box', 'extended_address', 'extended_components', 'free_text', 'label', 'geo_uri', 'timezone', 'country_code', 'place_uri']),
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
          envelope: envelope()
        }], supersede } };
      }
      case 'dates': {
        const value = dateValue.trim();
        const parsed = dateParts(value);
        return { dates: { add: [{
          ...keep(currentDate, ['calendar_scale']),
          date_kind: dateKind,
          original_value: value,
          date_text: value,
          ...(parsed ? { date: parsed } : {}),
          ...(dateLabel.trim() ? { label: dateLabel.trim() } : {}),
          envelope: envelope()
        }], supersede } };
      }
      case 'categories':
        return { categories: { add: [{ original_value: categoryValue.trim(), envelope: envelope() }], supersede } };
      case 'media': {
        const uri = mediaURI.trim();
        return { media: { add: [{
          media_kind: mediaKind,
          uri,
          original_value: mediaLabel.trim() || uri,
          ...(mediaType.trim() ? { media_type: mediaType.trim() } : {}),
          envelope: envelope()
        }], supersede } };
      }
    }
  }

  async function save(event: SubmitEvent): Promise<void> {
    event.preventDefault();
    if (submitting || !controller.canWriteProfile) return;
    validationError = null;
    const body = patch();
    if (initialSection === 'addresses') {
      const address = body.addresses?.add?.[0];
      if (!address || !addressHasValue(address)) {
        validationError = 'Enter at least one address component.';
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
    if (controller.conflict.code === 'person_revision_conflict') return 'This person changed elsewhere. Reload and retry.';
    return controller.conflict.message;
  }
</script>

{#if initialSection === 'media' && currentMedia?.has_data}
  <div class="profile-editor" role="note">
    <p class="editor-error">Inline media content cannot be replaced from metadata alone.</p>
    <div class="editor-actions"><Button label="Cancel" type="button" onclick={onCancel} /></div>
  </div>
{:else}
<form class="profile-editor" onsubmit={save} aria-label={`Edit ${section.replace('_', ' ')}`}>
  {#if section === 'names'}
    <label>Name kind
      <select bind:value={nameKind} disabled={submitting}>
        <option value="formatted">Formatted</option><option value="structured">Structured</option><option value="nickname">Nickname</option><option value="phonetic">Phonetic</option><option value="sort">Sort</option>
      </select>
    </label>
    <label>Formatted name<TextInput bind:value={formattedName} ariaLabel="Formatted name" required block disabled={submitting} /></label>
    <label>Given name<TextInput bind:value={givenName} ariaLabel="Given name" block disabled={submitting} /></label>
    <label>Family name<TextInput bind:value={familyName} ariaLabel="Family name" block disabled={submitting} /></label>
  {:else if section === 'contact_points'}
    <label>Contact kind
      <select bind:value={contactKind} disabled={submitting}>
        <option value="email">Email</option><option value="phone">Phone</option><option value="username">Username</option><option value="impp">Messaging address</option><option value="url">URL</option><option value="social">Social</option><option value="calendar">Calendar</option><option value="contact_uri">Contact URI</option><option value="org_directory">Organization directory</option><option value="language">Language</option>
      </select>
    </label>
    <label>{contactKind === 'email' ? 'Email' : 'Contact value'}<TextInput bind:value={contactValue} type={contactKind === 'email' ? 'email' : 'text'} ariaLabel={contactKind === 'email' ? 'Email' : 'Contact value'} required block disabled={submitting} /></label>
    <label>Service<TextInput bind:value={serviceSlug} ariaLabel="Service" block disabled={submitting} /></label>
    <label>Service scope kind<TextInput bind:value={scopeKind} ariaLabel="Service scope kind" block disabled={submitting} /></label>
    <label>Service scope value<TextInput bind:value={scopeValue} ariaLabel="Service scope value" block disabled={submitting} /></label>
  {:else if section === 'addresses'}
    <label>Address kind
      <select bind:value={addressKind} disabled={submitting}>
        <option value="postal">Postal</option><option value="birth_place">Birth place</option><option value="death_place">Death place</option>
      </select>
    </label>
    <label>Street address<TextInput bind:value={streetAddress} ariaLabel="Street address" block disabled={submitting} /></label>
    <label>Locality<TextInput bind:value={locality} ariaLabel="Locality" block disabled={submitting} /></label>
    <label>Region<TextInput bind:value={region} ariaLabel="Region" block disabled={submitting} /></label>
    <label>Postal code<TextInput bind:value={postalCode} ariaLabel="Postal code" block disabled={submitting} /></label>
    <label>Country<TextInput bind:value={countryName} ariaLabel="Country" block disabled={submitting} /></label>
  {:else if section === 'dates'}
    <label>Date kind
      <select bind:value={dateKind} disabled={submitting}>
        <option value="birthday">Birthday</option><option value="anniversary">Anniversary</option><option value="death">Death</option><option value="custom">Custom</option>
      </select>
    </label>
    <label>Date<TextInput bind:value={dateValue} ariaLabel="Date" placeholder="YYYY, YYYY-MM, or YYYY-MM-DD" required block disabled={submitting} /></label>
    <label>Label<TextInput bind:value={dateLabel} ariaLabel="Date label" block disabled={submitting} /></label>
  {:else if section === 'categories'}
    <label>Category<TextInput bind:value={categoryValue} ariaLabel="Category" required block disabled={submitting} /></label>
  {:else}
    <label>Media kind
      <select bind:value={mediaKind} disabled={submitting}>
        <option value="photo">Photo</option><option value="logo">Logo</option><option value="sound">Sound</option><option value="key">Key</option>
      </select>
    </label>
    <label>Media URI<TextInput bind:value={mediaURI} type="url" ariaLabel="Media URI" required block disabled={submitting} /></label>
    <label>Media type<TextInput bind:value={mediaType} ariaLabel="Media type" placeholder="image/png" block disabled={submitting} /></label>
    <label>Media label<TextInput bind:value={mediaLabel} ariaLabel="Media label" block disabled={submitting} /></label>
  {/if}

  {#if validationError}
    <p class="editor-error" role="alert">{validationError}</p>
  {:else if conflictMessage()}
    <p class="editor-error" role="alert">{conflictMessage()}</p>
  {/if}
  <div class="editor-actions">
    <Button label="Cancel" type="button" disabled={submitting} onclick={onCancel} />
    {#if controller.conflict}
      <Button label="Reload profile" type="button" disabled={!controller.canReload} onclick={() => void reload()} />
    {/if}
    <Button
      label={section === 'names' ? 'Save name' : section === 'contact_points' ? 'Save contact point' : section === 'addresses' ? 'Save address' : section === 'dates' ? 'Save date' : section === 'categories' ? 'Save category' : 'Save media metadata'}
      type="submit" tone="info" surface="solid" disabled={submitting || !controller.canWriteProfile}
    />
  </div>
</form>
{/if}

<style>
  .profile-editor { display: grid; gap: var(--space-3); padding: var(--space-3); border: 1px solid var(--border-default); border-radius: var(--radius-sm); background: var(--bg-inset); }
  label { display: grid; gap: var(--space-1); color: var(--text-secondary); font-size: var(--font-size-sm); }
  select { box-sizing: border-box; min-height: 28px; padding: 0 var(--space-3); border: 1px solid var(--border-default); border-radius: var(--radius-md); background: var(--bg-surface); color: var(--text-primary); font: inherit; }
  .editor-actions { display: flex; justify-content: flex-end; gap: var(--space-2); flex-wrap: wrap; }
  .editor-error { margin: 0; color: var(--text-danger); font-size: var(--font-size-sm); }
</style>
