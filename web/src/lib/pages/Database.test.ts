import { fireEvent, render, screen } from '@testing-library/svelte'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import Database from './Database.svelte'

const running = {
  id: 'i1',
  name: 'production',
  status: 'running',
  health: 'ok',
  desired_state: 'running',
  allocation: { storage_gb: 2, max_connections: 20 },
  connection: { host: 'x.db.example.com', port: 5432, database: 'otc_1', user: 'otc_1', tls: 'required' },
  operation: null,
  storage: { used_bytes: 1073741824, allocated_bytes: 2147483648, measured_at: null, state: 'ok' },
  expires_at: null,
  ephemeral: false,
  created_at: '2026-11-01T00:00:00Z',
}
const op = (kind: string, status = 'pending', message?: string) => ({ id: 'op1', kind, status, message })
const busy = (kind: string) => ({ ...running, operation: op(kind) })

type Answer = [number, unknown] | Error

// answers maps "METHOD /api/path" to one answer or a queue, the last of which
// repeats.
function stubAPI(answers: Record<string, Answer | Answer[]>) {
  vi.stubGlobal('fetch', vi.fn(async (url: string, init: RequestInit = {}) => {
    const a = answers[`${init.method ?? 'GET'} ${url}`]
    if (a === undefined) throw new Error(`no answer for ${init.method ?? 'GET'} ${url}`)
    const head = (a as Answer[])[0]
    const queue = (Array.isArray(head) || head instanceof Error) ? a as Answer[] : null
    const next = queue ? (queue.length > 1 ? queue.shift()! : queue[0]) : a as Answer
    if (next instanceof Error) throw next
    return new Response(JSON.stringify(next[1]), { status: next[0] })
  }))
}

const sent = (method: string) => vi.mocked(fetch).mock.calls.filter(([, init]) => init?.method === method)
const gets = () => vi.mocked(fetch).mock.calls.filter(([u, init]) => u === '/api/instances/i1' && !init?.method).length
const click = (name: string) => fireEvent.click(screen.getByRole('button', { name }))
const pause = (ms = 30) => new Promise((r) => setTimeout(r, ms))

beforeEach(() => {
  vi.stubGlobal('navigator', { clipboard: { writeText: vi.fn(async () => {}) } })
})

afterEach(() => vi.unstubAllGlobals())

describe('the database page', () => {
  it('shows status and health apart, the allocation and the size', async () => {
    stubAPI({ 'GET /api/instances/i1': [200, running] })
    render(Database, { id: 'i1' })
    await screen.findByRole('heading', { name: 'production' })
    expect(screen.getByText('Running')).toBeTruthy()
    expect(screen.getByText('OK')).toBeTruthy()
    expect(document.body.textContent).toContain('2 GB, 20 connections')
    expect(document.body.textContent).toContain('1 GB of 2 GB')
  })

  it('shows connection details with no password, and copies the string', async () => {
    stubAPI({ 'GET /api/instances/i1': [200, running] })
    render(Database, { id: 'i1' })
    const want = 'postgresql://otc_1@x.db.example.com:5432/otc_1?sslmode=require'
    await screen.findByText(want)
    await click('Copy connection string')
    expect(navigator.clipboard.writeText).toHaveBeenCalledWith(want)
  })

  it('has no connection details until it runs', async () => {
    stubAPI({ 'GET /api/instances/i1': [200, { ...running, status: 'stopped', connection: null }] })
    render(Database, { id: 'i1' })
    await screen.findByText(/appear while the database is running/)
  })

  it('shows the reason as the platform wrote it, and a 404 as written', async () => {
    stubAPI({ 'GET /api/instances/i1': [200, { ...running, message: 'The database is over its storage allocation.' }] })
    const first = render(Database, { id: 'i1' })
    await screen.findByText('The database is over its storage allocation.')
    first.unmount()
    stubAPI({ 'GET /api/instances/i1': [404, { error: 'no such database' }] })
    render(Database, { id: 'i1' })
    expect((await screen.findByRole('alert')).textContent).toBe('no such database')
  })

  it('warns from 80% of its storage', async () => {
    stubAPI({ 'GET /api/instances/i1': [200, { ...running, storage: { ...running.storage, state: 'warning' } }] })
    render(Database, { id: 'i1' })
    expect((await screen.findByRole('alert')).textContent).toMatch(/over 80%/)
  })
})

