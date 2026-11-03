import { fireEvent, render, screen, waitFor } from '@testing-library/svelte'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import CreateDatabase from './CreateDatabase.svelte'

const catalog = {
  service_types: [{
    code: 'postgresql',
    name: 'PostgreSQL',
    flavors: [
      { code: 'db-small', name: 'Small', storage_gb: 2, max_connections: 20 },
      { code: 'db-medium', name: 'Medium', storage_gb: 5, max_connections: 30 },
    ],
  }],
}

const subscription = {
  entitled: true,
  subscription: {
    plan: { name: 'Founder Beta', price_minor: 0, currency: 'BRL', interval: 'none' },
    status: 'active',
    payment_status: 'not_required',
    starts_at: '2026-11-01T00:00:00Z',
    ends_at: null,
    limits: { instances: { allowed: 2, allocated: 1 }, storage_gb: { allowed: 10, allocated: 2 } },
  },
}

const op = (status: string, message?: string) => ({ id: 'op1', kind: 'create', status, message })
const accepted = (extra: object) => [202, {
  instance: { id: 'i1', name: 'production', status: 'provisioning', health: 'unknown', operation: null },
  operation: op('pending'),
  ...extra,
}] as [number, unknown]

type Answer = [number, unknown] | Error

// answers maps "METHOD /api/path" to one answer or a queue of them, the last
// of which repeats.
function stubAPI(answers: Record<string, Answer | Answer[]>) {
  vi.stubGlobal('fetch', vi.fn(async (url: string, init: RequestInit = {}) => {
    const key = `${init.method ?? 'GET'} ${url}`
    const a = answers[key] ?? (url === '/api/catalog' ? [200, catalog] : [200, subscription])
    const next = (Array.isArray(a) && Array.isArray(a[0]) || a instanceof Array && a[0] instanceof Error)
      ? ((a as Answer[]).length > 1 ? (a as Answer[]).shift()! : (a as Answer[])[0])
      : a as Answer
    if (next instanceof Error) throw next
    return new Response(JSON.stringify(next[1]), { status: next[0] })
  }))
}

const posts = () => vi.mocked(fetch).mock.calls.filter(([, init]) => init?.method === 'POST')
const postedBody = (i = 0) => JSON.parse(String(posts()[i][1]?.body))
const postedKey = (i: number) => (posts()[i][1]?.headers as Record<string, string>)['Idempotency-Key']

async function fillAndCreate(name = 'production') {
  await fireEvent.input(await screen.findByLabelText(/^Name/), { target: { value: name } })
  await fireEvent.click(screen.getByRole('button', { name: 'Create database' }))
}

beforeEach(() => {
  vi.stubGlobal('navigator', { clipboard: { writeText: vi.fn(async () => {}) } })
})

afterEach(() => {
  vi.unstubAllGlobals()
  localStorage.clear()
  sessionStorage.clear()
})

