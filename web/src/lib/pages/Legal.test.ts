import { render, screen } from '@testing-library/svelte'
import { afterEach, describe, expect, it, vi } from 'vitest'
import Legal from './Legal.svelte'
import SignUp from './SignUp.svelte'

type Answer = [number, unknown]

function stubAPI(answers: Record<string, Answer>) {
  vi.stubGlobal('fetch', vi.fn(async (url: string, init: RequestInit = {}) => {
    const a = answers[`${init.method ?? 'GET'} ${url}`]
    if (a === undefined) throw new Error(`no answer for ${init.method ?? 'GET'} ${url}`)
    return new Response(JSON.stringify(a[1]), { status: a[0] })
  }))
}

const signups = () =>
  vi.mocked(fetch).mock.calls.filter(([u, init]) => u === '/api/signup' && init?.method === 'POST')

afterEach(() => {
  vi.unstubAllGlobals()
  history.replaceState({}, '', '/')
})

describe('a legal page', () => {
  it('shows the text the API rendered', async () => {
    stubAPI({ 'GET /api/legal/termos': [200, { title: 'Termos', html: '<h1>Termos de uso</h1><p>Olá</p>', version: '2026-11-20' }] })
    render(Legal, { name: 'termos' })
    expect(await screen.findByRole('heading', { name: 'Termos de uso' })).toBeTruthy()
  })

  it('says when the texts are not published', async () => {
    stubAPI({ 'GET /api/legal/privacidade': [404, { error: 'not published' }] })
    render(Legal, { name: 'privacidade' })
    expect((await screen.findByRole('alert')).textContent).toContain('not published yet')
  })
})

describe('signing up', () => {
  async function fill() {
    const { fireEvent } = await import('@testing-library/svelte')
    await fireEvent.input(screen.getByLabelText(/^Password/), { target: { value: 'correct horse battery' } })
    await fireEvent.input(screen.getByLabelText('Repeat the password'), { target: { value: 'correct horse battery' } })
    return fireEvent
  }

  it('asks to accept the published terms and sends their version', async () => {
    history.replaceState({}, '', '/signup#invite=tok')
    stubAPI({
      'GET /api/legal/termos': [200, { version: '2026-11-20' }],
      'POST /api/signup': [201, { email: 'ana@example.com' }],
    })
    render(SignUp)
    const box = await screen.findByRole('checkbox')
    expect((box as HTMLInputElement).required).toBe(true)
    const fireEvent = await fill()
    await fireEvent.click(box)
    await fireEvent.click(screen.getByRole('button', { name: 'Create account' }))
    await vi.waitFor(() => expect(signups()).toHaveLength(1))
    expect(JSON.parse(signups()[0][1]!.body as string)).toMatchObject({ token: 'tok', accepted_terms: '2026-11-20' })
  })

  it('asks nothing while the terms are not published', async () => {
    history.replaceState({}, '', '/signup#invite=tok')
    stubAPI({
      'GET /api/legal/termos': [404, { error: 'not published' }],
      'POST /api/signup': [201, { email: 'ana@example.com' }],
    })
    render(SignUp)
    await vi.waitFor(() => expect(fetch).toHaveBeenCalled())
    expect(screen.queryByRole('checkbox')).toBeNull()
    const fireEvent = await fill()
    await fireEvent.click(screen.getByRole('button', { name: 'Create account' }))
    await vi.waitFor(() => expect(signups()).toHaveLength(1))
    expect(JSON.parse(signups()[0][1]!.body as string).accepted_terms).toBe('')
  })
})
