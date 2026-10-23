import { useEffect, useMemo } from 'react'
import { api } from '../api'
import type { AccountLiveStateResponse } from '../types'
import { mergeAccountLiveState } from '../lib/accountLiveState'

export { mergeAccountLiveState }

export function useAccountLiveState(
  ids: number[],
  apply: (response: AccountLiveStateResponse) => void,
  enabled = true,
) {
  const idsKey = useMemo(() => ids.join(','), [ids])

  useEffect(() => {
    if (!enabled || !idsKey) return undefined
    let stopped = false
    let timer: number | undefined
    let controller: AbortController | undefined

    const poll = async () => {
      if (stopped) return
      if (document.hidden) {
        timer = window.setTimeout(poll, 1000)
        return
      }
      controller?.abort()
      const current = new AbortController()
      controller = current
      // A hung fetch must not stop the poll forever; abort it and let the next
      // tick retry. The list reload remains the fallback for stale data.
      const timeout = window.setTimeout(() => current.abort(), 5000)
      try {
        const response = await api.getAccountLiveState(
          idsKey.split(',').map(Number),
          current.signal,
        )
        if (!stopped && !current.signal.aborted) apply(response)
      } catch {
        // The next tick retries; live counters must never disrupt the page.
      } finally {
        window.clearTimeout(timeout)
      }
      if (!stopped) timer = window.setTimeout(poll, 1000)
    }

    void poll()
    return () => {
      stopped = true
      controller?.abort()
      if (timer !== undefined) window.clearTimeout(timer)
    }
  }, [apply, enabled, idsKey])
}
