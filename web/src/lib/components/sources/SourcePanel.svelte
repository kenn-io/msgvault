<script lang="ts" module>
  // Setting keys end in these suffixes. Each group renders in the order listed.
  const FILTERS = [
    ['accounts', 'Include accounts'],
    ['exclude_accounts', 'Exclude accounts'],
    ['channels', 'Include channels'],
    ['exclude_channels', 'Exclude channels'],
    ['dms', 'Direct messages'],
    ['group_dms', 'Group messages'],
    ['rate_limit_qps', 'Requests per second'],
  ] as const;
  const ATTACHMENTS = [
    ['media', 'Download attachments'],
    ['media_scope', 'From'],
    ['media_max_participants', 'Skip chats with more people than'],
    ['max_media_mb', 'Largest file (MiB)'],
  ] as const;
</script>

<script lang="ts">
  import { Button } from '@kenn-io/kit-ui';
  import { getContext, onDestroy, onMount, untrack } from 'svelte';
  import type { APIClient } from '../../api/client';
  import type { SourceStatus } from '../../api/generated/models';
  import { scheduleSummary } from '../../settings/cron';
  import Dialog from '../common/Dialog.svelte';
  import { SettingsDraft } from '../../settings/draft.svelte';
  import {
    SOURCE_SETTING_PREFIX,
    sourceFailure,
    sourceRunState,
    sourceTypeLabel,
    syncCommand,
    syncUnavailableLabel,
  } from '../../sources/labels';
  import { formatDateTime } from '../../util/format';
  import SidePanel from '../common/SidePanel.svelte';
  import StatusChip from '../common/StatusChip.svelte';
  import CronField from '../settings/CronField.svelte';
  import SettingField from '../settings/SettingField.svelte';
  import CommandLine from './CommandLine.svelte';

  let {
    client,
    source,
    syncing = false,
    onSync,
    onclose,
    onSaved = () => undefined,
  }: {
    client: APIClient;
    source: SourceStatus;
    syncing?: boolean;
    onSync: () => void;
    onclose: () => void;
    /** Runs after a save so the page can reload the source. */
    onSaved?: () => void;
  } = $props();

  const draft = new SettingsDraft(untrack(() => client));
  const prefix = $derived(SOURCE_SETTING_PREFIX[source.source_type]);
  const failure = $derived(sourceFailure(source));
  const command = $derived(syncCommand(source));
  const runState = $derived(sourceRunState(source));
  const lastGood = $derived(source.last_successful_sync?.completed_at);
  const latest = $derived(source.latest_sync);
  const lastSuccess = $derived(source.last_successful_sync);
  const runs = $derived(
    [latest, lastSuccess && lastSuccess.id !== latest?.id ? lastSuccess : undefined].filter(
      (run): run is NonNullable<typeof run> => Boolean(run),
    ),
  );
  const itemErrors = $derived(latest?.item_errors?.length ?? 0);

  onMount(() => {
    if (prefix) void draft.load();
  });
  onDestroy(() => draft.destroy());

  let pendingLeave = $state<{ leave: () => void; stay?: () => void }>();
  const registerLeave = getContext<((hold?: (leave: () => void, stay?: () => void) => boolean, cancel?: () => void) => void) | undefined>('msgvault:hold-settings-leave');
  function holdLeave(leave: () => void, stay: (() => void) | undefined = undefined): boolean {
    if (draft.saving) return true;
    if (!draft.dirty) return false;
    pendingLeave = { leave, stay };
    return true;
  }
  registerLeave?.(holdLeave, () => pendingLeave = undefined);
  onDestroy(() => registerLeave?.(undefined));
  function stay(): void {
    pendingLeave?.stay?.();
    pendingLeave = undefined;
  }
  function discardAndLeave(): void {
    const leave = pendingLeave?.leave;
    pendingLeave = undefined;
    draft.discard();
    leave?.();
  }
  export function requestClose(next: () => void = onclose): void {
    if (!holdLeave(next)) next();
  }
  function close(): void { requestClose(); }
  function key(suffix: string): string {
    return `${prefix}.${suffix}`;
  }
  function present(rows: readonly (readonly [string, string])[]) {
    return rows.filter(([suffix]) => draft.find(key(suffix)));
  }

  // One schedule control: Off turns the switch off and any other choice turns
  // it on with that schedule.
  const hasSwitch = $derived(Boolean(prefix && draft.find(key('enabled'))));
  const scheduleOn = $derived(!hasSwitch || Boolean(draft.value(key('enabled'))));
  const schedule = $derived(scheduleOn ? String(draft.value(key('schedule')) ?? '') : '');
  // The server's next time stays accurate until the schedule is edited.
  const nextSync = $derived(
    !(key('schedule') in draft.drafts) && !(key('enabled') in draft.drafts) ? source.next_sync_at : null,
  );
  function setSchedule(value: string): void {
    if (hasSwitch) draft.set(key('enabled'), value !== '');
    if (value !== '' || !hasSwitch) draft.set(key('schedule'), value);
  }

  async function save(): Promise<void> {
    if (await draft.save()) onSaved();
  }

  function runLine(run: { messages_processed: number; messages_added: number; errors_count: number }): string {
    const parts = [`${run.messages_processed.toLocaleString()} processed`, `${run.messages_added.toLocaleString()} added`];
    if (run.errors_count > 0) parts.push(`${run.errors_count.toLocaleString()} ${run.errors_count === 1 ? 'error' : 'errors'}`);
    return parts.join(' · ');
  }
</script>

