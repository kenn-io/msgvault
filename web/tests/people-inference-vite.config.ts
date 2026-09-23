import { mergeConfig } from 'vite';

import appConfig from '../vite.config';

// The dev dependency optimizer cannot parse kit-ui's published .svelte.ts
// sources; let the Svelte plugin transform them as the production build does.
export default mergeConfig(appConfig, {
  optimizeDeps: { exclude: ['@kenn-io/kit-ui'] },
});
