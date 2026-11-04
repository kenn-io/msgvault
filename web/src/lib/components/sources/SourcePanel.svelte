<script lang="ts">
  import { Disclosure, Button, Chip } from '@kenn-io/kit-ui';
  import { onMount, onDestroy, type Snippet } from 'svelte';
  import { getSettings } from '../../api/generated/api/api';
  import type { APIClient } from '../../api/client';
  import type { SourceStatus, Setting } from '../../api/generated/models';
  import { persistedValue, readOnlyValue } from '../../settings/catalog';
  import { scheduleSummary } from '../../settings/cron';
  import { sourceFailure, sourceRunState, sourceTypeLabel, syncRunning, syncCommand, syncUnavailableLabel, unmeasuredReasonLabel, SOURCE_SETTING_PREFIX } from '../../sources/labels';
  import { operationStateChip } from '../../operations/labels';
  import { formatDateTime } from '../../util/format';
  import SidePanel from '../common/SidePanel.svelte';
  import CommandLine from './CommandLine.svelte';

  let { client, source, syncing = false, starting = false, notice, onSync, onclose, onOpenSettings }: {
    client: APIClient;
    source: SourceStatus;
    syncing?: boolean;
    starting?: boolean;
    notice: Snippet;
    onSync: () => void;
    onclose: () => void;
    onOpenSettings: () => void;
  } = $props();
  const ATTACHMENTS = [
    ['media', 'Download attachments'], ['media_scope', 'From'],
    ['media_max_participants', 'Skip chats with more people than'], ['max_media_mb', 'Largest file (MiB)'],
  ] as const;
  let settings = $state<Setting[]>([]);
  let loading = $state(false);
  let error = $state('');
  let controller: AbortController | undefined;
  const prefix = $derived(SOURCE_SETTING_PREFIX[source.source_type]);
  const title = $derived(source.display_name || source.identifier);
  const failure = $derived(sourceFailure(source));
  const chip = $derived(operationStateChip(sourceRunState(source) ?? 'Never synced'));
  const lastGood = $derived(source.last_successful_sync?.completed_at);
  const latest = $derived(source.latest_sync);
  const unmeasured = $derived(unmeasuredReasonLabel(latest));
  const runs = $derived([latest, source.last_successful_sync?.id !== latest?.id ? source.last_successful_sync : undefined].filter((run): run is NonNullable<typeof run> => Boolean(run)));
  const attachments = $derived(ATTACHMENTS.flatMap(([suffix, label]) => {
    const setting = settings.find(setting => setting.key === `${prefix}.${suffix}`);
    return setting ? [{ setting, label }] : [];
  }));
  onMount(() => { if (prefix) void load(); });
  onDestroy(() => controller?.abort());
  async function load(): Promise<void> {
    controller?.abort();
    const current = controller = new AbortController();
    loading = true;
    try {
      const { data } = await getSettings({ ...client, signal: current.signal });
      if (current.signal.aborted) return;
      if (!data) throw new Error("Can't load attachment rules.");
      settings = data.settings;
      error = '';
    } catch (cause) {
      if (!current.signal.aborted) error = cause instanceof Error ? cause.message : "Can't load attachment rules.";
    } finally { if (!current.signal.aborted) loading = false; }
  }
  function runLine(run: { messages_processed: number; messages_added: number; errors_count: number }): string {
    const parts = [`${run.messages_processed.toLocaleString()} processed`, `${run.messages_added.toLocaleString()} added`];
    if (run.errors_count > 0) parts.push(`${run.errors_count.toLocaleString()} ${run.errors_count === 1 ? 'error' : 'errors'}`);
    return parts.join(' · ');
  }
</script>