{#snippet footer()}
  <Button label="Discard" disabled={draft.saving} onclick={() => draft.discard()} />
  <Button
    tone="info"
    surface="solid"
    label={draft.saving ? 'Saving…' : 'Save changes'}
    disabled={draft.saving || draft.incomplete}
    onclick={() => void save()}
  />
{/snippet}

{#snippet field(suffix: string, label: string)}
  {@const setting = draft.find(key(suffix))}
  {#if setting}
    <div class="field">
      <span class="label">{label}</span>
      <div class="control">
      {#if setting.read_only}<span>{String(draft.value(setting.key) ?? '')}</span>{:else}
        <SettingField
          {setting}
          {label}
          value={draft.value(setting.key)}
          onchange={(value) => draft.set(setting.key, value)}
        />
      {/if}
      </div>
    </div>
  {/if}
{/snippet}

<svelte:window onbeforeunload={(event) => { if (draft.dirty) event.preventDefault(); }} />

<SidePanel title={source.display_name || source.identifier} onclose={close} footer={draft.dirty ? footer : undefined}>
  <div class="panel">
    <div class="summary">
      <span class="type">{[sourceTypeLabel(source.source_type), source.identifier].filter(Boolean).join(' · ')}</span>
      <div class="status">
        {#if runState}<StatusChip state={runState} />{/if}
        <span class="muted">
          {#if lastGood}Last good sync {formatDateTime(lastGood)}{:else}No good sync yet{/if}
        </span>
        {#if source.can_sync}
          <Button
            size="sm"
            tone="info"
            surface="soft"
            label={syncing ? 'Syncing…' : 'Sync now'}
            disabled={syncing}
            onclick={onSync}
          />
        {/if}
      </div>
    </div>

    {#if draft.pendingRestart}<p class="muted" role="status">Restart msgvault to apply these changes.</p>{/if}

    {#if failure}
      <section class="block" aria-label="Failure">
        <p class="failure">{failure.reason}</p>
        {#if failure.command}<CommandLine command={failure.command} />{/if}
      </section>
    {/if}

    {#if !source.can_sync && !syncing}
      <section class="block" aria-label="Sync">
        <span class="muted">{source.source_type === 'meeting_import' ? 'On demand' : syncUnavailableLabel(source.sync_unavailable_reason)}</span>
        {#if command}<CommandLine {command} />{/if}
      </section>
    {/if}

    {#if prefix && draft.error}<p class="failure" role="alert">{draft.error}</p><Button label="Retry" onclick={() => void draft.load()} />{/if}

    {#if prefix && draft.find(key('schedule'))}
      <section class="block" aria-label="Schedule">
        <h3>Schedule</h3>
        <span class="muted">Applies to all {sourceTypeLabel(source.source_type)} accounts.</span>
        <CronField label="Schedule" value={schedule} disabled={draft.find(key('schedule'))?.read_only} oninput={setSchedule} />
        {#if nextSync}
          <span class="muted">Next sync <time datetime={nextSync} title={formatDateTime(nextSync, 'long')}>{formatDateTime(nextSync)}</time></span>
        {/if}
      </section>
    {/if}

    {#if !prefix || !draft.find(key('schedule'))}
      <section class="block" aria-label="Schedule">
        <h3>Schedule</h3>
        <span class="muted">{source.scheduled ? source.schedule ? scheduleSummary(source.schedule) : 'Schedule unavailable' : source.source_type === 'meeting_import' ? 'On demand' : 'Not scheduled'}</span>
        {#if source.scheduled && source.next_sync_at}<span class="muted">Next sync <time datetime={source.next_sync_at}>{formatDateTime(source.next_sync_at)}</time></span>{/if}
      </section>
    {/if}

    {#if prefix && present(FILTERS).length > 0}
      <section class="block" aria-label="Filters">
        <h3>Filters</h3>
        {#each present(FILTERS) as [suffix, label] (suffix)}
          {@render field(suffix, label)}
        {/each}
      </section>
    {/if}

    {#if prefix && present(ATTACHMENTS).length > 0}
      <section class="block" aria-label="Attachments">
        <h3>Attachment rules</h3>
        <span class="muted">Applies to all {sourceTypeLabel(source.source_type)} accounts.</span>
        {#each present(ATTACHMENTS) as [suffix, label] (suffix)}
          {@render field(suffix, label)}
        {/each}
      </section>
    {/if}

    {#if runs.length > 0}
      <section class="block" aria-label="Runs">
        <h3>Runs</h3>
        <ul class="runs">
          {#each runs as run (run.id)}
            <li>
              <StatusChip
                state={sourceRunState({ latest_sync: run, active_sync: null }) ?? 'succeeded'}
              />
              <time datetime={run.started_at} title={formatDateTime(run.started_at, 'long')}>
                {formatDateTime(run.started_at)}
              </time>
              <span class="muted">{runLine(run)}</span>
            </li>
          {/each}
        </ul>
        {#if itemErrors > 0}
          <details class="item-errors">
            <summary class="muted">{itemErrors.toLocaleString()} {itemErrors === 1 ? 'item' : 'items'} failed</summary>
            <ul>
              {#each latest?.item_errors ?? [] as item (`${item.source_message_id}:${item.phase}:${item.created_at}`)}
                <li>{item.error_message}</li>
              {/each}
            </ul>
          </details>
        {/if}
      </section>
    {/if}
  </div>
</SidePanel>
{#if pendingLeave}
  <Dialog title="Discard changes?" onclose={stay} primaryLabel="Discard changes" onprimary={discardAndLeave}>
    <p>Unsaved source settings will be discarded.</p>
    <Button label="Keep editing" onclick={stay} />
  </Dialog>
{/if}

<style>
  .item-errors summary {
    cursor: pointer;
  }

  .item-errors ul {
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
  .control {
    display: flex;
    align-items: center;
    gap: var(--control-gap);
    min-width: 0;
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
