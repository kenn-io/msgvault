<script lang="ts">
  import { Button } from '@kenn-io/kit-ui';
  import type { TimelineEntry, TimelinePerson } from '../../api/generated/models';
  import { defaultExploreURLState, serializeExploreURLState } from '../../explore/state.svelte';

  interface Props {
    entry: TimelineEntry;
    timezone: string;
    onAround: (reference: string) => void;
    onOpenMessage?: (id: number) => void;
    onOpenConversation?: (conversationID: number, messageID?: number) => void;
    onOpenPerson?: (id: number) => void;
    onOpenMeeting?: (messageID: number) => void;
  }
  let { entry, timezone, onAround, onOpenMessage, onOpenConversation, onOpenPerson, onOpenMeeting }: Props = $props();
  const text = $derived(entry.meeting?.untrusted_text ?? entry.burst?.untrusted_text ?? entry.message?.untrusted_text ?? entry.note?.untrusted_text);
  const people = $derived<TimelinePerson[]>(entry.meeting?.people ?? entry.meeting?.attendees ?? entry.burst?.people ?? entry.message?.people ?? entry.call?.people ?? entry.note?.people ?? (entry.loop ? [entry.loop.person] : []));
  const messageID = $derived(safeID(entry.open?.find((open) => open.tool === 'get_message')?.args.id));
  const conversationID = $derived(safeID(entry.open?.find((open) => open.tool === 'list_messages')?.args.conversation_id));
  const transcriptIDs = $derived(entry.open?.find((open) => open.tool === 'get_meeting_context')?.args.message_ids);
  const meetingID = $derived(Array.isArray(transcriptIDs) ? safeID(transcriptIDs[0]) : undefined);
  const calendarMessageID = $derived(messageIDFromEventRef(entry.open?.find((open) => open.tool === 'get_calendar_event')?.args.ref) ?? messageIDFromEventRef(entry.ref));
  const anchorMessageID = $derived(messageID ?? safeID(entry.burst?.latest_message_ids[0]));
  const copies = $derived(Object.values(entry.burst?.copies ?? {}).reduce((sum, count) => sum + Math.max(0, count - 1), 0));

  function safeID(value: unknown): number | undefined {
    return typeof value === 'number' && Number.isSafeInteger(value) && value > 0 ? value : undefined;
  }
  function messageIDFromEventRef(value: unknown): number | undefined {
    if (typeof value !== 'string') return undefined;
    const match = /^evt:(\d+)(?:@|$)/.exec(value);
    return match ? safeID(Number(match[1])) : undefined;
  }
  function navigate(event: MouseEvent, action: (() => void) | undefined): void {
    if (!action || event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;
    event.preventDefault();
    action();
  }
  function personHref(id: number): string {
    return `/${serializeExploreURLState({ ...defaultExploreURLState, workspace: 'directory', directoryPersonID: id })}`;
  }
  function conversationHref(id: number, messageID: number | undefined = undefined): string {
    return `/${serializeExploreURLState({ ...defaultExploreURLState, workspace: 'everything', selectedRow: `conversation:${id}`, conversationAnchor: messageID ? String(messageID) : null })}`;
  }
  function timeLabel(value: string): string {
    const date = new Date(value);
    return Number.isNaN(date.getTime()) ? '' : new Intl.DateTimeFormat(undefined, { timeZone: timezone, dateStyle: 'medium', timeStyle: 'short' }).format(date);
  }
</script>

<article class:anchor={entry.anchor} aria-label={entry.hidden_text ? 'Timeline entry with hidden text' : `${entry.kind} ${entry.ref}`}>
  {#if entry.hidden_text}
    <p class="hidden-warning">Hidden text is present. Open the original to review it.</p>
  {:else}
    <header>
      <span class="kind">{entry.anchor ? '◀ ' : ''}{entry.kind}</span>
      {#if entry.time}<time datetime={entry.time}>{timeLabel(entry.time)}</time>{/if}
      {#if entry.meeting?.all_day}<span>All day</span>{/if}
      {#if entry.meeting?.attended}<span aria-label="Linked transcript">✓</span>{/if}
      <span class="ref">{entry.ref}</span>
    </header>
    {#if text?.text}<p class="preview">{text.text}{text.truncated ? '…' : ''}</p>{/if}
    {#if entry.burst}<p class="detail">{entry.burst.channel} · {entry.burst.inbound} received · {entry.burst.outbound} sent{copies ? ` · ${copies} additional copies` : ''}</p>{/if}
    {#if entry.message}<p class="detail">{entry.message.direction}</p>{/if}
    {#if entry.call}<p class="detail">{entry.call.outcome}{entry.call.duration_seconds !== null ? ` · ${entry.call.duration_seconds}s` : ''}</p>{/if}
    {#if entry.loop}<p class="detail">{entry.loop.kind === 'reply_owed' ? 'Reply owed' : 'Awaiting reply'} · {entry.loop.age}</p>{/if}
    {#if entry.meeting?.action_item_count}<p class="detail">{entry.meeting.action_item_count} action items</p>{/if}
    {#if people.length}
      <ul class="people" aria-label="People">
        {#each people as person, index (`${person.person_id ?? person.participant_id ?? person.name}:${index}`)}
          <li>
            {#if safeID(person.person_id)}
              <a href={personHref(person.person_id!)} onclick={(event) => navigate(event, onOpenPerson ? () => onOpenPerson!(person.person_id!) : undefined)}>{person.name || 'Unnamed person'}</a>
            {:else}<span>{person.name || 'Unnamed person'}</span>{/if}
            {#if person.tier}<span class="tier">T{person.tier}</span>{/if}
            {#if person.participation}<span class="detail">{person.participation}</span>{/if}
          </li>
        {/each}
      </ul>
    {/if}
    <nav aria-label="Open entry">
      {#if meetingID}
        <a href={`/messages/${meetingID}`} onclick={(event) => navigate(event, onOpenMeeting ? () => onOpenMeeting!(meetingID!) : undefined)}>Open meeting</a>
      {:else if messageID}
        <a href={`/messages/${messageID}`} onclick={(event) => navigate(event, onOpenMessage ? () => onOpenMessage!(messageID!) : undefined)}>Open message</a>
      {:else if calendarMessageID}
        <a href={`/messages/${calendarMessageID}`} onclick={(event) => navigate(event, onOpenMessage ? () => onOpenMessage!(calendarMessageID!) : undefined)}>Open original message</a>
      {/if}
      {#if conversationID}
        <a href={conversationHref(conversationID, anchorMessageID)} onclick={(event) => navigate(event, onOpenConversation ? () => onOpenConversation!(conversationID!, anchorMessageID) : undefined)}>Open conversation</a>
      {/if}
    </nav>
  {/if}
  <Button label="Around" surface="soft" size="sm" onclick={() => onAround(entry.anchor_ref)} />
</article>

<style>
  article { padding: var(--space-3); border-bottom: 1px solid var(--border-muted); }
  article.anchor { border-left: 3px solid var(--text-primary); border-start-start-radius: 0; border-end-start-radius: 0; }
  header, nav, .people, .people li { display: flex; flex-wrap: wrap; align-items: center; gap: var(--space-2); }
  header, .detail, .ref, .tier { color: var(--text-muted); font-size: var(--font-size-xs); }
  .kind { text-transform: capitalize; font-weight: 600; }
  .preview { white-space: pre-wrap; overflow-wrap: anywhere; margin: var(--space-2) 0; }
  .people { list-style: none; padding: 0; margin: var(--space-2) 0; }
  nav { margin-block: var(--space-2); }
  .hidden-warning { color: var(--text-muted); }
</style>
