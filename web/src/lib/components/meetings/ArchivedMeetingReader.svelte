<script lang="ts">
  import { Button, Modal, appShortcuts } from '@kenn-io/kit-ui';
  import { onMount } from 'svelte';
  import type { APIClient } from '../../api/client';
  import type { MeetingRef, MessageDetail } from '../../api/generated/models';
  import type { ExplorePredicate } from '../../explore/models';
  import ReadingPane from '../reader/ReadingPane.svelte';

  let { client, message, loading, error, predicate, anchorID, onClose, onReload, onOpenMeeting, onAnchorChange }: {
    client: APIClient;
    message?: MessageDetail;
    loading: boolean;
    error: string;
    predicate: ExplorePredicate;
    anchorID?: number;
    onClose: () => void;
    onReload: () => void;
    onOpenMeeting: (meeting: MeetingRef) => void;
    onAnchorChange: (id: number) => void;
  } = $props();
  onMount(() => appShortcuts.pushScope('archived-meeting-reader'));
</script>

<Modal ariaLabel="Archived meeting" closeLabel="Close archived meeting" title="Archived meeting"
  width="min(1100px, 96vw)" maxWidth="min(1100px, 96vw)" onclose={onClose}>
  <div class="archive-reader">
    {#if message}
      <ReadingPane {client} selection={{ kind: 'archive', message }} {predicate}
        conversationAnchorId={anchorID} onConversationAnchorChange={onAnchorChange}
        {onOpenMeeting} onClose={onClose} />
    {:else if loading}
      <p role="status">Loading archived meeting…</p>
    {:else if error}
      <p role="alert">{error}</p>
      <Button label="Retry archived meeting" onclick={onReload} />
    {/if}
  </div>
</Modal>

<style>
  .archive-reader { height: min(760px, 78vh); min-height: 240px; }
  p { padding: var(--space-4); color: var(--text-secondary); }
</style>