describe('polling', () => {
  it('asks while an operation runs and stops when it ends', async () => {
    stubAPI({
      'GET /api/instances/i1': [[200, busy('stop')], [200, busy('stop')], [200, running]],
      'GET /api/operations/op1': [200, op('stop', 'succeeded')],
    })
    render(Database, { id: 'i1', pollMs: 5 })
    await screen.findByText('Stop in progress…')
    await vi.waitFor(() => expect(screen.queryByText('Stop in progress…')).toBeNull())
    const seen = gets()
    expect(seen).toBe(3)
    await pause()
    expect(gets()).toBe(seen)
    expect(screen.queryByRole('alert')).toBeNull()
  })

  it('does not poll with nothing in flight', async () => {
    stubAPI({ 'GET /api/instances/i1': [200, running] })
    render(Database, { id: 'i1', pollMs: 5 })
    await screen.findByText('Running')
    await pause()
    expect(gets()).toBe(1)
  })

  it('shows why a followed operation failed', async () => {
    stubAPI({
      'GET /api/instances/i1': [[200, busy('reset_password')], [200, running]],
      'GET /api/operations/op1': [200, op('reset_password', 'failed', 'The password could not be reset. Your current password still works; try again in a few minutes.')],
    })
    render(Database, { id: 'i1', pollMs: 5 })
    await screen.findByText(/Your current password still works/)
  })

  it('keeps asking through a failed check, and stops when the page is left', async () => {
    stubAPI({ 'GET /api/instances/i1': [[200, busy('stop')], new TypeError('Failed to fetch'), [200, busy('stop')]] })
    const { unmount } = render(Database, { id: 'i1', pollMs: 5 })
    await vi.waitFor(() => expect(gets()).toBeGreaterThan(2))
    expect(screen.getByText('production')).toBeTruthy()
    unmount()
    const seen = gets()
    await pause()
    expect(gets()).toBe(seen)
  })
})

describe('stop and start', () => {
  it('says what stopping means before it stops', async () => {
    stubAPI({
      'GET /api/instances/i1': [200, running],
      'POST /api/instances/i1/stop': [202, { instance: busy('stop'), operation: op('stop') }],
    })
    render(Database, { id: 'i1', pollMs: 1000 })
    await screen.findByText('Running')
    await click('Stop')
    expect(sent('POST')).toHaveLength(0)
    await screen.findByText(/No server stops, and the data stays/)
    await click('Stop database')
    await screen.findByText('Stop in progress…')
    expect(sent('POST')[0][0]).toBe('/api/instances/i1/stop')
  })

  it('starts a stopped database, and offers no stop', async () => {
    stubAPI({
      'GET /api/instances/i1': [200, { ...running, status: 'stopped', connection: null }],
      'POST /api/instances/i1/start': [202, { instance: busy('start'), operation: op('start') }],
    })
    render(Database, { id: 'i1', pollMs: 1000 })
    await screen.findByText('Stopped')
    expect(screen.queryByRole('button', { name: 'Stop' })).toBeNull()
    await click('Start')
    await screen.findByText('Start in progress…')
  })

  it('shows a refusal as written', async () => {
    stubAPI({
      'GET /api/instances/i1': [200, { ...running, status: 'stopped', connection: null }],
      'POST /api/instances/i1/start': [409, { error: 'this database is already running' }],
    })
    render(Database, { id: 'i1', pollMs: 1000 })
    await screen.findByText('Stopped')
    await click('Start')
    expect((await screen.findByRole('alert')).textContent).toBe('this database is already running')
  })

  it('disables the actions while an operation runs', async () => {
    stubAPI({ 'GET /api/instances/i1': [200, busy('restart')] })
    render(Database, { id: 'i1', pollMs: 1000 })
    await screen.findByText('Restart in progress…')
    expect((screen.getByRole('button', { name: 'Delete' }) as HTMLButtonElement).disabled).toBe(true)
  })
})

