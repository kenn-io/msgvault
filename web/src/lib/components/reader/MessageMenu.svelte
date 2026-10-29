<script lang="ts">
  import { Menu, MenuContent, MenuItem, MenuTrigger } from '@kenn-io/kit-ui';
  import Ellipsis from '@lucide/svelte/icons/ellipsis';

  import { downloadRawMessage } from '../../api/download';
  import { isEmailMessageType } from '../../explore/models';
  import type { APIClient } from '../../api/client';
  import type { ArchiveMessageDetail, MessageViewMode } from '../../archive/types';
  import { kataReadiness } from '../../kata/kata-ready.svelte';
  import Dialog from '../common/Dialog.svelte';
  import MeetingContextExport from '../meetings/MeetingContextExport.svelte';
  import AddToTaskDialog from '../tasks/AddToTaskDialog.svelte';

  let {
    client = undefined,
    message,
    hasRenderableHTML,
    viewMode,
    onViewModeChange,
    onError = undefined,
    onTasksChange = undefined,
    onOpenSettings = undefined,
  }: {
    client?: APIClient;
    onTasksChange?: () => void;
    onOpenSettings?: () => void;
    message: ArchiveMessageDetail;
    /** The message has an HTML body the reader can show, so the format can switch. */
    hasRenderableHTML: boolean;
    viewMode: MessageViewMode;
    onViewModeChange?: (id: number, mode: MessageViewMode) => void;
    /** Receives the reason a download failed. */
    onError?: (message: string) => void;
  } = $props();

  const kata = kataReadiness();
  const isMeeting = $derived(message.messageType === 'meeting_transcript');
  // Only email keeps its original bytes.
  const hasOriginal = $derived(isEmailMessageType(message.messageType ?? ''));
  const canAddToTask = $derived(Boolean(client) && (hasOriginal || kata?.ready));
  const canExportMeeting = $derived(Boolean(client) && isMeeting);
  const canDownload = $derived(Boolean(client) && hasOriginal);
  const hasItems = $derived(canAddToTask || canExportMeeting || canDownload || hasRenderableHTML);

  let taskOpen = $state(false);
  let exportOpen = $state(false);

  async function downloadOriginal(): Promise<void> {
    if (!client) return;
    try {
      await downloadRawMessage(client, message.id, `message-${message.id}.eml`);
      onError?.('');
    } catch (cause) {
      onError?.(cause instanceof Error ? cause.message : 'The original message isn’t available.');
    }
  }
</script>

{#if hasItems}
  <Menu align="end" onopenchange={(open) => { if (open) onError?.(''); }}>
    <MenuTrigger ariaLabel={`More actions for message ${message.id}`} title="More actions">
      <Ellipsis size={16} aria-hidden="true" />
    </MenuTrigger>
    <MenuContent ariaLabel="Message actions">
      {#if canAddToTask}
        <MenuItem onselect={() => (taskOpen = true)}>Add to task…</MenuItem>
      {/if}
      {#if canExportMeeting}
        <MenuItem onselect={() => (exportOpen = true)}>Export meeting…</MenuItem>
      {/if}
      {#if canDownload}
        <MenuItem onselect={() => void downloadOriginal()}>Download original</MenuItem>
      {/if}
      {#if hasRenderableHTML}
        {#if viewMode === 'html'}
          <MenuItem onselect={() => onViewModeChange?.(message.id, 'text')}>Show plain text</MenuItem>
        {:else}
          <MenuItem onselect={() => onViewModeChange?.(message.id, 'html')}>Show formatted HTML</MenuItem>
        {/if}
      {/if}
    </MenuContent>
  </Menu>
{/if}

{#if client && taskOpen}
  <AddToTaskDialog
    {client}
    messageId={message.id}
    title={message.subject || 'Follow up on this message'}
    email={hasOriginal}
    onsettings={onOpenSettings}
    onclose={() => { taskOpen = false; onTasksChange?.(); }}
  />
{/if}
{#if client && exportOpen}
  <Dialog title="Export meeting" onclose={() => exportOpen = false}><MeetingContextExport {client} request={{ message_ids: [message.id] }} /></Dialog>
{/if}
