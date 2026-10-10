<script lang="ts">
  import { messageTitle } from '../../archive/message-title';
  import { Button } from '@kenn-io/kit-ui';
  import { ArchiveMessageLoader } from '../../archive/message-loader.svelte';
  import type { APIClient } from '../../api/client';
  import ConversationView from './ConversationView.svelte';

  let { client, messageID }: { client: APIClient; messageID: number } = $props();
  const loader = new ArchiveMessageLoader();
  let retry = $state(0);

  $effect(() => {
    void retry;
    void loader.load(client, messageID);
    return () => loader.cancel();
  });
</script>

<main aria-label="Linked message" class="message-page">
  <header>
    <Button onclick={() => window.location.assign('/')}>Back to archive</Button>
    <h1>{messageTitle(loader.detail?.subject, loader.detail?.conversation_title) || 'Message'}</h1>
  </header>
  {#if loader.error}
    <p role="alert">{loader.error}</p>
    <Button onclick={() => retry += 1}>Retry</Button>
  {:else if loader.detail}
    <ConversationView {client} conversationId={loader.detail.conversation_id!} anchorId={loader.detail.id!} />
  {:else}
    <p role="status">Loading message…</p>
  {/if}
</main>

<style>
  .message-page { display: flex; flex-direction: column; height: 100dvh; padding: 1rem; }
  header { display: flex; align-items: center; flex-wrap: wrap; gap: 1rem; margin-bottom: 1rem; }
  h1 { min-width: 0; overflow-wrap: anywhere; font-size: 1rem; font-weight: 600; }
</style>
