import { useEffect, useState } from 'react'
import { api } from '../api'
import { isQualityTestActive, type QualityTestJob, type QualityTestJobsFilter, type QualityTestJobsResponse } from '../lib/qualityTest'
import { getErrorMessage } from '../utils/error'

export function useQualityTestJobs(page: number, revision: number, filter: QualityTestJobsFilter = {}) {
  const { plan, model, effort, account_id, preset, latest, channel } = filter
  const [data, setData] = useState<QualityTestJobsResponse>({ jobs: [], active_jobs: [], total: 0, concurrency_limit: 3 })
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(true)
  useEffect(() => {
    const controller = new AbortController()
    let timer: ReturnType<typeof setTimeout>
    setLoading(true)
    const poll = async () => {
      let delay = 3000
      try {
        const result = await api.getQualityTests(page, { plan, model, effort, account_id, preset, latest, channel }, controller.signal)
        if (controller.signal.aborted) return
        setData(result); setError('')
        delay = result.active_jobs.length > 0 ? 1500 : 5000
      } catch (err) { if (!controller.signal.aborted) setError(getErrorMessage(err)) }
      finally {
        if (!controller.signal.aborted) { setLoading(false); timer = setTimeout(poll, delay) }
      }
    }
    void poll()
    // Only polling is cancelled; the server owns each task's lifetime.
    return () => { controller.abort(); clearTimeout(timer) }
  }, [page, revision, plan, model, effort, account_id, preset, latest, channel])
  return { ...data, error, loading }
}

export function useQualityTestDetail(id: number | undefined, revision: number) {
  const [job, setJob] = useState<QualityTestJob | null>(null)
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(false)
  useEffect(() => {
    const controller = new AbortController()
    let timer: ReturnType<typeof setTimeout>
    setError(''); setLoading(Boolean(id))
    if (!id) { setJob(null); return () => controller.abort() }
    const poll = async () => {
      let again = true
      try {
        const result = await api.getQualityTest(id, controller.signal)
        if (controller.signal.aborted) return
        setJob(result.job); setError('')
        again = isQualityTestActive(result.job)
      } catch (err) { if (!controller.signal.aborted) setError(getErrorMessage(err)) }
      finally {
        if (!controller.signal.aborted) { setLoading(false); if (again) timer = setTimeout(poll, 1000) }
      }
    }
    void poll()
    return () => { controller.abort(); clearTimeout(timer) }
  }, [id, revision])
  return { job: job?.id === id ? job : null, error, loading }
}
