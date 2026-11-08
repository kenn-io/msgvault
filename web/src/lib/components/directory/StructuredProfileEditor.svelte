<script lang="ts">
  import { Button, TextInput } from '@kenn-io/kit-ui';
  import { onDestroy, onMount, tick, untrack } from 'svelte';
  import type { Input } from '../../api/generated/models';
  import { PostalReviewSession, postalAddressText, postalReasons, copyPostalInput, type PostalCandidate } from '../../directory/postal-review.svelte';

  import type {
    PartialDate as GeneratedPartialDate,
    PersonAddress as GeneratedPersonAddress,
    PersonAddressInputRequest as GeneratedPersonAddressInputRequest,
    PersonCategory as GeneratedPersonCategory,
    PersonContactPoint as GeneratedPersonContactPoint,
    PersonDate as GeneratedPersonDate,
    PersonMedia as GeneratedPersonMedia,
    PersonName as GeneratedPersonName,
    ValueEnvelopeInput as GeneratedValueEnvelopeInput,
  } from '../../api/generated/models';
  import { isTextProfileDate, profileDateError } from '../../directory/dates';
  import type { PersonProfilePatchRequest } from '../../directory/models';
  import type { DirectoryProfileController } from '../../directory/profile-controller.svelte';

  export type StructuredProfileSectionName =
    | 'names'
    | 'contact_points'
    | 'addresses'
    | 'dates'
    | 'categories'
    | 'media';
  export type StructuredProfileRecord =
    | GeneratedPersonName
    | GeneratedPersonContactPoint
    | GeneratedPersonAddress
    | GeneratedPersonDate
    | GeneratedPersonCategory
    | GeneratedPersonMedia;
  type PersonAddressInputRequest = GeneratedPersonAddressInputRequest;

  interface Props {
    controller: DirectoryProfileController;
    section: StructuredProfileSectionName;
    current?: StructuredProfileRecord;
    contactKind?: string;
    nameKind?: string;
    postalCandidate?: PostalCandidate;
    onDone?: () => void;
    onCancel?: () => void;
  }

  let {
    controller,
    section,
    current = undefined,
    contactKind: presetContactKind,
    nameKind: presetNameKind,
    postalCandidate = undefined,
    onDone = () => undefined,
    onCancel = () => undefined,
  }: Props = $props();

  // One editor instance owns one explicit draft. The section replaces the
  // component when the target changes, so props are intentionally captured
  // once instead of rebasing an in-progress draft under the user's cursor.
  const initialSection = untrack(() => section);
  const initialCurrent = untrack(() => current);
  const initialCandidate = untrack(() => postalCandidate);
  const initialController = untrack(() => controller);
  const initialPersonID = untrack(() => controller.person?.id ?? controller.structuredProfile?.person.id ?? 0);
  const reviewedETag = initialCandidate ? `"person-${initialPersonID}-r${initialCandidate.personRevision}"` : untrack(() => controller.structuredProfileETag ?? undefined);
  const review = new PostalReviewSession(initialPersonID, (id, input, external, signal) => initialController.assessPostalAddress(id, input, external, signal));
  onDestroy(() => review.destroy());
  $effect(() => { if (controller !== initialController || (controller.person?.id ?? controller.structuredProfile?.person.id ?? 0) !== initialPersonID) review.destroy(); });
  const currentName = initialCurrent as GeneratedPersonName | undefined;
  const currentContact = initialCurrent as GeneratedPersonContactPoint | undefined;
  const currentAddress = initialCurrent as GeneratedPersonAddress | undefined;
  const currentDate = initialCurrent as GeneratedPersonDate | undefined;
  const currentMedia = initialCurrent as GeneratedPersonMedia | undefined;

  let nameKind = $state(currentName?.name_kind ?? untrack(() => presetNameKind) ?? 'formatted');
  let formattedName = $state(currentName?.formatted ?? initialCurrent?.original_value ?? '');
  const formattedPronunciation = currentName?.name_kind === 'phonetic' && !currentName.given_name && !currentName.family_name ? currentName.formatted ?? currentName.original_value : '';
  let givenName = $state(currentName?.given_name || formattedPronunciation || '');
  let familyName = $state(currentName?.family_name ?? '');
  let contactKind = $state(currentContact?.address_kind ?? untrack(() => presetContactKind) ?? 'email');
  let contactValue = $state(initialSection === 'contact_points' ? (initialCurrent?.original_value ?? '') : '');
  let serviceSlug = $state(currentContact?.service_slug ?? '');
  let scopeKind = $state(currentContact?.scope_kind ?? '');
  let scopeValue = $state(currentContact?.scope_value ?? '');
  let addressKind = $state(currentAddress?.address_kind ?? 'postal');
  const seedAddress = initialCandidate?.input ?? currentAddress;
  let originalInput = $state<Input | undefined>(initialCandidate?.input ?? (currentAddress?.address_kind === 'postal' ? currentAddress.assessment?.original ?? copyPostalInput(currentAddress) : undefined));
  let addressTarget = $state('');
  let addressEdited = $state(false);
  let postOfficeBox = $state(seedAddress?.post_office_box ?? '');
  let extendedAddress = $state(seedAddress?.extended_address ?? '');
  let countryCode = $state(seedAddress?.country_code ?? '');
  let originalText = $state(untrack(() => originalInput ? originalInput.original_value ?? '' : seedAddress?.original_value ?? ''));
  let currentText = $state(seedAddress?.free_text ?? '');
  let currentTextTouched = $state(false);
  let streetAddress = $state(seedAddress?.street_address ?? '');
  let locality = $state(seedAddress?.locality ?? '');
  let region = $state(seedAddress?.region ?? '');
  let postalCode = $state(seedAddress?.postal_code ?? '');
  let countryName = $state(seedAddress?.country_name ?? '');
  let dateKind = $state(currentDate?.date_kind ?? (untrack(() => controller.structuredProfile?.dates.some(date => date.date_kind === 'birthday')) ? 'anniversary' : 'birthday'));
  let dateValue = $state(
    formatDate(currentDate?.date) ??
      currentDate?.date_text ??
      (initialSection === 'dates' ? (initialCurrent?.original_value ?? '') : ''),
  );
  let dateLabel = $state(currentDate?.label ?? '');
  const uid = $props.id();
  const dateError = $derived(profileDateError(dateValue));
  const textDate = $derived(isTextProfileDate(dateValue));
  const dateDescribedBy = $derived(
    [dateError ? `${uid}-date-error` : '', textDate ? `${uid}-date-hint` : ''].filter(Boolean).join(' ') || undefined
  );
  let categoryValue = $state(initialSection === 'categories' ? (initialCurrent?.original_value ?? '') : '');
  let mediaKind = $state(currentMedia?.media_kind ?? 'photo');
  let mediaURI = $state(
    currentMedia?.uri ?? (initialSection === 'media' ? (initialCurrent?.original_value ?? '') : ''),
  );
  let mediaType = $state(currentMedia?.media_type ?? '');
  let mediaLabel = $state(initialSection === 'media' ? (initialCurrent?.original_value ?? '') : '');
  let submitting = $state(false);
  let validationError = $state<string | null>(null);
  let formElement = $state<HTMLFormElement>();

  onMount(() => {
    void tick().then(() => formElement?.querySelector<HTMLElement>('input, select')?.focus());
  });

  const ordinal = initialCurrent?.envelope.ordinal ?? 0;
  const supersede = initialCurrent ? [initialCurrent.envelope.id] : [];
  const replacementAddress = () => initialCandidate && addressTarget.startsWith('supersede:')
    ? controller.structuredProfile?.addresses.find(address => address.envelope.id === Number(addressTarget.slice('supersede:'.length)))
    : undefined;
  const envelope = (): GeneratedValueEnvelopeInput => ({
    source: 'user',
    ...(replacementAddress() ? { ordinal: replacementAddress()!.envelope.ordinal } : initialCurrent && sameDiscriminator() ? { ordinal } : {}),
    ...(initialSection === 'addresses' ? {
      ...keep((replacementAddress() ?? currentAddress)?.envelope, ['pref', 'type_label', 'type_tokens']),
      ...(initialCandidate ? { source_ref: initialCandidate.sourceRef } : currentAddress?.envelope.source_ref?.startsWith('person-fact-claim:') ? { source_ref: currentAddress.envelope.source_ref } : {}),
    } : {}),
  });

  function dateParts(value: string): GeneratedPartialDate | undefined {
    const yearBearing = /^(\d{4})(?:-(\d{2})(?:-(\d{2}))?)?$/.exec(value);
    if (yearBearing)
      return {
        year: Number(yearBearing[1]),
        ...(yearBearing[2] ? { month: Number(yearBearing[2]) } : {}),
        ...(yearBearing[3] ? { day: Number(yearBearing[3]) } : {}),
      };
    const monthDay = /^--(\d{2})-(\d{2})$/.exec(value);
    if (monthDay) return { month: Number(monthDay[1]), day: Number(monthDay[2]) };
    const month = /^--(\d{2})$/.exec(value);
    if (month) return { month: Number(month[1]) };
    const day = /^---(\d{2})$/.exec(value);
    return day ? { day: Number(day[1]) } : undefined;
  }

  function formatDate(value: GeneratedPartialDate | undefined): string | undefined {
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
      case 'names':
        return currentName?.name_kind === nameKind;
      case 'contact_points':
        return currentContact?.address_kind === contactKind;
      case 'addresses':
        return currentAddress?.address_kind === addressKind;
      case 'dates':
        return currentDate?.date_kind === dateKind;
      case 'media':
        return currentMedia?.media_kind === mediaKind;
      case 'categories':
        return true;
    }
  }

  function addressHasValue(input: PersonAddressInputRequest): boolean {
    return [
      input.original_value,
      input.post_office_box,
      input.extended_address,
      input.street_address,
      input.locality,
      input.region,
      input.postal_code,
      input.country_name,
      input.extended_components,
      input.free_text,
      input.place_uri,
      input.geo_uri,
      input.label,
      input.country_code,
    ].some((value) => typeof value === 'string' && value.trim() !== '');
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
                ...(givenName.trim() && !(formattedPronunciation && givenName === formattedPronunciation && !familyName.trim()) ? { given_name: givenName.trim() } : {}),
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
                address_kind: contactKind,
                original_value: contactValue.trim(),
                ...(serviceSlug.trim() ? { service_slug: serviceSlug.trim() } : {}),
                ...(scopeKind.trim() ? { scope_kind: scopeKind.trim() } : {}),
                ...(scopeValue.trim() ? { scope_value: scopeValue.trim() } : {}),
                envelope: envelope(),
              },
            ],
            supersede,
          },
        };
      case 'addresses': {
        const input = addressDraft();
        if (addressKind === 'postal') captureOriginal();
        else if (streetAddress.trim()) {
          input.original_value = streetAddress.trim();
          if (currentAddress?.free_text) input.free_text = streetAddress.trim();
        } else if (!input.original_value) {
          input.original_value = [locality, region, postalCode, countryName].map(value => value.trim()).filter(Boolean).join(', ');
        }
        return { addresses: { add: [{ ...copyPostalInput(input), ...(addressKind === 'postal' ? { original_input: originalInput, ...(currentTextTouched && !currentText ? { free_text: '' } : {}) } : {}), envelope: envelope() }], supersede: initialCandidate ? (addressTarget.startsWith('supersede:') ? [Number(addressTarget.slice('supersede:'.length))] : []) : supersede } };
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
      case 'media': {
        const uri = mediaURI.trim();
        return {
          media: {
            add: [
              {
                media_kind: mediaKind,
                uri,
                original_value: mediaLabel.trim() || uri,
                ...(mediaType.trim() ? { media_type: mediaType.trim() } : {}),
                envelope: envelope(),
              },
            ],
            supersede,
          },
        };
      }
    }
  }

  function addressDraft(): Input {
    return { ...keep(seedAddress, ['extended_components', 'label', 'geo_uri', 'timezone', 'place_uri']), address_kind: addressKind,
      post_office_box: postOfficeBox, extended_address: extendedAddress, street_address: streetAddress,
      locality, region, postal_code: postalCode, country_name: countryName, country_code: countryCode, original_value: originalText, free_text: currentText };
  }
  function captureOriginal(): void {
    if (!originalInput) originalInput = copyPostalInput(addressDraft());
  }
  function edited(): void {
    if (initialSection !== 'addresses') return;
    addressEdited = true;
    review.invalidate();
  }
  async function checkAddress(external: boolean): Promise<void> {
    captureOriginal();
    await review.check(addressDraft(), external);
  }
  async function chooseAddress(corrected: boolean): Promise<void> {
    const result = review.result;
    if (!result) return;
    captureOriginal();
    const input = corrected ? result.proposed : result.original;
    streetAddress = input.street_address ?? ''; extendedAddress = input.extended_address ?? ''; postOfficeBox = input.post_office_box ?? '';
    locality = input.locality ?? ''; region = input.region ?? ''; postalCode = input.postal_code ?? ''; countryName = input.country_name ?? ''; countryCode = input.country_code ?? ''; currentText = input.free_text ?? '';
    review.invalidate();
    await save();
  }
  async function save(event: SubmitEvent | undefined = undefined): Promise<void> {
    event?.preventDefault();
    if (submitting || !controller.canWriteProfile || controller !== initialController || (controller.person?.id ?? controller.structuredProfile?.person.id ?? 0) !== initialPersonID) return;
    validationError = null;
    if (initialCandidate && !addressTarget) { validationError = 'Choose how to save this address.'; return; }
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
      const result = await controller.patchProfile(body, initialSection === 'addresses' ? reviewedETag : undefined);
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
    if (controller.conflict.code === 'person_revision_conflict')
      return initialSection === 'addresses' ? 'This person changed elsewhere. Reload and review the address again.' : 'This person changed elsewhere. Reload and retry.';
    return controller.conflict.message;
  }
