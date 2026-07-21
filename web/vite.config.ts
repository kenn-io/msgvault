import { svelte } from '@sveltejs/vite-plugin-svelte';
import { defineConfig } from 'vitest/config';

export default defineConfig({
  plugins: [svelte()],
  build: {
    manifest: true
  },
  resolve: {
    conditions: ['browser']
  },
  ssr: {
    // kit-ui publishes Svelte source. Keep it and its icon components inside
    // Vite's transform pipeline when Vitest runs in its server environment.
    noExternal: ['@kenn-io/kit-ui', '@lucide/svelte']
  },
  test: {
    environment: 'jsdom',
    setupFiles: ['./src/test/setup.ts'],
    exclude: ['tests/**', 'node_modules/**', 'dist/**']
  }
});
