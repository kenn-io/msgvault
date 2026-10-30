<script lang="ts">
  import { Chip } from '@kenn-io/kit-ui';
  import type { ActionRow, MeetingRef } from '../../api/generated/models';

  let { row, compact = false, onOpenMeeting }: { row: ActionRow; compact?: boolean; onOpenMeeting?: (meeting: MeetingRef) => void } = $props();
  const assignee = $derived(row.action.assignee_name && row.action.assignee_email
    ? `${row.action.assignee_name} · ${row.action.assignee_email}`
    : row.action.assignee_name || row.action.assignee_email || 'Unassigned');
  const sourceDiffers = $derived(row.action.source_status != null && row.action.source_status !== row.action.status);
</script>

<li class:compact>
  {#if compact}
    <Chip size="xs" uppercase={false} tone={row.action.status === 'completed' || row.action.status === 'cancelled' ? 'muted' : 'info'}>{row.action.status === 'completed' ? 'Done' : row.action.status === 'pending' ? 'Open' : row.action.status}</Chip>
  {/if}
  <div class="action-content">
    <strong>{row.action.title}</strong>
    {#if compact && sourceDiffers}<p class="compact-meta">Source status: {row.action.source_status}</p>{/if}
    {#if row.action.description}<p>{row.action.description}</p>{/if}
    {#if compact}
      <p class="compact-meta">{assignee}{row.action.due_date ? ` · Due ${row.action.due_date}` : ''}</p>
    {:else}
      <dl>
        <div><dt>Source status</dt><dd>{sourceDiffers ? `${row.action.status} (source: ${row.action.source_status})` : row.action.status}</dd></div>
        <div><dt>Assignee</dt><dd>{assignee}</dd></div>
        {#if row.action.due_date}<div><dt>Due</dt><dd>{row.action.due_date}</dd></div>{/if}
      </dl>
    {/if}
    <a href={`/messages/${row.meeting.message_id}`} onclick={(event) => { if (onOpenMeeting) { event.preventDefault(); onOpenMeeting(row.meeting); } }}>{compact ? row.meeting.title || 'Open archived meeting' : 'Open archived meeting'}</a>
  </div>
</li>

<style>
  li { color: var(--text-secondary); font-size: var(--font-size-sm); }
  li:not(.compact) { padding: var(--space-4) 0; border-top: 1px solid var(--border-muted); }
  li:not(.compact):first-child { border-top: 0; }
  li:not(.compact) .action-content > * + * { margin-top: var(--space-2); }
  p, dl, dd { margin: 0; }
  dl { display: flex; flex-wrap: wrap; gap: var(--space-2) var(--space-4); }
  dl div { display: inline-flex; gap: var(--space-1); }
  dt::after { content: ':'; }
  dd { color: var(--text-primary); }
  a { color: var(--link-ink); }
  .compact { display: grid; grid-template-columns: 4.5rem minmax(0, 1fr); gap: var(--space-3); align-items: baseline; }
  .compact .action-content { display: grid; gap: var(--space-1); }
  .compact-meta { font-size: var(--font-size-xs); color: var(--text-muted); }
</style>
