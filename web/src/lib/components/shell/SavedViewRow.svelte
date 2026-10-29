<script lang="ts">
  import { appShortcuts, IconButton, Menu, MenuContent, MenuItem, MenuTrigger, TextInput } from '@kenn-io/kit-ui';
  import Ellipsis from '@lucide/svelte/icons/ellipsis';
  import { tick } from 'svelte';

  import type { SavedView } from '../../api/generated/models';
  import { savedViewSummary, type CanonicalState } from '../../saved-views/canonical';
  import Dialog from '../common/Dialog.svelte';
  import { viewIncompatibility } from './shell-data.svelte';

  interface Props {
    view: SavedView;
    onOpen: (view: SavedView) => void;
    onRename: (view: SavedView, name: string) => Promise<string | undefined>;
    onDelete: (view: SavedView) => void;
    onDescribe?: (view: SavedView, description: string) => Promise<string | undefined>;
  }

  let { view, onOpen, onRename, onDelete, onDescribe = undefined }: Props = $props();

  let describing = $state(false);
  let descriptionView: SavedView;
  let description = $state('');
  let describeError = $state('');
  let describeBusy = $state(false);

  function beginDescribe(): void {
    descriptionView = view;
    description = view.description ?? '';
    describeError = '';
    describing = true;
  }

  async function saveDescription(): Promise<void> {
    if (!onDescribe || describeBusy) return;
    describeBusy = true;
    const failure = await onDescribe(descriptionView, description);
    describeBusy = false;
    if (failure) describeError = failure;
    else {
      describing = false;
      await focusOpen();
    }
  }

  async function closeDescription(): Promise<void> {
    if (describeBusy) return;
    describing = false;
    await focusOpen();
  }

  let renaming = $state(false);
  let renameView: SavedView;
  let renameBusy = $state(false);
  let draft = $state('');
  let error = $state('');
  let input = $state<HTMLInputElement>();
  let menuOpen = $state(false);
  $effect(() => {
    if (menuOpen) return appShortcuts.pushScope('saved-view-menu');
  });
  let row = $state<HTMLElement>();
  const incompatible = $derived(viewIncompatibility(view));
  const tooltip = $derived(
    incompatible ||
      [savedViewSummary(view.canonical_state as CanonicalState).join(' · '), view.description]
        .filter(Boolean)
        .join('\n'),
  );

  async function beginRename(): Promise<void> {
    renameView = view;
    draft = view.name;
    error = '';
    renaming = true;
    await tick();
    input?.focus();
    input?.select();
  }

  async function commitRename(): Promise<void> {
    if (!renaming || renameBusy) return;
    renameBusy = true;
    const failure = await onRename(renameView, draft);
    renameBusy = false;
    if (failure) {
      error = failure;
      return;
    }
    const restoreFocus = document.activeElement === input;
    renaming = false;
    if (restoreFocus) await focusOpen();
  }

  async function cancelRename(): Promise<void> {
    if (renameBusy) return;
    renaming = false;
    error = '';
    await focusOpen();
  }

  async function focusOpen(): Promise<void> {
    await tick();
    row?.querySelector<HTMLElement>('.saved-view__open')?.focus();
  }

  function renameKeydown(event: KeyboardEvent): void {
    if (event.key === 'Enter') {
      event.preventDefault();
      void commitRename();
    } else if (event.key === 'Escape') {
      event.preventDefault();
      event.stopPropagation();
      cancelRename();
    }
  }
</script>

<div class="saved-view" class:saved-view--open={menuOpen} bind:this={row}>
  {#if renaming}
    <div class="saved-view__rename">
      <TextInput
        ariaLabel={`Rename ${view.name}`}
        size="sm"
        block
        readonly={renameBusy}
        bind:value={draft}
        bind:inputEl={input}
        onkeydown={renameKeydown}
        onblur={() => void commitRename()}
      />
      {#if error}<span class="saved-view__error" role="alert">{error}</span>{/if}
    </div>
  {:else}
    <button
      type="button"
      class="saved-view__open"
      title={tooltip}
      aria-disabled={incompatible ? 'true' : undefined}
      onclick={() => {
        if (!incompatible) onOpen(view);
      }}
    >
      <span>{view.name}</span>
    </button>
    <span class="saved-view__menu">
      <Menu align="end" bind:open={menuOpen}>
        <MenuTrigger ariaLabel={`${view.name} actions`} title="Actions">
          {#snippet child({ attachment })}
            <IconButton size="sm" ariaLabel={`${view.name} actions`} title="Actions" {@attach attachment}>
              <Ellipsis size={14} aria-hidden="true" />
            </IconButton>
          {/snippet}
        </MenuTrigger>
        <MenuContent ariaLabel={`${view.name} actions`}>
          {#if !incompatible}
            <MenuItem onselect={() => void beginRename()}>Rename</MenuItem>
            {#if onDescribe}<MenuItem onselect={beginDescribe}>Edit description…</MenuItem>{/if}
          {/if}
          <MenuItem tone="danger" onselect={() => onDelete(view)}>Delete</MenuItem>
        </MenuContent>
      </Menu>
    </span>
  {/if}
</div>

{#if describing}
  <Dialog title={`Describe ${view.name}`} onclose={() => void closeDescription()} primaryLabel="Save" onprimary={() => void saveDescription()} primaryDisabled={describeBusy}>
    <TextInput ariaLabel="Description" bind:value={description} placeholder="What this view is for" block disabled={describeBusy} />
    {#if describeError}<span class="saved-view__error" role="alert">{describeError}</span>{/if}
  </Dialog>
{/if}

<style>

  .saved-view {
    position: relative;
    display: flex;
    min-height: var(--control-h);
    align-items: center;
    border-radius: var(--radius-md);
  }

  .saved-view:hover {
    background: var(--bg-surface-hover);
  }

  .saved-view__open {
    display: flex;
    min-width: 0;
    height: var(--control-h);
    flex: 1;
    align-items: center;
    padding: 0 var(--pane-pad-x) 0 var(--space-6);
    border: 0;
    background: transparent;
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
    text-align: left;
    cursor: pointer;
  }

  .saved-view__open:hover {
    color: var(--text-primary);
  }

  .saved-view__open[aria-disabled='true'] {
    color: var(--text-muted);
    cursor: not-allowed;
  }

  .saved-view__open span {
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  /* The menu button appears on hover and keyboard focus, and stays while its menu is open. */
  .saved-view__menu {
    display: none;
    flex: none;
    padding-right: var(--space-2);
  }

  .saved-view:hover .saved-view__menu,
  .saved-view:focus-within .saved-view__menu,
  .saved-view--open .saved-view__menu {
    display: flex;
  }

  @media (hover: none) {
    .saved-view__menu { display: flex; }
  }

  .saved-view__rename {
    display: flex;
    width: 100%;
    flex-direction: column;
    gap: var(--label-gap);
    padding: var(--space-1) var(--space-2);
  }

  .saved-view__error {
    color: var(--text-danger);
    font-size: var(--font-size-xs);
  }
</style>
