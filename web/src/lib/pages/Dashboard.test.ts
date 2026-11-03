import { render, screen } from '@testing-library/svelte'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { navigate } from '../router.svelte'
import { session } from '../session.svelte'
import Dashboard from './Dashboard.svelte'

const founder = {
  plan: { name: 'Founder Beta', price_minor: 0, currency: 'BRL', interval: 'none' },
  status: 'active',
  payment_status: 'not_required',
  starts_at: '2026-11-01T00:00:00Z',
  ends_at: null,
  limits: { instances: { allowed: 2, allocated: 1 }, storage_gb: { allowed: 10, allocated: 2 } },
}

function stubAPI(answers: Record<string, [number, unknown]>) {
  vi.stubGlobal('fetch', vi.fn(async (url: string) => {
    const [status, body] = answers[url]
    return new Response(JSON.stringify(body), { status })
  }))
}

afterEach(() => vi.unstubAllGlobals())

describe('the dashboard', () => {
  it('shows the plan by name, its limits and the databases', async () => {
    stubAPI({
      '/api/subscription': [200, { entitled: true, subscription: founder }],
      '/api/instances': [200, { instances: [{
        id: '1', name: 'production', status: 'provisioning', health: 'unknown',
        operation: { kind: 'create', status: 'running' },
      }] }],
    })
    render(Dashboard)
    await screen.findByText('Founder Beta')
    expect(document.body.textContent).not.toContain('founder-beta')
    expect(document.body.textContent).toContain('Free')
    expect(document.body.textContent).toContain('1 of 2')
    expect(document.body.textContent).toContain('2 of 10 GB')
    expect(screen.getByText('production')).toBeTruthy()
    expect(screen.getByText('Provisioning · health: unknown')).toBeTruthy()
    expect(screen.getByText('Create in progress')).toBeTruthy()
    expect(screen.queryByRole('alert')).toBeNull()
  })

  it('says when there is no subscription', async () => {
    stubAPI({
      '/api/subscription': [200, { entitled: false, subscription: null }],
      '/api/instances': [200, { instances: [] }],
    })
    render(Dashboard)
    expect((await screen.findByRole('alert')).textContent).toMatch(/no subscription/)
    expect(screen.getByText('No databases yet.')).toBeTruthy()
  })

  it('says when the subscription is not in effect, by what the server says', async () => {
    stubAPI({
      '/api/subscription': [200, { entitled: false, subscription: { ...founder, status: 'suspended' } }],
      '/api/instances': [200, { instances: [] }],
    })
    render(Dashboard)
    expect((await screen.findByRole('alert')).textContent).toMatch(/not in effect \(Suspended/)
  })

  it('shows an API error as written', async () => {
    stubAPI({
      '/api/subscription': [500, { error: 'internal error' }],
      '/api/instances': [200, { instances: [] }],
    })
    render(Dashboard)
    expect((await screen.findByRole('alert')).textContent).toBe('internal error')
  })

  it('treats a 401 as the session having ended', async () => {
    navigate('/', true)
    session.me = { email: 'ana@example.com' }
    stubAPI({
      '/api/subscription': [401, { error: 'not logged in' }],
      '/api/instances': [401, { error: 'not logged in' }],
    })
    render(Dashboard)
    await screen.findByRole('alert')
    expect(session.me).toBeNull()
  })
})
