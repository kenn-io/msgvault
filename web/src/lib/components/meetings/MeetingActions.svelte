<script lang="ts">
  import { Button, Card, EmptyState } from '@kenn-io/kit-ui';
  import { onDestroy, untrack } from 'svelte';

  import type { APIClient } from '../../api/client';
  import type {
    ActionCoverage,
    ActionRow,
    ActionsPage,
    MeetingActionsRequest,
    MeetingRef,
  } from '../../api/generated/models';
  import { createMeetingsAPI } from '../../meetings/api';
  import { MeetingActionsController } from '../../meetings/controller.svelte';

  let {
    client,
    request = undefined,
    evidence = undefined,
    onOpenMeeting = undefined,
  }: {
    client: APIClient;
    request?: MeetingActionsRequest;
    evidence?: ActionsPage;
    onOpenMeeting?: (meeting: MeetingRef) => void;
  } = $props();

  const headingID = $props.id();
  const api = createMeetingsAPI(untrack(() => client));
  const controller = new MeetingActionsController(api);
  const displayedPage = $derived(evidence ?? controller.page);
  const requestFingerprint = $derived(JSON.stringify(request));

  $effect(() => {
    void requestFingerprint;
    untrack(() => { void controller.load(request); });
  });

  onDestroy(() => controller.destroy());

  function retry(): void {
    if (displayedPage?.next_cursor && controller.error?.recovery === 'retry') void controller.loadMore();
    else void controller.load(request);
  }

  function emptyState(coverage: ActionCoverage): string {
    if (coverage.meeting_count === 0) return 'No meetings in this scope';
    if (coverage.meeting_count > 0 && coverage.available === coverage.meeting_count) {
      return 'No recorded action items';
    }
    if (coverage.unavailable > 0) return 'Action item evidence is unavailable for this meeting.';
    if (coverage.unsupported > 0) return 'Action items are not supported by this meeting source.';
    if (coverage.partial > 0) return 'Action item evidence is partial for this meeting.';
    return 'Action item evidence is unavailable for this meeting.';
  }

  function coverageSummary(coverage: ActionCoverage): string {
    return `Coverage: ${coverage.available} available · ${coverage.partial} partial · ${coverage.unsupported} unsupported · ${coverage.unavailable} unavailable`;
  }

  function assignee(row: ActionRow): string {
    const { assignee_name: name, assignee_email: email } = row.action;
    if (name && email) return `${name} · ${email}`;
    return name || email || 'Unassigned';
  }

  function openMeeting(event: MouseEvent, meeting: MeetingRef): void {
    if (!onOpenMeeting) return;
    event.preventDefault();
    onOpenMeeting(meeting);
  }
</script>