<SidePanel {title} {onclose}>
  <div class="panel">
    <div class="summary">
      <span class="type">{[sourceTypeLabel(source.source_type), source.identifier !== title ? source.identifier : undefined].filter(Boolean).join(' · ')}</span>
      <div class="status">
        <Chip size="sm" tone={chip.tone} dot uppercase={false}>{chip.label}</Chip>
        <span class="muted">{lastGood ? `Last good sync ${formatDateTime(lastGood)}` : 'No good sync yet'}</span>
        {#if source.can_sync}<Button size="sm" tone="info" surface="soft" label={syncRunning(source) ? 'Syncing…' : syncing ? 'Starting…' : 'Sync now'} disabled={starting || syncing} onclick={onSync} />{/if}
      </div>
    </div>
    {@render notice()}
    {#if unmeasured}<section class="block" aria-label="Measurement"><p>{unmeasured}</p></section>{/if}
    {#if failure}<section class="block" aria-label="Failure"><p class="failure">{failure}</p>{#if source.scheduler_last_error && source.scheduler_last_error !== failure}<p class="failure">{source.scheduler_last_error}</p>{/if}</section>{/if}
    {#if source.source_type !== 'meeting_import' && !source.can_sync && !syncing}
      {@const command = syncCommand(source)}
      <section class="block" aria-label="Sync">
        <span class="muted">{source.scheduler_queued && !syncRunning(source) ? 'Sync queued' : syncUnavailableLabel(source.sync_unavailable_reason)}</span>
        {#if command}<CommandLine {command} />{/if}
      </section>
    {/if}
    <section class="block" aria-label="Schedule">
      <h3>Schedule</h3>
      <span class="muted" title={source.scheduled ? source.schedule : undefined}>{source.scheduled ? source.schedule ? scheduleSummary(source.schedule) : 'Schedule unavailable' : source.source_type === 'meeting_import' ? 'On demand' : 'Not scheduled'}</span>
      {#if source.scheduled && source.next_sync_at}<span class="muted">Next sync <time datetime={source.next_sync_at} title={formatDateTime(source.next_sync_at, 'long')}>{formatDateTime(source.next_sync_at)}</time></span>{/if}
    </section>
    {#if prefix}
      <section class="block" aria-label="Attachments">
        <h3>Attachment rules</h3>
        <span class="muted">Defaults for {sourceTypeLabel(source.source_type)} accounts. Account settings can override these values.</span>
        {#if loading}<span class="muted">Loading…</span>{:else if error}<p class="failure" role="alert">{error}</p><Button label="Retry" onclick={() => void load()} />{:else}
          {#each attachments as { setting, label } (setting.key)}<div class="field"><span class="label">{label}</span><span>{setting.kind === 'boolean' ? persistedValue(setting) ? 'On' : 'Off' : readOnlyValue(setting)}</span></div>{/each}
        {/if}
      </section>
      <Button size="sm" label="Edit in Settings › Sources" onclick={onOpenSettings} />
    {/if}
    {#if runs.length > 0}
      <section class="block" aria-label="Runs">
        <h3>Runs</h3>
        <ul class="runs">
          {#each runs as run (run.id)}
            {@const runChip = operationStateChip(sourceRunState({ latest_sync: run, active_sync: null }) ?? run.status)}
            <li><Chip size="sm" tone={runChip.tone} dot uppercase={false}>{runChip.label}</Chip><time datetime={run.started_at}>{formatDateTime(run.started_at)}</time><span class="muted">{runLine(run)}</span></li>
          {/each}
        </ul>
        {#if latest?.item_errors?.length}
          {@const itemErrors = latest.item_errors}
          <Disclosure variant="plain" class="item-errors">{#snippet summary()}{itemErrors.length.toLocaleString()} {itemErrors.length === 1 ? 'item' : 'items'} failed{/snippet}<ul>{#each itemErrors as item (`${item.source_message_id}:${item.phase}:${item.created_at}`)}<li>{item.error_message}</li>{/each}</ul></Disclosure>
        {/if}
      </section>
    {/if}
  </div>
</SidePanel>

<style>
  .panel :global(.item-errors .kit-disclosure__summary) {
    cursor: pointer;
  }

  .panel :global(.item-errors ul) {
    display: grid;
    gap: var(--space-1);
    margin: var(--space-2) 0 0;
    padding-left: var(--space-4);
    color: var(--text-danger);
    font-size: var(--font-size-xs);
  }

  .panel {
    display: grid;
    gap: var(--section-gap);
    padding: var(--pane-pad-y) var(--pane-pad-x);
  }
  .summary,
  .block {
    display: grid;
    gap: var(--field-gap);
    min-width: 0;
  }
  .block {
    justify-items: start;
  }
  .status {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--control-gap);
  }
  .type,
  .muted {
    color: var(--text-muted);
    font-size: var(--font-size-sm);
  }
  .failure {
    margin: 0;
    color: var(--text-danger);
    font-size: var(--font-size-md);
  }
  h3 {
    margin: 0;
    color: var(--text-primary);
    font-size: var(--font-size-md);
    font-weight: var(--font-weight-semibold);
  }
  .field {
    display: grid;
    width: 100%;
    gap: var(--label-gap);
  }
  .label {
    color: var(--text-secondary);
    font-size: var(--font-size-xs);
  }
  .runs {
    display: grid;
    width: 100%;
    gap: var(--field-gap);
    margin: 0;
    padding: 0;
    list-style: none;
  }
  .runs li {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--control-gap);
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
  }
</style>
