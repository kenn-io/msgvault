<script lang="ts">
  import { Button, Checkbox, Modal, SelectDropdown, TextInput } from '@kenn-io/kit-ui';

  import type { components } from '../../api/generated/schema';
  import { attributeTextLength, trimAttributeText } from '../../directory/attribute-text';
  import type { DirectoryProfileController } from '../../directory/profile-controller.svelte';

  type AttributeDefinition = components['schemas']['AttributeDefinition'];
  type CreateAttributeDefinitionRequest = components['schemas']['CreateAttributeDefinitionRequest'];

  interface Props {
    controller: DirectoryProfileController;
    onClose: () => void;
  }

  let { controller, onClose }: Props = $props();
  let label = $state('');
  let description = $state('');
  let valueType = $state('text');
  let cardinality = $state<'single' | 'multi'>('single');
  let choices = $state('');
  let unit = $state('');
  let maxLength = $state('');
  let sensitive = $state(false);
  let pending = $state(false);
  let creating = $state(false);
  let error = $state<string>();
  let created = $state<AttributeDefinition>();
  let validationActive = false;

  const hasChoices = $derived(choices.split(/\r?\n/).some((line) => trimAttributeText(line) !== ''));

  const valueTypeOptions = [
    { value: 'text', label: 'Text' },
    { value: 'integer', label: 'Integer' },
    { value: 'real', label: 'Number' },
    { value: 'boolean', label: 'Boolean' },
    { value: 'date', label: 'Date' },
    { value: 'timestamp', label: 'Timestamp' },
    { value: 'json', label: 'JSON' },
    { value: 'record_reference', label: 'Person reference' }
  ];
  const cardinalityOptions = [
    { value: 'single', label: 'Single value' },
    { value: 'multi', label: 'Multiple values' }
  ];

  async function create(): Promise<void> {
    if (pending) return;
    validationActive = true;
    error = undefined;
    const body = requestBody();
    if (!body) return;
    pending = true;
    creating = true;
    let completed: AttributeDefinition | undefined;
    try {
      await controller.createDefinition(body);
      if (controller.createdDefinition && controller.conflict === null && controller.draft === null) {
        completed = controller.createdDefinition;
      } else {
        error = controller.conflict?.message ?? controller.definitionsError ?? 'Unable to create field.';
      }
    } finally {
      creating = false;
      pending = false;
    }
    if (completed) {
      await showCreated(completed);
    }
  }

  async function retryRegistryRefresh(): Promise<void> {
    if (pending) return;
    pending = true;
    error = undefined;
    let completed: AttributeDefinition | undefined;
    try {
      await controller.retryDefinitionRefresh();
      if (controller.createdDefinition && controller.definitionCreationCommit === null && controller.conflict === null && controller.draft === null) {
        completed = controller.createdDefinition;
      } else {
        error = controller.conflict?.message ?? controller.definitionsError ?? 'Unable to refresh the person attribute registry.';
      }
    } finally {
      pending = false;
    }
    if (completed) await showCreated(completed);
  }

  async function reloadDirectory(): Promise<void> {
    if (pending) return;
    pending = true;
    error = undefined;
    try {
      await controller.reload();
      error = controller.conflict?.message ?? controller.definitionsError ??
        'Definition creation may have succeeded. Reload the page before creating another field.';
    } finally {
      pending = false;
    }
  }

  async function showCreated(definition: AttributeDefinition): Promise<void> {
    created = definition;
  }

  function focusResult(node: HTMLElement): void {
    node.focus();
  }

  function requestBody(): CreateAttributeDefinitionRequest | undefined {
    const normalizedLabel = trimAttributeText(label);
    if (!normalizedLabel) {
      error = 'Enter a label.';
      return undefined;
    }
    const normalizedDescription = trimAttributeText(description);
    const parsedMaxLength = valueType === 'text' ? optionMaxLength() : null;
    if (parsedMaxLength === undefined) return undefined;
    const parsedChoices = choiceOptions(parsedMaxLength);
    if (!parsedChoices) return undefined;
    const normalizedUnit = supportsUnit(valueType) && !parsedChoices.length ? trimAttributeText(unit) : '';
    const options = parsedChoices.length || normalizedUnit || parsedMaxLength !== null
      ? {
          ...(parsedChoices.length ? { choices: parsedChoices } : {}),
          ...(normalizedUnit ? { unit: normalizedUnit } : {}),
          ...(parsedMaxLength !== null ? { max_length: parsedMaxLength } : {})
        }
      : undefined;
    const hasChoices = parsedChoices.length > 0;
    return {
      object_type: 'person',
      label: normalizedLabel,
      ...(normalizedDescription ? { description: normalizedDescription } : {}),
      value_type: valueType,
      field_type: hasChoices ? (cardinality === 'multi' ? 'multiselect' : 'select') : fieldType(valueType),
      ...(valueType === 'record_reference' ? { record_target: 'person' } : {}),
      cardinality,
      is_sensitive: sensitive,
      ...(options ? { options } : {})
    };
  }

  function choiceOptions(textMaxLength: number | null): components['schemas']['AttributeChoice'][] | undefined {
    const lines = choices.split(/\r?\n/).filter((line) => trimAttributeText(line) !== '');
    if (!lines.length) return [];
    if (valueType === 'json' || valueType === 'record_reference') {
      error = `Choices are not supported for ${valueType === 'json' ? 'JSON' : 'person reference'} values.`;
      return undefined;
    }
    const parsed: components['schemas']['AttributeChoice'][] = [];
    const seen = new Set<string>();
    for (const line of lines) {
      const separator = line.indexOf('|');
      const rawValue = trimAttributeText(separator === -1 ? line : line.slice(0, separator));
      const choiceLabel = trimAttributeText(separator === -1 ? rawValue : line.slice(separator + 1));
      if (!rawValue) {
        error = 'Each choice needs a value.';
        return undefined;
      }
      if (!choiceLabel) {
        error = 'Each choice needs a label.';
        return undefined;
      }
      const canonical = canonicalChoiceValue(valueType, rawValue);
      if (canonical === undefined) return undefined;
      if (valueType === 'text' && textMaxLength !== null && attributeTextLength(canonical) > textMaxLength) {
        error = `Each text choice must be ${textMaxLength} characters or fewer.`;
        return undefined;
      }
      if (seen.has(canonical)) {
        error = 'Choice values must be unique.';
        return undefined;
      }
      seen.add(canonical);
      parsed.push({ value: canonical, label: choiceLabel });
    }
    return parsed;
  }

  function canonicalChoiceValue(type: string, value: string): string | undefined {
    switch (type) {
      case 'text': return value;
      case 'integer': {
        if (!/^[+-]?\d+$/.test(value)) return choiceError('Each integer choice must be a whole number.');
        try {
          const parsed = BigInt(value);
          if (parsed === 0n && value.startsWith('-')) return choiceError('Integer choices cannot use negative zero because generated JSON collapses it to zero.');
          if (parsed < BigInt(Number.MIN_SAFE_INTEGER) || parsed > BigInt(Number.MAX_SAFE_INTEGER)) {
            return choiceError('Each integer choice must be a JavaScript-safe whole number so the generated editor can save it exactly.');
          }
          return parsed.toString();
        } catch {
          return choiceError('Each integer choice must be a whole number.');
        }
      }
      case 'real': {
        if (value === '-0') return choiceError('Number choices cannot use negative zero because generated JSON collapses it to zero.');
        if (!/^-?(?:0|[1-9]\d*)(?:\.\d+)?$/.test(value)) {
          return choiceError('Each number choice must use an ordinary decimal without exponent notation.');
        }
        const parsed = Number(value);
        if (!Number.isFinite(parsed)) return choiceError('Each number choice must be finite.');
        const magnitude = Math.abs(parsed);
        if ((magnitude !== 0 && magnitude < 0.0001) || magnitude >= 1_000_000 || JSON.stringify(parsed) !== value) {
          return choiceError('Each number choice must be an exact ordinary decimal from 0.0001 up to (but not including) 1000000.');
        }
        return value;
      }
      case 'boolean': {
        if (['1', 't', 'T', 'TRUE', 'true', 'True'].includes(value)) return 'true';
        if (['0', 'f', 'F', 'FALSE', 'false', 'False'].includes(value)) return 'false';
        return choiceError('Each boolean choice must be true or false.');
      }
      case 'date': return validDate(value) ? value : choiceError('Each date choice must be an exact YYYY-MM-DD calendar date.');
      case 'timestamp': return validCanonicalTimestamp(value) ? value : timestampChoiceError(value);
      default: return choiceError('Choices are not supported for this value type.');
    }
  }

  function choiceError(message: string): undefined {
    error = message;
    return undefined;
  }

  function validDate(value: string): boolean {
    const match = /^(\d{4})-(\d{2})-(\d{2})$/.exec(value);
    if (!match) return false;
    return validCalendarParts(Number(match[1]), Number(match[2]), Number(match[3]));
  }

  function validCanonicalTimestamp(value: string): boolean {
    const match = /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2}):(\d{2})(?:\.(\d{1,9}))?Z$/.exec(value);
    if (!match) return false;
    const year = Number(match[1]);
    const month = Number(match[2]);
    const day = Number(match[3]);
    const hour = Number(match[4]);
    const minute = Number(match[5]);
    const second = Number(match[6]);
    const fraction = match[7];
    return validCalendarParts(year, month, day) && hour <= 23 && minute <= 59 && second <= 59 &&
      (!fraction || !fraction.endsWith('0'));
  }

  function timestampChoiceError(value: string): undefined {
    if (!value.endsWith('Z')) return choiceError('Each timestamp choice must already use canonical UTC with a Z suffix; offsets are not normalized in the web dialog.');
    const fraction = /\.(\d+)Z$/.exec(value)?.[1];
    if (fraction && fraction.length > 9) return choiceError('Each timestamp choice may use at most nine fractional digits.');
    if (fraction?.endsWith('0')) return choiceError('Each timestamp choice must omit trailing zero fractional digits to match the server registry.');
    return choiceError('Each timestamp choice must be a canonical RFC3339 UTC timestamp.');
  }

  function validCalendarParts(year: number, month: number, day: number): boolean {
    if (month < 1 || month > 12) return false;
    const leapYear = year % 4 === 0 && (year % 100 !== 0 || year % 400 === 0);
    const daysInMonth = [31, leapYear ? 29 : 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31];
    return day >= 1 && day <= daysInMonth[month - 1]!;
  }

  function optionMaxLength(): number | null | undefined {
    const normalized = trimAttributeText(maxLength);
    if (!normalized || normalized === '0') return null;
    if (!/^\d+$/.test(normalized)) {
      error = 'Maximum length must be a positive whole number; use zero or blank for no limit.';
      return undefined;
    }
    const parsed = Number(normalized);
    if (!Number.isSafeInteger(parsed) || parsed <= 0) {
      error = 'Maximum length must be a positive whole number; use zero or blank for no limit.';
      return undefined;
    }
    return parsed;
  }

  function fieldType(type: string): string {
    switch (type) {
      case 'integer': return 'duration';
      case 'boolean': return 'checkbox';
      case 'date': return 'date';
      case 'timestamp': return 'timestamp';
      case 'json': return 'json';
      case 'record_reference': return 'person';
      default: return 'text';
    }
  }

  function supportsUnit(type: string): boolean {
    return type === 'integer' || type === 'real';
  }

  function supportsChoices(type: string): boolean {
    return type !== 'json' && type !== 'record_reference';
  }

  function selectValueType(type: string): void {
    valueType = type;
    if (type !== 'text') maxLength = '';
    if (!supportsUnit(type)) unit = '';
    if (!supportsChoices(type)) choices = '';
    revalidate();
  }

  function updateChoices(value: string): void {
    choices = value;
    if (value.split(/\r?\n/).some((line) => trimAttributeText(line) !== '')) unit = '';
    revalidate();
  }

  function requestClose(): void {
    if (pending) return;
    if (created) controller.acknowledgeDefinitionCreation();
    onClose();
  }

  function updateMaxLength(value: string): void {
    maxLength = value;
    revalidate();
  }

  function revalidate(): void {
    if (!validationActive) return;
    error = undefined;
    requestBody();
  }
