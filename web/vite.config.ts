import { svelte } from '@sveltejs/vite-plugin-svelte'
import { defineConfig } from 'vitest/config'

export default defineConfig({
  plugins: [svelte()],
  // make web-dev: the API on its default listener.
  server: { proxy: { '/api': 'http://127.0.0.1:8080' } },
  build: {
    outDir: 'dist',
    emptyOutDir: true,
    // Nothing inlined as a data: URL, which the CSP refuses.
    assetsInlineLimit: 0,
  },
  test: { include: ['src/**/*.test.ts'] },
})
