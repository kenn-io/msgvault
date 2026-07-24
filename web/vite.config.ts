import { svelte } from '@sveltejs/vite-plugin-svelte';
import { defineConfig } from 'vitest/config';

export default defineConfig({
  plugins: [svelte()],
  build: {
    manifest: true
  },
  preview: {
    headers: {
      // Mirrors shellCSP in internal/web/handler.go so Playwright runs
      // exercise the same policy intersection the daemon enforces on
      // sandboxed srcdoc mail frames. Keep the two in sync.
      'Content-Security-Policy':
        "default-src 'self'; img-src 'self' data: blob: https: http:; script-src 'self'; style-src 'self'; style-src-attr 'unsafe-inline'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'"
    }
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
