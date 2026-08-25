<script lang="ts">
  import { Button, TextInput } from '@kenn-io/kit-ui';

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
    <TextInput
      id="api-key"
      name="api-key"
      type="password"
      autocomplete="current-password"
      bind:value={apiKey}
      required
      block
    />

    {#if session.error}
      <p role="alert">{session.error}</p>
    {/if}

    <Button
      type="submit"
      tone="info"
      surface="solid"
      disabled={session.loading}
      label={session.loading ? 'Logging in…' : 'Log in'}
    />
  </form>
</main>
