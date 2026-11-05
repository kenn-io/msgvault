<script lang="ts">
  import { Button, EmptyState, Spinner, Toggle } from '@kenn-io/kit-ui';
  import { onDestroy, tick, untrack } from 'svelte';

  import type { AttributeDefinition } from '../../api/generated/models';
  import { sentenceCase } from '../../explore/labels';
  import { formatDateTime } from '../../util/format';
  import type { APIClient } from '../../api/client';
  import { PersonTrackingController } from '../../directory/person-tracking-controller.svelte';

  interface Props {
    client: APIClient;
    personID: number;
    definitions?: AttributeDefinition[];
    onAnnounce?: (message: string) => void;
  }

  let { client, personID, definitions = [], onAnnounce = () => undefined }: Props = $props();
  const controller = new PersonTrackingController(untrack(() => client));
  let root = $state<HTMLElement>();
  let catalogRetryIncludesSensitive = $state(false);

  $effect.pre(() => {
    const nextPersonID = personID;
    catalogRetryIncludesSensitive = false;
    untrack(() => { void controller.setPerson(nextPersonID); });
  });

  onDestroy(() => controller.destroy());

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

<section bind:this={root} class="maintenance" aria-label="Profile maintenance">
  <div class="tracking-section">
    {#if controller.trackingLoading}
      <p class="working" role="status" aria-busy="true"><Spinner size={14} label="Loading profile maintenance" />Loading profile maintenance…</p>
    {/if}
    {#if controller.trackingError}
      <div class="notice notice--error" role="alert">
        <span>{controller.trackingError}</span>
        <Button size="sm" label={controller.stateUnknown ? 'Retry profile maintenance state' : 'Retry profile maintenance'} disabled={controller.trackingLoading || controller.pending} onclick={() => void retryTracking()} />
      </div>
    {/if}
    {#if controller.tracking}
      <div class="tracking-row">
        <Toggle label="Update automatically" checked={controller.tracking.tracked} disabled={controller.trackingLoading || controller.pending || controller.stateUnknown} onchange={(checked) => void changeTracking(checked)} />
        <span class="tracked-time">
          {#if controller.tracking.tracked && controller.tracking.tracked_at}
            On since <time datetime={controller.tracking.tracked_at}>{formatDateTime(controller.tracking.tracked_at)}</time>
          {:else}{controller.tracking.tracked ? 'On' : 'Off'}{/if}
        </span>
      </div>
    {:else if !controller.trackingLoading && !controller.trackingError}
      <p>Profile maintenance state is unavailable.</p>
    {/if}
    <p class="helper">Uses archived conversations when a people sweep runs.</p>
    {#if controller.pending}
      <p class="working" role="status" aria-busy="true"><Spinner size={14} label="Updating profile maintenance" />Updating profile maintenance…</p>
    {/if}
  </div>

  <div class="catalog" aria-labelledby={`person-${personID}-eligible-fields-heading`}>
    <h3 id={`person-${personID}-eligible-fields-heading`} tabindex="-1">Fields it updates</h3>
    {#if controller.catalogLoading}
      <p class="working" role="status" aria-busy="true"><Spinner size={14} label="Loading profile fields" />Loading profile fields…</p>
    {/if}
    {#if controller.catalogError}
      <div class="notice notice--error" role="alert">
        <span>{controller.catalogError}</span>
        <Button size="sm" label="Retry eligible profile fields" disabled={controller.catalogLoading} onclick={() => void controller.retryCatalog(catalogRetryIncludesSensitive)} />
      </div>
    {/if}
    {#if controller.targets.length > 0}
      <ul class="target-list" class:inactive={controller.tracking?.tracked === false}>
        {#each controller.targets as target}
          <li>
            <span class="value">
              {target.kind === 'employment' ? 'Work history' : definitions.find((definition) => definition.slug === target.slug)?.label ?? sentenceCase(target.slug)}
              {#if target.kind === 'employment'}<small>Employer, title, and dates</small>{/if}
            </span>
            {#if target.sensitive}<span class="meta">Sensitive</span>{/if}
          </li>
        {/each}
      </ul>
    {:else if !controller.catalogLoading && !controller.catalogError}
      <EmptyState title="No eligible profile fields" description="The server catalog does not currently expose fields for automatic maintenance." />
    {/if}
    {#if !controller.catalogIncludesSensitive}
      <button type="button" class="reveal" disabled={controller.catalogLoading} onclick={() => void loadSensitiveCatalog()}>Show sensitive fields</button>
    {:else}
      <p class="disclosure">Sensitive fields are shown.</p>
    {/if}
  </div>
</section>

<style>
  .maintenance { display: grid; gap: var(--space-6); min-width: 0; }
  .tracking-section, .catalog { display: grid; gap: var(--space-2); min-width: 0; }
  .tracking-row, .notice { display: flex; align-items: center; justify-content: space-between; gap: var(--field-gap); }
  h3, p, ul { margin: 0; }
  .helper, .tracked-time, .meta, small, .disclosure { color: var(--text-muted); font-size: var(--font-size-sm); }
  .working { display: flex; align-items: center; gap: var(--space-2); }
  .notice { padding: var(--space-3); border: 1px solid var(--border-default); border-radius: var(--radius-md); }
  .notice--error { border-color: var(--status-error-ink); background: var(--status-error-bg); color: var(--status-error-ink); }
  .target-list { display: grid; padding: 0; list-style: none; }
  .target-list li { display: flex; align-items: baseline; justify-content: space-between; gap: var(--field-gap); padding: var(--space-2) 0; border-bottom: 1px solid var(--border-muted); }
  .value { display: grid; min-width: 0; overflow-wrap: anywhere; }
  .inactive .value { color: var(--text-muted); }
  .meta { min-width: 0; max-width: 100%; overflow-wrap: anywhere; }
  .reveal { justify-self: start; border: 0; padding: 0; background: none; color: var(--link-ink); font: inherit; font-size: var(--font-size-sm); cursor: pointer; }
  .reveal:hover { text-decoration: underline; }
  .reveal:disabled { color: var(--text-muted); cursor: default; }
  @media (max-width: 640px) {
    .target-list li, .tracking-row, .notice { align-items: flex-start; flex-direction: column; gap: var(--space-1); }
  }
</style>
