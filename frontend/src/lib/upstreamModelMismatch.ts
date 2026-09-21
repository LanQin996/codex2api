// 与后端 proxy/upstream_model_mismatch.go 的 upstreamModelMatches 保持同一口径：
// 别名解析成带日期的官方快照（gpt-5 → gpt-5-2025-08-07）属于正常行为，不算不一致。

const SNAPSHOT_SUFFIX = /^[-@_](\d{4}-\d{2}-\d{2}|\d{8}|\d{4})$/
const COMPACT_SUFFIX = '-openai-compact'
const LATEST_SUFFIX = '-latest'

function normalizeModel(model?: string | null): string {
  let value = (model ?? '').trim().toLowerCase()
  const slash = value.lastIndexOf('/')
  if (slash >= 0) value = value.slice(slash + 1)
  const bracket = value.indexOf('[')
  if (bracket > 0) value = value.slice(0, bracket)
  if (value.endsWith(COMPACT_SUFFIX)) value = value.slice(0, -COMPACT_SUFFIX.length)
  if (value.endsWith(LATEST_SUFFIX)) value = value.slice(0, -LATEST_SUFFIX.length)
  return value
}

export function isUpstreamModelMismatch(sentModel?: string | null, upstreamModel?: string | null): boolean {
  const sent = normalizeModel(sentModel)
  const upstream = normalizeModel(upstreamModel)
  if (!sent || !upstream || sent === upstream) return false
  const [short, long] = sent.length <= upstream.length ? [sent, upstream] : [upstream, sent]
  return !(long.startsWith(short) && SNAPSHOT_SUFFIX.test(long.slice(short.length)))
}
