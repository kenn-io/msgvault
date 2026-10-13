<script lang="ts">
  import { Button, Card, EmptyState, SettingsSection, Spinner } from '@kenn-io/kit-ui';
  import { tick } from 'svelte';

  import type {
    CardDAVConflictChoice,
    CardDAVConflictFocusRequest,
    CardDAVConflictSummary,
    CardDAVConflictsController
  } from '../../carddav/conflicts-controller.svelte';
  import CardDAVConflictDecisionModal from './CardDAVConflictDecisionModal.svelte';

  let { controller }: { controller: CardDAVConflictsController } = $props();
  let activeChoice = $state<CardDAVConflictChoice>();
  let decisionTrigger = $state<HTMLButtonElement>();
  let conflictsHeading = $state<HTMLHeadingElement>();
  let detailHeading = $state<HTMLHeadingElement>();
  let conflictSurface = $state<HTMLDivElement>();
  let unavailableStatus = $state<HTMLDivElement>();
  let surfaceHadFocus = false;
  let handledFocusKey = 0;
  let focusContext = 0;

  const comparisonCards = $derived(controller.selectedDetail ? [
    { key: 'base', label: 'Last synced version', summary: controller.selectedDetail.base, deleted: 'Contact deleted.' },
    { key: 'local', label: 'msgvault', summary: controller.selectedDetail.local, deleted: 'Marked for removal from the address book.' },
    { key: 'remote', label: controller.selectedDetail.address_book.name, summary: controller.selectedDetail.remote, deleted: 'Contact deleted.' }
  ] : []);

  $effect(() => {
    if (controller.unavailable) {
      const shouldFocusUnavailable = surfaceHadFocus;
      focusContext += 1;
      activeChoice = undefined;
      decisionTrigger = undefined;
      handledFocusKey = 0;
      if (shouldFocusUnavailable) void focusUnavailableSurface(focusContext);
      return;
    }
    const request = controller.focusRequest;
    if (!request || request.key === handledFocusKey || activeChoice || controller.pendingResolutionID !== undefined) return;
    handledFocusKey = request.key;
    void focusRequestedSurface(request, focusContext);
  });

  function stateLabel(state: CardDAVConflictSummary['state']): string {
    return state === 'present' ? 'Contact saved' : state === 'deleted' ? 'Contact deleted' : 'Details unavailable';
  }

  function resolutionLabel(choice: CardDAVConflictChoice): string {
    return choice === 'keep_local' ? 'msgvault' : 'address book';
  }

  function formatTimestamp(value: string): string {
    const parsed = new Date(value);
    if (!Number.isFinite(parsed.getTime())) return value;
    return new Intl.DateTimeFormat(undefined, { dateStyle: 'medium', timeStyle: 'short' }).format(parsed);
  }

  function openDecision(choice: CardDAVConflictChoice, event: MouseEvent): void {
    if (!controller.isResolutionAllowed(choice)) return;
    surfaceHadFocus = true;
    decisionTrigger = event.currentTarget as HTMLButtonElement;
    activeChoice = choice;
  }

  async function closeDecision(): Promise<void> {
    if (controller.pendingResolutionID !== undefined) return;
    activeChoice = undefined;
    await tick();
    if (decisionTrigger?.isConnected) decisionTrigger.focus();
    else if (detailHeading?.isConnected) detailHeading.focus();
  }

  async function confirmDecision(): Promise<void> {
    const choice = activeChoice;
    const id = controller.selectedDetail?.id;
    if (!choice || id === undefined) return;
    const outcome = await controller.resolve(id, choice);
    if (outcome.kind === 'error' || outcome.kind === 'ignored') return;
    activeChoice = undefined;
    await tick();
    if (outcome.kind !== 'resolved') {
      if (decisionTrigger?.isConnected) decisionTrigger.focus();
      else if (detailHeading?.isConnected) detailHeading.focus();
    }
  }

  async function focusRequestedSurface(request: CardDAVConflictFocusRequest, context: number): Promise<void> {
    await tick();
    if (context !== focusContext || controller.unavailable) return;
    const target = request.detail
      ? detailHeading
      : request.conflictID === undefined
        ? conflictsHeading
        : document.getElementById(`carddav-conflict-row-${request.conflictID}`);
    if (target?.isConnected) {
      target.focus();
      return;
    }
    if (conflictsHeading?.isConnected) conflictsHeading.focus();
  }

  async function focusUnavailableSurface(context: number): Promise<void> {
    await tick();
    if (context !== focusContext || !controller.unavailable || !surfaceHadFocus) return;
    const active = document.activeElement;
    if (active?.isConnected && active !== document.body && !conflictSurface?.contains(active)) return;
    if (unavailableStatus?.isConnected) unavailableStatus.focus();
  }

  function rememberSurfaceFocus(): void {
    surfaceHadFocus = true;
  }

  function forgetSurfaceFocus(event: FocusEvent): void {
    if (event.relatedTarget instanceof Node) {
      if (
        activeChoice &&
        event.relatedTarget instanceof Element &&
        event.relatedTarget.closest('[data-carddav-conflict-decision]')
      ) return;
      if (!conflictSurface?.contains(event.relatedTarget)) surfaceHadFocus = false;
      return;
    }
    // A removed focused subtree also reports a null relatedTarget. Preserve
    // ownership only for that exact unavailable replacement; ordinary blur
    // while the available surface remains mounted relinquishes it.
    if (!controller.unavailable) surfaceHadFocus = false;
  }
