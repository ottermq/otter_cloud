import { fireEvent, render, screen, waitFor } from '@testing-library/svelte'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import App from './App.svelte'
import { navigate } from './lib/router.svelte'
import { session } from './lib/session.svelte'

// answers maps "METHOD /api/path" to [status, body]. Otherwise /api/me is a
// 401, as when signed out, and the dashboard's two calls have nothing to show.
function fallback(url: string): [number, unknown] {
  if (url === '/api/subscription') return [200, { entitled: false, subscription: null }]
  if (url === '/api/instances') return [200, { instances: [] }]
  return [401, { error: 'not logged in' }]
}

function stubAPI(answers: Record<string, [number, unknown]>) {
  vi.stubGlobal('fetch', vi.fn(async (url: string, init: RequestInit = {}) => {
    const [status, body] = answers[`${init.method ?? 'GET'} ${url}`] ?? fallback(url)
    return new Response(status === 204 ? null : JSON.stringify(body), { status })
  }))
}

function posted(url: string) {
  const call = vi.mocked(fetch).mock.calls.find(([u, init]) => u === url && init?.method === 'POST')
  return JSON.parse(String(call?.[1]?.body))
}

async function logIn(email: string, password: string) {
  await fireEvent.input(await screen.findByLabelText('Email'), { target: { value: email } })
  await fireEvent.input(screen.getByLabelText('Password'), { target: { value: password } })
  await fireEvent.click(screen.getByRole('button', { name: 'Log in' }))
}

beforeEach(() => {
  session.checked = false
  session.me = null
  session.error = ''
  navigate('/', true)
})

afterEach(() => vi.unstubAllGlobals())

describe('signing in', () => {
  it('sends a signed-out visitor to the login page, and back after logging in', async () => {
    navigate('/databases', true)
    stubAPI({ 'POST /api/login': [200, { email: 'ana@example.com' }] })
    render(App)
    await logIn('ana@example.com', 'a long enough password')
    await screen.findByText('ana@example.com')
    expect(window.location.pathname).toBe('/databases')
    expect(posted('/api/login')).toEqual({ email: 'ana@example.com', password: 'a long enough password' })
  })

  it('shows the API error as written and stays on the page', async () => {
    stubAPI({ 'POST /api/login': [429, { error: 'too many failed attempts; try again in 15 minutes' }] })
    render(App)
    await logIn('ana@example.com', 'wrong')
    const alert = await screen.findByRole('alert')
    expect(alert.textContent).toBe('too many failed attempts; try again in 15 minutes')
    expect(window.location.pathname).toBe('/login')
  })

  it('says so when the API cannot be reached', async () => {
    stubAPI({})
    render(App)
    await screen.findByLabelText('Email')
    vi.mocked(fetch).mockRejectedValueOnce(new TypeError('Failed to fetch'))
    await logIn('ana@example.com', 'whatever')
    expect((await screen.findByRole('alert')).textContent).toMatch(/Could not reach/)
  })

  it('takes a logged-in user off the login page', async () => {
    navigate('/login', true)
    stubAPI({ 'GET /api/me': [200, { email: 'ana@example.com' }] })
    render(App)
    await screen.findByText('ana@example.com')
    expect(window.location.pathname).toBe('/')
  })

  it('does not call a failed session check signed out', async () => {
    stubAPI({ 'GET /api/me': [500, { error: 'internal error' }] })
    render(App)
    expect((await screen.findByRole('alert')).textContent).toBe('internal error')
    expect(window.location.pathname).toBe('/')
  })
})

describe('a session that ends while a page is open', () => {
  it('goes to login, and back to the page afterwards', async () => {
    navigate('/subscription', true)
    stubAPI({
      'GET /api/me': [200, { email: 'ana@example.com' }],
      'GET /api/subscription': [401, { error: 'not logged in' }],
      'POST /api/login': [200, { email: 'ana@example.com' }],
    })
    render(App)
    await screen.findByLabelText('Email')
    expect(window.location.pathname).toBe('/login')
    stubAPI({ 'POST /api/login': [200, { email: 'ana@example.com' }] })
    await logIn('ana@example.com', 'a long enough password')
    await screen.findByText('ana@example.com')
    expect(window.location.pathname).toBe('/subscription')
  })
})

