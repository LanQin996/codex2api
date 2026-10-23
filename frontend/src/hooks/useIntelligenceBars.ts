import { useEffect, useState } from 'react'
import { api } from '../api'
import type { IntelligenceBar } from '../lib/intelligenceCheck'

const refreshEvent = 'intelligence-bars-refresh'
export function refreshIntelligenceBars() {
  window.dispatchEvent(new Event(refreshEvent))
}

// Poll only the visible account page. One batch serves table, cards and details.
export function useIntelligenceBars(idsKey: string, reloadToken: number) {
  const [bars, setBars] = useState<Record<string, IntelligenceBar>>({})
  const [error, setError] = useState(false)
  useEffect(() => { setBars({}); setError(false) }, [idsKey])
  useEffect(() => {
    if (!idsKey) return
    const ids = idsKey.split(',').map(Number)
    let disposed = false
    let controller: AbortController | undefined
    let timer: ReturnType<typeof setTimeout> | undefined
    const load = async () => {
      clearTimeout(timer)
      controller?.abort()
      const request = new AbortController()
      controller = request
      if (document.hidden) return
      try {
        const result = await api.getIntelligenceBars(ids, request.signal)
        if (disposed || request.signal.aborted) return
        setBars(result.bars ?? {})
        setError(false)
      } catch {
        if (!disposed && !request.signal.aborted) setError(true)
      } finally {
        if (!disposed && !request.signal.aborted) timer = setTimeout(() => void load(), 4000)
      }
    }
    const refresh = () => void load()
    refresh()
    window.addEventListener(refreshEvent, refresh)
    document.addEventListener('visibilitychange', refresh)
    return () => {
      disposed = true
      controller?.abort()
      clearTimeout(timer)
      window.removeEventListener(refreshEvent, refresh)
      document.removeEventListener('visibilitychange', refresh)
    }
  }, [idsKey, reloadToken])
  return { bars, error }
}
