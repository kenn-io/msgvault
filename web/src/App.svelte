<script lang="ts">
  import { onMount } from 'svelte';

  import { createSessionController, type SessionController } from './lib/api/session.svelte';
  import Login from './lib/components/auth/Login.svelte';
  import SettingsWorkspace from './lib/components/settings/SettingsWorkspace.svelte';
  import AppShell from './lib/components/shell/AppShell.svelte';
  import type { AppearanceDefaults } from './lib/theme/preferences.svelte';

  let { session = createSessionController() }: { session?: SessionController } = $props();
  let appearanceDefaults = $state<AppearanceDefaults>({ theme: 'system', density: 'compact' });
  let authenticated = false;
  let appearanceRequestGeneration = 0;

  onMount(() => {
    void session.bootstrap();
  });

  $effect(() => {
    const isAuthenticated = session.authMode !== undefined && session.authMode !== 'required';
    if (!isAuthenticated) {
      if (authenticated) appearanceRequestGeneration += 1;
      authenticated = false;
      return;
    }
    if (authenticated) return;
    authenticated = true;
    const generation = ++appearanceRequestGeneration;
    void loadAppearanceDefaults(generation);
  });

  async function loadAppearanceDefaults(generation: number): Promise<void> {
    try {
      const { data } = await session.client.GET('/api/v1/settings');
      if (generation !== appearanceRequestGeneration || session.authMode === 'required') return;
      const theme = settingString(data?.settings.find(({ key }) => key === 'web.theme'));
      const density = settingString(data?.settings.find(({ key }) => key === 'web.density'));
      appearanceDefaults = {
        theme: theme === 'light' || theme === 'dark' || theme === 'system' ? theme : 'system',
        density: density === 'comfortable' ? density : 'compact'
      };
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
{:else}
  <AppShell client={session.client} enabled={session.authMode !== undefined} {appearanceDefaults}>
    {#snippet settings()}
      <SettingsWorkspace client={session.client} plainHTTPWarning={session.status?.plain_http_warning ?? false} />
    {/snippet}
  </AppShell>
{/if}
