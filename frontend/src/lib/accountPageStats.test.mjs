import assert from 'node:assert/strict'
import test from 'node:test'
import { mergeAccountPageStats } from './accountPageStats.ts'

test('fresh costs and usage replace populated row snapshots, including zero', () => {
  const oldUsage = { requests: 1, tokens: 10, account_billed: 7.11, user_billed: 8 }
  const newUsage = { requests: 0, tokens: 0, account_billed: 0, user_billed: 0 }
  const account = {
    id: 1, active_requests: 2, billed_5h: 7.11, billed_7d: 7.11,
    usage_5h_detail: oldUsage, usage_7d_detail: oldUsage, usage_today_detail: oldUsage,
  }
  const stats = {
    billed_5h: 0, billed_7d: 9.42,
    usage_5h_detail: newUsage, usage_7d_detail: newUsage, usage_today_detail: newUsage,
  }
  assert.deepEqual(mergeAccountPageStats(account, stats), { ...account, ...stats })
  assert.equal(account.billed_7d, 7.11, 'do not mutate the stored list/detail row')
  assert.equal(mergeAccountPageStats(account), account)
  assert.deepEqual(mergeAccountPageStats(account, {}), account, 'missing fields are not zeroes')
})

test('gateway polling preserves official settlement semantics', () => {
  const account = { official_usd: 3, official_usd_7d: 3 }
  assert.deepEqual(mergeAccountPageStats(account, { billed_7d: 7.11 }), {
    ...account, billed_7d: 7.11,
  })
  assert.deepEqual(mergeAccountPageStats(account, { official_usd: 0, official_usd_7d: 9 }), {
    official_usd: 0, official_usd_7d: 0,
  })
  assert.deepEqual(mergeAccountPageStats(account, { official_usd_7d: 2 }), {
    official_usd: 2, official_usd_7d: 2,
  })
  assert.deepEqual(mergeAccountPageStats(account, { official_usage_synced: true }), {
    official_usd: undefined, official_usd_7d: undefined, official_usage_synced: true,
  })
})
