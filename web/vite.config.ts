import { svelte } from '@sveltejs/vite-plugin-svelte'
import { svelteTesting } from '@testing-library/svelte/vite'
import { defineConfig } from 'vitest/config'

export default defineConfig({
  // svelteTesting mounts components with Svelte's client runtime under test.
  plugins: [svelte(), svelteTesting()],
  // make web-dev: the API on its default listener.
  server: { proxy: { '/api': 'http://127.0.0.1:8080' } },
  build: {
    outDir: 'dist',
    emptyOutDir: true,
    // Nothing inlined as a data: URL, which the CSP refuses.
    assetsInlineLimit: 0,
  },
  test: { environment: 'jsdom', include: ['src/**/*.test.ts'] },
})
