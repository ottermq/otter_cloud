<script lang="ts">
  import { api } from '../api'
  import { signedIn, type Me } from '../session.svelte'
  import SignedOutPage from './SignedOutPage.svelte'

  let email = $state('')
  let password = $state('')

  async function login() {
    signedIn(await api<Me>('/login', { method: 'POST', body: JSON.stringify({ email, password }) }))
  }
</script>

<SignedOutPage title="Log in to Otter Labs Cloud" action="Log in" onsubmit={login}>
  <label>
    Email
    <input type="email" autocomplete="username" required bind:value={email} />
  </label>
  <label>
    Password
    <input type="password" autocomplete="current-password" required bind:value={password} />
  </label>
  {#snippet footer()}
    Forgot your password? Ask the operator for a reset link.
  {/snippet}
</SignedOutPage>
