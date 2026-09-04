import { ReactNode, useEffect, useId } from 'react'
import { X } from 'lucide-react'

type Props = {
  title: string
  children: ReactNode
  onClose: () => void
  width?: 'normal' | 'wide'
}

export function Modal({ title, children, onClose, width = 'normal' }: Props) {
  const titleId = useId()

  useEffect(() => {
    const close = (event: KeyboardEvent) => event.key === 'Escape' && onClose()
    const previousOverflow = document.body.style.overflow
    document.body.style.overflow = 'hidden'
    window.addEventListener('keydown', close)
    return () => {
      document.body.style.overflow = previousOverflow
      window.removeEventListener('keydown', close)
    }
  }, [onClose])

  return (
    <div className="modal-backdrop" role="presentation" onMouseDown={onClose}>
      <section className={`modal ${width === 'wide' ? 'modal-wide' : ''}`} role="dialog" aria-modal="true" aria-labelledby={titleId} onMouseDown={(event) => event.stopPropagation()}>
        <header className="modal-header">
          <h2 id={titleId}>{title}</h2>
          <button className="icon-button" type="button" onClick={onClose} title="关闭" aria-label="关闭"><X size={18} /></button>
        </header>
        {children}
      </section>
    </div>
  )
}
