<script lang="ts">
  import type { SessionController } from '../../api/session.svelte';

  let { session }: { session: SessionController } = $props();
  let apiKey = $state('');

  async function submit(event: SubmitEvent) {
    event.preventDefault();
    await session.login(apiKey);
  }
</script>

<main class="login" aria-label="Authentication">
  <form aria-label="Log in" onsubmit={submit}>
    <p class="eyebrow">msgvault</p>
    <h1>Log in</h1>
    <p>Enter the API key configured for this daemon.</p>

    <label for="api-key">API key</label>
    <input
      id="api-key"
      name="api-key"
      type="password"
      autocomplete="current-password"
      bind:value={apiKey}
      required
    />

    {#if session.error}
      <p role="alert">{session.error}</p>
    {/if}

    <button type="submit" disabled={session.loading}>
      {session.loading ? 'Logging in…' : 'Log in'}
    </button>
  </form>
</main>