<section class="meeting-actions" aria-labelledby={headingID}>
  <Card level="inset" padding="sm">
    <header class="actions-heading">
      <div>
        <span class="eyebrow">Archive evidence</span>
        <h3 id={headingID}>Archived action items</h3>
      </div>
      {#if displayedPage}
        <span class="meeting-count">{displayedPage.coverage.meeting_count.toLocaleString()} in scope</span>
      {/if}
    </header>
    {#if controller.loading}
      <p role="status">Loading action evidence…</p>
    {/if}
    {#if controller.error}
      <p role="alert">{controller.error.message}</p>
      {#if controller.error.recovery === 'retry' || controller.error.recovery === 'reload'}
        <Button label={controller.error.recovery === 'retry' ? 'Retry action items' : 'Reload action items'} size="sm" surface="outline" onclick={retry} />
      {/if}
    {/if}
    {#if displayedPage}
      <div class="coverage" role="status">
        <span class="kit-sr-only">{coverageSummary(displayedPage.coverage)}</span>
        <div aria-hidden="true">
          <span>Evidence coverage</span>
          <ul>
            <li class:empty={displayedPage.coverage.available === 0}><strong>{displayedPage.coverage.available}</strong> available</li>
            <li class:empty={displayedPage.coverage.partial === 0}><strong>{displayedPage.coverage.partial}</strong> partial</li>
            <li class:empty={displayedPage.coverage.unsupported === 0}><strong>{displayedPage.coverage.unsupported}</strong> unsupported</li>
            <li class:empty={displayedPage.coverage.unavailable === 0}><strong>{displayedPage.coverage.unavailable}</strong> unavailable</li>
          </ul>
        </div>
      </div>
      {#if displayedPage.rows.length === 0}
        <EmptyState title={emptyState(displayedPage.coverage)} description="Source evidence is shown without local completion state." />
      {:else}
        <ol>
          {#each displayedPage.rows as row (`${row.meeting.message_id}:${row.action.locator}`)}
            <li>
              <strong>{row.action.title}</strong>
              {#if row.action.description}<p>{row.action.description}</p>{/if}
              <dl>
                <div><dt>Source status</dt><dd>{row.action.source_status != null && row.action.source_status !== row.action.status ? `${row.action.status} (source: ${row.action.source_status})` : row.action.status}</dd></div>
                <div><dt>Assignee</dt><dd>{assignee(row)}</dd></div>
                {#if row.action.due_date}<div><dt>Due</dt><dd>{row.action.due_date}</dd></div>{/if}
              </dl>
              <a href={row.meeting.archive_path} onclick={(event) => openMeeting(event, row.meeting)}>
                Open archived meeting
              </a>
            </li>
          {/each}
        </ol>
      {/if}
      {#if request}
        <p class="showing" role="status">Showing {displayedPage.rows.length.toLocaleString()} of {displayedPage.total_count.toLocaleString()} action items</p>
        {#if displayedPage.next_cursor && !controller.error}
          <Button label="Load more action items" size="sm" surface="outline" disabled={controller.loading} onclick={() => void controller.loadMore()} />
        {/if}
      {/if}
    {/if}
  </Card>
</section>

<style>
  .meeting-actions { min-width: 0; }

  h3,
  p,
  dl,
  dd {
    margin: 0;
  }

  .actions-heading {
    display: flex;
    align-items: end;
    justify-content: space-between;
    gap: var(--space-4);
    margin-bottom: var(--space-4);
  }
  .actions-heading > div { display: grid; gap: var(--space-1); }
  .eyebrow { color: var(--text-muted); font-size: var(--font-size-2xs); font-weight: 600; letter-spacing: .04em; text-transform: uppercase; }
  h3 {
    color: var(--text-primary);
    font-size: var(--font-size-md);
  }
  .meeting-count { color: var(--text-muted); font-size: var(--font-size-xs); white-space: nowrap; }

  .coverage {
    display: grid;
    gap: var(--space-2);
    margin-bottom: var(--space-3);
    padding-block: var(--space-3);
    border-block: 1px solid var(--border-muted);
    color: var(--text-muted);
    font-size: var(--font-size-xs);
  }
  .coverage > div { display: grid; gap: var(--space-2); }
  .coverage > div > span { font-weight: 600; color: var(--text-secondary); }
  .coverage ul { display: flex; flex-wrap: wrap; gap: var(--space-2) var(--space-4); margin: 0; padding: 0; list-style: none; }
  .coverage li { color: var(--text-secondary); }
  .coverage li.empty { color: var(--text-muted); }
  .coverage strong { color: var(--text-primary); font-variant-numeric: tabular-nums; }
  .coverage li.empty strong { color: inherit; }
  .meeting-actions :global(.kit-empty-state) { margin: 0; padding: var(--space-5) var(--space-3); }

  ol {
    display: grid;
    gap: 0;
    margin: 0;
    padding: 0;
    list-style: none;
  }

  li {
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
  }
  ol > li { padding: var(--space-4) 0; border-top: 1px solid var(--border-muted); }
  ol > li:first-child { border-top: 0; }

  li > * + * {
    margin-top: var(--space-2);
  }

  dl {
    display: flex;
    flex-wrap: wrap;
    gap: var(--space-2) var(--space-4);
  }

  dl div {
    display: inline-flex;
    gap: var(--space-1);
  }

  dt::after {
    content: ':';
  }

  dd {
    color: var(--text-primary);
  }

  a {
    color: var(--link-ink);
  }
  .showing { margin-top: var(--space-3); color: var(--text-muted); font-size: var(--font-size-xs); }
</style>
