import { useState } from 'react'

// Charts are hand-drawn SVG on purpose: one data hue, thin marks, rounded
// data-ends, recessive axes, and a hover readout on every mark. No chart
// library — a demo should be readable end to end.

const money = (n: number) =>
  n >= 1_000_000
    ? `$${(n / 1_000_000).toFixed(1)}M`
    : n >= 1_000
      ? `$${Math.round(n / 1_000)}k`
      : `$${Math.round(n)}`

export function BarRow({ label, value, max }: { label: string; value: number; max: number }) {
  const pct = max > 0 ? (value / max) * 100 : 0
  return (
    <div className="grid grid-cols-[7rem_1fr_4.5rem] items-center gap-3 py-1.5">
      <span className="truncate text-sm text-ink-soft">{label}</span>
      <div className="h-5 rounded-sm bg-line/60">
        <div
          className="h-5 rounded-sm bg-data transition-[width] duration-500"
          style={{ width: `${Math.max(pct, 1.5)}%` }}
          role="img"
          aria-label={`${label}: ${money(value)}`}
        />
      </div>
      {/* Direct label: the number belongs next to its bar, not on an axis. */}
      <span className="text-right font-mono text-sm tabular-nums">{money(value)}</span>
    </div>
  )
}

export function TrendLine({ points }: { points: { month: string; revenue: number }[] }) {
  const [hover, setHover] = useState<number | null>(null)
  if (points.length < 2) {
    return <p className="py-8 text-center text-sm text-muted">Not enough months yet.</p>
  }

  const w = 640
  const h = 180
  const pad = { top: 12, right: 12, bottom: 24, left: 12 }
  const max = Math.max(...points.map((p) => p.revenue))
  const x = (i: number) => pad.left + (i * (w - pad.left - pad.right)) / (points.length - 1)
  const y = (v: number) => pad.top + (1 - v / max) * (h - pad.top - pad.bottom)

  const line = points.map((p, i) => `${i === 0 ? 'M' : 'L'} ${x(i)} ${y(p.revenue)}`).join(' ')
  const area = `${line} L ${x(points.length - 1)} ${h - pad.bottom} L ${x(0)} ${h - pad.bottom} Z`
  const active = hover === null ? null : points[hover]

  return (
    <div className="relative">
      <svg
        viewBox={`0 0 ${w} ${h}`}
        className="w-full"
        role="img"
        aria-label="Revenue per month over the last twelve months"
        onMouseLeave={() => setHover(null)}
      >
        <path d={area} fill="var(--color-data)" opacity="0.10" />
        <path
          d={line}
          fill="none"
          stroke="var(--color-data)"
          strokeWidth="2"
          strokeLinecap="round"
          strokeLinejoin="round"
        />
        {points.map((p, i) => (
          <g key={p.month}>
            {/* A hit target far bigger than the mark. */}
            <rect
              x={x(i) - (w / points.length) / 2}
              y={0}
              width={w / points.length}
              height={h}
              fill="transparent"
              onMouseEnter={() => setHover(i)}
            />
            {hover === i && (
              <>
                <line
                  x1={x(i)}
                  y1={pad.top}
                  x2={x(i)}
                  y2={h - pad.bottom}
                  stroke="var(--color-muted)"
                  strokeWidth="1"
                  strokeDasharray="3 3"
                />
                {/* A surface ring keeps the marker legible over the line. */}
                <circle cx={x(i)} cy={y(p.revenue)} r="5" fill="var(--color-data)" stroke="var(--color-card)" strokeWidth="2" />
              </>
            )}
          </g>
        ))}
        {points.map((p, i) =>
          i % 2 === 0 ? (
            <text
              key={p.month}
              x={x(i)}
              y={h - 6}
              textAnchor="middle"
              className="fill-muted"
              style={{ fontSize: 10 }}
            >
              {p.month.slice(2)}
            </text>
          ) : null,
        )}
      </svg>
      {active && (
        <div className="pointer-events-none absolute left-0 top-0 rounded-md border border-line bg-card px-2 py-1 text-xs shadow-sm">
          <span className="text-muted">{active.month}</span>{' '}
          <span className="font-mono tabular-nums">{money(active.revenue)}</span>
        </div>
      )}
    </div>
  )
}
