import { useEffect, useMemo, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { api } from '../api'
import { Button } from '../components/ui/button'
import {
  clampQualityTestFrameHeight,
  extractQualityTestHTML,
  isQualityTestActive,
  qualityTestPreviewDocument,
  type QualityTestJob,
} from '../lib/qualityTest'
import { formatBeijingTime } from '../utils/time'
import { getErrorMessage } from '../utils/error'

// Bound both payload retention and parallel detail requests. Lists never carry output.
const cache = new Map<number, QualityTestJob>()
let inFlight = 0
const waiters: (() => void)[] = []
async function loadDetail(id: number, signal: AbortSignal) {
  if (cache.has(id)) return cache.get(id)!
  await new Promise<void>((resolve) => {
    const next = () => {
      inFlight++
      resolve()
    }
    if (inFlight < 3) next()
    else waiters.push(next)
  })
  try {
    signal.throwIfAborted()
    if (cache.has(id)) return cache.get(id)!
    const { job } = await api.getQualityTest(id, signal)
    if (!isQualityTestActive(job)) {
      cache.set(id, job)
      while (cache.size > 24) cache.delete(cache.keys().next().value!)
    }
    return job
  } finally {
    inFlight--
    waiters.shift()?.()
  }
}

export default function QualityTestGallery({
  jobs,
  onOpen,
  onHistory,
  onChanged,
}: {
  jobs: QualityTestJob[]
  onOpen: (id: number) => void
  onHistory: (id: number) => void
  onChanged: () => void
}) {
  return (
    <div className="quality-test-gallery">
      {jobs.map((job) => (
        <ResultCard
          key={job.id}
          job={job}
          onOpen={onOpen}
          onHistory={onHistory}
          onChanged={onChanged}
        />
      ))}
    </div>
  )
}
function ResultCard({
  job,
  onOpen,
  onHistory,
  onChanged,
}: {
  job: QualityTestJob
  onOpen: (id: number) => void
  onHistory: (id: number) => void
  onChanged: () => void
}) {
  const { t } = useTranslation()
  const container = useRef<HTMLElement>(null)
  const [visible, setVisible] = useState(false)
  const [detail, setDetail] = useState<QualityTestJob>()
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  const [retry, setRetry] = useState(0)
  const active = isQualityTestActive(job)
  useEffect(() => {
    const observer = new IntersectionObserver(([entry]) =>
      setVisible(entry.isIntersecting),
    )
    if (container.current) observer.observe(container.current)
    return () => observer.disconnect()
  }, [])
  useEffect(() => {
    if (!visible || active) {
      setDetail(undefined)
      return
    }
    const controller = new AbortController()
    setError('')
    void loadDetail(job.id, controller.signal)
      .then((result) => {
        if (!controller.signal.aborted) setDetail(result)
      })
      .catch((err) => {
        if (!controller.signal.aborted) setError(getErrorMessage(err))
      })
    return () => controller.abort()
  }, [job.id, job.status, visible, active, retry])
  const preview = useMemo(() => {
    const html = extractQualityTestHTML(detail?.output ?? '')
    return html ? qualityTestPreviewDocument(html) : ''
  }, [detail])
  async function act(cancel: boolean) {
    setBusy(true)
    setError('')
    try {
      if (cancel) await api.cancelQualityTest(job.id)
      else await api.retryQualityTest(job.id)
      onChanged()
    } catch (err) {
      setError(getErrorMessage(err))
    } finally {
      setBusy(false)
    }
  }
  return (
    <article ref={container} className="quality-test-result-card">
      <header>
        <strong title={job.account_name}>{job.account_name}</strong>
        <span>
          #{job.account_id} · {job.plan_type || '—'}
        </span>
      </header>
      <div className="quality-test-card-config">
        <span>{job.model}</span>
        <span>{job.reasoning_effort || t('qualityTest.efforts.default')}</span>
        <span>
          {job.preset_kind === 'builtin'
            ? t(`qualityTest.presets.builtins.${job.preset_ref}`, {
                defaultValue: job.preset_name,
              })
            : job.preset_name || t('qualityTest.presets.handwritten')}
        </span>
      </div>
      <div className="quality-test-card-preview">
        {visible && !active && preview ? (
          <ThumbnailPreview
            preview={preview}
            title={t('qualityTest.gallery.preview', {
              account: job.account_name,
            })}
          />
        ) : (
          <span>
            {active
              ? t(`qualityTest.status.${job.status}`)
              : error
                ? t('qualityTest.gallery.loadFailed')
                : !detail
                  ? t('qualityTest.loadingRecords')
                  : t('qualityTest.noHTML')}
          </span>
        )}
      </div>
      <div className="quality-test-card-status">
        <span
          className={`quality-test-status quality-test-status-pill ${job.status}`}
        >
          {t(`qualityTest.status.${job.status}`)}
        </span>
        <span>{(job.duration_ms / 1000).toFixed(1)} s</span>
      </div>
      <time>{formatBeijingTime(job.created_at)}</time>
      {job.error ? (
        <p className="quality-test-card-error" title={job.error}>
          {job.error}
        </p>
      ) : null}
      {error ? (
        <div role="alert" className="quality-test-card-error">
          {error}
          <Button
            size="sm"
            variant="ghost"
            onClick={() => setRetry((x) => x + 1)}
          >
            {t('common.retry')}
          </Button>
        </div>
      ) : null}
      <footer>
        <Button size="sm" variant="outline" onClick={() => onOpen(job.id)}>
          {t('qualityTest.viewResult')}
        </Button>
        <Button
          size="sm"
          variant="ghost"
          onClick={() => onHistory(job.account_id)}
        >
          {t('qualityTest.recordsTab')}
        </Button>
        <Button
          size="sm"
          disabled={busy || job.status === 'cancelling'}
          onClick={() => void act(active)}
        >
          {t(
            active
              ? 'qualityTest.gallery.cancel'
              : 'qualityTest.gallery.retest',
          )}
        </Button>
      </footer>
    </article>
  )
}

// Use a stable logical viewport and scale the whole scene, not a cropped mobile slice.
function ThumbnailPreview({
  preview,
  title,
}: {
  preview: string
  title: string
}) {
  const frame = useRef<HTMLIFrameElement>(null)
  const [width, setWidth] = useState(264)
  const [height, setHeight] = useState(480)
  useEffect(() => {
    setHeight(480)
    const host = frame.current?.parentElement
    if (!host) return
    const observer = new ResizeObserver(() => setWidth(host.clientWidth))
    observer.observe(host)
    const receive = (event: MessageEvent) => {
      if (
        event.source !== frame.current?.contentWindow ||
        event.data?.type !== 'quality-test-preview-size'
      )
        return
      const measured = clampQualityTestFrameHeight(event.data.height)
      if (measured !== undefined) setHeight(measured)
    }
    window.addEventListener('message', receive)
    return () => {
      observer.disconnect()
      window.removeEventListener('message', receive)
    }
  }, [preview])
  return (
    <iframe
      ref={frame}
      title={title}
      src="/api/quality-test/preview"
      sandbox="allow-scripts"
      referrerPolicy="no-referrer"
      style={{
        width: 640,
        height,
        flexShrink: 0,
        transform: 'scale(' + Math.min(width / 640, 260 / height) + ')',
      }}
      onLoad={(event) =>
        event.currentTarget.contentWindow?.postMessage(
          { type: 'quality-test-preview', html: preview },
          '*',
        )
      }
    />
  )
}
