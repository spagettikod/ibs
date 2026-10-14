import { useId } from 'react'
import { fullMark, idPrefix, smallMark } from './logo-paths'

// The IBS mark: a floppy disk on a steel shield, in greyscale. The shapes come
// from docs/logo/generate.py; the small variant drops the label lines and the
// shadow so it stays clear at 32px and below.

export function Mark({ size = 28, title }: { size?: number; title?: string }) {
  // Gradient ids must be unique on the page, so each mark gets its own prefix.
  const uid = 'm' + useId().replace(/[^a-zA-Z0-9]/g, '') + '-'
  const mark = (size <= 32 ? smallMark : fullMark).split(idPrefix).join(uid + idPrefix)
  return (
    <svg viewBox="0 0 100 100" width={size} height={size} role={title ? 'img' : undefined} aria-hidden={title ? undefined : true}>
      {title && <title>{title}</title>}
      <g dangerouslySetInnerHTML={{ __html: mark }} />
    </svg>
  )
}

/** Mark plus the name: "IBS", or with stacked, "Intergalactic Backup System"
 *  on three lines as tall as the mark. */
export function Logo({ size = 28, stacked = false }: { size?: number; stacked?: boolean }) {
  if (stacked) {
    return (
      <div className="logo stacked" style={{ gap: size * 0.15 }}>
        <Mark size={size} />
        <span style={{ fontSize: size / 3.7, lineHeight: `${size / 3}px` }}>Intergalactic<br />Backup<br />System</span>
      </div>
    )
  }
  return (
    <div className="logo" style={{ gap: size * 0.32 }}>
      <Mark size={size} />
      <span style={{ fontSize: size * 0.62 }}>IBS</span>
    </div>
  )
}
