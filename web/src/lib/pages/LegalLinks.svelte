<script lang="ts">
  import { link } from '../router.svelte'
</script>

<nav aria-label="Legal">
  <a href="/termos" onclick={(e) => link(e, '/termos')}>Termos de uso</a>
  <a href="/privacidade" onclick={(e) => link(e, '/privacidade')}>Aviso de privacidade</a>
</nav>

<style>
  nav {
    display: flex;
    justify-content: center;
    gap: var(--space-4);
    margin: var(--space-6) 0 var(--space-4);
    font-size: 0.875rem;
  }
</style>
