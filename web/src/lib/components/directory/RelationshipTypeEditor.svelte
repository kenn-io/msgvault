<script lang="ts">
  import { appShortcuts, Button, Checkbox, Modal, TextInput } from '@kenn-io/kit-ui';
  import { onDestroy, onMount, untrack } from 'svelte';

  import type { DirectoryEntityController } from '../../directory/entity-controller.svelte';
  import type { CreateRelationshipTypeRequest, PatchRelationshipTypeRequest, RelationshipType } from '../../directory/models';

  interface Props {
    controller: DirectoryEntityController;
    relationshipType?: RelationshipType;
    onDone?: () => void;
    onClose?: () => void;
  }

  let { controller, relationshipType = undefined, onDone = () => undefined, onClose = () => undefined }: Props = $props();
  const initialType = untrack(() => relationshipType);
  let slug = $state(initialType?.slug ?? '');
  let forwardLabel = $state(initialType?.forward_label ?? '');
  let reverseLabel = $state(initialType?.reverse_label ?? '');
  let symmetric = $state(initialType?.is_symmetric ?? false);
  let vcardRelatedType = $state(initialType?.vcard_related_type ?? '');
  let color = $state(initialType?.color ?? '');
  let icon = $state(initialType?.icon ?? '');
  let description = $state(initialType?.description ?? '');
  let submitting = $state(false);
  let committed = $state(false);
  let message = $state('');
  let conflictCurrent = $state<RelationshipType>();
  let releaseScope: (() => void) | undefined;
  const symmetricLabelsMismatch = $derived(symmetric && forwardLabel.trim() !== reverseLabel.trim());

  onMount(() => { releaseScope = appShortcuts.pushScope('directory-relationship-type-editor'); });
  onDestroy(() => releaseScope?.());

  async function submit(): Promise<void> {
    if (submitting || committed || !slug.trim() || !forwardLabel.trim() || !reverseLabel.trim() || symmetricLabelsMismatch || isSystemType(initialType)) return;
    const patch = initialType ? patchBody() : undefined;
    if (patch && Object.keys(patch).length === 0) {
      onDone();
      return;
    }
    submitting = true;
    message = '';
    conflictCurrent = undefined;
    try {
      const result = initialType
        ? await controller.updateRelationshipType(initialType.id, patch!)
        : await controller.createRelationshipType(createBody());
      if (result.ok) {
        if (initialType && controller.errors.relationships) {
          committed = true;
          message = `The relationship type was saved, but relationship labels could not be refreshed. ${controller.errors.relationships}`;
        } else {
          onDone();
        }
        return;
      }
      if (result.kind === 'conflict') {
        conflictCurrent = result.current;
        message = `This relationship type changed elsewhere. ${result.message}`;
      } else if (result.kind === 'unknown' || result.kind === 'blocked') {
        message = `The create outcome is unknown. ${result.message}`;
      } else {
        message = result.message;
      }
    } finally {
      submitting = false;
    }
  }

  function createBody(): CreateRelationshipTypeRequest {
    return {
      slug: slug.trim(),
      forward_label: forwardLabel.trim(),
      reverse_label: reverseLabel.trim(),
      is_symmetric: symmetric,
      ...(vcardRelatedType.trim() ? { vcard_related_type: vcardRelatedType.trim() } : {}),
      ...(color.trim() ? { color: color.trim() } : {}),
      ...(icon.trim() ? { icon: icon.trim() } : {}),
      ...(description.trim() ? { description: description.trim() } : {})
    };
  }

  function patchBody(): PatchRelationshipTypeRequest {
    if (!initialType) return {};
    const body: PatchRelationshipTypeRequest = {};
    const nextForwardLabel = forwardLabel.trim();
    const nextReverseLabel = reverseLabel.trim();
    const nextVCardRelatedType = vcardRelatedType.trim();
    const nextColor = color.trim();
    const nextIcon = icon.trim();
    const nextDescription = description.trim();
    if (nextForwardLabel !== initialType.forward_label.trim()) body.forward_label = nextForwardLabel;
    if (nextReverseLabel !== initialType.reverse_label.trim()) body.reverse_label = nextReverseLabel;
    if (nextVCardRelatedType !== (initialType.vcard_related_type?.trim() ?? '')) body.vcard_related_type = nextVCardRelatedType;
    if (nextColor !== (initialType.color?.trim() ?? '')) body.color = nextColor;
    if (nextIcon !== (initialType.icon?.trim() ?? '')) body.icon = nextIcon;
    if (nextDescription !== (initialType.description?.trim() ?? '')) body.description = nextDescription;
    return body;
  }

  function isSystemType(type: RelationshipType | undefined): boolean {
    return type?.ownership === 'system';
  }

  function optionalText(value: string | undefined): string {
    return value?.trim() || 'Not set';
  }

  async function refreshAfterUncertainOutcome(): Promise<void> {
    if (controller.createBlocked.relationshipTypes) await controller.refreshRelationshipTypes();
    if (committed) await controller.refreshRelationships();
    if (!controller.createBlocked.relationshipTypes && !controller.errors.relationshipTypes && !controller.errors.relationships) onDone();
  }

  function requestClose(): void {
    if (!submitting) onClose();
  }
