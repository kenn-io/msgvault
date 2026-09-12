<script lang="ts">
  import { Button, EmptyState } from '@kenn-io/kit-ui';
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
  <h3 id={headingID}>Archived action items</h3>
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
    <p class="coverage" role="status">{coverageSummary(displayedPage.coverage)}</p>
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
      <p class="coverage" role="status">Showing {displayedPage.rows.length.toLocaleString()} of {displayedPage.total_count.toLocaleString()} action items</p>
      {#if displayedPage.next_cursor && !controller.error}
        <Button label="Load more action items" size="sm" surface="outline" disabled={controller.loading} onclick={() => void controller.loadMore()} />
      {/if}
    {/if}
  {/if}
</section>

<style>
  .meeting-actions {
    display: grid;
    gap: var(--space-3);
    padding: var(--space-4);
    border-bottom: 1px solid var(--border-muted);
  }

  h3,
  p,
  dl,
  dd {
    margin: 0;
  }

  h3 {
    color: var(--text-primary);
    font-size: var(--font-size-sm);
  }

  .coverage {
    color: var(--text-muted);
    font-size: var(--font-size-xs);
  }

  ol {
    display: grid;
    gap: var(--space-4);
    margin: 0;
    padding-left: var(--space-5);
  }

  li {
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
  }

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
</style>
