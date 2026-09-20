import assert from 'node:assert/strict'
import { after, before, test } from 'node:test'
import { fileURLToPath } from 'node:url'
import { chromium } from 'playwright'
import { createServer } from 'vite'

let server, browser, baseURL
before(async () => {
  process.env.VITE_APP_VERSION ||= 'test'
  server = await createServer({
    root: fileURLToPath(new URL('../', import.meta.url)),
    server: { host: '127.0.0.1', port: 0 },
  })
  await server.listen()
  baseURL = `http://127.0.0.1:${server.httpServer.address().port}`
  browser = await chromium.launch({ headless: true })
})
after(async () => {
  await browser?.close()
  await server?.close()
})

async function openAccounts({ detailedRow = false } = {}) {
  const page = await browser.newPage({ viewport: { width: 1440, height: 1000 } })
  const errors = []
  page.on('pageerror', error => errors.push(error.message))
  const state = { billed: 7.11, requests: 1, polls: 0, lists: 0, fail: false, hold: null, ids: [] }
  const account = {
    id: 1, name: 'Billing fixture', email: 'billing@example.test',
    plan_type: 'plus', status: 'active', enabled: true, locked: false,
    usage_percent_5h: 10, usage_percent_7d: 20,
    reset_5h_at: '2026-09-20T14:00:00Z', reset_7d_at: '2026-09-25T12:00:00Z',
    created_at: '2026-09-01T12:00:00Z', updated_at: '2026-09-20T12:00:00Z',
    tags: [], groups: [], success_count: 1, error_count: 0,
    ...(detailedRow ? { billed_5h: 7.11, billed_7d: 7.11 } : {}),
  }
  await page.route('**/api/**', async route => {
    const url = new URL(route.request().url())
    const pathname = url.pathname.replace('/api/admin', '')
    const json = data => route.fulfill({ json: data })
    if (pathname === '/bootstrap-status') return json({ needs_bootstrap: false, source: 'env' })
    if (pathname === '/health') return json({ status: 'ok' })
    if (pathname === '/settings/visible-channels') return json({ channels: ['codex', 'grok'] })
    if (pathname === '/accounts') {
      state.lists++
      return json({
        accounts: url.searchParams.get('channel') === 'grok' ? [] : [account],
        page: 1, page_size: 20, total: 1, summary: { total: 1, normal: 1, active: 1 },
        facets: { tags: [], email_domains: [] }, snapshot_at: account.updated_at,
        stats_state: 'ready', disabled_sorts: [],
      })
    }
    if (pathname === '/accounts/page-stats') {
      state.polls++
      state.ids.push(url.searchParams.get('ids'))
      const billed = state.billed
      const usage = { requests: state.requests, tokens: 100, account_billed: billed, user_billed: billed }
      if (state.hold) await state.hold
      if (state.fail) return route.fulfill({ status: 500, json: { error: 'fixture unavailable' } })
      return json({ stats: { 1: {
        billed_5h: billed, billed_7d: billed, official_usage_synced: true,
        usage_5h_detail: usage, usage_7d_detail: usage, usage_today_detail: usage,
      } } })
    }
    if (pathname === '/accounts/1') return json({ ...account, detail_loaded: true, billed_5h: 7.11, billed_7d: 7.11 })
    if (pathname === '/accounts/live') return json({ accounts: {}, session_slot_buffer_enabled: false })
    if (pathname === '/accounts/health-bars') return json({ buckets: {} })
    return json({})
  })
  await page.addInitScript(() => {
    localStorage.setItem('lang', 'en')
    localStorage.setItem('admin_key', 'fixture-key')
    localStorage.setItem('codex2api:first_setup_review_done_v1', '1')
    localStorage.setItem('codex2api:accounts:analysis-visible', 'false')
    localStorage.setItem('codex2api:accounts:page-mode', 'personal')
  })
  await page.clock.install({ time: new Date('2026-09-20T12:00:00Z') })
  await page.goto(baseURL + '/admin/accounts')
  await page.locator('.account-billed-window__value').getByText('$7.11', { exact: true }).first().waitFor()
  await page.clock.pauseAt(new Date('2026-09-20T12:01:00Z'))
  // Let any poll fired by pauseAt finish before changing the fixture.
  await page.waitForTimeout(100)
  return { page, state, errors }
}

