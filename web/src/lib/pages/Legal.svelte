<script lang="ts">
  import { api } from '../api'

  // A legal text (#125), rendered by the API from the versioned source with
  // the controller's details filled in; its HTML is the server's own.
  let { name }: { name: 'termos' | 'privacidade' } = $props()

  type Doc = { title: string; html: string; version: string }

  let doc = $state<Doc | null>(null)
  let error = $state('')

  $effect(() => {
    doc = null
    error = ''
    api<Doc>(`/legal/${name}`).then(
      (d) => (doc = d),
      (err) => (error = err.status === 404 ? 'This page is not published yet.' : err.message),
    )
  })
</script>

<main lang="pt-BR">
  {#if doc}
    <article>{@html doc.html}</article>
  {:else if error}
    <p class="status" role="alert">{error}</p>
  {:else}
    <p class="status">Loading…</p>
  {/if}
</main>

<style>
  main {
    max-width: 48rem;
    margin: var(--space-6) auto;
    padding: var(--space-6) var(--space-4);
    background: var(--surface);
    border: 1px solid var(--border);
    border-radius: var(--radius);
    overflow-wrap: anywhere;
  }

  article :global(h1) {
    margin-top: 0;
    font-size: 1.5rem;
    color: var(--text-strong);
  }

  article :global(h2) {
    font-size: 1.125rem;
    color: var(--text-strong);
  }

  article :global(table) {
    display: block;
    overflow-x: auto;
    border-collapse: collapse;
  }

  article :global(th),
  article :global(td) {
    padding: var(--space-1) var(--space-2);
    border: 1px solid var(--border);
    text-align: left;
    vertical-align: top;
  }

  .status {
    color: var(--text-muted);
    text-align: center;
  }
</style>
