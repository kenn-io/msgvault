<script lang="ts">
  import { Button, Calendar, Checkbox, TextInput, todayStr } from '@kenn-io/kit-ui';
  import { onMount, tick, untrack } from 'svelte';

  import type { components } from '../../api/generated/schema';
  import { attributeTextLength, trimAttributeText } from '../../directory/attribute-text';
  import type { SetPersonAttributeRequest } from '../../directory/models';
  import type { DirectoryProfileController } from '../../directory/profile-controller.svelte';

  type AttributeDefinition = components['schemas']['AttributeDefinition'];
  type AttributeValue = components['schemas']['AttributeValue'];
  type PersonAttributeValue = components['schemas']['PersonAttributeValue'];
  type EditorKind = 'boolean' | 'number' | 'date' | 'timestamp' | 'json' | 'choice' | 'person' | 'text' | 'unsupported';

  interface Props {
    controller: DirectoryProfileController;
    definition: AttributeDefinition;
    current?: PersonAttributeValue;
    sensitiveRevealed?: boolean;
    onDone?: () => void;
    onCancel?: () => void;
  }

  let {
    controller,
    definition,
    current = undefined,
    sensitiveRevealed = false,
    onDone = () => undefined,
    onCancel = () => undefined
  }: Props = $props();

  // One editor owns one explicit draft. AttributeSection keys instances by
  // portable definition identity and current value ID, so the selected CAS
  // lineage cannot change underneath an in-progress draft.
  const initialDefinition = untrack(() => definition);
  const initialCurrent = untrack(() => current);
  const mayReadInitialValue = !initialDefinition.is_sensitive || untrack(() => sensitiveRevealed);
  const kind = editorKind(initialDefinition);
  const inputID = `attribute-${initialDefinition.id}-${initialCurrent?.id ?? 'new'}`;
  const constraintID = `${inputID}-constraint`;
  const editing = initialCurrent !== undefined;
  const targetOrdinal = initialCurrent?.ordinal ?? 0;

  const initialDraft = mayReadInitialValue && initialCurrent ? canonicalValue(initialCurrent.value) : defaultDraft(initialDefinition);
  let draft = $state(initialDraft);
  let booleanDraft = $state(mayReadInitialValue && initialCurrent?.value.boolean === true);
  let expectedValueID = $state<number | undefined>(initialCurrent?.id);
  let lineageMissing = $state(false);
  let convertToNew = $state(false);
  let submitting = $state(false);
  let validationError = $state<string | null>(null);
  let formElement = $state<HTMLFormElement>();
  let calendarMonth = $state(/^\d{4}-\d{2}-\d{2}$/.test(initialDraft) ? initialDraft : todayStr());

  onMount(() => {
    void tick().then(() => formElement?.querySelector<HTMLElement>('input, select, textarea, [aria-pressed="true"], .kit-calendar__day')?.focus());
  });

  const relevantConflict = $derived(
    (controller.draft?.kind === 'setAttribute' || controller.draft?.kind === 'clearAttribute') &&
    controller.draft.slug === initialDefinition.slug
      ? controller.conflict
      : null
  );
  const needsReload = $derived(
    relevantConflict?.code === 'attribute_conflict' || relevantConflict?.code === 'precondition_required'
  );
  const definitionAllowsSet = $derived(initialDefinition.is_active && initialDefinition.api_mutable &&
    (editing && !convertToNew ? initialDefinition.ui_editable : initialDefinition.ui_creatable) && kind !== 'unsupported');
  const replacing = $derived(editing && !convertToNew);
  const canSave = $derived(
    definitionAllowsSet && !lineageMissing && !submitting && !controller.mutationPending && !controller.reloadPending &&
    !controller.hasUnresolvedConflict && !needsReload
  );
  const normalizedTextDraft = $derived(trimAttributeText(draft));
  const trimmedCharacterCount = $derived(attributeTextLength(normalizedTextDraft));

  function editorKind(value: AttributeDefinition): EditorKind {
    if (value.options?.choices?.length) return 'choice';
    if (value.value_type === 'boolean') return 'boolean';
    if (value.value_type === 'integer' || value.value_type === 'real') return 'number';
    if (value.value_type === 'date') return 'date';
    if (value.value_type === 'timestamp') return 'timestamp';
    if (value.value_type === 'json') return 'json';
    if (value.value_type === 'record_reference' && value.record_target === 'person') return 'person';
    if (value.value_type === 'text') return 'text';
    return 'unsupported';
  }

  function defaultDraft(value: AttributeDefinition): string {
    if (value.is_sensitive) return '';
    return value.options?.choices?.[0]?.value ?? '';
  }

  function canonicalValue(value: AttributeValue): string {
    switch (value.type) {
      case 'text': return value.text ?? '';
      case 'integer': return value.integer?.toString() ?? '';
      case 'real': return value.real?.toString() ?? '';
      case 'boolean': return value.boolean?.toString() ?? '';
      case 'date': return value.date ?? '';
      case 'timestamp': return value.timestamp ?? '';
      case 'record_reference': return value.record_id?.toString() ?? '';
      default: return value.json === undefined ? '' : JSON.stringify(value.json);
    }
  }

  function typedValue(): AttributeValue | undefined {
    const value = draft.trim();
    if (kind === 'choice') return choiceValue(initialDefinition, draft);
    switch (kind) {
      case 'boolean': return { type: 'boolean', boolean: booleanDraft };
      case 'number': {
        if (value === '') return invalid('Enter a number.');
        const parsed = Number(value);
        if (!Number.isFinite(parsed)) return invalid('Enter a number.');
        if (initialDefinition.value_type === 'integer') {
          if (!Number.isSafeInteger(parsed)) return invalid('Enter a whole number.');
          return { type: 'integer', integer: parsed };
        }
        return { type: 'real', real: parsed };
      }
      case 'date':
        if (!/^\d{4}-\d{2}-\d{2}$/.test(value)) return invalid('Enter an exact YYYY-MM-DD date.');
        return { type: 'date', date: value };
      case 'timestamp': return timestampValue(value);
      case 'json': return jsonValue(value);
      case 'person': {
        const recordID = Number(value);
        if (!Number.isSafeInteger(recordID) || recordID < 1) return invalid('Enter a positive person ID.');
        return { type: 'record_reference', record_type: 'person', record_id: recordID };
      }
      case 'text': {
        if (!normalizedTextDraft) return invalid('Enter a value.');
        if (exceedsMaxLength(normalizedTextDraft)) return invalid(`Use ${initialDefinition.options?.max_length} characters or fewer.`);
        return { type: 'text', text: normalizedTextDraft };
      }
      case 'unsupported': return undefined;
    }
  }

  function choiceValue(value: AttributeDefinition, selected: string): AttributeValue | undefined {
    const normalizedSelected = value.value_type === 'text' ? trimAttributeText(selected) : selected;
    if (!value.options?.choices?.some((choice) => choice.value === normalizedSelected)) {
      return invalid('Choose an allowed value.');
    }
    switch (value.value_type) {
      case 'text':
        if (exceedsMaxLength(normalizedSelected)) return invalid(`Use ${initialDefinition.options?.max_length} characters or fewer.`);
        return { type: 'text', text: normalizedSelected };
      case 'integer': {
        const parsed = Number(selected);
        return Number.isSafeInteger(parsed) ? { type: 'integer', integer: parsed } : invalid('The selected value is not a valid integer.');
      }
      case 'real': {
        const parsed = Number(selected);
        return Number.isFinite(parsed) ? { type: 'real', real: parsed } : invalid('The selected value is not a valid number.');
      }
      case 'boolean':
        if (selected === 'true' || selected === 'false') return { type: 'boolean', boolean: selected === 'true' };
        return invalid('The selected value is not a valid boolean.');
      case 'date': return { type: 'date', date: selected };
      case 'timestamp': return timestampValue(selected);
      default: return invalid('This choice type is not editable here.');
    }
  }

  function timestampValue(value: string): AttributeValue | undefined {
    if (!isRFC3339(value)) {
      return invalid('Enter an RFC3339 timestamp.');
    }
    return { type: 'timestamp', timestamp: value };
  }

  function isRFC3339(value: string): boolean {
    const match = /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2}):(\d{2})(?:\.\d+)?(?:Z|[+-](\d{2}):(\d{2}))$/.exec(value);
    if (!match) return false;
    const year = Number(match[1]);
    const month = Number(match[2]);
    const day = Number(match[3]);
    const hour = Number(match[4]);
    const minute = Number(match[5]);
    const second = Number(match[6]);
    const offsetHour = match[7] === undefined ? 0 : Number(match[7]);
    const offsetMinute = match[8] === undefined ? 0 : Number(match[8]);
    if (month < 1 || month > 12 || hour > 23 || minute > 59 || second > 59 || offsetHour > 23 || offsetMinute > 59) {
      return false;
    }
    const leapYear = year % 4 === 0 && (year % 100 !== 0 || year % 400 === 0);
    const daysInMonth = [31, leapYear ? 29 : 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31];
    return day >= 1 && day <= daysInMonth[month - 1]!;
  }

  function jsonValue(value: string): AttributeValue | undefined {
    let parsed: unknown;
    try {
      parsed = JSON.parse(value);
    } catch {
      return invalid('Enter valid JSON.');
    }
    if (parsed === null) return invalid('JSON cannot be null.');
    return { type: 'json', json: parsed };
  }

  function exceedsMaxLength(value: string): boolean {
    const limit = initialDefinition.options?.max_length;
    return limit !== undefined && limit > 0 && attributeTextLength(value) > limit;
  }

  function invalid(message: string): undefined {
    validationError = message;
    return undefined;
  }

  function request(value: AttributeValue): SetPersonAttributeRequest {
    return {
      value,
      ...(!convertToNew && expectedValueID !== undefined ? { expected_value_id: expectedValueID } : {}),
      ...(!convertToNew && editing && initialDefinition.cardinality === 'multi' ? { ordinal: targetOrdinal } : {})
    };
  }

  async function save(event: SubmitEvent): Promise<void> {
    event.preventDefault();
    if (!canSave) return;
    validationError = null;
    const value = typedValue();
    if (!value) return;
    submitting = true;
    try {
      const result = await controller.setAttribute(initialDefinition.slug, request(value));
      if (result === undefined && controller.draft === null && controller.conflict === null) onDone();
    } finally {
      submitting = false;
    }
  }

  async function reload(): Promise<void> {
    if (!controller.canReload) return;
    const result = await controller.reload();
    if (!result.ok || controller.conflict) return;
    const group = controller.attributes?.attributes?.find(
      (candidate) => candidate.definition.universal_id === initialDefinition.universal_id
    );
    const currentAtOrdinal = group?.current?.find((value) => value.ordinal === targetOrdinal);
    expectedValueID = currentAtOrdinal?.id;
    lineageMissing = editing && initialDefinition.cardinality === 'multi' && currentAtOrdinal === undefined;
  }

  function convertDraftToNew(): void {
    convertToNew = true;
    lineageMissing = false;
    expectedValueID = undefined;
  }

  function conflictMessage(): string | null {
    if (!relevantConflict) return null;
    if (relevantConflict.code === 'attribute_conflict') return 'This person changed elsewhere. Reload and retry.';
    return relevantConflict.message;
  }