async function expectCost(page, text) {
  await page.waitForFunction(
    value => [...document.querySelectorAll('.account-billed-window__value')].some(el => el.textContent === value),
    text,
    { polling: 50, timeout: 3000 },
  )
}

test('visible page refreshes costs without reloading the account list, including old detailed rows', async () => {
  const { page, state, errors } = await openAccounts({ detailedRow: true })
  try {
    const lists = state.lists
    state.billed = 9.42
    await page.clock.fastForward(10000)
    await expectCost(page, '$9.42')
    state.billed = 0
    await page.clock.fastForward(10000)
    await expectCost(page, '$0.00')
    assert.equal(state.lists, lists, 'cost refresh must not scan/reload the whole account pool')
    assert.ok(state.ids.every(ids => ids === '1'), 'only query visible account IDs')
    assert.deepEqual(errors, [])
  } finally {
    await page.close()
  }
})

test('manual list refresh also refreshes costs when IDs and snapshot time are unchanged', async () => {
  const { page, state } = await openAccounts()
  try {
    state.billed = 12.34
    await page.getByRole('button', { name: 'Refresh', exact: true }).first().click()
    await expectCost(page, '$12.34')
  } finally {
    await page.close()
  }
})

test('hidden pages pause cost polling and refresh immediately on return; failures retain the last value', async () => {
  const { page, state } = await openAccounts()
  try {
    await page.evaluate(() => {
      Object.defineProperty(document, 'hidden', { configurable: true, value: true })
      document.dispatchEvent(new Event('visibilitychange'))
    })
    const polls = state.polls
    state.billed = 15.67
    await page.clock.fastForward(30000)
    await page.waitForTimeout(100)
    assert.equal(state.polls, polls)
    await page.evaluate(() => {
      Object.defineProperty(document, 'hidden', { configurable: true, value: false })
      document.dispatchEvent(new Event('visibilitychange'))
    })
    await expectCost(page, '$15.67')
    state.fail = true
    await page.clock.fastForward(10000)
    await page.waitForTimeout(100)
    await expectCost(page, '$15.67')
    state.fail = false
    state.billed = 18.90
    await page.clock.fastForward(10000)
    await expectCost(page, '$18.90')
  } finally {
    await page.close()
  }
})

test('open account details follow refreshed page stats rather than the old detail snapshot', async () => {
  const { page, state, errors } = await openAccounts()
  try {
    await page.locator('.codex-account-card__name').click()
    // The quick-config sheet also opens above the detail sheet; inspect the
    // underlying detail DOM even while Radix marks it aria-hidden.
    await page.locator('[role="dialog"]').getByText('7d: $7.11', { exact: true }).waitFor()
    state.billed = 21.23
    await page.clock.fastForward(10000)
    await page.locator('[role="dialog"]').getByText('7d: $21.23', { exact: true }).waitFor()
    assert.deepEqual(errors, [])
  } finally {
    await page.close()
  }
})

test('slow polls never overlap and are ignored after switching provider', async () => {
  const { page, state, errors } = await openAccounts()
  let release
  try {
    state.hold = new Promise(resolve => { release = resolve })
    const pending = page.waitForRequest(request => request.url().includes('/accounts/page-stats'))
    await page.clock.fastForward(10000)
    await pending
    await page.waitForTimeout(100)
    const polls = state.polls
    await page.clock.fastForward(30000)
    await page.waitForTimeout(100)
    assert.equal(state.polls, polls, 'a slow statistics query must not overlap with another timer tick')
    await page.getByRole('button', { name: 'Grok', exact: true }).click()
    await page.waitForURL('**/admin/accounts/grok')
    state.hold = null
    release()
    await page.clock.fastForward(30000)
    await page.waitForTimeout(100)
    assert.ok(state.ids.slice(polls).every(ids => ids !== '1'), 'Codex statistics polling stops while viewing another provider')
    assert.deepEqual(errors, [])
  } finally {
    release?.()
    await page.close()
  }
})
