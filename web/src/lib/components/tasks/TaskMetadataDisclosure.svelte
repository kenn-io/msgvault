<script lang="ts">
  import { Disclosure } from '@kenn-io/kit-ui';
  let { messageURL = '', addedAt = '', archiveUID, messageId, conversationId, sourceType, sourceIdentifier, sourceMessageId, subject, from, sentAt }: {
    messageURL?: string; addedAt?: string; archiveUID: string; messageId: number; conversationId: number; sourceType: string; sourceIdentifier: string; sourceMessageId: string; subject: string; from: string; sentAt: string;
  } = $props();
</script>

<div class="metadata">
    <Disclosure variant="plain">{#snippet summary()}What's sent{/snippet}
      <dl>
        {#if messageURL}<div>
          <dt>Message URL</dt>
          <dd>{messageURL}</dd>
        </div>{/if}
        <div>
          <dt>Archive UID</dt>
          <dd>{archiveUID}</dd>
        </div>
        <div>
          <dt>Message ID</dt>
          <dd>{messageId}</dd>
        </div>
        <div>
          <dt>Conversation ID</dt>
          <dd>{conversationId}</dd>
        </div>
        <div>
          <dt>Source type</dt>
          <dd>{sourceType}</dd>
        </div>
        <div>
          <dt>Source identifier</dt>
          <dd>{sourceIdentifier}</dd>
        </div>
        <div>
          <dt>Source message ID</dt>
          <dd>{sourceMessageId}</dd>
        </div>
        <div>
          <dt>Subject snapshot</dt>
          <dd>{subject}</dd>
        </div>
        <div>
          <dt>Sender snapshot</dt>
          <dd>{from}</dd>
        </div>
        <div>
          <dt>Sent at</dt>
          <dd>{sentAt}</dd>
        </div>
        <div>
          <dt>Link added at</dt>
          <dd>{addedAt || "When linked"}</dd>
        </div>
      </dl>
    </Disclosure>
</div>

<style>
  .metadata :global(.kit-disclosure) { color: var(--text-muted); font-size: var(--font-size-xs); }
  .metadata :global(.kit-disclosure--plain > .kit-disclosure__content) { padding-bottom: 0; }
  dl {
    display: grid;
    gap: var(--space-1);
    margin-bottom: 0;
  }
  dl div {
    display: grid;
    grid-template-columns: 8rem minmax(0, 1fr);
    gap: var(--space-2);
  }
  dt {
    color: var(--text-muted);
  }
  dd {
    margin: 0;
    overflow-wrap: anywhere;
    color: var(--text-primary);
  }
  @media (max-width: 640px) {
    .metadata :global(.kit-disclosure--plain > .kit-disclosure__summary) { font-size: 12px; }
    dl { grid-template-columns: max-content minmax(0, 1fr); column-gap: var(--space-5); font-size: 12px; }
    dl div { display: contents; }
    dt { white-space: nowrap; }
  }
</style>