</script>

<SettingsSection
  title="Contact conflicts"
>
  <div bind:this={conflictSurface} class="conflict-surface" onfocusin={rememberSurfaceFocus} onfocusout={forgetSurfaceFocus}>
    {#if controller.unavailable}
      <div
        bind:this={unavailableStatus}
        class="unavailable-status"
        role="status"
        aria-label="Contact conflicts unavailable."
        tabindex="-1"
      >
        <EmptyState
          title="Contact conflicts unavailable."
          description="Check your address-book connection in Settings."
        />
      </div>
    {:else}
      <div class="conflicts" aria-label="Contact conflicts" aria-busy={controller.listLoading}>
    <div class="queue-panel">
      <h3 bind:this={conflictsHeading} tabindex="-1">Unresolved conflicts</h3>
      {#if controller.listLoading}
        <p class="working"><Spinner size={14} label="Loading contact conflicts" /> Loading contact conflicts…</p>
      {/if}
      {#if controller.listError}
        <div class="notice notice--error" role="alert">
          <span>{controller.listError}</span>
          <Button size="sm" label="Retry contact conflicts" disabled={controller.listLoading} onclick={() => void controller.retryList()} />
        </div>
      {/if}
      {#if !controller.listLoading && controller.conflicts.length === 0}
        <EmptyState title="No contact conflicts." />
      {:else}
        <div class="conflict-list">
          {#each controller.conflicts as conflict (conflict.id)}
            <button
              id={`carddav-conflict-row-${conflict.id}`}
              class="conflict-row"
              class:conflict-row--selected={controller.selectedID === conflict.id}
              type="button"
              aria-label={`Review conflict ${conflict.id} in ${conflict.address_book.name}`}
              aria-pressed={controller.selectedID === conflict.id}
              disabled={controller.pendingResolutionID !== undefined}
              onclick={() => void controller.select(conflict.id)}
            >
              <strong>Conflict {conflict.id}</strong>
              <span>{conflict.address_book.name}</span>
              <span>msgvault: {conflict.local_state === 'deleted' ? 'Remove from address book' : stateLabel(conflict.local_state)} · {conflict.address_book.name}: {stateLabel(conflict.remote_state)}</span>
              <time datetime={conflict.updated_at}>Updated {formatTimestamp(conflict.updated_at)}</time>
            </button>
          {/each}
        </div>
      {/if}
    </div>

    <section class="detail-panel" aria-labelledby="carddav-conflict-detail-heading">
      <h3 bind:this={detailHeading} id="carddav-conflict-detail-heading" tabindex="-1">Conflict comparison</h3>
      <p class="disclosure">Only display name, email addresses, and phone numbers are shown. Your choice also applies to contact details not shown here.</p>

      {#if controller.detailLoading && !controller.selectedDetail}
        <p class="working" aria-busy="true"><Spinner size={14} label="Loading contact details" /> Loading conflict details…</p>
      {/if}
      {#if controller.detailError}
        <div class="notice notice--error" role="alert">
          <span>{controller.detailError}</span>
          <Button size="sm" label="Retry conflict details" disabled={controller.detailLoading} onclick={() => void controller.retrySelectedState()} />
        </div>
      {/if}
      {#if controller.resolutionError && !controller.resolutionUnknown}
        <p class="notice notice--error" role="alert">{controller.resolutionError}</p>
      {/if}
      {#if controller.resolutionUnknown}
        <div class="notice notice--error" role="alert">
          <span>Unable to confirm whether your choice was saved. Refresh before choosing again.</span>
          <Button size="sm" label="Refresh contact" disabled={controller.listLoading || controller.detailLoading} onclick={() => void controller.retrySelectedState()} />
        </div>
      {/if}

      {#if controller.selectedDetail}
        {@const selected = controller.selectedDetail}
        <div class="comparison" role="region" aria-label={`Contact conflict ${selected.id} comparison`} aria-busy={controller.detailLoading || controller.pendingResolutionID === selected.id}>
          {#each comparisonCards as card (card.key)}
            <Card level="default" padding="sm" class="comparison-card" ariaLabel={`${card.label} contact details`}>
              <div class="summary">
                <h4>{card.label}</h4>
                {#if card.summary.state === 'present'}
                  <dl>
                    <div><dt>Display name</dt><dd>{card.summary.display_name || 'No display name'}</dd></div>
                    <div>
                      <dt>Email addresses</dt>
                      <dd>
                        {#if card.summary.emails.length === 0}<span>No email addresses</span>
                        {:else}<ul>{#each card.summary.emails as email}<li>{email}</li>{/each}</ul>{/if}
                      </dd>
                    </div>
                    <div>
                      <dt>Phone numbers</dt>
                      <dd>
                        {#if card.summary.phones.length === 0}<span>No phone numbers</span>
                        {:else}<ul>{#each card.summary.phones as phone}<li>{phone}</li>{/each}</ul>{/if}
                      </dd>
                    </div>
                  </dl>
                  {#if card.summary.truncated}<p class="truncated">Additional name, email, or phone values are not shown.</p>{/if}
                {:else if card.summary.state === 'deleted'}
                  <p class="state-text">{card.deleted}</p>
                {:else}
                  <p class="state-text">Contact details unavailable.</p>
                {/if}
              </div>
            </Card>
          {/each}
        </div>

        {#if selected.status === 'resolved'}
          <p class="notice notice--success">Resolved{selected.resolution ? ` using the ${resolutionLabel(selected.resolution)} version.` : '.'}</p>
        {:else if !controller.resolutionUnknown}
          <div class="actions" aria-label="Conflict resolution choices">
            {#each selected.allowed_resolutions as choice (choice)}
              <Button
                tone="info"
                surface="solid"
                label={`Use ${resolutionLabel(choice)} version`}
                disabled={!controller.isResolutionAllowed(choice)}
                onclick={(event) => openDecision(choice, event)}
              />
            {/each}
          </div>
        {/if}
      {:else if !controller.detailLoading && !controller.detailError}
        <p>Select a contact to compare versions.</p>
      {/if}
    </section>
      </div>
    {/if}
  </div>

  {#if controller.announcement}
    <p class="sr-announcement" role="status" aria-live="polite">{controller.announcement}</p>
  {/if}
</SettingsSection>

{#if activeChoice && controller.selectedDetail}
  <div data-carddav-conflict-decision>
    <CardDAVConflictDecisionModal
      addressBookName={controller.selectedDetail.address_book.name}
      localState={controller.selectedDetail.local.state}
      remoteState={controller.selectedDetail.remote.state}
      choice={activeChoice}
      pending={controller.pendingResolutionID === controller.selectedDetail.id}
      error={controller.resolutionError}
      onConfirm={confirmDecision}
      onClose={() => void closeDecision()}
    />
  </div>
{/if}

<style>
  .conflicts { display: grid; grid-template-columns: minmax(14rem, 0.7fr) minmax(0, 2fr); gap: var(--space-5); min-width: 0; }
  .conflict-surface { min-width: 0; }
  .unavailable-status:focus-visible { outline: var(--focus-ring); outline-offset: var(--focus-ring-offset, 2px); }
  .queue-panel, .detail-panel, .conflict-list, .summary { display: grid; align-content: start; gap: var(--space-3); min-width: 0; }
  h3, h4, p, dl, dd { margin: 0; }
  .working { display: flex; align-items: center; gap: var(--space-2); }
  .conflict-row { display: grid; gap: var(--space-1); width: 100%; padding: var(--space-3); border: 1px solid var(--border-default); border-radius: var(--radius-md); background: var(--bg-surface); color: var(--text-secondary); font: inherit; text-align: left; overflow-wrap: anywhere; cursor: pointer; }
  .conflict-row strong { color: var(--text-primary); }
  .conflict-row--selected { border-color: var(--accent-blue); background: color-mix(in srgb, var(--accent-blue) 7%, var(--bg-surface)); }
  .conflict-row:hover:not(:disabled) { border-color: var(--text-muted); }
  .conflict-row:focus-visible { outline: var(--focus-ring); outline-offset: var(--focus-ring-offset, 2px); }
  .conflict-row:disabled { cursor: not-allowed; opacity: var(--opacity-disabled); }
  .comparison { display: grid; grid-template-columns: repeat(3, minmax(0, 1fr)); gap: var(--space-3); min-width: 0; }
  :global(.comparison-card .kit-card__body) { min-width: 0; }
  .summary, .summary dd, .summary li { overflow-wrap: anywhere; word-break: break-word; }
  .summary dl { display: grid; gap: var(--space-3); }
  .summary dl > div { display: grid; gap: var(--space-1); }
  .summary dt { color: var(--text-muted); font-size: var(--font-size-xs); font-weight: var(--font-weight-semibold, 600); }
  .summary ul { margin: 0; padding-left: var(--space-5); }
  .state-text { color: var(--text-primary); font-weight: var(--font-weight-semibold, 600); }
  .truncated, .disclosure { color: var(--text-muted); font-size: var(--font-size-sm); }
  .actions { display: flex; flex-wrap: wrap; gap: var(--space-3); }
  .notice { display: flex; align-items: center; justify-content: space-between; gap: var(--space-3); padding: var(--space-3); border: 1px solid var(--border-default); border-radius: var(--radius-md); }
  .notice--error { border-color: var(--status-error-ink); background: var(--status-error-bg); color: var(--status-error-ink); }
  .notice--success { border-color: var(--status-success-ink); background: var(--status-success-bg); color: var(--status-success-ink); }
  .sr-announcement { margin: 0; }

  @media (max-width: 760px) {
    .conflicts, .comparison { grid-template-columns: 1fr; }
    .actions { align-items: stretch; flex-direction: column; }
    .actions :global(button) { width: 100%; }
  }
</style>