describe('logging out', () => {
  it('ends at the login page, and the next login lands on the home page', async () => {
    navigate('/databases', true)
    stubAPI({ 'GET /api/me': [200, { email: 'ana@example.com' }], 'POST /api/logout': [204, null] })
    render(App)
    await fireEvent.click(await screen.findByRole('button', { name: 'Log out' }))
    await screen.findByLabelText('Email')
    expect(window.location.pathname).toBe('/login')
    stubAPI({ 'POST /api/login': [200, { email: 'bo@example.com' }] })
    await logIn('bo@example.com', 'a long enough password')
    await screen.findByText('bo@example.com')
    expect(window.location.pathname).toBe('/')
  })

  it('stays logged in, and says why, if the API refuses', async () => {
    stubAPI({ 'GET /api/me': [200, { email: 'ana@example.com' }], 'POST /api/logout': [500, { error: 'internal error' }] })
    render(App)
    await fireEvent.click(await screen.findByRole('button', { name: 'Log out' }))
    expect((await screen.findByText('internal error')).getAttribute('role')).toBe('alert')
    expect(screen.getByText('ana@example.com')).toBeTruthy()
  })
})

describe('invite and reset links', () => {
  async function fill(fields: Record<string, string>, action: string) {
    for (const [label, value] of Object.entries(fields)) {
      await fireEvent.input(await screen.findByLabelText(new RegExp(`^${label}`)), { target: { value } })
    }
    await fireEvent.click(screen.getByRole('button', { name: action }))
  }

  function open(link: string) {
    navigate(link.split('#')[0], true)
    history.replaceState({}, '', link)
  }

  it('signs up with the token from the fragment, which leaves the address bar', async () => {
    open('/signup#invite=abc-_123')
    stubAPI({ 'POST /api/signup': [201, { email: 'ana@example.com' }] })
    render(App)
    expect(window.location.hash).toBe('')
    await fill({ 'Account name': 'Ana', 'Password': 'a long enough password', 'Repeat': 'a long enough password' }, 'Create account')
    await waitFor(() => expect(window.location.pathname).toBe('/'))
    expect(posted('/api/signup')).toEqual({ token: 'abc-_123', password: 'a long enough password', account_name: 'Ana' })
  })

  it('shows why an invite was refused, as written', async () => {
    open('/signup#invite=used')
    stubAPI({ 'POST /api/signup': [400, { error: 'this invite link is not valid or has already been used' }] })
    render(App)
    await fill({ 'Password': 'a long enough password', 'Repeat': 'a long enough password' }, 'Create account')
    expect((await screen.findByRole('alert')).textContent).toBe('this invite link is not valid or has already been used')
    expect(window.location.pathname).toBe('/signup')
  })

  it('does not send two different passwords', async () => {
    open('/signup#invite=abc')
    stubAPI({})
    render(App)
    await fill({ 'Password': 'a long enough password', 'Repeat': 'another long password' }, 'Create account')
    expect((await screen.findByRole('alert')).textContent).toBe('The two passwords are different.')
    expect(vi.mocked(fetch).mock.calls.some(([u]) => u === '/api/signup')).toBe(false)
  })

  it('asks for the link when the token is missing', async () => {
    open('/signup')
    stubAPI({})
    render(App)
    expect((await screen.findByRole('alert')).textContent).toMatch(/whole invite link/)
    expect(screen.queryByRole('button')).toBeNull()
  })

  it('sets a new password from a reset link and logs in', async () => {
    open('/reset-password#token=abc-_123')
    stubAPI({ 'POST /api/password-reset': [200, { email: 'ana@example.com' }] })
    render(App)
    expect(window.location.hash).toBe('')
    await fill({ 'New password': 'a long enough password', 'Repeat': 'a long enough password' }, 'Set password')
    await waitFor(() => expect(window.location.pathname).toBe('/'))
    expect(posted('/api/password-reset')).toEqual({ token: 'abc-_123', password: 'a long enough password' })
  })
})