</script>

{#if initialDefinition.is_sensitive && !sensitiveRevealed}
  <p class="editor-error" role="alert">Reveal this sensitive value before editing it.</p>
{:else if kind === 'unsupported'}
  <p class="editor-error" role="status">This {initialDefinition.value_type} value is read-only in the web editor.</p>
{:else}
  <form bind:this={formElement} class="attribute-editor" onsubmit={save} aria-label={`${replacing ? 'Edit' : 'Add'} ${initialDefinition.label} value`}>
    {#if kind === 'boolean'}
      <Checkbox id={inputID} label={initialDefinition.label} bind:checked={booleanDraft} disabled={!definitionAllowsSet} />
    {:else if kind === 'choice'}
      <label for={inputID}>{initialDefinition.label}</label>
      <select id={inputID} bind:value={draft} aria-describedby={initialDefinition.value_type === 'text' && initialDefinition.options?.max_length ? constraintID : undefined} disabled={!definitionAllowsSet}>
        <option value="" disabled>Select a value</option>
        {#each initialDefinition.options?.choices ?? [] as choice (choice.value)}
          <option value={choice.value}>{choice.label}</option>
        {/each}
      </select>
    {:else if kind === 'date'}
      <fieldset>
        <legend>{initialDefinition.label}</legend>
        <Calendar
          bind:month={calendarMonth}
          selected={/^\d{4}-\d{2}-\d{2}$/.test(draft) ? { from: draft, to: draft } : null}
          onpick={(date) => { draft = date; calendarMonth = date; }}
        />
      </fieldset>
    {:else if kind === 'timestamp'}
      <label for={inputID}>{initialDefinition.label}</label>
      <TextInput id={inputID} bind:value={draft} placeholder="2026-08-28T14:30:45Z" block size="lg" disabled={!definitionAllowsSet} />
    {:else if kind === 'json'}
      <label for={inputID}>{initialDefinition.label}</label>
      <textarea id={inputID} bind:value={draft} spellcheck="false" disabled={!definitionAllowsSet}></textarea>
    {:else if kind === 'number'}
      <label for={inputID}>{initialDefinition.label}</label>
      <div class="number-control">
        <input id={inputID} type="number" step={initialDefinition.value_type === 'integer' ? '1' : 'any'} value={draft} oninput={(event) => { draft = event.currentTarget.value; }} disabled={!definitionAllowsSet} />
        {#if initialDefinition.options?.unit}<span>{initialDefinition.options.unit}</span>{/if}
      </div>
    {:else if kind === 'person'}
      <label for={inputID}>{initialDefinition.label}</label>
      <input id={inputID} type="number" min="1" step="1" value={draft} oninput={(event) => { draft = event.currentTarget.value; }} disabled={!definitionAllowsSet} />
    {:else if initialDefinition.field_type === 'textarea'}
      <label for={inputID}>{initialDefinition.label}</label>
      <textarea id={inputID} bind:value={draft} aria-describedby={initialDefinition.options?.max_length ? constraintID : undefined} disabled={!definitionAllowsSet}></textarea>
    {:else}
      <label for={inputID}>{initialDefinition.label}</label>
      <TextInput id={inputID} bind:value={draft} type={initialDefinition.field_type === 'email' ? 'email' : initialDefinition.field_type === 'url' ? 'url' : initialDefinition.field_type === 'phone' ? 'tel' : 'text'} ariaDescribedby={initialDefinition.options?.max_length ? constraintID : undefined} block size="lg" disabled={!definitionAllowsSet} />
    {/if}

    {#if initialDefinition.value_type === 'text' && initialDefinition.options?.max_length}
      <small id={constraintID}>{trimmedCharacterCount} / {initialDefinition.options.max_length} characters.</small>
    {/if}

    {#if validationError}
      <p class="editor-error" role="alert">{validationError}</p>
    {:else if lineageMissing}
      <p class="editor-error" role="status">The selected value is no longer current. Cancel or add this draft as a new value.</p>
    {:else if convertToNew}
      <p role="status">This draft will be added as a new value.</p>
    {:else if conflictMessage()}
      <p class="editor-error" role="alert">{conflictMessage()}</p>
    {/if}

    <div class="editor-actions">
      <Button label="Cancel" type="button" disabled={submitting} onclick={onCancel} />
      {#if needsReload}
        <Button label="Reload attributes" type="button" disabled={!controller.canReload} onclick={() => void reload()} />
      {/if}
      {#if lineageMissing}
        <Button label="Add draft as new value" type="button" disabled={!initialDefinition.ui_creatable} onclick={convertDraftToNew} />
      {/if}
      <Button label="Save attribute" type="submit" tone="info" surface="solid" disabled={!canSave} />
    </div>
  </form>
{/if}

<style>
  .attribute-editor { display: grid; gap: var(--space-2); padding: var(--space-3); border: 1px solid var(--border-default); border-radius: var(--radius-sm); background: var(--bg-inset); }
  label { color: var(--text-secondary); font-size: var(--font-size-sm); }
  select, input, textarea { box-sizing: border-box; width: 100%; min-height: 36px; padding: var(--space-2) var(--space-3); border: 1px solid var(--border-default); border-radius: var(--radius-md); background: var(--bg-surface); color: var(--text-primary); font: inherit; }
  textarea { min-height: 96px; resize: vertical; }
  select:focus-visible, input:focus-visible, textarea:focus-visible { outline: var(--focus-ring); outline-offset: 2px; }
  fieldset { margin: 0; padding: var(--space-2); border: 1px solid var(--border-default); border-radius: var(--radius-md); }
  legend { color: var(--text-secondary); font-size: var(--font-size-sm); }
  .number-control { display: flex; align-items: center; gap: var(--space-2); color: var(--text-muted); font-size: var(--font-size-sm); }
  .editor-actions { display: flex; justify-content: flex-end; gap: var(--space-2); flex-wrap: wrap; }
  .editor-error { margin: 0; color: var(--text-danger); font-size: var(--font-size-sm); }
</style>
