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
        "default-src 'self'; img-src 'self' data: blob: https: http:; media-src 'self' blob:; script-src 'self'; style-src 'self'; style-src-attr 'unsafe-inline'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'"
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
    exclude: ['tests/**', 'node_modules/**', 'dist/**'],
    // In GitHub Actions, retry a failed test twice and record every result in
    // a JSON report that CI uploads, so a test that passed on retry stays
    // visible as flaky. The tree reporter lists each test under its file.
    // Naming reporters drops Vitest's automatic github-actions annotations, so
    // keep them explicitly.
    ...(process.env.GITHUB_ACTIONS === 'true' && {
      retry: 2,
      reporters: ['tree', 'github-actions', ['json', { outputFile: 'test-results/vitest/unit.json' }]]
    })
  }
});
