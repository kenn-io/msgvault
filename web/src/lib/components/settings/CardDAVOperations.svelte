<script lang="ts">
  import {
    Button,
    Card,
    Checkbox,
    Chip,
    SettingsSection,
    Spinner,
    StatusDot,
    Table,
    TableHeaderCell,
    type ChipTone
  } from '@kenn-io/kit-ui';

  import type { CardDAVBook, CardDAVBookRoles, CardDAVController } from '../../carddav/controller.svelte';
  import type { components } from '../../api/generated/schema';

  type CardDAVRun = components['schemas']['CardDAVRunResponse'];
  type RepairReason = NonNullable<components['schemas']['CardDAVStatusResponse']['repair_reason']>;

  let { controller }: { controller: CardDAVController } = $props();

  const repairCopy: Record<RepairReason, string> = {
    account_missing: 'CardDAV account discovery is missing. Test and save the account again.',
    credential_missing: 'No CardDAV password is stored. Enter the password and save the account.',
    credential_mismatch: 'The stored password belongs to different CardDAV account settings. Enter it again and save.',
    credential_unavailable: 'The stored CardDAV password cannot be read. Enter it again and save.',
    runtime_unavailable: 'CardDAV is configured but unavailable in this daemon session. Test and save the account again, or restart the daemon.'
  };

  function changeRole(book: CardDAVBook, field: keyof CardDAVBookRoles, checked: boolean): void {
    const next = { ...controller.rolesFor(book), [field]: checked };
    if (field === 'write_target' && checked) next.subscribed = true;
    if (field === 'subscribed' && !checked) next.write_target = false;
    controller.setBookDraft(book.id, next);
  }

  function statusTone(state: CardDAVRun['state']): ChipTone {
    if (state === 'succeeded') return 'success';
    if (state === 'failed') return 'danger';
    if (state === 'partial') return 'warning';
    if (state === 'cancelled') return 'canceled';
    return 'info';
  }

  function statusLabel(state: CardDAVRun['state']): string {
    return state.charAt(0).toUpperCase() + state.slice(1);
  }

  function counters(run: CardDAVRun): string {
    return `${run.books.toLocaleString()} books · ${run.created.toLocaleString()} created · ${run.updated.toLocaleString()} updated · ${run.removed.toLocaleString()} removed`;
  }

  function formatTimestamp(value: string | undefined): string {
    if (!value) return 'Not available';
    const parsed = new Date(value);
    if (!Number.isFinite(parsed.getTime())) return value;
    return new Intl.DateTimeFormat(undefined, { dateStyle: 'medium', timeStyle: 'short' }).format(parsed);
  }
</script>