</script>

<Modal
  title={initialType ? `Edit ${initialType.slug} relationship type` : 'Add relationship type'}
  ariaLabel={initialType ? `Edit ${initialType.slug} relationship type` : 'Add relationship type'}
  closeLabel="Close relationship type editor"
  onclose={requestClose}
>
  <form class="editor" aria-busy={submitting} onsubmit={(event) => { event.preventDefault(); void submit(); }}>
    <label>Slug<TextInput ariaLabel="Relationship type slug" bind:value={slug} required block disabled={submitting || !!initialType} /></label>
    <label>Forward label<TextInput ariaLabel="Relationship type forward label" bind:value={forwardLabel} required block disabled={submitting || isSystemType(initialType)} /></label>
    <label>Reverse label<TextInput ariaLabel="Relationship type reverse label" bind:value={reverseLabel} required block disabled={submitting || isSystemType(initialType)} /></label>
    {#if initialType}
      <p><strong>{initialType.is_symmetric ? 'Symmetric' : 'Directional'}</strong></p>
      <p class="muted">Symmetry cannot be changed after creation.</p>
    {:else}
      <Checkbox checked={symmetric} label="Symmetric relationship" onchange={(checked) => { symmetric = checked; }} disabled={submitting} />
    {/if}
    {#if symmetricLabelsMismatch}<p class="validation" role="alert">Symmetric relationship labels must match.</p>{/if}
    <label>vCard RELATED type<TextInput ariaLabel="Relationship type vCard type" bind:value={vcardRelatedType} block disabled={submitting || isSystemType(initialType)} /></label>
    <label>Color<TextInput ariaLabel="Relationship type color" bind:value={color} block disabled={submitting || isSystemType(initialType)} /></label>
    <label>Icon<TextInput ariaLabel="Relationship type icon" bind:value={icon} block disabled={submitting || isSystemType(initialType)} /></label>
    <label>Description<TextInput ariaLabel="Relationship type description" bind:value={description} block disabled={submitting || isSystemType(initialType)} /></label>
    {#if isSystemType(initialType)}<p role="status">System relationship types are read-only.</p>{/if}
    {#if message}
      <div role="alert">
        <p>{message}</p>
        {#if conflictCurrent}
          <section class="conflict-current" aria-label="Current saved relationship type">
            <h3>Current saved relationship type</h3>
            <dl>
              <div><dt>Forward label</dt><dd>{conflictCurrent.forward_label}</dd></div>
              <div><dt>Reverse label</dt><dd>{conflictCurrent.reverse_label}</dd></div>
              <div><dt>vCard RELATED type</dt><dd>{optionalText(conflictCurrent.vcard_related_type)}</dd></div>
              <div><dt>Color</dt><dd>{optionalText(conflictCurrent.color)}</dd></div>
              <div><dt>Icon</dt><dd>{optionalText(conflictCurrent.icon)}</dd></div>
              <div><dt>Description</dt><dd>{optionalText(conflictCurrent.description)}</dd></div>
              <div><dt>Symmetry</dt><dd>{conflictCurrent.is_symmetric ? 'Symmetric' : 'Directional'}</dd></div>
              <div><dt>Ownership</dt><dd>{conflictCurrent.ownership === 'system' ? 'Built in' : 'Custom'}</dd></div>
            </dl>
          </section>
        {/if}
      </div>
    {/if}
    {#if controller.createBlocked.relationshipTypes || committed}
      <Button label={committed ? 'Refresh relationships' : 'Refresh relationship types'} disabled={submitting} onclick={() => void refreshAfterUncertainOutcome()} />
    {/if}
    <div class="actions">
      <Button label="Cancel" disabled={submitting} onclick={requestClose} />
      {#if !isSystemType(initialType)}
        <Button type="submit" tone="info" surface="solid" label={initialType ? 'Save relationship type' : 'Create relationship type'}
          disabled={submitting || committed || !slug.trim() || !forwardLabel.trim() || !reverseLabel.trim() || symmetricLabelsMismatch || controller.createBlocked.relationshipTypes} />
      {/if}
    </div>
  </form>
</Modal>

<style>
  .editor { display: grid; gap: var(--space-3); min-width: min(30rem, 80vw); }
  label { display: grid; gap: var(--space-1); color: var(--text-muted); font-size: var(--font-size-xs); }
  p { margin: 0; }
  .muted { color: var(--text-muted); font-size: var(--font-size-sm); }
  [role="alert"] { display: grid; gap: var(--space-1); color: var(--text-danger); }
  .validation { color: var(--text-danger); }
  .conflict-current { display: grid; gap: var(--space-2); color: var(--text-default); }
  .conflict-current h3, .conflict-current dl, .conflict-current dd { margin: 0; }
  .conflict-current dl { display: grid; gap: var(--space-1); }
  .conflict-current dl > div { display: grid; grid-template-columns: minmax(8rem, 1fr) minmax(8rem, 2fr); gap: var(--space-2); }
  .conflict-current dt { color: var(--text-muted); }
  .actions { display: flex; justify-content: flex-end; gap: var(--space-2); flex-wrap: wrap; }
</style>
