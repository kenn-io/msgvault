<script lang="ts">
  import { Button, IconButton, Menu, MenuContent, MenuItem, MenuSeparator, MenuTrigger } from '@kenn-io/kit-ui';
  import Eye from '@lucide/svelte/icons/eye';
  import EyeOff from '@lucide/svelte/icons/eye-off';
  import Pencil from '@lucide/svelte/icons/pencil';
  import Plus from '@lucide/svelte/icons/plus';
  import X from '@lucide/svelte/icons/x';

  import type {
    AttributeDefinition,
    PersonAttributeGroup,
    PersonAttributeValue,
  } from '../../api/generated/models';
  import type { PersonProfilePatchRequest } from '../../directory/models';
  import type { DirectoryProfileController } from '../../directory/profile-controller.svelte';
  import type { PendingUndo } from '../../people/pending-undo.svelte';
  import AttributeEditor from '../directory/AttributeEditor.svelte';
  import { displayAttributeValue } from '../directory/attribute-value';
  import DetailRow from './DetailRow.svelte';
  import FieldEditor, { type FieldRecord, type FieldSection } from './FieldEditor.svelte';

  interface Props {
    controller: DirectoryProfileController;
    pending: PendingUndo;
    /** Shown beside the contact frequency when the person is overdue. */
    overdue?: boolean;
  }

  let { controller, pending, overdue = false }: Props = $props();

  type Editing =
    | { kind: 'profile'; section: FieldSection; current?: FieldRecord; contactKind?: string; nameKind?: string }
    | { kind: 'attribute'; definition: AttributeDefinition; current?: PersonAttributeValue };

  type Row =
    | { id: string; kind: 'profile'; section: FieldSection; label: string; value: string; record: FieldRecord }
    | { id: string; kind: 'attribute'; label: string; value: string; definition: AttributeDefinition; current: PersonAttributeValue }
    | { id: string; kind: 'add'; label: string; target: Editing };

  let removalError = $state('');
  let editing = $state<Editing>();
  /** Where a new value's form opens: the add row that was clicked, or "more" for the More fields menu. */
  let addingAt = $state<string>();
  let revealed = $state<Record<string, boolean>>({});

  const profile = $derived(controller.structuredProfile);
  const groups = $derived(controller.attributes?.attributes ?? []);

  const CONTACT_LABELS: Record<string, string> = {
    email: 'Email',
    phone: 'Phone',
    username: 'Username',
    impp: 'Messaging',
    url: 'Website',
    social: 'Social',
    calendar: 'Calendar',
    contact_uri: 'Contact link',
    org_directory: 'Directory page',
    language: 'Language',
  };
  const DATE_LABELS: Record<string, string> = { birthday: 'Birthday', anniversary: 'Anniversary', death: 'Died' };
  const NAME_LABELS: Record<string, string> = { nickname: 'Nickname', phonetic: 'Pronunciation', sort: 'Sorted as' };

  function nameText(name: NonNullable<typeof profile>['names'][number]): string {
    return name.formatted ?? ([name.given_name, name.family_name].filter(Boolean).join(' ') || name.original_value);
  }

  // Like a phone's contact card: one row per value, and an add row under each common kind.
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
    for (const kind of ['phone', 'email']) {
      for (const point of points) if (point.address_kind === kind) result.push(pointRow(point));
      result.push({ id: `add:${kind}`, kind: 'add', label: `Add ${kind}`, target: { kind: 'profile', section: 'contact_points', contactKind: kind } });
    }
    for (const point of points) if (point.address_kind !== 'phone' && point.address_kind !== 'email') result.push(pointRow(point));
    for (const address of profile?.addresses ?? []) {
      result.push({ id: `addresses:${address.envelope.id}`, kind: 'profile', section: 'addresses', label: 'Address', value: address.original_value || address.free_text || [address.street_address, address.locality, address.region, address.postal_code, address.country_name].filter(Boolean).join(', '), record: address });
    }
    result.push({ id: 'add:address', kind: 'add', label: 'Add address', target: { kind: 'profile', section: 'addresses' } });
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
    if (!(profile?.dates ?? []).some((date) => date.date_kind === 'birthday')) {
      result.push({ id: 'add:birthday', kind: 'add', label: 'Add birthday', target: { kind: 'profile', section: 'dates' } });
    }
    for (const category of profile?.categories ?? []) {
      result.push({ id: `categories:${category.envelope.id}`, kind: 'profile', section: 'categories', label: 'Category', value: category.original_value, record: category });
    }
    const ordered = [...groups].sort((left, right) => left.definition.display_order - right.definition.display_order);
    for (const group of ordered) {
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

  const visibleRows = $derived(rows.filter((row) => !pending.hides(row.id)));

  const addableAttributes = $derived.by((): AttributeDefinition[] => {
    const filled = new Map<string, number>(groups.map((group: PersonAttributeGroup) => [group.definition.universal_id, (group.current ?? []).length]));
    return controller.definitions
      .filter(
        (definition) =>
          definition.is_active &&
          definition.api_mutable &&
          definition.ui_creatable &&
          (definition.cardinality === 'multi' || (filled.get(definition.universal_id) ?? 0) === 0),
      )
      .sort((left, right) => left.display_order - right.display_order);
  });

  const hasPronunciation = $derived((profile?.names ?? []).some((name) => name.name_kind === 'phonetic'));

  const moreProfile = $derived.by(() => {
    const items: Array<{ label: string; section: FieldSection; contactKind?: string; nameKind?: string }> = [
      { label: 'Nickname', section: 'names', nameKind: 'nickname' },
    ];
    if (!hasPronunciation) items.push({ label: 'Pronunciation', section: 'names', nameKind: 'phonetic' });
    items.push(
      { label: 'Website', section: 'contact_points', contactKind: 'url' },
      { label: 'Messaging address', section: 'contact_points', contactKind: 'impp' },
      { label: 'Social profile', section: 'contact_points', contactKind: 'social' },
      { label: 'Language', section: 'contact_points', contactKind: 'language' },
      { label: 'Other contact detail', section: 'contact_points', contactKind: 'calendar' },
      { label: 'Other date', section: 'dates' },
      { label: 'Another name', section: 'names' },
      { label: 'Category', section: 'categories' },
    );
    return items;
  });

  function startAdd(target: Editing, at: string): void {
    editing = target;
    addingAt = at;
  }

  function editorKey(target: Editing | undefined): string {
    if (!target) return '';
    if (target.kind === 'profile') return `${target.section}:${target.current?.envelope.id ?? 'new'}`;
    return `${target.definition.universal_id}:${target.current?.id ?? 'new'}`;
  }

  function editingRow(row: Row): boolean {
    if (row.kind === 'add') return editing !== undefined && !editing.current && addingAt === row.id;
    if (!editing || !editing.current) return false;
    if (row.kind === 'profile') return editing.kind === 'profile' && editing.current.envelope.id === row.record.envelope.id && editing.section === row.section;
    return editing.kind === 'attribute' && editing.current?.id === row.current.id;
  }

  function startEdit(row: Row & { kind: 'profile' | 'attribute' }): void {
    addingAt = undefined;
    editing =
      row.kind === 'profile'
        ? { kind: 'profile', section: row.section, current: row.record }
        : { kind: 'attribute', definition: row.definition, current: row.current };
  }

  function close(): void {
    if (editing?.kind === 'profile') controller.discardProfileDraft();
    else if (editing) controller.discardAttributeDraft(editing.definition.slug);
    editing = undefined;
    addingAt = undefined;
  }

  function sensitive(row: Row): boolean {
    return row.kind === 'attribute' && row.definition.is_sensitive && !revealed[row.id];
  }

  function closePatch(section: FieldSection, id: number): PersonProfilePatchRequest {
    return { [section]: { supersede: [id] } } as PersonProfilePatchRequest;
  }

  function remove(row: Row & { kind: 'profile' | 'attribute' }): void {
    removalError = '';
    const text = row.kind === 'attribute' && row.definition.is_sensitive ? row.label : row.value.length > 40 ? `${row.value.slice(0, 40)}…` : row.value;
    pending.start(row.id, `Removed ${text}`, async () => {
      if (row.kind === 'profile') await controller.patchProfile(closePatch(row.section, row.record.envelope.id));
      else await controller.clearAttribute(row.definition.slug, row.current.id, row.current.ordinal);
      if (controller.draft || controller.conflict) removalError = controller.conflict?.message ?? 'Could not remove this value.';
    });
  }

  function canEdit(row: Row & { kind: 'profile' | 'attribute' }): boolean {
    if (row.kind === 'profile') return controller.canWriteProfile;
    return row.definition.api_mutable && row.definition.ui_editable && !controller.mutationPending && !controller.reloadPending && !controller.hasUnresolvedConflict;
  }
</script>

<div class="fields">
  {#if pending.error}<p role="alert">{pending.error}</p>{/if}
  {#if !controller.structuredProfileETag}<p role="status">Profile revision unavailable<Button label="Reload profile" disabled={!controller.canReload} onclick={() => void controller.reload()} /></p>{/if}
  {#if removalError}<p role="alert">{removalError}</p>{/if}
  {#if controller.definitionsError}<p role="alert">{controller.definitionsError}<Button label="Retry fields" onclick={() => void controller.reloadDefinitions()} /></p>{/if}
  {#each visibleRows as row (row.id)}
    {#if editingRow(row)}
      {#key editorKey(editing)}
        {#if editing?.kind === 'profile'}
          <FieldEditor {controller} section={editing.section} current={editing.current} contactKind={editing.contactKind} nameKind={editing.nameKind} onDone={close} onCancel={close} />
        {:else if editing?.kind === 'attribute'}
          <AttributeEditor {controller} definition={editing.definition} current={editing.current} sensitiveRevealed onDone={close} onCancel={close} />
        {/if}
      {/key}
    {:else if row.kind === 'add'}
      <!-- kit-ui-check-ignore: an add row in the field list, styled like a contact card's add rows. -->
      <button type="button" class="add-row" disabled={!controller.canWriteProfile} onclick={() => startAdd(row.target, row.id)}>
        <Plus size={14} aria-hidden="true" />{row.label}
      </button>
    {:else}
      <DetailRow label={row.label}>
        {#if sensitive(row)}<span class="hidden-value">••••••</span>{:else}{row.value}{/if}
        {#if row.kind === 'profile' && row.section === 'contact_points'}
          {@const point = row.record as import('../../api/generated/models').PersonContactPoint}
          {#if point.service_slug || point.scope_kind || point.scope_value}<small>{[point.service_slug, point.scope_kind, point.scope_value].filter(Boolean).join(' · ')}</small>{/if}
        {/if}
        {#if overdue && row.kind === 'attribute' && row.definition.slug === 'contact_frequency'}<span class="overdue">Overdue</span>{/if}
        {#snippet actions()}
          {#if row.kind === 'attribute' && row.definition.is_sensitive}
            <IconButton
              size="sm"
              ariaLabel={revealed[row.id] ? `Hide ${row.label}` : `Show ${row.label}`}
              onclick={() => (revealed = { ...revealed, [row.id]: !revealed[row.id] })}
            >
              {#if revealed[row.id]}<EyeOff size={14} aria-hidden="true" />{:else}<Eye size={14} aria-hidden="true" />{/if}
            </IconButton>
          {/if}
          <IconButton size="sm" ariaLabel={`Edit ${row.label}`} disabled={!canEdit(row) || sensitive(row)} onclick={() => startEdit(row)}>
            <Pencil size={14} aria-hidden="true" />
          </IconButton>
          <IconButton size="sm" ariaLabel={`Remove ${row.label}`} disabled={!canEdit(row)} onclick={() => remove(row)}>
            <X size={14} aria-hidden="true" />
          </IconButton>
        {/snippet}
      </DetailRow>
    {/if}
  {/each}

  {#if editing && !editing.current && addingAt === 'more'}
    {#key editorKey(editing)}
      {#if editing.kind === 'profile'}
        <FieldEditor {controller} section={editing.section} contactKind={editing.contactKind} nameKind={editing.nameKind} onDone={close} onCancel={close} />
      {:else}
        <AttributeEditor {controller} definition={editing.definition} sensitiveRevealed onDone={close} onCancel={close} />
      {/if}
    {/key}
  {/if}

  <div class="add">
    <Menu onopenchange={(open) => { if (open) void controller.reloadDefinitions(); }}>
      <MenuTrigger class="add-trigger" ariaLabel="More fields">
        <Plus size={14} aria-hidden="true" /> More fields
      </MenuTrigger>
      <MenuContent ariaLabel="More fields">
        {#if addableAttributes.length > 0}
          {#each addableAttributes as definition (definition.universal_id)}
            <MenuItem textValue={definition.label} onselect={() => startAdd({ kind: 'attribute', definition }, 'more')}>{definition.label}</MenuItem>
          {/each}
          <MenuSeparator />
        {/if}
        {#each moreProfile as item (item.label)}
          <MenuItem
            textValue={item.label}
            disabled={!controller.canWriteProfile}
            onselect={() => startAdd({ kind: 'profile', section: item.section, contactKind: item.contactKind, nameKind: item.nameKind }, 'more')}
          >
            {item.label}
          </MenuItem>
        {/each}
      </MenuContent>
    </Menu>
  </div>

  {#if controller.conflict && !editing}
    <div class="notice" role="alert">
      <span>{controller.conflict.code === 'person_revision_conflict' ? 'This person changed elsewhere.' : controller.conflict.message}</span>
      <Button size="sm" label="Reload" disabled={!controller.canReload} onclick={() => void controller.reload()} />
    </div>
  {/if}
</div>

<style>
  .fields {
    display: flex;
    flex-direction: column;
  }

  .fields > :global(form) {
    margin: var(--control-gap) 0;
  }

  .hidden-value {
    color: var(--text-muted);
    letter-spacing: 0.1em;
  }

  .overdue {
    margin-left: var(--control-gap);
    padding: 0 var(--chip-pad-x);
    border-radius: var(--radius-md);
    background: var(--status-warning-bg);
    color: var(--status-warning-ink);
    font-size: var(--font-size-xs);
  }

  .add {
    padding-top: var(--field-gap);
  }

  /* kit-ui-check-ignore: add rows read as list rows, not buttons. */
  .add-row {
    display: flex;
    min-height: var(--control-h-lg);
    align-items: center;
    gap: var(--space-2);
    padding: var(--space-2) 0;
    border: 0;
    border-bottom: 1px solid var(--border-muted);
    background: none;
    color: var(--link-ink);
    cursor: pointer;
    font: inherit;
    font-size: var(--font-size-sm);
    text-align: left;
  }

  .add-row:hover:not(:disabled) {
    text-decoration: underline;
  }

  .add-row:disabled {
    color: var(--text-muted);
    cursor: default;
  }

  .notice {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--control-gap);
    margin-top: var(--field-gap);
    padding: var(--pane-pad-y) var(--pane-pad-x);
    border-radius: var(--radius-md);
    background: var(--bg-inset);
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
  }
</style>