describe('resetting the password', () => {
  it('confirms, then shows the new one once', async () => {
    stubAPI({
      'GET /api/instances/i1': [200, running],
      'POST /api/instances/i1/reset-password': [202, { instance: busy('reset_password'), operation: op('reset_password'), password: 'newsekret' }],
    })
    const first = render(Database, { id: 'i1', pollMs: 1000 })
    await screen.findByText('Running')
    await click('Reset password')
    expect(sent('POST')).toHaveLength(0)
    await click('Reset password')
    await screen.findByText('newsekret')
    expect(screen.getByRole('alert').textContent).toMatch(/only time this password is shown/)
    first.unmount()
    stubAPI({ 'GET /api/instances/i1': [200, running] })
    render(Database, { id: 'i1', pollMs: 1000 })
    await screen.findByText('Running')
    expect(document.body.textContent).not.toContain('newsekret')
  })

  it('takes the new password away if the reset fails', async () => {
    stubAPI({
      'GET /api/instances/i1': [[200, running], [200, running]],
      'POST /api/instances/i1/reset-password': [202, { instance: busy('reset_password'), operation: op('reset_password'), password: 'newsekret' }],
      'GET /api/operations/op1': [200, op('reset_password', 'failed', 'The password could not be reset. Your current password still works; try again in a few minutes.')],
    })
    render(Database, { id: 'i1', pollMs: 5 })
    await screen.findByText('Running')
    await click('Reset password')
    await click('Reset password')
    await screen.findByText(/Your current password still works/)
    expect(document.body.textContent).not.toContain('newsekret')
  })

  it('retries with the same key after no answer, and says when it can not show it', async () => {
    stubAPI({
      'GET /api/instances/i1': [200, running],
      'POST /api/instances/i1/reset-password': [new TypeError('Failed to fetch'), [202, { instance: running, operation: op('reset_password'), password_shown: false }]],
    })
    render(Database, { id: 'i1', pollMs: 1000 })
    await screen.findByText('Running')
    await click('Reset password')
    await click('Reset password')
    await screen.findByText(/Could not reach/)
    await click('Reset password')
    await screen.findByText(/can't be shown again/)
    const keys = sent('POST').map(([, init]) => (init?.headers as Record<string, string>)['Idempotency-Key'])
    expect(keys[0]).toBeTruthy()
    expect(keys[1]).toBe(keys[0])
  })
})

describe('deleting', () => {
  it('only after typing the name, which it sends', async () => {
    stubAPI({
      'GET /api/instances/i1': [200, running],
      'DELETE /api/instances/i1': [202, { instance: { ...busy('delete'), status: 'deleting', desired_state: 'deleted' }, operation: op('delete') }],
    })
    render(Database, { id: 'i1', pollMs: 1000 })
    await screen.findByText('Running')
    await click('Delete')
    await screen.findByText(/A final copy is kept for 3 days/)
    const button = screen.getByRole('button', { name: 'Delete database' }) as HTMLButtonElement
    expect(button.disabled).toBe(true)
    const name = screen.getByLabelText(/^Type production/)
    await fireEvent.input(name, { target: { value: 'productio' } })
    expect(button.disabled).toBe(true)
    await fireEvent.input(name, { target: { value: 'production' } })
    expect(button.disabled).toBe(false)
    await fireEvent.click(button)
    await screen.findByText('Delete in progress…')
    expect(JSON.parse(String(sent('DELETE')[0][1]?.body))).toEqual({ confirm: 'production' })
    expect(screen.queryByRole('button', { name: 'Delete' })).toBeNull()
  })

  it('says an ephemeral database keeps no final copy', async () => {
    stubAPI({ 'GET /api/instances/i1': [200, { ...running, ephemeral: true }] })
    render(Database, { id: 'i1', pollMs: 1000 })
    await screen.findByText('Running')
    await click('Delete')
    await screen.findByText(/no final copy is kept/)
  })

  it('says when it is gone', async () => {
    stubAPI({ 'GET /api/instances/i1': [200, { ...running, status: 'deleted', desired_state: 'deleted', connection: null }] })
    render(Database, { id: 'i1', pollMs: 1000 })
    await screen.findByText('This database was deleted.')
    expect(screen.queryByRole('button', { name: 'Delete' })).toBeNull()
  })
})

describe('the expiry', () => {
  it('sets and clears it, and shows a refusal as written', async () => {
    const at = '2030-01-02T03:04:00Z'
    stubAPI({
      'GET /api/instances/i1': [200, running],
      'PATCH /api/instances/i1': [[422, { error: 'an expiry must be at least 1h0m0s away' }], [200, { ...running, expires_at: at }], [200, running]],
    })
    render(Database, { id: 'i1', pollMs: 1000 })
    await screen.findByText('Running')
    await fireEvent.input(screen.getByLabelText(/No expiry/), { target: { value: '2030-01-02T03:04' } })
    await click('Set expiry')
    await screen.findByText(/at least 1h0m0s away/)
    await click('Set expiry')
    await screen.findByRole('button', { name: 'Clear expiry' })
    await click('Clear expiry')
    await vi.waitFor(() => expect(screen.queryByRole('button', { name: 'Clear expiry' })).toBeNull())
    const bodies = sent('PATCH').map(([, init]) => JSON.parse(String(init?.body)))
    expect(bodies[0].expires_at).toBe(new Date('2030-01-02T03:04').toISOString())
    expect(bodies[2]).toEqual({ expires_at: null })
  })
})
