<script lang="ts">
  import { Button, Chip } from '@kenn-io/kit-ui';
  import type { CardDAVConnectionResponse } from '../../api/generated/models';
  import { scheduleSummary } from '../../settings/cron';
  import type { CardDAVController } from '../../carddav/controller.svelte';
  import { contactCanSync, contactFailure, contactLabel, contactLastGood, contactRunState } from '../../sources/contacts';
  import { formatDateTime } from '../../util/format';
  import SidePanel from '../common/SidePanel.svelte';
  import { operationStateChip } from '../../operations/labels';

  let {
    connection,
    controller,
    onSync,
    onclose,
    onOpenSettings,
  }: {
    connection: CardDAVConnectionResponse;
    controller: CardDAVController;
    onSync: () => void;
    onclose: () => void;
    onOpenSettings: () => void;
  } = $props();

  const status = $derived(controller.status ?? connection.status);
  const chip = $derived(operationStateChip(contactRunState(status) ?? 'Never synced'));
  const failure = $derived(contactFailure({ ...connection, status }));
  const lastGood = $derived(contactLastGood(status));
  const latest = $derived(status.latest);
</script>

<SidePanel title={contactLabel(connection)} {onclose}>
  <div class="panel">
    <div class="summary">
      <span class="muted">Contacts{status.account?.base_url ? ` · ${status.account.base_url}` : ''}</span>
      <div class="status">
        <Chip size="sm" tone={chip.tone} dot uppercase={false}>{chip.label}</Chip>
        <span class="muted">
          {#if lastGood}Last good sync {formatDateTime(lastGood)}{:else}No good sync yet{/if}
        </span>
        {#if contactCanSync(status) || controller.syncPending}
          <Button
            size="sm"
            tone="info"
            surface="soft"
            label={controller.syncPending ? 'Syncing…' : 'Sync now'}
            disabled={!controller.canSync}
            onclick={onSync}
          />
        {/if}
      </div>
    </div>

    {#if failure}
      <section class="block" aria-label="Failure">
        <p class="failure">{failure}</p>
        <Button size="sm" label="Open Settings › Contacts" onclick={onOpenSettings} />
      </section>
    {/if}

    <section class="block" aria-label="Schedule">
      <h3>Schedule</h3>
      <span class="muted">
        {#if status.scheduled && status.schedule}{scheduleSummary(status.schedule)}{:else}Not scheduled{/if}
      </span>
      {#if status.scheduled && status.next_scheduled_at}
        <span class="muted">
          Next sync <time datetime={status.next_scheduled_at} title={formatDateTime(status.next_scheduled_at, 'long')}>{formatDateTime(status.next_scheduled_at)}</time>
        </span>
      {/if}
      <Button size="sm" label="Edit in Settings" onclick={onOpenSettings} />
    </section>

    {#if latest}
      <section class="block" aria-label="Runs">
        <h3>Last run</h3>
        <span class="muted">
          {formatDateTime(latest.started_at)} · {latest.books.toLocaleString()}
          {latest.books === 1 ? 'book' : 'books'} · {latest.created.toLocaleString()} new ·
          {latest.updated.toLocaleString()} updated · {latest.removed.toLocaleString()} removed
        </span>
      </section>
    {/if}
  </div>
</SidePanel>

<style>
  .panel {
    display: grid;
    gap: var(--section-gap);
    padding: var(--pane-pad-y) var(--pane-pad-x);
  }
  .summary,
  .block {
    display: grid;
    justify-items: start;
    gap: var(--field-gap);
    min-width: 0;
  }
  .status {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--control-gap);
  }
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
</style>
