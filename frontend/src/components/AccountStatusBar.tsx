import { useEffect, useRef, useState, type ReactNode } from 'react'

// Shared renderer extracted from AccountHealthBar. Keep both account bars on
// the same dimensions, percentage badge and mouse/touch tooltip behavior.
export default function AccountStatusBar({ blocks, rate, rateClass, label }: {
  blocks: { color?: string; tooltip: ReactNode; label?: string; onClick?: () => void }[]
  rate: number | null
  rateClass: string
  label?: string
}) {
  const [activeTooltip, setActiveTooltip] = useState<number | null>(null)
  const blocksRef = useRef<HTMLDivElement>(null)
  useEffect(() => {
    if (activeTooltip === null) return
    const handler = (e: PointerEvent) => {
      if (blocksRef.current && !blocksRef.current.contains(e.target as Node)) setActiveTooltip(null)
    }
    document.addEventListener('pointerdown', handler)
    return () => document.removeEventListener('pointerdown', handler)
  }, [activeTooltip])
  const rounded = rate?.toFixed(1)
  return (
    <div className="flex max-w-full items-center gap-1.5" aria-label={label}>
      <div className="relative flex min-w-[120px] flex-1 gap-[2px]" ref={blocksRef}>
        {blocks.map((block, idx) => {
          const active = activeTooltip === idx
          const posClass = idx <= 2 ? 'left-0 translate-x-0' : idx >= blocks.length - 3 ? 'right-0 left-auto translate-x-0' : 'left-1/2 -translate-x-1/2'
          return (
            <div key={idx} className="group relative min-w-[4px] flex-1 cursor-pointer"
              role={block.onClick ? 'button' : undefined} tabIndex={block.onClick ? 0 : undefined} aria-label={block.label}
              onClick={block.onClick}
              onKeyDown={e => { if (block.onClick && (e.key === 'Enter' || e.key === ' ')) { e.preventDefault(); block.onClick() } }}
              onFocus={() => setActiveTooltip(idx)} onBlur={() => setActiveTooltip(null)}
              onPointerEnter={e => { if (e.pointerType === 'mouse') setActiveTooltip(idx) }}
              onPointerLeave={e => { if (e.pointerType === 'mouse') setActiveTooltip(null) }}
              onPointerDown={e => { if (e.pointerType === 'touch') { e.preventDefault(); setActiveTooltip(prev => prev === idx ? null : idx) } }}>
              <div className={`h-1.5 w-full rounded-[2px] transition-transform duration-150 group-hover:scale-y-[1.8] ${block.color ? '' : 'bg-border'} ${active ? 'scale-y-[1.8] opacity-90' : ''}`}
                style={block.color ? { backgroundColor: block.color } : undefined} />
              {active && <div className={`pointer-events-none absolute bottom-[calc(100%+8px)] z-30 whitespace-nowrap rounded-md border bg-popover px-2.5 py-1.5 text-[11px] leading-snug text-popover-foreground shadow-md ${posClass}`} role="tooltip">{block.tooltip}</div>}
            </div>
          )
        })}
      </div>
      <span className={`inline-flex items-center rounded px-1.5 py-0.5 text-[11px] font-semibold tabular-nums ${rateClass}`}>
        {rounded ? `${rounded.endsWith('.0') ? rounded.slice(0, -2) : rounded}%` : '--'}
      </span>
    </div>
  )
}
