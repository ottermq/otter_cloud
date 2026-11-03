import { fireEvent, render, screen } from '@testing-library/svelte'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { session } from '../session.svelte'
import Account from './Account.svelte'

const here = {
  id: 's1',
  created_at: '2026-11-01T00:00:00Z',
  last_seen_at: '2026-11-02T00:00:00Z',
  ip: '203.0.113.7',
  user_agent: 'Firefox on this laptop',
  current: true,
}
const there = { ...here, id: 's2', ip: null, user_agent: '', current: false }
const list = (...sessions: object[]) => [200, { sessions }] as [number, unknown]

type Answer = [number, unknown]

// answers maps "METHOD /api/path" to one answer or a queue, the last of which
// repeats.
function stubAPI(answers: Record<string, Answer | Answer[]>) {
  vi.stubGlobal('fetch', vi.fn(async (url: string, init: RequestInit = {}) => {
    const a = answers[`${init.method ?? 'GET'} ${url}`]
    if (a === undefined) throw new Error(`no answer for ${init.method ?? 'GET'} ${url}`)
    const queue = Array.isArray(a[0]) ? a as Answer[] : null
    const next = queue ? (queue.length > 1 ? queue.shift()! : queue[0]) : a as Answer
    return new Response(next[0] === 204 ? null : JSON.stringify(next[1]), { status: next[0] })
  }))
}

const sent = (method: string, url: string) =>
  vi.mocked(fetch).mock.calls.filter(([u, init]) => u === url && init?.method === method)

async function change(currentPw: string, next: string, repeat = next) {
  await fireEvent.input(screen.getByLabelText('Current password'), { target: { value: currentPw } })
  await fireEvent.input(screen.getByLabelText(/^New password/), { target: { value: next } })
  await fireEvent.input(screen.getByLabelText('Repeat the new password'), { target: { value: repeat } })
  await fireEvent.click(screen.getByRole('button', { name: 'Change password' }))
}

beforeEach(() => {
  session.me = { email: 'ana@example.com' }
})

afterEach(() => vi.unstubAllGlobals())

describe('changing the password', () => {
  it('sends both, clears the form, says the other sessions ended and reloads them', async () => {
    stubAPI({
      'GET /api/account/sessions': [list(here, there), list(here)],
      'POST /api/account/password': [200, { email: 'ana@example.com' }],
    })
    render(Account)
    await screen.findByText('This session')
    await change('the old password', 'a long enough password')
    await screen.findByText(/Your other sessions have ended/)
    expect(JSON.parse(String(sent('POST', '/api/account/password')[0][1]?.body))).toEqual({
      current_password: 'the old password',
      new_password: 'a long enough password',
    })
    expect((screen.getByLabelText('Current password') as HTMLInputElement).value).toBe('')
    await vi.waitFor(() => expect(screen.queryByRole('button', { name: 'End this session' })).toBeNull())
  })

  it('shows a refusal as written, and stays logged in on a wrong password', async () => {
    stubAPI({
      'GET /api/account/sessions': list(here),
      'POST /api/account/password': [403, { error: 'the current password is wrong' }],
    })
    render(Account)
    await screen.findByText('This session')
    await change('wrong', 'a long enough password')
    expect((await screen.findByRole('alert')).textContent).toBe('the current password is wrong')
    expect(session.me).not.toBeNull()
  })

  it('does not send two different new passwords', async () => {
    stubAPI({ 'GET /api/account/sessions': list(here) })
    render(Account)
    await screen.findByText('This session')
    await change('old', 'a long enough password', 'another long password')
    expect((await screen.findByRole('alert')).textContent).toBe('The two new passwords are different.')
  })
})

describe('sessions', () => {
  it('marks this one, offers no ending it here, and copes with missing details', async () => {
    stubAPI({ 'GET /api/account/sessions': list(here, there) })
    render(Account)
    await screen.findByText('Firefox on this laptop')
    expect(screen.getByText('This session')).toBeTruthy()
    expect(screen.getByText('Unknown browser')).toBeTruthy()
    expect(screen.getAllByRole('button', { name: 'End this session' })).toHaveLength(1)
  })

  it('lists this session first', async () => {
    stubAPI({ 'GET /api/account/sessions': list(there, here) })
    render(Account)
    await screen.findByText('This session')
    expect(screen.getAllByRole('listitem')[0].textContent).toContain('Firefox on this laptop')
  })

  it('ends one', async () => {
    stubAPI({
      'GET /api/account/sessions': [list(here, there), list(here)],
      'DELETE /api/account/sessions/s2': [204, null],
    })
    render(Account)
    await fireEvent.click(await screen.findByRole('button', { name: 'End this session' }))
    await vi.waitFor(() => expect(screen.queryByText('Unknown browser')).toBeNull())
    expect(sent('DELETE', '/api/account/sessions/s2')).toHaveLength(1)
  })

  it('ends all the others, and says how many', async () => {
    stubAPI({
      'GET /api/account/sessions': [list(here, there), list(here)],
      'DELETE /api/account/sessions': [200, { ended: 1 }],
    })
    render(Account)
    await fireEvent.click(await screen.findByRole('button', { name: 'End all other sessions' }))
    await screen.findByText('Ended 1 session.')
    await vi.waitFor(() => expect(screen.queryByRole('button', { name: 'End all other sessions' })).toBeNull())
  })

  it('has nothing to end with one session', async () => {
    stubAPI({ 'GET /api/account/sessions': list(here) })
    render(Account)
    await screen.findByText('This session')
    expect(screen.queryByRole('button', { name: 'End all other sessions' })).toBeNull()
  })

  it('shows an error as written', async () => {
    stubAPI({
      'GET /api/account/sessions': list(here, there),
      'DELETE /api/account/sessions/s2': [404, { error: 'no such session' }],
    })
    render(Account)
    await fireEvent.click(await screen.findByRole('button', { name: 'End this session' }))
    expect((await screen.findByRole('alert')).textContent).toBe('no such session')
  })
})
