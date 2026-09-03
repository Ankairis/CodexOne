import { ReactNode, useEffect } from 'react'
import { X } from 'lucide-react'

type Props = {
  title: string
  children: ReactNode
  onClose: () => void
  width?: 'normal' | 'wide'
}

export function Modal({ title, children, onClose, width = 'normal' }: Props) {
  useEffect(() => {
    const close = (event: KeyboardEvent) => event.key === 'Escape' && onClose()
    window.addEventListener('keydown', close)
    return () => window.removeEventListener('keydown', close)
  }, [onClose])

  return (
    <div className="modal-backdrop" role="presentation" onMouseDown={onClose}>
      <section className={`modal ${width === 'wide' ? 'modal-wide' : ''}`} role="dialog" aria-modal="true" aria-label={title} onMouseDown={(event) => event.stopPropagation()}>
        <header className="modal-header">
          <h2>{title}</h2>
          <button className="icon-button" type="button" onClick={onClose} title="关闭"><X size={18} /></button>
        </header>
        {children}
      </section>
    </div>
  )
}
