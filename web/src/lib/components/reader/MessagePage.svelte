<script lang="ts">
  import { Button } from '@kenn-io/kit-ui';
  import { getMessage } from '../../api/generated/api/api';
  import type { MessageDetail } from '../../api/generated/models';
  import type { APIClient } from '../../api/client';
  import ConversationView from './ConversationView.svelte';

  let { client, messageID }: { client: APIClient; messageID: number } = $props();
  let message = $state<MessageDetail>();
  let error = $state('');
  let retry = $state(0);

  $effect(() => {
    const id = messageID;
    void retry;
    const controller = new AbortController();
    message = undefined;
    error = '';
    void getMessage({ id }, { ...client, signal: controller.signal }).then(({ data, response }) => {
      if (controller.signal.aborted) return;
      if (!data) {
        error = response.status === 404 ? 'Message not found.' : 'Could not load this message.';
        return;
      }
      message = data;
    }).catch(() => {
      if (!controller.signal.aborted) error = 'Could not load this message.';
    });
    return () => controller.abort();
  });
</script>

<main aria-label="Linked message" class="message-page">
  <header>
    <Button onclick={() => window.location.assign('/')}>Back to archive</Button>
    <h1>{message?.subject || 'Message'}</h1>
  </header>
  {#if error}
    <p role="alert">{error}</p>
    <Button onclick={() => retry += 1}>Retry</Button>
  {:else if message}
    <ConversationView {client} conversationId={message.conversation_id!} anchorId={message.id!} />
  {:else}
    <p role="status">Loading message…</p>
  {/if}
</main>

<style>
  .message-page { display: flex; flex-direction: column; height: 100dvh; padding: 1rem; }
  header { display: flex; align-items: center; gap: 1rem; margin-bottom: 1rem; }
  h1 { font-size: 1rem; font-weight: 600; }
</style>
