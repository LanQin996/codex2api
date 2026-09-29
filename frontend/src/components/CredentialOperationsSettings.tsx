import { useEffect, useState } from 'react'
import { api } from '../api'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { getErrorMessage } from '../utils/error'

export default function CredentialOperationsSettings() {
  const [value, setValue] = useState('')
  const [loaded, setLoaded] = useState(false)
  const [saving, setSaving] = useState(false)
  const [message, setMessage] = useState('')
  const [failed, setFailed] = useState(false)
  const [reload, setReload] = useState(0)
  useEffect(() => {
    let live = true
    setLoaded(false)
    setMessage('')
    api.getCredentialOperationsSettings().then(s => {
      if (live) { setValue(String(s.concurrency)); setLoaded(true); setFailed(false) }
    }).catch(error => {
      if (live) { setMessage(getErrorMessage(error)); setFailed(true) }
    })
    return () => { live = false }
  }, [reload])
  const n = Number(value)
  const valid = value.trim() !== '' && Number.isInteger(n) && n >= 1 && n <= 8
  const save = async () => {
    if (!loaded || !valid || saving) return
    setSaving(true); setMessage('')
    try {
      const s = await api.updateCredentialOperationsSettings(n)
      setValue(String(s.concurrency)); setFailed(false)
      setMessage('已保存，后台将在 5 秒内按新上限调度，无需重启。')
    } catch (error) { setFailed(true); setMessage(getErrorMessage(error)) }
    finally { setSaving(false) }
  }
  return (
    <div className="space-y-4">
      <p className="text-sm text-muted-foreground">登录、补登记和巡检在后台执行，关闭导入窗口不影响任务。同一账号由数据库租约防止重复执行。</p>
      <label className="block space-y-2">
        <span className="text-sm font-medium">后台并发数（每个实例）</span>
        <Input type="number" min={1} max={8} step={1} value={value} disabled={!loaded || saving} onChange={e => { setValue(e.target.value); setMessage('') }} />
      </label>
      <p className="text-xs text-muted-foreground">默认 2，范围 1–8。调低上限不会中断执行中的任务，待其结束后减少并发；多个实例的上限分别计算。并发越高，内存与出口压力越大。</p>
      {loaded && !valid && <p className="text-sm text-destructive">请输入 1–8 的整数。</p>}
      <Button type="button" disabled={!loaded || !valid || saving} onClick={() => void save()}>{saving ? '保存中…' : '保存 2FA 设置'}</Button>
      {!loaded && failed && <Button type="button" variant="outline" onClick={() => setReload(x => x + 1)}>重新加载</Button>}
      {message && <p role={failed ? 'alert' : 'status'} className={failed ? 'text-sm text-destructive' : 'text-sm text-muted-foreground'}>{message}</p>}
    </div>
  )
}
