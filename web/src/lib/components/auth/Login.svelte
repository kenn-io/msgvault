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
    <p class="login__brand">msgvault</p>
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

<style>
  .login {
    max-width: 24rem;
    margin: 0 auto;
    padding: var(--space-8) var(--space-6);
    font-size: var(--font-size-md);
  }

  form {
    display: flex;
    flex-direction: column;
    align-items: flex-start;
    gap: var(--space-5);
  }

  form > :global(*) {
    align-self: stretch;
  }

  form > :global(button) {
    align-self: flex-start;
  }

  p,
  h1 {
    margin: 0;
  }

  h1 {
    font-size: var(--font-size-xl);
    font-weight: 650;
  }

  .login__brand {
    color: var(--text-primary);
    font-size: var(--font-size-md);
    font-weight: 650;
  }

  p:not(.login__brand) {
    color: var(--text-muted);
  }

  p[role='alert'] {
    color: var(--text-danger);
  }
</style>
