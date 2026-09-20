import type { AccountPageStatsItem } from '../types'

// Page stats are refreshed independently of the list/detail snapshot. A value
// already present on the row must not mask a newer cost (including a reset to 0).
export function mergeAccountPageStats<T extends AccountPageStatsItem>(
  account: T,
  stats?: AccountPageStatsItem,
): T {
  if (!stats) return account
  const merged = { ...account }
  if (stats.billed_5h != null) merged.billed_5h = stats.billed_5h
  if (stats.billed_7d != null) merged.billed_7d = stats.billed_7d
  if (stats.usage_5h_detail) merged.usage_5h_detail = stats.usage_5h_detail
  if (stats.usage_7d_detail) merged.usage_7d_detail = stats.usage_7d_detail
  if (stats.usage_today_detail) merged.usage_today_detail = stats.usage_today_detail

  const officialUsd = stats.official_usd ?? stats.official_usd_7d
  if (officialUsd != null) {
    merged.official_usd = officialUsd
    merged.official_usd_7d = officialUsd
  } else if (stats.official_usage_synced) {
    merged.official_usd = undefined
    merged.official_usd_7d = undefined
  }
  if (stats.official_usage_synced != null) {
    merged.official_usage_synced = stats.official_usage_synced
  }
  return merged
}