describe('the create form', () => {
  it('offers the flavors by name, with what is left', async () => {
    stubAPI({})
    render(CreateDatabase, { pollMs: 5 })
    await screen.findByLabelText(/^Small: 2 GB, 20 connections/)
    expect(document.body.textContent).not.toContain('db-small')
    expect(document.body.textContent).toContain('1 database, 8 GB')
  })

  it('sends the chosen flavor, and no expiry unless one is set', async () => {
    stubAPI({ 'POST /api/instances': accepted({ password: 'once' }), 'GET /api/operations/op1': [200, op('running')] })
    render(CreateDatabase, { pollMs: 1000 })
    await fireEvent.click(await screen.findByLabelText(/^Medium/))
    await fillAndCreate()
    await screen.findByText('once')
    expect(postedBody()).toEqual({ name: 'production', flavor: 'db-medium' })
  })

  it('offers ephemeral only with an expiry, and sends both', async () => {
    stubAPI({ 'POST /api/instances': accepted({ password: 'once' }), 'GET /api/operations/op1': [200, op('running')] })
    render(CreateDatabase, { pollMs: 1000 })
    await screen.findByLabelText(/^Name/)
    expect(screen.queryByLabelText(/^Ephemeral/)).toBeNull()
    await fireEvent.input(screen.getByLabelText(/^Expiry/), { target: { value: '2030-01-02T03:04' } })
    await fireEvent.click(await screen.findByLabelText(/^Ephemeral/))
    await fillAndCreate()
    await screen.findByText('once')
    expect(postedBody().ephemeral).toBe(true)
    expect(postedBody().expires_at).toBe(new Date('2030-01-02T03:04').toISOString())
  })

  it('shows a refusal as written and keeps the form', async () => {
    stubAPI({ 'POST /api/instances': [409, { error: 'you already have a database with this name' }] })
    render(CreateDatabase)
    await fillAndCreate()
    expect((await screen.findByRole('alert')).textContent).toBe('you already have a database with this name')
    expect(screen.getByRole('button', { name: 'Create database' })).toBeTruthy()
  })

  it('has no form for an account the API says is not entitled', async () => {
    stubAPI({ 'GET /api/subscription': [200, { entitled: false, subscription: null }] })
    render(CreateDatabase)
    expect((await screen.findByRole('alert')).textContent).toMatch(/can't be created/)
    expect(screen.queryByRole('button', { name: 'Create database' })).toBeNull()
  })
})

describe('the Idempotency-Key', () => {
  it('stays the same when a request with no answer is retried', async () => {
    stubAPI({ 'POST /api/instances': [new TypeError('Failed to fetch'), accepted({ password_shown: false })] })
    render(CreateDatabase, { pollMs: 1000 })
    await fillAndCreate()
    expect((await screen.findByRole('alert')).textContent).toMatch(/Could not reach/)
    await fireEvent.click(screen.getByRole('button', { name: 'Create database' }))
    await screen.findByText(/already created by an earlier attempt/)
    expect(postedKey(1)).toBe(postedKey(0))
  })

  it('changes after a refusal, and when the request changes', async () => {
    stubAPI({ 'POST /api/instances': [[409, { error: 'taken' }], new TypeError('Failed to fetch'), [409, { error: 'taken' }]] })
    render(CreateDatabase)
    await fillAndCreate('one')
    await screen.findByText('taken')
    await fillAndCreate('one')
    await screen.findByText(/Could not reach/)
    await fillAndCreate('two')
    await screen.findByText('taken')
    expect(new Set([postedKey(0), postedKey(1), postedKey(2)]).size).toBe(3)
  })
})

describe('after the create is accepted', () => {
  it('shows the password once, with a warning and a copy button', async () => {
    stubAPI({ 'POST /api/instances': accepted({ password: 'sekret123' }), 'GET /api/operations/op1': [200, op('running')] })
    render(CreateDatabase, { pollMs: 1000 })
    await fillAndCreate()
    await screen.findByText('sekret123')
    expect(screen.getByRole('alert').textContent).toMatch(/only time this password is shown/)
    await fireEvent.click(screen.getByRole('button', { name: 'Copy password' }))
    expect(navigator.clipboard.writeText).toHaveBeenCalledWith('sekret123')
    await screen.findByText('Copied.')
  })

  it('never shows it again, and keeps it out of storage and the URL', async () => {
    stubAPI({ 'POST /api/instances': accepted({ password: 'sekret123' }), 'GET /api/operations/op1': [200, op('running')] })
    const first = render(CreateDatabase, { pollMs: 1000 })
    await fillAndCreate()
    await screen.findByText('sekret123')
    expect(JSON.stringify({ ...localStorage })).not.toContain('sekret123')
    expect(JSON.stringify({ ...sessionStorage })).not.toContain('sekret123')
    expect(window.location.href).not.toContain('sekret123')
    first.unmount()
    render(CreateDatabase, { pollMs: 1000 })
    await screen.findByLabelText(/^Name/)
    expect(document.body.textContent).not.toContain('sekret123')
  })

  it('says so when the Idempotency-Key replayed and there is no password', async () => {
    stubAPI({ 'POST /api/instances': accepted({ password_shown: false }), 'GET /api/operations/op1': [200, op('running')] })
    render(CreateDatabase, { pollMs: 1000 })
    await fillAndCreate()
    await screen.findByText(/Reset the password once the database is running/)
    expect(screen.queryByRole('button', { name: 'Copy password' })).toBeNull()
  })

  it('follows the operation to the end and stops, keeping the password on screen', async () => {
    stubAPI({
      'POST /api/instances': accepted({ password: 'sekret123' }),
      'GET /api/operations/op1': [[200, op('running')], [200, op('succeeded')]],
    })
    render(CreateDatabase, { pollMs: 5 })
    await fillAndCreate()
    await screen.findByText('The database is ready.')
    expect(screen.getByText('sekret123')).toBeTruthy()
    const polls = () => vi.mocked(fetch).mock.calls.filter(([u]) => u === '/api/operations/op1').length
    const seen = polls()
    await new Promise((r) => setTimeout(r, 30))
    expect(polls()).toBe(seen)
  })

  it('shows the message the platform wrote when it fails', async () => {
    stubAPI({
      'POST /api/instances': accepted({ password: 'sekret123' }),
      'GET /api/operations/op1': [200, op('failed', 'The database could not be created. Delete it and try again, or contact support.')],
    })
    render(CreateDatabase, { pollMs: 5 })
    await fillAndCreate()
    await screen.findByText(/could not be created. Delete it/)
  })

  it('keeps asking when a check fails', async () => {
    stubAPI({
      'POST /api/instances': accepted({ password: 'sekret123' }),
      'GET /api/operations/op1': [new TypeError('Failed to fetch'), [200, op('succeeded')]],
    })
    render(CreateDatabase, { pollMs: 5 })
    await fillAndCreate()
    await screen.findByText('The database is ready.')
    await waitFor(() => expect(screen.queryByText(/Could not check/)).toBeNull())
  })

  it('stops asking when the page is left', async () => {
    stubAPI({ 'POST /api/instances': accepted({ password: 'sekret123' }), 'GET /api/operations/op1': [200, op('running')] })
    const { unmount } = render(CreateDatabase, { pollMs: 5 })
    await fillAndCreate()
    await screen.findByText('sekret123')
    unmount()
    const polls = () => vi.mocked(fetch).mock.calls.filter(([u]) => u === '/api/operations/op1').length
    const seen = polls()
    await new Promise((r) => setTimeout(r, 30))
    expect(polls()).toBe(seen)
  })
})
