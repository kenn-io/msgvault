<script lang="ts">
  import { Button, IconButton, Menu, MenuContent, MenuItem, MenuSeparator, MenuTrigger, copyToClipboard } from '@kenn-io/kit-ui';
  import Eye from '@lucide/svelte/icons/eye';
  import EyeOff from '@lucide/svelte/icons/eye-off';
  import Plus from '@lucide/svelte/icons/plus';
  import X from '@lucide/svelte/icons/x';
  import Copy from '@lucide/svelte/icons/copy';

  import type {
    AttributeDefinition,
    PersonAttributeValue,
  } from '../../api/generated/models';
  import type { PersonProfilePatchRequest } from '../../directory/models';
  import type { DirectoryProfileController } from '../../directory/profile-controller.svelte';
  import { CONTACT_LABELS } from '../../directory/contact-labels';
  import AttributeEditor from '../directory/AttributeEditor.svelte';
  import { displayAttributeValue, joinDefinitions, isAttributeRevealed, supportedAttribute, canAddAttribute, canEditAttribute, canCloseAttribute } from '../directory/attribute-value';
  import DetailRow from './DetailRow.svelte';
  import InlineNotice from '../common/InlineNotice.svelte';
  import StructuredProfileEditor, { type StructuredProfileRecord as FieldRecord, type StructuredProfileSectionName as FieldSection } from '../directory/StructuredProfileEditor.svelte';

  interface Props {
    controller: DirectoryProfileController;
    onAnnounce?: (text: string) => void;
    /** Shown beside the contact frequency when the person is overdue. */
    overdue?: boolean;
    /** Rows shown after the values, before the add line. */
    summary?: import('svelte').Snippet;
  }

  let { controller, onAnnounce, overdue = false, summary = undefined }: Props = $props();

  async function copyValue(value: string): Promise<void> {
    const copied = await copyToClipboard(value);
    onAnnounce?.(copied ? 'Copied' : "Couldn't copy.");
  }

  type Editing =
    | { kind: 'profile'; section: FieldSection; current?: FieldRecord; contactKind?: string; nameKind?: string }
    | { kind: 'attribute'; universalID: string; current?: PersonAttributeValue };

  type Row =
    | { id: string; kind: 'profile'; section: FieldSection; label: string; value: string; record: FieldRecord }
    | { id: string; kind: 'attribute'; label: string; value: string; definition: AttributeDefinition; current: PersonAttributeValue };

  let confirming = $state<Row & { kind: 'profile' | 'attribute' }>();
  let editing = $state<Editing>();
  let revealed = $state<Record<string, boolean>>({});

  const profile = $derived(controller.structuredProfile);
  const groups = $derived(joinDefinitions(controller.definitions, controller.attributes?.attributes ?? []));
  const activeEditor = $derived.by(() => { const target = editing; return target?.kind === 'attribute' ? groups.find((field) => field.definition.universal_id === target.universalID) : undefined; });
  $effect(() => {
    if (editing?.kind !== 'attribute') return;
    if (!activeEditor || !activeEditor.definition.is_active || !activeEditor.definition.ui_editable || !activeEditor.definition.api_mutable || !supportedAttribute(activeEditor.definition) || !isAttributeRevealed(activeEditor.definition, revealed)) close();
  });

  const DATE_LABELS: Record<string, string> = { birthday: 'Birthday', anniversary: 'Anniversary', death: 'Died' };
  const NAME_LABELS: Record<string, string> = { nickname: 'Nickname', phonetic: 'Pronunciation', sort: 'Sorted as' };

  function nameText(name: NonNullable<typeof profile>['names'][number]): string {
    return name.formatted ?? ([name.given_name, name.family_name].filter(Boolean).join(' ') || name.original_value);
  }

  // Like a phone's contact card: one row per value. The common kinds with no value yet
  // gather into one add line after the values, so values come first.
  const rows = $derived.by((): Row[] => {
    const result: Row[] = [];
    for (const name of profile?.names ?? []) {
      result.push({ id: `names:${name.envelope.id}`, kind: 'profile', section: 'names', label: NAME_LABELS[name.name_kind] ?? 'Name', value: nameText(name), record: name });
    }
    const points = profile?.contact_points ?? [];
    const pointRow = (point: (typeof points)[number]): Row => ({
      id: `contact_points:${point.envelope.id}`,
      kind: 'profile',
      section: 'contact_points',
      label: CONTACT_LABELS[point.address_kind] ?? 'Contact',
      value: point.original_value,
      record: point,
    });
    for (const kind of ['phone', 'email']) for (const point of points) if (point.address_kind === kind) result.push(pointRow(point));
    for (const point of points) if (point.address_kind !== 'phone' && point.address_kind !== 'email') result.push(pointRow(point));
    for (const address of profile?.addresses ?? []) {
      result.push({ id: `addresses:${address.envelope.id}`, kind: 'profile', section: 'addresses', label: 'Address', value: [address.post_office_box, address.extended_address, address.street_address, address.locality, address.region, address.postal_code, address.country_name, address.extended_components].filter(Boolean).join(', ') || address.free_text || address.original_value, record: address });
    }
    for (const date of profile?.dates ?? []) {
      result.push({
        id: `dates:${date.envelope.id}`,
        kind: 'profile',
        section: 'dates',
        label: date.label ?? DATE_LABELS[date.date_kind] ?? 'Date',
        value: date.date_text ?? date.original_value,
        record: date,
      });
    }
    for (const category of profile?.categories ?? []) {
      result.push({ id: `categories:${category.envelope.id}`, kind: 'profile', section: 'categories', label: 'Category', value: category.original_value, record: category });
    }
    for (const media of profile?.media ?? []) result.push({ id: `media:${media.envelope.id}`, kind: 'profile', section: 'media', label: media.media_kind.charAt(0).toUpperCase() + media.media_kind.slice(1), value: media.original_value || media.uri || 'Inline media', record: media });
    for (const group of groups) {
      for (const value of group.current ?? []) {
        result.push({
          id: `attribute:${group.definition.universal_id}:${value.id}`,
          kind: 'attribute',
          label: group.definition.label,
          value: displayAttributeValue(group.definition, value.value),
          definition: group.definition,
          current: value,
        });
      }
    }
    return result;
  });

  // A common kind with a value moves to More fields, where another one can still be added.
  const common = $derived.by(() => {
    const points = profile?.contact_points ?? [];
    return [
      { label: 'Phone', filled: points.some((point) => point.address_kind === 'phone'), target: { kind: 'profile', section: 'contact_points', contactKind: 'phone' } as Editing },
      { label: 'Email', filled: points.some((point) => point.address_kind === 'email'), target: { kind: 'profile', section: 'contact_points', contactKind: 'email' } as Editing },
      { label: 'Address', filled: (profile?.addresses ?? []).length > 0, target: { kind: 'profile', section: 'addresses' } as Editing },
      { label: 'Birthday', filled: (profile?.dates ?? []).some((date) => date.date_kind === 'birthday'), target: { kind: 'profile', section: 'dates' } as Editing },
    ];
  });

  const addableAttributes = $derived(groups.filter((field) => field.definition.is_active && field.definition.api_mutable && field.definition.ui_creatable && supportedAttribute(field.definition) && (field.definition.cardinality === 'multi' || field.current.length === 0)));

  const hasPronunciation = $derived((profile?.names ?? []).some((name) => name.name_kind === 'phonetic'));

  const moreProfile = $derived.by(() => {
    const items: Array<{ label: string; section: FieldSection; contactKind?: string; nameKind?: string }> = [];
    for (const kind of common) if (kind.filled && kind.label !== 'Birthday' && kind.target.kind === 'profile') items.push({ label: kind.label, section: kind.target.section, contactKind: kind.target.contactKind });
    items.push({ label: 'Nickname', section: 'names', nameKind: 'nickname' });
    if (!hasPronunciation) items.push({ label: 'Pronunciation', section: 'names', nameKind: 'phonetic' });
    items.push(
      { label: 'Website', section: 'contact_points', contactKind: 'url' },
      { label: 'Messaging address', section: 'contact_points', contactKind: 'impp' },
      { label: 'Social profile', section: 'contact_points', contactKind: 'social' },
      { label: 'Language', section: 'contact_points', contactKind: 'language' },
      { label: 'Calendar', section: 'contact_points', contactKind: 'calendar' },
      { label: 'Date', section: 'dates' },
      { label: 'Another name', section: 'names' },
      { label: 'Category', section: 'categories' },
      { label: 'Media metadata', section: 'media' },
    );
    return items;
  });

  function startAdd(target: Editing): void {
    confirming = undefined;
    editing = target;
  }

  function editorKey(target: Editing | undefined): string {
    if (!target) return '';
    if (target.kind === 'profile') return `${target.section}:${target.current?.envelope.id ?? 'new'}:${target.contactKind ?? ''}:${target.nameKind ?? ''}`;
    return `${target.universalID}:${target.current?.id ?? 'new'}`;
  }

  function startEdit(row: Row & { kind: 'profile' | 'attribute' }): void {
    confirming = undefined;
    editing =
      row.kind === 'profile'
        ? { kind: 'profile', section: row.section, current: row.record }
        : { kind: 'attribute', universalID: row.definition.universal_id, current: row.current };
  }

  function close(): void {
    if (editing?.kind === 'profile' && controller.draft?.kind === 'profile' && !controller.discardProfileDraft()) return;
    else if (editing?.kind === 'attribute') controller.discardAttributeDraft(activeEditor?.definition.slug ?? '');
    editing = undefined;
  }

  function sensitive(row: Row): boolean {
    return row.kind === 'attribute' && row.definition.is_sensitive && !isAttributeRevealed(row.definition, revealed);
  }

  function closePatch(section: FieldSection, id: number): PersonProfilePatchRequest {
    return { [section]: { supersede: [id] } } as PersonProfilePatchRequest;
  }

  async function closeCurrent(): Promise<void> {
    const pending = confirming;
    if (!pending) return;
    if (pending.kind === 'profile') {
      if (!controller.canWriteProfile) return;
      await controller.patchProfile(closePatch(pending.section, pending.record.envelope.id));
    } else {
      const field = groups.find((field) => field.definition.universal_id === pending.definition.universal_id);
      if (!field || !isAttributeRevealed(field.definition, revealed) || !canCloseAttribute(field.definition, controller)) return;
      await controller.clearAttribute(field.definition.slug, pending.current.id, field.definition.cardinality === 'multi' ? pending.current.ordinal : undefined);
    }
    if (controller.draft === null && controller.conflict === null) confirming = undefined;
  }

  async function reload(): Promise<void> {
    const result = await controller.reload();
    if (result.ok && controller.draft === null) confirming = undefined;
  }

  function canEdit(row: Row & { kind: 'profile' | 'attribute' }): boolean {
    if (row.kind === 'profile') return controller.canWriteProfile && !(row.section === 'media' && 'has_data' in row.record && row.record.has_data);
    return canEditAttribute(row.definition, controller);
  }

  function canRemove(row: Row & { kind: 'profile' | 'attribute' }): boolean {
    return row.kind === 'profile' ? controller.canWriteProfile : canCloseAttribute(row.definition, controller);
  }

  function toggleReveal(definition: AttributeDefinition): void {
    const shown = !revealed[definition.universal_id];
    revealed = { ...revealed, [definition.universal_id]: shown };
    if (!shown) {
      controller.discardAttributeDraft(definition.slug);
      if (editing?.kind === 'attribute' && editing.universalID === definition.universal_id) close();
      if (confirming?.kind === 'attribute' && confirming.definition.universal_id === definition.universal_id) confirming = undefined;
    }
  }
