<script lang="ts">
  import { getSettings as generatedGetSettings } from './lib/api/generated/api/api';
  import { Button } from '@kenn-io/kit-ui';
  import { onMount } from 'svelte';
  import { receiveGoogleContactsCallback } from './lib/settings/google-authorization';
  import { createSessionController, type SessionController } from './lib/api/session.svelte';
  import Login from './lib/components/auth/Login.svelte';
  import SettingsWorkspace from './lib/components/settings/SettingsWorkspace.svelte';
  import AppShell from './lib/components/shell/AppShell.svelte';
  import MessagePage from './lib/components/reader/MessagePage.svelte';
  import type { ExploreSearchMode } from './lib/explore/models';
  import { parseSearchMode } from './lib/search/modes';
  import { createAppearancePreferences, type AppearanceDefaults } from './lib/theme/preferences.svelte';
  let {
    session = createSessionController(),
  }: {
    session?: SessionController;
  } = $props();
  let oauthCallback = $state(false);
  let pathname = $state(window.location.pathname);
  const messageID = $derived(Number(/^\/messages\/([1-9]\d*)\/?$/.exec(pathname)?.[1]) || undefined);
  let appearanceDefaults = $state<AppearanceDefaults>({ theme: 'system', density: 'compact' });
  let shellMounted = $derived(session.status !== undefined && session.authMode !== 'required');
  let searchModeDefault = $state<ExploreSearchMode | undefined>();
  let authenticated = false;
  let browserDefaultsRequestGeneration = 0;
  onMount(() => {
    oauthCallback = receiveGoogleContactsCallback();
    if (!oauthCallback) void session.bootstrap();
  });
  // AppShell owns appearance while mounted; the boot and login screens apply
  // the same defaults and stored override so they render in the right theme.
  $effect(() => {
    if (shellMounted && messageID === undefined) return;
    const appearance = createAppearancePreferences(appearanceDefaults);
    return () => appearance.destroy();
  });
  $effect(() => {
    const isAuthenticated = session.authMode !== undefined && session.authMode !== 'required';
    if (!isAuthenticated) {
      if (authenticated) browserDefaultsRequestGeneration += 1;
      authenticated = false;
      return;
    }
    if (authenticated) return;
    authenticated = true;
    const generation = ++browserDefaultsRequestGeneration;
    void loadBrowserDefaults(generation);
  });
  async function loadBrowserDefaults(generation: number): Promise<void> {
    try {
      const { data } = await generatedGetSettings(session.client);
      if (generation !== browserDefaultsRequestGeneration || session.authMode === 'required') return;
      const theme = settingString(data?.settings.find(({ key }) => key === 'web.theme'));
      const density = settingString(data?.settings.find(({ key }) => key === 'web.density'));
      appearanceDefaults = {
        theme: theme === 'light' || theme === 'dark' || theme === 'system' ? theme : 'system',
        density: density === 'comfortable' ? density : 'compact',
      };
      searchModeDefault = parseSearchMode(
        settingString(data?.settings.find(({ key }) => key === 'web.default_search_mode')),
      );
    } catch {
      // Keep the safe fallback when settings authority is temporarily unavailable.
    }
  }
  function settingString(
    setting:
      | {
          value?: unknown;
        }
      | undefined,
  ): string | undefined {
    const value = setting?.value;
    return value && typeof value === 'object' && 'string' in value && typeof value.string === 'string'
      ? value.string
      : undefined;
  }
</script>

<svelte:window onpopstate={() => pathname = window.location.pathname} />

<svelte:head>
  {#if !shellMounted || messageID !== undefined}<title>msgvault</title>{/if}
</svelte:head>

{#if oauthCallback}
  <main class="boot"><p>Return to CardDAV settings to finish connecting. You can close this window.</p></main>
{:else if session.authMode === 'required'}
  <Login {session} />
{:else if shellMounted}
  {#if messageID !== undefined}
    <MessagePage client={session.client} {messageID} />
  {:else}
  <AppShell client={session.client} {appearanceDefaults} {searchModeDefault}>
    {#snippet settings(cardDAVRequest, onCardDAVRequestConsumed, navigationTarget)}
      <SettingsWorkspace
        client={session.client}
        plainHTTPWarning={session.status?.plain_http_warning ?? false}
        {cardDAVRequest}
        {onCardDAVRequestConsumed}
        {navigationTarget}
      />
    {/snippet}
  </AppShell>
  {/if}
{:else if session.error !== undefined}
  <main class="boot" aria-label="Connection error">
    <p class="boot__brand">msgvault</p>
    <h1>Can't reach the msgvault daemon</h1>
    <p role="alert">{session.error}</p>
    <Button tone="info" surface="solid" label="Retry" onclick={() => void session.bootstrap()} />
  </main>
{:else}
  <main class="boot" aria-label="Connecting">
    <p class="boot__brand">msgvault</p>
    <p>Connecting…</p>
  </main>
{/if}

<style>
  .boot {
    display: flex;
    flex-direction: column;
    align-items: flex-start;
    gap: var(--space-5);
    max-width: 28rem;
    margin: 0 auto;
    padding: var(--space-8) var(--space-6);
    font-size: var(--font-size-md);
  }

  .boot p,
  .boot h1 {
    margin: 0;
  }

  .boot h1 {
    font-size: var(--font-size-xl);
    font-weight: 650;
  }

  .boot__brand {
    color: var(--text-primary);
    font-size: var(--font-size-md);
    font-weight: 650;
  }

  .boot p:not(.boot__brand) {
    color: var(--text-muted);
  }

  .boot p[role='alert'] {
    color: var(--text-danger);
  }
</style>
