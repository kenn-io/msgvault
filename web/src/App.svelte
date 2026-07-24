<script lang="ts">
  import { onMount } from 'svelte';

  import { createSessionController, type SessionController } from './lib/api/session.svelte';
  import Login from './lib/components/auth/Login.svelte';
  import SettingsWorkspace from './lib/components/settings/SettingsWorkspace.svelte';
  import AppShell from './lib/components/shell/AppShell.svelte';
  import type { ExploreSearchMode } from './lib/explore/models';
  import { parseSearchMode } from './lib/search/modes';
  import { createAppearancePreferences, type AppearanceDefaults } from './lib/theme/preferences.svelte';

  let { session = createSessionController() }: { session?: SessionController } = $props();
  let appearanceDefaults = $state<AppearanceDefaults>({ theme: 'system', density: 'compact' });
  let shellMounted = $derived(session.status !== undefined && session.authMode !== 'required');
  let searchModeDefault = $state<ExploreSearchMode | undefined>();
  let authenticated = false;
  let browserDefaultsRequestGeneration = 0;

  onMount(() => {
    void session.bootstrap();
  });

  // AppShell owns appearance while mounted; the boot and login screens apply
  // the same defaults and stored override so they render in the right theme.
  $effect(() => {
    if (shellMounted) return;
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
      const { data } = await session.client.GET('/api/v1/settings');
      if (generation !== browserDefaultsRequestGeneration || session.authMode === 'required') return;
      const theme = settingString(data?.settings.find(({ key }) => key === 'web.theme'));
      const density = settingString(data?.settings.find(({ key }) => key === 'web.density'));
      appearanceDefaults = {
        theme: theme === 'light' || theme === 'dark' || theme === 'system' ? theme : 'system',
        density: density === 'comfortable' ? density : 'compact'
      };
      searchModeDefault = parseSearchMode(
        settingString(data?.settings.find(({ key }) => key === 'web.default_search_mode'))
      );
    } catch {
      // Keep the safe fallback when settings authority is temporarily unavailable.
    }
  }

  function settingString(setting: { value?: unknown } | undefined): string | undefined {
    const value = setting?.value;
    return value && typeof value === 'object' && 'string' in value && typeof value.string === 'string'
      ? value.string
      : undefined;
  }
</script>

<svelte:head>
  <title>Everything · msgvault</title>
</svelte:head>

{#if session.authMode === 'required'}
  <Login {session} />
{:else if shellMounted}
  <AppShell client={session.client} {appearanceDefaults} {searchModeDefault}>
    {#snippet settings()}
      <SettingsWorkspace client={session.client} plainHTTPWarning={session.status?.plain_http_warning ?? false} />
    {/snippet}
  </AppShell>
{:else if session.error !== undefined}
  <main class="boot" aria-label="Connection error">
    <p class="eyebrow">msgvault</p>
    <h1>Can't reach the msgvault daemon</h1>
    <p role="alert">{session.error}</p>
    <button type="button" onclick={() => void session.bootstrap()}>Retry</button>
  </main>
{:else}
  <main class="boot" aria-label="Connecting">
    <p class="eyebrow">msgvault</p>
    <p>Connecting…</p>
  </main>
{/if}
