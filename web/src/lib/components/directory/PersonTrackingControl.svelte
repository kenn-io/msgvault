<script lang="ts">
  import { Button, Card, EmptyState, Spinner, Toggle } from '@kenn-io/kit-ui';
  import { onDestroy, tick, untrack } from 'svelte';

  import type { APIClient } from '../../api/client';
  import { PersonTrackingController } from '../../directory/person-tracking-controller.svelte';

  interface Props {
    client: APIClient;
    personID: number;
    onAnnounce?: (message: string) => void;
  }

  let { client, personID, onAnnounce = () => undefined }: Props = $props();
  const controller = new PersonTrackingController(untrack(() => client));
  let root = $state<HTMLElement>();
  let catalogRetryIncludesSensitive = $state(false);

  $effect.pre(() => {
    const nextPersonID = personID;
    catalogRetryIncludesSensitive = false;
    untrack(() => { void controller.setPerson(nextPersonID); });
  });

  onDestroy(() => controller.destroy());

  function kindLabel(kind: string): string {
    return kind.length === 0 ? 'Unknown' : `${kind[0]!.toUpperCase()}${kind.slice(1)}`;
  }

  async function finishInContext(contextToken: number, announcement: string | null = null): Promise<void> {
    if (!controller.isContextCurrent(contextToken)) return;
    await tick();
    if (!controller.isContextCurrent(contextToken)) return;
    if (announcement) onAnnounce(announcement);
    const target = root?.querySelector<HTMLElement>('input:not(:disabled)') ??
      root?.querySelector<HTMLElement>('h3');
    if (target?.isConnected) target.focus();
  }

  async function changeTracking(desired: boolean): Promise<void> {
    const contextToken = controller.contextToken;
    const outcome = await controller.setTracked(desired);
    if (outcome.kind !== 'confirmed' && outcome.kind !== 'reconciled') return;
    if (!controller.isContextCurrent(contextToken)) return;
    const announcement = controller.announcement;
    await finishInContext(contextToken, announcement);
  }

  async function retryTracking(): Promise<void> {
    const contextToken = controller.contextToken;
    await controller.retryTracking();
    if (!controller.isContextCurrent(contextToken)) return;
    if (!controller.trackingError && controller.tracking) await finishInContext(contextToken);
  }

  async function loadSensitiveCatalog(): Promise<void> {
    catalogRetryIncludesSensitive = true;
    await controller.retryCatalog(true);
  }
</script>

<section
  bind:this={root}
  class="maintenance"
  aria-labelledby={`person-${personID}-profile-maintenance-heading`}
>
  <Card level="default" padding="sm">
    <div class="maintenance-content">
      <div class="heading-row">
        <div>
          <h3 id={`person-${personID}-profile-maintenance-heading`} tabindex="-1">Profile maintenance</h3>
          <p>Tracking makes this person eligible for future automatic profile maintenance.</p>
        </div>
        {#if controller.trackingLoading || controller.catalogLoading}
          <span class="working" aria-label="Loading profile maintenance" aria-busy="true">
            <Spinner size={14} label="Loading profile maintenance" />
          </span>
        {/if}
      </div>

      {#if controller.trackingError}
        <div class="notice notice--error" role="alert">
          <span>{controller.trackingError}</span>
          <Button
            size="sm"
            label={controller.stateUnknown ? 'Retry profile maintenance state' : 'Retry profile maintenance'}
            disabled={controller.trackingLoading || controller.pending}
            onclick={() => void retryTracking()}
          />
        </div>
      {/if}

      {#if controller.tracking}
        <div class="tracking-row">
          <Toggle
            checked={controller.tracking.tracked}
            ariaLabel="Track this person for profile maintenance"
            disabled={controller.trackingLoading || controller.pending || controller.stateUnknown}
            onchange={(checked) => void changeTracking(checked)}
          />
          {#if controller.tracking.tracked_at}
            <span class="tracked-time">
              Tracked since
              <time datetime={controller.tracking.tracked_at}>{controller.tracking.tracked_at}</time>
            </span>
          {/if}
        </div>
      {:else if !controller.trackingLoading && !controller.trackingError}
        <p>Profile maintenance state is unavailable.</p>
      {/if}

      {#if controller.pending}
        <p class="working" role="status" aria-busy="true">
          <Spinner size={14} label="Updating profile maintenance" />
          Updating profile maintenance…
        </p>
      {/if}

      <div class="catalog" aria-labelledby={`person-${personID}-eligible-fields-heading`}>
        <div class="catalog-heading">
          <div>
            <h4 id={`person-${personID}-eligible-fields-heading`}>Eligible profile fields</h4>
            <p>Definitions describe fields the maintenance system may update; they do not show this person's values.</p>
          </div>
          {#if !controller.catalogIncludesSensitive}
            <Button
              size="sm"
              label="Show sensitive eligible fields"
              disabled={controller.catalogLoading}
              onclick={() => void loadSensitiveCatalog()}
            />
          {:else}
            <span class="disclosure">Sensitive eligible fields are shown.</span>
          {/if}
        </div>

        {#if controller.catalogError}
          <div class="notice notice--error" role="alert">
            <span>{controller.catalogError}</span>
            <Button
              size="sm"
              label="Retry eligible profile fields"
              disabled={controller.catalogLoading}
              onclick={() => void controller.retryCatalog(catalogRetryIncludesSensitive)}
            />
          </div>
        {/if}

        {#if controller.targets.length > 0}
          <ul class="target-list">
            {#each controller.targets as target}
              <li>
                <strong>{target.description}</strong>
                <span>{kindLabel(target.kind)} · {target.value_type} · {target.cardinality}</span>
                {#if target.sensitive}<span class="sensitive">Sensitive</span>{/if}
              </li>
            {/each}
          </ul>
        {:else if !controller.catalogLoading && !controller.catalogError}
          <EmptyState
            title="No eligible profile fields"
            description="The server catalog does not currently expose fields for automatic maintenance."
          />
        {/if}
      </div>
    </div>
  </Card>
</section>

<style>
  .maintenance, .maintenance-content, .catalog { display: grid; gap: var(--space-3); min-width: 0; }
  .heading-row, .catalog-heading, .tracking-row, .notice { display: flex; align-items: flex-start; justify-content: space-between; gap: var(--space-3); }
  .heading-row > div, .catalog-heading > div { display: grid; gap: var(--space-1); }
  h3, h4, p, ul { margin: 0; }
  .heading-row p, .catalog-heading p, .target-list span, .tracked-time, .disclosure { color: var(--text-muted); font-size: var(--font-size-sm); }
  .working, .tracked-time { display: flex; align-items: center; gap: var(--space-2); }
  .notice { align-items: center; padding: var(--space-3); border: 1px solid var(--border-default); border-radius: var(--radius-md); }
  .notice--error { border-color: var(--status-error-ink); background: var(--status-error-bg); color: var(--status-error-ink); }
  .target-list { display: grid; grid-template-columns: repeat(3, minmax(0, 1fr)); gap: var(--space-2); padding: 0; list-style: none; }
  .target-list li { display: grid; align-content: start; gap: var(--space-1); min-width: 0; padding: var(--space-3); border: 1px solid var(--border-default); border-radius: var(--radius-md); overflow-wrap: anywhere; }
  .sensitive { width: fit-content; padding: 1px 5px; border-radius: var(--radius-sm); background: var(--bg-warning); color: var(--text-primary) !important; }

  @media (max-width: 760px) {
    .heading-row, .catalog-heading, .tracking-row, .notice { align-items: stretch; flex-direction: column; }
    .target-list { grid-template-columns: minmax(0, 1fr); }
  }
</style>
