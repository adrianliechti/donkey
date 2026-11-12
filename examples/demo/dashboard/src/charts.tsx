import { useState } from 'react'

// Hand-drawn SVG on purpose — a demo should be readable end to end. The
// marks follow one quiet spec: 2px lines with round joins, area washes at
// 10%, hairline solid gridlines, bars that stay thin with a rounded data-end
// and a square baseline, values in ink (never in the data color).

const weekLabel = (iso: string) =>
  new Date(iso).toLocaleDateString('en-US', { month: 'short', day: 'numeric' })

export function TrendLine({ points }: { points: { week: string; count: number }[] }) {
  const [hover, setHover] = useState<number | null>(null)
  if (points.length < 2) {
    return <p className="py-8 text-center text-sm text-muted">Not enough weeks yet.</p>
  }

  const w = 640
  const h = 190
  const pad = { top: 12, right: 12, bottom: 22, left: 30 }
  // A rounded ceiling keeps the gridline values clean (0 / half / max).
  const max = Math.max(4, Math.ceil(Math.max(...points.map((p) => p.count)) / 4) * 4)
  const x = (i: number) => pad.left + (i * (w - pad.left - pad.right)) / (points.length - 1)
  const y = (v: number) => pad.top + (1 - v / max) * (h - pad.top - pad.bottom)

  const line = points.map((p, i) => `${i === 0 ? 'M' : 'L'} ${x(i)} ${y(p.count)}`).join(' ')
  const area = `${line} L ${x(points.length - 1)} ${h - pad.bottom} L ${x(0)} ${h - pad.bottom} Z`
  const active = hover === null ? null : points[hover]

  return (
    <div className="relative">
      <svg
        viewBox={`0 0 ${w} ${h}`}
        className="w-full"
        role="img"
        aria-label="New tickets per week over the last twelve weeks"
        onMouseLeave={() => setHover(null)}
      >
        {[0, max / 2, max].map((v) => (
          <g key={v}>
            <line
              x1={pad.left}
              y1={y(v)}
              x2={w - pad.right}
              y2={y(v)}
              stroke="var(--color-line)"
              strokeWidth="1"
            />
            <text x={pad.left - 6} y={y(v) + 3} textAnchor="end" className="fill-muted" style={{ fontSize: 10 }}>
              {v}
            </text>
          </g>
        ))}
        <path d={area} fill="var(--color-accent)" opacity="0.1" />
        <path
          d={line}
          fill="none"
          stroke="var(--color-accent)"
          strokeWidth="2"
          strokeLinecap="round"
          strokeLinejoin="round"
        />
        {points.map((p, i) => (
          <g key={p.week}>
            {/* A hit target far bigger than the mark. */}
            <rect
              x={x(i) - (w - pad.left - pad.right) / (points.length - 1) / 2}
              y={0}
              width={(w - pad.left - pad.right) / (points.length - 1)}
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
                />
                {/* The surface ring keeps the marker legible over the line. */}
                <circle cx={x(i)} cy={y(p.count)} r="5" fill="var(--color-accent)" stroke="var(--color-card)" strokeWidth="2" />
              </>
            )}
          </g>
        ))}
        {points.map((p, i) =>
          i % 2 === 0 ? (
            <text key={p.week} x={x(i)} y={h - 6} textAnchor="middle" className="fill-muted" style={{ fontSize: 10 }}>
              {weekLabel(p.week)}
            </text>
          ) : null,
        )}
      </svg>
      {active && (
        <div className="pointer-events-none absolute left-8 top-0 rounded-md border border-line bg-card px-2 py-1 text-xs shadow-sm">
          <span className="text-muted">week of {weekLabel(active.week)}</span>{' '}
          <span className="font-medium">{active.count} tickets</span>
        </div>
      )}
    </div>
  )
}

export function BarRow({ label, value, max }: { label: string; value: number; max: number }) {
  const pct = max > 0 ? (value / max) * 100 : 0
  return (
    <div className="grid grid-cols-[6rem_1fr_3rem] items-center gap-3 py-1.5">
      <span className="truncate text-sm text-ink-soft">{label}</span>
      <div className="h-4">
        {/* Rounded at the data end, square at the baseline. */}
        <div
          className="h-4 rounded-r-[4px] bg-accent transition-[width] duration-500"
          style={{ width: `${Math.max(pct, 1.5)}%` }}
          role="img"
          aria-label={`${label}: ${value}`}
        />
      </div>
      {/* Direct label: the number belongs next to its bar, not on an axis. */}
      <span className="text-right text-sm tabular-nums">{value.toLocaleString('en-US')}</span>
    </div>
  )
}
