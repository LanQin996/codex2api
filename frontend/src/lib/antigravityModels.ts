import type { AntigravityQuotaSnapshot } from '../types'

// Fallback only: account-synchronized model discovery takes precedence.
// These are public gateway IDs, never Cloud Code's private backing IDs.
// Claude is omitted: available Claude IDs differ per account and only the
// synchronized catalog knows them.
export const ANTIGRAVITY_DEFAULT_MODELS = [
  'gemini-3.8-flash-low', 'gemini-3.8-flash-medium', 'gemini-3.8-flash-high',
  'gemini-3.7-flash-low', 'gemini-3.7-flash-medium', 'gemini-3.7-flash-high',
  'gemini-3.6-flash-low', 'gemini-3.6-flash-medium', 'gemini-3.6-flash-high',
  'gemini-3.1-pro-low', 'gemini-3.1-pro-high',
  'gpt-oss-120b-medium',
]

// antigravityModelVersion 取 gemini-<major>.<minor>-… 里的数字版本;解析不出返回 0。
export function antigravityModelVersion(model: string): number {
  const match = /^gemini-(\d+(?:\.\d+)?)/.exec(model.trim().toLowerCase())
  return match ? Number.parseFloat(match[1]) : 0
}

// Google publishes disabled placeholders as model IDs. Derive their state from
// the current ID so a later catalog entry without this segment recovers normally.
export function isAntigravityDisabledModel(model: string): boolean {
  return /(?:^|[-_])disabled(?:[-_]|$)/i.test(model.trim())
}

// A projected catalog is the current admin model list, including models with
// unknown quota. Older snapshots lack it: only recover disabled placeholders
// from account.models so historical ordinary models do not reappear.
export function antigravityDisplayCatalogModels(
  models: readonly string[],
  quota?: AntigravityQuotaSnapshot | null,
): string[] {
  const catalog = Array.isArray(quota?.catalog_model_ids)
    ? quota.catalog_model_ids
    : models.filter(isAntigravityDisabledModel)
  return [...new Set(catalog.map(model => model.trim()).filter(Boolean))]
}

// A catalog refresh must also clear a previously selected disabled model.
export function resolveAntigravityTestModelSelection(models: readonly string[], current = ''): string {
  const available = models.filter(model => !isAntigravityDisabledModel(model))
  return available.includes(current) ? current : available[0] ?? ''
}

// 测连候选跟随最新已投影目录;有目录但没有额度的模型也允许测试。
// 旧快照没有 catalog_model_ids 时保留额度列表优先的规则,只从账号目录补
// disabled 占位供额度展示,不恢复历史普通模型。没有额度快照时才使用完整账号目录。
// 测连下拉跳过禁用占位和生图模型。首选依次为系统设置里该渠道的
// 测试模型、版本最新的 flash 低档——目录会长期残留已下线旧版(gemini-3.5-flash 上游
// 只回一句下线提示就断流),按目录顺序取首个会必失败;与后端自动选模规则一致。
export function orderAntigravityTestModels(
  models: readonly string[],
  preferred = '',
  quota?: AntigravityQuotaSnapshot,
): string[] {
  const quotaModels = quota?.models
  const source = quotaModels == null ? models : Array.isArray(quotaModels)
    ? quotaModels.map((info) => {
      const record = info as unknown as Record<string, unknown>
      return ['model', 'model_id', 'modelId', 'name', 'id']
        .map((key) => record[key])
        .find((value): value is string => typeof value === 'string' && value.trim() !== '') ?? ''
    })
    : Object.keys(quotaModels)
  const catalog = antigravityDisplayCatalogModels(models, quota)
  const currentCatalog = new Set(catalog.map(model => model.toLowerCase()))
  const authoritativeCatalog = Array.isArray(quota?.catalog_model_ids)
  const candidates = [...source, ...catalog]
    .map((m) => m.trim())
    .filter((m) => m && !m.toLowerCase().includes('image') && !isAntigravityDisabledModel(m))
    .filter((m) => !authoritativeCatalog || currentCatalog.has(m.toLowerCase()))
  const available = [...new Set(candidates)]
  const configured = available.find((m) => m.toLowerCase() === preferred.trim().toLowerCase())
  let first = configured
  if (!first) {
    for (const m of available) {
      const lower = m.toLowerCase()
      if (!lower.includes('flash') || !lower.endsWith('-low')) continue
      if (!first || antigravityModelVersion(m) > antigravityModelVersion(first)) first = m
    }
  }
  if (!first) return available
  return [first, ...available.filter((m) => m !== first)]
}
