import { useEffect, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { api } from '../api'
import type { AccountRow, UpstreamChannel } from '../types'
import { Button } from '../components/ui/button'
import { Input } from '../components/ui/input'
import { Select } from '../components/ui/select'
import {
  Sheet,
  SheetBody,
  SheetContent,
  SheetDescription,
  SheetFooter,
  SheetHeader,
  SheetTitle,
} from '../components/ui/sheet'
import Pagination from '../components/Pagination'
import {
  PELICAN_PROMPT,
  QUALITY_TEST_BUILTIN_PRESETS,
  type QualityTestBatch,
  type QualityTestPrompt,
} from '../lib/qualityTest'
import { getErrorMessage } from '../utils/error'

export function QualityTestBatchProgress({
  id,
  onChanged,
}: {
  id: string
  onChanged: () => void
}) {
  const { t } = useTranslation()
  const [batch, setBatch] = useState<QualityTestBatch>()
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  useEffect(() => {
    const controller = new AbortController()
    let timer: ReturnType<typeof setTimeout>
    const poll = async () => {
      try {
        const result = await api.getQualityTestBatch(id, controller.signal)
        if (!controller.signal.aborted) {
          setBatch(result)
          setError('')
          if (
            ['queued', 'running', 'cancelling'].some((s) => result.counts?.[s])
          )
            timer = setTimeout(poll, 1500)
        }
      } catch (err) {
        if (!controller.signal.aborted) {
          setError(getErrorMessage(err))
          timer = setTimeout(poll, 5000)
        }
      }
    }
    setBatch(undefined)
    void poll()
    return () => {
      controller.abort()
      clearTimeout(timer)
    }
  }, [id])
  const pending = ['queued', 'running', 'cancelling'].some(
    (s) => batch?.counts?.[s],
  )
  async function cancel() {
    setBusy(true)
    try {
      setBatch(await api.cancelQualityTestBatch(id))
      onChanged()
    } catch (err) {
      setError(getErrorMessage(err))
    } finally {
      setBusy(false)
    }
  }
  return (
    <section
      className="quality-test-batch-progress"
      aria-label={t('qualityTest.batch.progress')}
    >
      <strong>{t('qualityTest.batch.progress')}</strong>
      <div>
        {[
          'queued',
          'running',
          'completed',
          'error',
          'interrupted',
          'stopped',
          'cancelling',
        ].map((status) => (
          <span key={status}>
            {t(`qualityTest.status.${status}`)}{' '}
            <b>{batch?.counts?.[status] ?? 0}</b>
          </span>
        ))}
      </div>
      {pending ? (
        <Button
          size="sm"
          variant="outline"
          disabled={busy}
          onClick={() => void cancel()}
        >
          {t('qualityTest.batch.cancelRemaining')}
        </Button>
      ) : null}
      {batch?.rejected.length ? (
        <details>
          <summary>
            {t('qualityTest.batch.rejected', { count: batch.rejected.length })}
          </summary>
          {batch.rejected.map((item) => (
            <p key={item.account_id}>
              #{item.account_id}: {item.error}
            </p>
          ))}
        </details>
      ) : null}
      {error ? <p role="alert">{error}</p> : null}
    </section>
  )
}

export default function QualityTestBatchPanel({
  open,
  channels,
  presets,
  onClose,
  onCreated,
}: {
  open: boolean
  channels: UpstreamChannel[]
  presets: QualityTestPrompt[]
  onClose: () => void
  onCreated: (batch: QualityTestBatch) => void
}) {
  const { t } = useTranslation()
  const [channel, setChannel] = useState<UpstreamChannel>('codex')
  const [search, setSearch] = useState('')
  const [page, setPage] = useState(1)
  const [accounts, setAccounts] = useState<AccountRow[]>([])
  const [total, setTotal] = useState(0)
  const [selected, setSelected] = useState<Map<number, AccountRow>>(new Map())
  const [loading, setLoading] = useState(false)
  const [optionsLoading, setOptionsLoading] = useState(false)
  const [options, setOptions] = useState<{
    models: string[]
    reasoning_efforts: string[]
  }>()
  const [model, setModel] = useState('')
  const [effort, setEffort] = useState('high')
  const [prompt, setPrompt] = useState(PELICAN_PROMPT)
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  const [revision, setRevision] = useState(0)
  const submission = useRef<
    { fingerprint: string; requestID: string } | undefined
  >(undefined)
  const ids = [...selected.keys()].sort((a, b) => a - b).join(',')
  useEffect(() => {
    if (!open) return
    const controller = new AbortController()
    setLoading(true)
    setAccounts([])
    const timer = setTimeout(() => {
      void api
        .getAccountsPage(
          { channel, search, page, pageSize: 20 },
          controller.signal,
        )
        .then((result) => {
          if (!controller.signal.aborted) {
            setAccounts(result.accounts)
            setTotal(result.total)
            setError('')
          }
        })
        .catch((err) => {
          if (!controller.signal.aborted) setError(getErrorMessage(err))
        })
        .finally(() => {
          if (!controller.signal.aborted) setLoading(false)
        })
    }, 200)
    return () => {
      controller.abort()
      clearTimeout(timer)
    }
  }, [open, channel, search, page, revision])
  useEffect(() => {
    setOptions(undefined)
    if (!open || !ids) {
      setOptionsLoading(false)
      return
    }
    const controller = new AbortController()
    setOptionsLoading(true)
    void api
      .getQualityTestBatchOptions(ids.split(',').map(Number), controller.signal)
      .then((result) => {
        if (controller.signal.aborted) return
        setOptions(result)
        setModel((current) =>
          result.models.includes(current) ? current : (result.models[0] ?? ''),
        )
        setEffort((current) =>
          result.reasoning_efforts.includes(current)
            ? current
            : (result.reasoning_efforts[0] ?? ''),
        )
      })
      .catch((err) => {
        if (!controller.signal.aborted) setError(getErrorMessage(err))
      })
      .finally(() => {
        if (!controller.signal.aborted) setOptionsLoading(false)
      })
    return () => controller.abort()
  }, [ids, open, revision])
  const custom = presets.find((item) => item.prompt === prompt)
  const builtin = !custom
    ? QUALITY_TEST_BUILTIN_PRESETS.find((item) => item.prompt === prompt)
    : undefined
  const valid =
    selected.size > 0 &&
    selected.size <= 1000 &&
    !optionsLoading &&
    options?.models.includes(model) &&
    options.reasoning_efforts.includes(effort) &&
    prompt.trim() &&
    new TextEncoder().encode(prompt).length <= 16000
  async function submit() {
    if (!valid || busy) return
    setBusy(true)
    setError('')
    const body = {
      account_ids: [...selected.keys()].sort((a, b) => a - b),
      channel,
      model,
      reasoning_effort: effort,
      prompt,
      prompt_id: custom?.id,
      preset_key: builtin?.key,
      preset_name: builtin
        ? t(`qualityTest.presets.builtins.${builtin.key}`)
        : undefined,
    }
    const fingerprint = JSON.stringify(body)
    if (submission.current?.fingerprint !== fingerprint)
      submission.current = {
        fingerprint,
        requestID: crypto.getRandomValues(new Uint32Array(4)).join('-'),
      }
    try {
      const batch = await api.createQualityTestBatch({
        ...body,
        request_id: submission.current.requestID,
      })
      onCreated(batch)
      submission.current = undefined
      onClose()
    } catch (err) {
      setError(getErrorMessage(err))
    } finally {
      setBusy(false)
    }
  }
  return (
    <Sheet
      open={open}
      onOpenChange={(value) => {
        if (!value && !busy) onClose()
      }}
    >
      <SheetContent
        className="quality-test-batch-panel"
        showCloseButton={!busy}
        onEscapeKeyDown={(event) => {
          if (busy) event.preventDefault()
        }}
        onPointerDownOutside={(event) => {
          if (busy) event.preventDefault()
        }}
      >
        <SheetHeader>
          <SheetTitle>{t('qualityTest.batch.title')}</SheetTitle>
          <SheetDescription>{t('qualityTest.batch.hint')}</SheetDescription>
        </SheetHeader>
        <SheetBody>
        <fieldset disabled={busy} className="quality-test-batch-fields">
          <label>
            {t('qualityTest.channel')}
            <Select
              value={channel}
              onValueChange={(value) => {
                setChannel(value as UpstreamChannel)
                setSelected(new Map())
                setPage(1)
                setSearch('')
              }}
              options={channels.map((value) => ({ value, label: value }))}
            />
          </label>
          <Input
            aria-label={t('qualityTest.batch.search')}
            placeholder={t('qualityTest.batch.search')}
            value={search}
            onChange={(event) => {
              setSearch(event.target.value)
              setPage(1)
            }}
          />
          <div className="quality-test-batch-selection">
            <Button
              type="button"
              size="sm"
              variant="outline"
              disabled={loading}
              onClick={() =>
                setSelected((previous) => {
                  const next = new Map(previous)
                  accounts.forEach((row) => next.set(row.id, row))
                  return next
                })
              }
            >
              {t('qualityTest.batch.selectPage')}
            </Button>
            <Button
              type="button"
              size="sm"
              variant="ghost"
              onClick={() => setSelected(new Map())}
            >
              {t('qualityTest.batch.clear')}
            </Button>
            <span>
              {t('qualityTest.batch.selected', { count: selected.size })}
            </span>
          </div>
          <div className="quality-test-batch-accounts" aria-busy={loading}>
            {loading ? (
              <p>{t('qualityTest.loadingAccounts')}</p>
            ) : accounts.length === 0 ? (
              <p>{t('qualityTest.filters.noMatch')}</p>
            ) : (
              accounts.map((row) => (
                <label key={row.id}>
                  <input
                    type="checkbox"
                    checked={selected.has(row.id)}
                    onChange={(event) =>
                      setSelected((previous) => {
                        const next = new Map(previous)
                        if (event.target.checked) next.set(row.id, row)
                        else next.delete(row.id)
                        return next
                      })
                    }
                  />
                  <span>
                    {row.name || row.email || '#' + row.id}
                    <small>
                      #{row.id} · {row.plan_type || '—'}
                    </small>
                  </span>
                </label>
              ))
            )}
          </div>
          <Pagination
            page={page}
            totalPages={Math.ceil(total / 20)}
            totalItems={total}
            pageSize={20}
            onPageChange={setPage}
          />
          <label>
            {t('qualityTest.model')}
            <Select
              value={model}
              disabled={!options || optionsLoading}
              onValueChange={setModel}
              options={(options?.models ?? []).map((value) => ({
                value,
                label: value,
              }))}
            />
          </label>
          <label>
            {t('qualityTest.effort')}
            <Select
              value={effort}
              disabled={!options || optionsLoading}
              onValueChange={setEffort}
              options={(options?.reasoning_efforts ?? []).map((value) => ({
                value,
                label: value || t('qualityTest.efforts.default'),
              }))}
            />
          </label>
          {selected.size > 0 &&
          options &&
          (!options.models.length || !options.reasoning_efforts.length) ? (
            <p role="alert">{t('qualityTest.batch.noCommon')}</p>
          ) : null}
          <label>
            {t('qualityTest.filters.preset')}
            <Select
              value={
                custom
                  ? 'custom:' + custom.id
                  : builtin
                    ? 'builtin:' + builtin.key
                    : 'manual'
              }
              onValueChange={(value) => {
                const next = value.startsWith('builtin:')
                  ? QUALITY_TEST_BUILTIN_PRESETS.find(
                      (item) => item.key === value.slice(8),
                    )
                  : presets.find((item) => String(item.id) === value.slice(7))
                if (next) setPrompt(next.prompt)
              }}
              options={[
                ...QUALITY_TEST_BUILTIN_PRESETS.map((item) => ({
                  value: 'builtin:' + item.key,
                  label: t(`qualityTest.presets.builtins.${item.key}`),
                })),
                ...presets.map((item) => ({
                  value: 'custom:' + item.id,
                  label: item.name,
                })),
                {
                  value: 'manual',
                  label: t('qualityTest.presets.handwritten'),
                },
              ]}
            />
          </label>
          <label>
            {t('qualityTest.prompt')}
            <textarea
              rows={5}
              value={prompt}
              onChange={(event) => setPrompt(event.target.value)}
            />
          </label>
          <p className="quality-test-batch-summary">
            {t('qualityTest.batch.summary', {
              count: selected.size,
              channel,
              model: model || '—',
              effort: effort || t('qualityTest.efforts.default'),
            })}
          </p>
          {error ? (
            <div role="alert" className="quality-test-error">
              {error}
              <Button
                variant="outline"
                size="sm"
                onClick={() => setRevision((x) => x + 1)}
              >
                {t('common.retry')}
              </Button>
            </div>
          ) : null}
        </fieldset>
        </SheetBody>
        <SheetFooter>
          <Button disabled={!valid || busy} onClick={() => void submit()}>
            {t(busy ? 'qualityTest.submitting' : 'qualityTest.batch.submit')}
          </Button>
        </SheetFooter>
      </SheetContent>
    </Sheet>
  )
}
