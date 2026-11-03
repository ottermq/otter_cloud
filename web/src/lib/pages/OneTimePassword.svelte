<script lang="ts">
  import CopyButton from './CopyButton.svelte'

  // The password from a create or a reset: shown this once. It lives in the
  // parent's variable only, never in storage or a URL, and is gone when the
  // page is left or reloaded (docs/portals.md). Empty: the request was a
  // replay of one already made, and the password can't be shown again.
  let { password, replayed }: { password: string; replayed: string } = $props()
</script>

<section class="card">
  <h2>Password</h2>
  {#if password}
    <p class="failed" role="alert">
      This is the only time this password is shown. Save it now: leaving or reloading this page
      discards it.
    </p>
    <code>{password}</code>
    <CopyButton text={password} label="Copy password" />
  {:else}
    <p role="alert">{replayed}</p>
  {/if}
</section>

<style>
  code {
    display: block;
    padding: var(--space-2);
    overflow-wrap: anywhere;
    color: var(--text-strong);
    background: var(--bg);
    border: 1px solid var(--border);
    border-radius: var(--radius);
  }
</style>