<SettingsSection title="CardDAV status" description="Runtime, credential, scheduler, and recent synchronization state.">
  <div class="section-state" aria-busy={controller.statusLoading}>
    {#if controller.statusLoading}<p class="working"><Spinner size={14} label="Loading CardDAV status" /> Loading CardDAV status…</p>{/if}
    {#if controller.statusError}
      <div class="notice notice--error" role="alert">
        <span>{controller.statusError}</span>
        <Button size="sm" label="Retry CardDAV status" onclick={() => void controller.retryStatus()} />
      </div>
    {/if}
    {#if controller.status}
      {@const status = controller.status}
      <div class="status-grid" aria-label="CardDAV status summary">
        <span><span aria-hidden="true"><StatusDot status={status.configured ? 'idle' : 'unclean'} /></span> {status.configured ? 'Configured' : 'Not configured'}</span>
        <span><span aria-hidden="true"><StatusDot status={status.available ? 'idle' : 'unclean'} /></span> {status.available ? 'Runtime available' : 'Runtime unavailable'}</span>
        <span><span aria-hidden="true"><StatusDot status={status.credential_configured ? 'idle' : 'unclean'} /></span> {status.credential_configured ? 'Credential ready' : 'Credential needs attention'}</span>
        <span>{status.enabled ? 'Scheduled sync enabled' : 'Scheduled sync disabled'}</span>
        <span>{status.scheduled ? `Scheduled · ${status.schedule || 'Schedule unavailable'}` : 'Manual sync only'}</span>
        {#if status.next_scheduled_at}
          <span>Next run <time datetime={status.next_scheduled_at} title={status.next_scheduled_at}>{formatTimestamp(status.next_scheduled_at)}</time></span>
        {/if}
      </div>
      {#if status.repair_reason}<p class="notice notice--error" role="alert">{repairCopy[status.repair_reason]}</p>{/if}
      {#if status.active}
        <div class="run-summary" aria-label="Active CardDAV sync">
          <span class="working"><Spinner size={14} label="CardDAV sync running" /> Running</span>
          <strong>{counters(status.active)}</strong>
          <time datetime={status.active.started_at} title={status.active.started_at}>Started {formatTimestamp(status.active.started_at)}</time>
        </div>
      {:else if status.latest}
        <div class="run-summary" aria-label="Latest CardDAV sync">
          <Chip size="sm" tone={statusTone(status.latest.state)} uppercase={false}>{statusLabel(status.latest.state)}</Chip>
          <strong>{counters(status.latest)}</strong>
          <time datetime={status.latest.finished_at ?? status.latest.started_at} title={status.latest.finished_at ?? status.latest.started_at}>{formatTimestamp(status.latest.finished_at ?? status.latest.started_at)}</time>
          {#if status.latest.error_message}<span class="error-copy">{status.latest.error_message}</span>{/if}
        </div>
      {:else}<p>No CardDAV sync has run yet.</p>{/if}
      {#if status.latest_successful && status.latest_successful.id !== status.latest?.id}
        <p>Last successful sync: <time datetime={status.latest_successful.finished_at ?? status.latest_successful.started_at} title={status.latest_successful.finished_at ?? status.latest_successful.started_at}>{formatTimestamp(status.latest_successful.finished_at ?? status.latest_successful.started_at)}</time></p>
      {/if}
    {/if}
    {#if controller.syncError}<p class="notice notice--error" role="alert">{controller.syncError}</p>{/if}
    {#if controller.syncStatus}<p class="notice notice--success" role="status">{controller.syncStatus}</p>{/if}
    {#if controller.syncUnknown}
      <div class="notice notice--error" role="alert">
        <span>Current CardDAV sync state is unknown. Refresh state before starting another sync.</span>
        <Button size="sm" label="Retry CardDAV sync state" disabled={controller.syncPending} onclick={() => void controller.retrySyncState()} />
      </div>
    {/if}
    <div class="actions">
      <Button label={controller.syncPending ? 'Checking CardDAV state…' : 'Sync now'} disabled={!controller.canSync} onclick={() => void controller.sync(false)} />
      <Button label={controller.syncPending ? 'Checking CardDAV state…' : 'Full sync'} disabled={!controller.canSync} tone="info" onclick={() => void controller.sync(true)} />
    </div>
  </div>
</SettingsSection>

<SettingsSection title="Address books" description="Choose which discovered books participate in synchronization, lookup, and publication.">
  <div class="section-state" aria-busy={controller.booksLoading}>
    {#if controller.booksLoading}<p class="working"><Spinner size={14} label="Loading CardDAV address books" /> Loading address books…</p>{/if}
    {#if controller.booksError}
      <div class="notice notice--error" role="alert">
        <span>{controller.booksError}</span>
        <Button size="sm" label="Retry CardDAV books" onclick={() => void controller.retryBooks()} />
      </div>
    {/if}
    {#if controller.bookError}<p class="notice notice--error" role="alert">{controller.bookError}</p>{/if}
    {#if controller.bookStatus}<p class="notice notice--success" role="status">{controller.bookStatus}</p>{/if}
    {#if controller.booksUnknown}<p class="notice notice--error" role="alert">Current address-book roles are unknown. Retry book state before editing.</p>{/if}
    {#if !controller.booksLoading && controller.books.length === 0}<p>No discovered address books.</p>{/if}
    <div class="book-list">
      {#each controller.books as book (book.id)}
        {@const roles = controller.rolesFor(book)}
        {@const draft = controller.bookDraft(book.id)}
        <Card level="default" padding="sm" class="book-card" ariaLabel={`Address book ${book.name}`} title={book.name}>
          {#snippet actions()}{#if book.needs_full_reconcile}<Chip size="sm" tone="warning" uppercase={false}>Full reconciliation required</Chip>{/if}{/snippet}
          <div class="role-controls">
            <Checkbox checked={roles.subscribed} label="Sync contacts" ariaLabel={`Sync contacts for ${book.name}`} disabled={!controller.canSetBookRoles} onchange={(checked) => changeRole(book, 'subscribed', checked)} />
            <Checkbox checked={roles.lookup_source} label="Use for lookup" ariaLabel={`Use for lookup for ${book.name}`} disabled={!controller.canSetBookRoles} onchange={(checked) => changeRole(book, 'lookup_source', checked)} />
            <Checkbox checked={roles.write_target} label="Publish here" ariaLabel={`Publish here for ${book.name}`} disabled={!controller.canSetBookRoles} onchange={(checked) => changeRole(book, 'write_target', checked)} />
          </div>
          {#if roles.write_target && !book.write_target && !book.subscribed}<p class="helper" role="status">Publishing here also enables contact sync for this book.</p>{/if}
          <Button size="sm" label={controller.bookPendingID === book.id ? `Applying roles for ${book.name}…` : `Apply roles for ${book.name}`} disabled={!draft || !controller.canSetBookRoles} onclick={() => void controller.setBookRoles(book.id, roles)} />
        </Card>
      {/each}
    </div>
  </div>
</SettingsSection>

<SettingsSection title="Sync history" description="Recent manual and scheduled CardDAV runs.">
  <div class="section-state" aria-busy={controller.runsLoading || controller.runsPageLoading}>
    {#if controller.runsLoading}<p class="working"><Spinner size={14} label="Loading CardDAV sync history" /> Loading sync history…</p>{/if}
    {#if controller.runsError}
      <div class="notice notice--error" role="alert"><span>{controller.runsError}</span><Button size="sm" label="Retry CardDAV history" onclick={() => void controller.retryRuns()} /></div>
    {/if}
    {#if !controller.runsLoading && controller.runs.length === 0}<p>No CardDAV sync history yet.</p>{/if}
    {#if controller.runs.length > 0}
      <!-- svelte-ignore a11y_no_noninteractive_tabindex (the overflow region must accept keyboard focus) -->
      <div class="history-scroll" role="region" aria-label="Scrollable CardDAV sync history" tabindex="0">
        <Table ariaLabel="CardDAV sync history" zebra={false} class="history-table">
          {#snippet header()}
            <TableHeaderCell label="Result" />
            <TableHeaderCell label="Started" />
            <TableHeaderCell label="Trigger" />
            <TableHeaderCell label="Changes" />
          {/snippet}
          {#each controller.runs as historyRun (historyRun.id)}
            <tr>
              <td><Chip size="sm" tone={statusTone(historyRun.state)} uppercase={false}>{statusLabel(historyRun.state)}</Chip>{#if historyRun.full} <span>Full</span>{/if}</td>
              <td><time datetime={historyRun.started_at} title={historyRun.started_at}>{formatTimestamp(historyRun.started_at)}</time></td>
              <td>{historyRun.trigger === 'scheduled' ? 'Scheduled' : 'Manual'}</td>
              <td>{counters(historyRun)}{#if historyRun.error_message}<span class="error-copy">{historyRun.error_message}</span>{/if}</td>
            </tr>
          {/each}
        </Table>
      </div>
    {/if}
    {#if controller.runsPageError}<p class="notice notice--error" role="alert">{controller.runsPageError}</p>{/if}
    {#if controller.nextBeforeID !== undefined || controller.runsPageError}
      <Button label={controller.runsPageError ? 'Retry more CardDAV history' : controller.runsPageLoading ? 'Loading more…' : 'Load more history'} disabled={controller.runsPageLoading} onclick={() => void (controller.runsPageError ? controller.retryRuns() : controller.loadMoreRuns())} />
    {/if}
  </div>
</SettingsSection>

<style>
  .section-state, .run-summary, .book-list { display: grid; gap: var(--space-3); min-width: 0; }
  .status-grid { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: var(--space-2) var(--space-5); }
  .status-grid span, .working { display: flex; align-items: center; gap: var(--space-2); }
  .actions, .role-controls { display: flex; align-items: center; flex-wrap: wrap; gap: var(--space-3); }
  :global(.book-card .kit-card__body) { display: grid; gap: var(--space-3); min-width: 0; }
  .helper { margin: 0; color: var(--text-muted); font-size: var(--font-size-sm); }
  .notice { display: flex; align-items: center; justify-content: space-between; gap: var(--space-3); margin: 0; padding: var(--space-3); border: 1px solid var(--border-default); border-radius: var(--radius-md); }
  .notice--error { border-color: var(--status-error-ink); background: var(--status-error-bg); color: var(--status-error-ink); }
  .notice--success { border-color: var(--status-success-ink); background: var(--status-success-bg); color: var(--status-success-ink); }
  .error-copy { display: block; color: var(--text-danger); font-size: var(--font-size-sm); }
  .history-scroll { overflow: auto; border: 1px solid var(--border-muted); border-radius: var(--radius-md); }
  .history-scroll:focus-visible { outline: var(--focus-ring); outline-offset: var(--focus-ring-offset, 2px); }
  .history-scroll :global(.history-table) { overflow: visible; flex: none; }
  :global(.history-table .kit-table) { min-width: 720px; }
  :global(.history-table td) { vertical-align: top; }
  p { margin: 0; }

  @media (max-width: 760px) {
    .status-grid { grid-template-columns: 1fr; }
    .actions, .role-controls { align-items: stretch; flex-direction: column; }
    .actions :global(button), :global(.book-card button) { width: 100%; }
  }
</style>
