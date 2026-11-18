<script lang="ts">
  import { api } from '../api'
  import { takeFragment } from '../router.svelte'
  import { signedIn, type Me } from '../session.svelte'
  import SignedOutPage from './SignedOutPage.svelte'

  const token = takeFragment('invite')

  let accountName = $state('')
  let password = $state('')
  let repeat = $state('')
  let accepted = $state(false)
  // The version of the terms and privacy notice in force (#125); empty while
  // they aren't published, and then nothing is asked.
  let terms = $state('')

  $effect(() => {
    api<{ version: string }>('/legal/termos').then(
      (d) => (terms = d.version),
      () => (terms = ''),
    )
  })

  async function signUp() {
    // There is no email to recover a mistyped password with.
    if (password !== repeat) throw new Error('The two passwords are different.')
    signedIn(await api<Me>('/signup', {
      method: 'POST',
      body: JSON.stringify({
        token,
        password,
        account_name: accountName,
        accepted_terms: terms && accepted ? terms : '',
      }),
    }))
  }
</script>

{#if token}
  <SignedOutPage title="Create your account" action="Create account" onsubmit={signUp}>
    <label>
      Account name (optional)
      <input type="text" autocomplete="organization" bind:value={accountName} />
    </label>
    <label>
      Password
      <input type="password" autocomplete="new-password" required bind:value={password} />
      <small>At least 12 characters.</small>
    </label>
    <label>
      Repeat the password
      <input type="password" autocomplete="new-password" required bind:value={repeat} />
    </label>
    {#if terms}
      <label class="accept">
        <input type="checkbox" required bind:checked={accepted} />
        <span>
          I have read and accept the <a href="/termos" target="_blank">Termos de uso</a> and the
          <a href="/privacidade" target="_blank">Aviso de privacidade</a>.
        </span>
      </label>
    {/if}
  </SignedOutPage>
{:else}
  <SignedOutPage title="Create your account">
    <p role="alert">This page needs the whole invite link. Open the link you were given again.</p>
  </SignedOutPage>
{/if}

<style>
  .accept {
    display: flex;
    flex-direction: row;
    align-items: flex-start;
    gap: var(--space-2);
  }
</style>
