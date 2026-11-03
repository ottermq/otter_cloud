<script lang="ts">
  let { text, label }: { text: string; label: string } = $props()

  let note = $state('')

  async function copy() {
    try {
      await navigator.clipboard.writeText(text)
      note = 'Copied.'
    } catch {
      note = 'Could not copy it. Select the text and copy it by hand.'
    }
  }
</script>

<p class="actions">
  <button type="button" onclick={copy}>{label}</button>
  <span class="muted" role="status">{note}</span>
</p>

<style>
  .actions {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--space-2);
    margin-top: var(--space-2);
  }
</style>