</script>

<div class="fields">
  {#if !controller.structuredProfileETag}<InlineNotice tone="warning" message="Couldn't load this person's latest version." actionLabel={controller.canReload ? 'Reload' : undefined} onaction={() => void controller.reload()} />{/if}
  {#if controller.definitionsError}<InlineNotice message="Couldn't load fields." details={controller.definitionsError} actionLabel="Retry" onaction={() => void controller.reloadDefinitions()} />{/if}
  {#if !controller.attributes || controller.attributesError}<InlineNotice message="Couldn't load field values." details={controller.attributesError ?? undefined} actionLabel={controller.canReload ? 'Retry' : undefined} onaction={() => void controller.reload()} />{/if}
  {#snippet editor()}
    {#key editorKey(editing)}
      {#if editing?.kind === 'profile'}
        <StructuredProfileEditor {controller} section={editing.section} current={editing.current} contactKind={editing.contactKind} nameKind={editing.nameKind} onDone={close} onCancel={close} />
      {:else if editing?.kind === 'attribute' && activeEditor}
        <AttributeEditor {controller} definition={activeEditor!.definition} current={editing.current} sensitiveRevealed onDone={close} onCancel={close} />
        {#if activeEditor.definition.is_sensitive}<Button label={`Hide ${activeEditor.definition.label}`} onclick={() => toggleReveal(activeEditor!.definition)} />{/if}
      {/if}
    {/key}
  {/snippet}
  {#each rows as row, index (row.id)}
      {@const point = row.kind === 'profile' && row.section === 'contact_points' ? row.record as import('../../api/generated/models').PersonContactPoint : undefined}
      {@const scope = point ? [point.service_slug, point.scope_kind, point.scope_value].filter(Boolean).join(' · ') : ''}
      {@const valueLabel = `${row.label}: ${sensitive(row) && row.kind === 'attribute' ? `value ${row.current.ordinal + 1}` : row.value}${scope ? ` · ${scope}` : ''}`}
      <!-- Like a contact card, a run of the same kind shows its label once. -->
      <!-- The row is the edit control; a separate pencil would be a second path to the same editor. -->
      <DetailRow label={rows[index - 1]?.label === row.label ? '' : row.label}
        actionLabel={`Edit ${valueLabel}`} onclick={canEdit(row) && !sensitive(row) ? () => startEdit(row) : undefined}>
        {#if sensitive(row)}<span class="hidden-value">Hidden</span>{:else}{row.value}{/if}
        {#if scope}<small>{scope}</small>{/if}
        {#if overdue && row.kind === 'attribute' && row.definition.slug === 'contact_frequency'}<span class="overdue">Overdue</span>{/if}
        {#snippet actions()}
          {#if point && (point.address_kind === 'email' || point.address_kind === 'phone')}
            <IconButton size="sm" ariaLabel={`Copy ${point.original_value}`} onclick={() => void copyValue(point.original_value)}><Copy size={14} aria-hidden="true" /></IconButton>
          {/if}
          {#if row.kind === 'attribute' && row.definition.is_sensitive}
            <IconButton
              size="sm"
              ariaLabel={isAttributeRevealed(row.definition, revealed) ? `Hide ${valueLabel}` : `Show ${valueLabel}`}
              onclick={() => toggleReveal(row.definition)}
            >
              {#if isAttributeRevealed(row.definition, revealed)}<EyeOff size={14} aria-hidden="true" />{:else}<Eye size={14} aria-hidden="true" />{/if}
            </IconButton>
          {/if}
          <IconButton size="sm" ariaLabel={`Remove ${valueLabel}`} disabled={!canRemove(row) || sensitive(row)} onclick={() => { close(); confirming = row; }}>
            <X size={14} aria-hidden="true" />
          </IconButton>
        {/snippet}
      </DetailRow>
      {#if confirming?.id === row.id}
        <div class="notice" role="group" aria-label={`Confirm removing ${valueLabel}`}>
          <span>Remove {valueLabel} and keep it in History?</span>
          <Button label="Cancel" disabled={controller.mutationPending} onclick={() => confirming = undefined} />
          <Button label="Remove" tone="danger" disabled={!canRemove(row) || sensitive(row)} onclick={() => void closeCurrent()} />
        </div>
      {/if}
  {/each}

  {#if editing}{@render editor()}{/if}
  {@render summary?.()}

  <div class="add">
    {#each common.filter((kind) => !kind.filled) as kind (kind.label)}
      <!-- kit-ui-check-ignore: add links in the field list, styled like a contact card's add rows. -->
      <button type="button" class="add-link" disabled={!controller.canWriteProfile} onclick={() => startAdd(kind.target)}>
        <Plus size={14} aria-hidden="true" />Add {kind.label.toLowerCase()}
      </button>
    {/each}
    <Menu onopenchange={(open) => { if (open) void controller.reloadDefinitions(); }}>
      <MenuTrigger class="add-trigger" ariaLabel="More fields">
        <Plus size={14} aria-hidden="true" /> More fields
      </MenuTrigger>
      <MenuContent ariaLabel="More fields">
        {#if addableAttributes.length > 0}
          {#each addableAttributes as field (field.definition.universal_id)}
            {@const definition = field.definition}
            <!-- Choosing a hidden field reveals it, then opens its form. -->
            <MenuItem textValue={definition.label} disabled={!controller.attributes || !!controller.attributesError || (isAttributeRevealed(definition, revealed) && !canAddAttribute(field, controller, revealed))}
              onselect={() => { if (!isAttributeRevealed(definition, revealed)) toggleReveal(definition); if (canAddAttribute(field, controller, revealed)) startAdd({ kind: 'attribute', universalID: definition.universal_id }); }}>{definition.label}</MenuItem>
          {/each}
          <MenuSeparator />
        {/if}
        {#each moreProfile as item (item.label)}
          <MenuItem
            textValue={item.label}
            disabled={!controller.canWriteProfile}
            onselect={() => startAdd({ kind: 'profile', section: item.section, contactKind: item.contactKind, nameKind: item.nameKind })}
          >
            {item.label}
          </MenuItem>
        {/each}
      </MenuContent>
    </Menu>
  </div>

  {#if controller.conflict && !editing}
    {@const changed = controller.conflict.code === 'person_revision_conflict'}
    <InlineNotice tone={changed ? 'warning' : 'error'} message={changed ? 'This person changed since you opened it.' : "Couldn't save this person."}
      details={changed ? undefined : controller.conflict.message} actionLabel={controller.canReload ? 'Reload' : undefined} onaction={() => void reload()} />
  {/if}
</div>

<style>
  /* L3 label | value rows, 36 for one line, with 28 icon buttons for × and the eye. */
  .fields {
    display: flex;
    flex-direction: column;
  }

  /* An inline editor sits 8 below the row it opens from, and 8 above what follows. */
  .fields > :global(form) {
    margin: var(--space-group) 0;
  }

  .hidden-value {
    color: var(--text-muted);
  }

  small {
    margin-left: var(--space-group);
    color: var(--text-muted);
    font-size: var(--type-meta-size);
    line-height: var(--type-meta-line);
  }

  .overdue {
    margin-left: var(--space-group);
    padding: 0 var(--chip-pad-x);
    border-radius: var(--radius-md);
    background: var(--status-warning-bg);
    color: var(--status-warning-ink);
    font-size: var(--type-small-size);
    line-height: var(--type-small-line);
  }

  /* One add line after the values, starting at the value column: the empty common kinds, then More fields,
   * 12 apart on a 28 line. */
  .add {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    column-gap: var(--space-box);
    padding-top: var(--space-tight);
    padding-left: calc(var(--detail-label-width) + var(--space-group));
  }

  /* kit-ui-check-ignore: add links read as links in the field list, not buttons. */
  .add-link,
  .add :global(.add-trigger) {
    display: flex;
    min-height: var(--control-h);
    align-items: center;
    gap: var(--space-tight);
    padding: 0;
    border: 0;
    background: none;
    box-shadow: none;
    color: var(--link-ink);
    cursor: pointer;
    font: inherit;
    font-size: var(--type-meta-size);
    line-height: var(--type-meta-line);
  }

  .add-link:hover:not(:disabled),
  .add :global(.add-trigger:hover) {
    text-decoration: underline;
  }

  .add-link:disabled {
    color: var(--text-muted);
    cursor: default;
  }

  /* The remove confirmation: a box padded 12 under its row, its sentence (Meta) then the two buttons 8 apart. */
  .notice {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-group);
    margin: var(--space-group) 0;
    padding: var(--space-box);
    border-radius: var(--radius-md);
    background: var(--bg-inset);
    color: var(--text-secondary);
    font-size: var(--type-meta-size);
    line-height: var(--type-meta-line);
  }

  .notice > span {
    min-width: 0;
    flex: 1 1 12rem;
    overflow-wrap: anywhere;
  }

  .fields > :global(.inline-notice) {
    margin: var(--space-pane) 0;
  }

  .fields > :global(.inline-notice:first-child) {
    margin-top: 0;
  }
</style>