</script>

{#if initialSection === 'media' && currentMedia?.has_data}
  <div class="profile-editor" role="note">
    <p class="editor-error">Inline media content cannot be replaced from metadata alone.</p>
    <div class="editor-actions"><Button label="Cancel" type="button" onclick={onCancel} /></div>
  </div>
{:else}
  <form class="profile-editor" bind:this={formElement} onsubmit={save} oninput={edited} onchange={edited} aria-label={`Edit ${section.replace('_', ' ')}`}>
    {#if section === 'names'}
      <label
        >Name kind
        <select bind:value={nameKind} disabled={submitting}>
          <option value="formatted">Formatted</option><option value="structured">Structured</option><option
            value="nickname">Nickname</option
          ><option value="phonetic">Phonetic</option><option value="sort">Sort</option>
        </select>
      </label>
      {#if nameKind !== 'phonetic'}<label
        >Formatted name<TextInput
          bind:value={formattedName}
          ariaLabel="Formatted name"
          required
          block
          disabled={submitting}
        /></label
      >
      {/if}
      <label>{nameKind === 'phonetic' ? 'First name, said as' : 'Given name'}<TextInput bind:value={givenName} required={nameKind === 'phonetic' && !familyName.trim()} ariaLabel={nameKind === 'phonetic' ? 'First name, said as' : 'Given name'} block disabled={submitting} /></label>
      <label>{nameKind === 'phonetic' ? 'Last name, said as' : 'Family name'}<TextInput bind:value={familyName} ariaLabel={nameKind === 'phonetic' ? 'Last name, said as' : 'Family name'} block disabled={submitting} /></label
      >
    {:else if section === 'contact_points'}
      <label
        >Contact kind
        <select bind:value={contactKind} disabled={submitting}>
          <option value="email">Email</option><option value="phone">Phone</option><option value="username"
            >Username</option
          ><option value="impp">Messaging address</option><option value="url">URL</option><option value="social"
            >Social</option
          ><option value="calendar">Calendar</option><option value="contact_uri">Contact URI</option><option
            value="org_directory">Organization directory</option
          ><option value="language">Language</option>
        </select>
      </label>
      <label
        >{contactKind === 'email' ? 'Email' : 'Contact value'}<TextInput
          bind:value={contactValue}
          type={contactKind === 'email' ? 'email' : 'text'}
          ariaLabel={contactKind === 'email' ? 'Email' : 'Contact value'}
          required
          block
          disabled={submitting}
        /></label
      >
      {#if ['username', 'impp', 'social'].includes(contactKind) || serviceSlug || scopeKind || scopeValue}
      <label>Service<TextInput bind:value={serviceSlug} ariaLabel="Service" block disabled={submitting} /></label>
      <label
        >Service scope kind<TextInput
          bind:value={scopeKind}
          ariaLabel="Service scope kind"
          block
          disabled={submitting}
        /></label
      >
      <label
        >Service scope value<TextInput
          bind:value={scopeValue}
          ariaLabel="Service scope value"
          block
          disabled={submitting}
        /></label
      >
      {/if}
    {:else if section === 'addresses'}
      {#if initialCandidate}
        <p class="postal-note">Edits keep the original source.</p>
        <label>Save address as<select bind:value={addressTarget} disabled={submitting}>
          <option value="">Choose…</option><option value="add">Add a new address</option>
          {#each controller.structuredProfile?.addresses.filter(address => address.address_kind === 'postal') ?? [] as address (address.envelope.id)}
            <option value={`supersede:${address.envelope.id}`}>Replace: {postalAddressText(address)}</option>
          {/each}
        </select></label>
      {/if}
      <label
        >Address kind
        <select bind:value={addressKind} disabled={submitting}>
          <option value="postal">Postal</option><option value="birth_place">Birth place</option><option
            value="death_place">Death place</option
          >
        </select>
      </label>
      <label
        >Street address<TextInput
          bind:value={streetAddress}
          ariaLabel="Street address"
          block
          disabled={submitting}
        /></label
      >
      {#if addressKind === 'postal'}
        <label>Unit or apartment<TextInput bind:value={extendedAddress} ariaLabel="Unit or apartment" block disabled={submitting} /></label>
        <label>PO box<TextInput bind:value={postOfficeBox} ariaLabel="PO box" block disabled={submitting} /></label>
        <label>Country code<TextInput bind:value={countryCode} ariaLabel="Country code" block disabled={submitting} /></label>
        <label>Current address text<textarea aria-label="Current address text" bind:value={currentText} oninput={() => { currentTextTouched = true; }} disabled={submitting}></textarea></label>
        <label>Original text<textarea aria-label="Original text" bind:value={originalText} readonly={addressKind === 'postal' && !!originalInput} disabled={submitting}></textarea></label>
      {/if}
      <label>Locality<TextInput bind:value={locality} ariaLabel="Locality" block disabled={submitting} /></label>
      <label>Region<TextInput bind:value={region} ariaLabel="Region" block disabled={submitting} /></label>
      <label>Postal code<TextInput bind:value={postalCode} ariaLabel="Postal code" block disabled={submitting} /></label
      >
      <label>Country<TextInput bind:value={countryName} ariaLabel="Country" block disabled={submitting} /></label>
      {#if addressKind === 'postal'}
        <div class="postal-review">
          <p>Local cleanup does not verify that an address exists or is deliverable.</p>
          <div class="editor-actions">
            <Button label="Check locally" type="button" disabled={submitting || review.pending || !controller.canWriteProfile} onclick={() => void checkAddress(false)} />
            <Button label="Check with Google" type="button" disabled={submitting || review.pending || !controller.canWriteProfile} onclick={() => void checkAddress(true)} />
          </div>
          <p>Checking with Google sends postal components, including unit and PO box, to Google.</p>
          {#if review.pending}<p role="status">Checking address…</p>{/if}
          {#if review.error}<p role="alert">{review.error}</p>{/if}
          {#if review.result}
            {@const result = review.result}
            <dl class="postal-comparison">
              <div><dt>Original address</dt><dd>{postalAddressText(result.original)}</dd></div>
              {#if postalAddressText(result.original) !== postalAddressText(result.proposed)}<div><dt>Suggested address</dt><dd>{postalAddressText(result.proposed)}</dd></div>{/if}
            </dl>
            {#if result.status !== 'unavailable' || !postalReasons(result.reason_codes).length}<p role="status">{result.status === 'validated' ? 'Validated — review before saving' : result.status === 'unavailable' ? 'Provider unavailable' : result.status === 'needs_review' ? 'Needs review' : 'Normalized, unverified'}</p>{/if}
            {#if postalReasons(result.reason_codes).length}<p>{postalReasons(result.reason_codes).join(' · ')}</p>{/if}
            {#if result.provider === 'google' && result.checked_at && (result.status === 'validated' || result.status === 'needs_review')}<p class="google-attribution"><a href="https://maps.google.com" target="_blank" rel="noreferrer" translate="no" style="white-space: nowrap; font-family: Arial, sans-serif; font-style: normal; font-weight: 400; font-size: 14px; color: #1f1f1f; background-color: #ffffff; letter-spacing: normal">Google Maps</a></p>{/if}
            <div class="editor-actions">
              <Button label="Keep original" type="button" disabled={submitting || !controller.canWriteProfile} onclick={() => void chooseAddress(false)} />
              <Button label="Confirm corrected address" type="button" disabled={submitting || !controller.canWriteProfile} onclick={() => void chooseAddress(true)} />
            </div>
          {:else if !addressEdited && currentAddress?.assessment}
            <p role="status">Local assessment: {currentAddress.assessment.status.replaceAll('_', ' ')}</p>
          {/if}
        </div>
      {/if}
    {:else if section === 'dates'}
      <label
        >Date kind
        <select bind:value={dateKind} disabled={submitting}>
          <option value="birthday">Birthday</option><option value="anniversary">Anniversary</option><option
            value="death">Death</option
          ><option value="custom">Custom</option>
        </select>
      </label>
      <label
        >Date<TextInput
          bind:value={dateValue}
          ariaLabel="Date"
          placeholder="YYYY, YYYY-MM, or YYYY-MM-DD"
          required
          block
          disabled={submitting}
          ariaDescribedby={dateDescribedBy}
        /></label
      >
      {#if dateError}<p id="{uid}-date-error" class="field-error">{dateError}</p>{/if}
      {#if textDate}<p id="{uid}-date-hint" class="field-hint">Saved as text</p>{/if}
      <label>Label<TextInput bind:value={dateLabel} ariaLabel="Date label" block disabled={submitting} /></label>
    {:else if section === 'categories'}
      <label
        >Category<TextInput
          bind:value={categoryValue}
          ariaLabel="Category"
          required
          block
          disabled={submitting}
        /></label
      >
    {:else}
      <label
        >Media kind
        <select bind:value={mediaKind} disabled={submitting}>
          <option value="photo">Photo</option><option value="logo">Logo</option><option value="sound">Sound</option
          ><option value="key">Key</option>
        </select>
      </label>
      <label
        >Media URI<TextInput
          bind:value={mediaURI}
          type="url"
          ariaLabel="Media URI"
          required
          block
          disabled={submitting}
        /></label
      >
      <label
        >Media type<TextInput
          bind:value={mediaType}
          ariaLabel="Media type"
          placeholder="image/png"
          block
          disabled={submitting}
        /></label
      >
      <label>Media label<TextInput bind:value={mediaLabel} ariaLabel="Media label" block disabled={submitting} /></label
      >
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
        label={section === 'names'
          ? 'Save name'
          : section === 'contact_points'
            ? 'Save contact point'
            : section === 'addresses'
              ? 'Save address'
              : section === 'dates'
                ? 'Save date'
                : section === 'categories'
                  ? 'Save category'
                  : 'Save media metadata'}
        type="submit"
        tone="info"
        surface="solid"
        disabled={submitting || !controller.canWriteProfile || (section === 'dates' && Boolean(dateError))}
      />
    </div>
  </form>
{/if}

<style>
  .postal-review, .postal-note { width: 100%; }
  .postal-comparison { display: grid; gap: var(--space-2); }
  .postal-comparison dd { margin: 0; white-space: pre-wrap; overflow-wrap: anywhere; }
  textarea { box-sizing: border-box; width: 100%; min-width: 0; min-height: 5rem; }
  label :global(input) { box-sizing: border-box; min-width: 0; max-width: 100%; }
  .profile-editor {
    display: flex;
    flex-wrap: wrap;
    gap: var(--field-gap);
    padding: var(--space-3);
    border: 1px solid var(--border-default);
    border-radius: var(--radius-sm);
    background: var(--bg-inset);
  }
  label {
    flex: 1 1 180px;
    min-width: 0;
    display: grid;
    gap: var(--space-1);
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
  }
  select {
    box-sizing: border-box;
    min-height: 28px;
    padding: 0 var(--space-3);
    border: 1px solid var(--border-default);
    border-radius: var(--radius-md);
    background: var(--bg-surface);
    color: var(--text-primary);
    font: inherit;
  }
  .editor-actions {
    flex-basis: 100%;
    display: flex;
    justify-content: flex-end;
    gap: var(--space-2);
    flex-wrap: wrap;
  }
  .field-error {
    margin: 0;
    color: var(--text-danger);
    font-size: var(--font-size-xs);
  }
  .field-hint {
    margin: 0;
    color: var(--text-muted);
    font-size: var(--font-size-xs);
  }
  .editor-error {
    margin: 0;
    color: var(--text-danger);
    font-size: var(--font-size-sm);
  }
</style>