</script>

<Modal title="Create attribute field" ariaLabel="Create attribute field" closeLabel="Close create attribute field" onclose={requestClose}>
  {#if created}
    <div class="created" role="status" tabindex="-1" use:focusResult>
      <strong>Created {created.label}</strong>
      <span>Slug: {created.slug}</span>
      <span>Universal ID: {created.universal_id}</span>
    </div>
    <div class="actions">
      <Button label="Done" tone="info" surface="solid" onclick={requestClose} />
    </div>
  {:else if controller.definitionCreationCommit}
    <div class="recovery" aria-busy={pending}>
      {#if !creating && (error ?? controller.conflict?.message ?? controller.definitionsError)}
        <p class="error" role="alert">{error ?? controller.conflict?.message ?? controller.definitionsError}</p>
      {/if}
      {#if controller.definitionCreationCommit.kind === 'target'}
        <p>The field was created, but the person attribute registry has not returned its exact identity yet. Retry the registry refresh; do not create the field again.</p>
        {#if !creating}
          <div class="actions">
            <Button label="Retry registry refresh" tone="info" surface="solid" disabled={pending} onclick={() => { void retryRegistryRefresh(); }} />
          </div>
        {/if}
      {:else}
        <p>The server accepted the create request without a usable identity. Do not create the field again. Reload Directory before making another attempt.</p>
        {#if !creating}
          <div class="actions">
            <Button label="Reload Directory" tone="info" surface="solid" disabled={pending} onclick={() => { void reloadDirectory(); }} />
          </div>
        {/if}
      {/if}
    </div>
  {:else}
    <form aria-busy={pending} onsubmit={(event) => { event.preventDefault(); void create(); }}>
      <label>Label<TextInput ariaLabel="Label" bind:value={label} autocomplete="off" block /></label>
      <label>Description<textarea aria-label="Description" bind:value={description}></textarea></label>
      <label>Value type<SelectDropdown title="Value type" value={valueType} options={valueTypeOptions} onchange={selectValueType} /></label>
      <label>Cardinality<SelectDropdown title="Cardinality" value={cardinality} options={cardinalityOptions} onchange={(value) => { cardinality = value as 'single' | 'multi'; }} /></label>
      {#if supportsChoices(valueType)}
        <label>Choices<textarea aria-label="Choices" value={choices} oninput={(event) => updateChoices(event.currentTarget.value)} placeholder="One value or value | label per line" aria-describedby="definition-choices-help"></textarea></label>
        <small id="definition-choices-help">Optional. Choices support text, integer, number, boolean, date, and timestamp values.</small>
      {/if}
      {#if supportsUnit(valueType) && !hasChoices}<label>Unit<TextInput ariaLabel="Unit" bind:value={unit} autocomplete="off" block /></label>{/if}
      {#if valueType === 'text'}<label>Maximum length<TextInput ariaLabel="Maximum length" bind:value={maxLength} oninput={updateMaxLength} autocomplete="off" block /></label>{/if}
      <Checkbox label="Sensitive" bind:checked={sensitive} disabled={pending} />
      {#if error}<p class="error" role="alert">{error}</p>{/if}
      <div class="actions">
        <Button label="Cancel" disabled={pending} onclick={requestClose} />
        <Button type="submit" label="Create field" tone="info" surface="solid" disabled={pending} />
      </div>
    </form>
  {/if}
</Modal>

<style>
  form, .created, .recovery { display: grid; gap: var(--space-3); min-width: min(28rem, calc(100vw - 64px)); }
  label { display: grid; gap: var(--space-1); color: var(--text-muted); font-size: var(--font-size-xs); }
  textarea { min-height: 5rem; resize: vertical; padding: var(--space-2); border: 1px solid var(--border-default); border-radius: var(--radius-sm); background: var(--bg-canvas); color: var(--text-primary); }
  .created span { overflow-wrap: anywhere; }
  small { color: var(--text-muted); }
  .actions { display: flex; justify-content: flex-end; gap: var(--space-2); margin-top: var(--space-2); }
  .error { margin: 0; color: var(--text-danger); }
</style>
