/** App shortcuts yield to the focused remote desktop, including when
 * ordinary DOM events provide the keyboard fallback. */
export function remoteDesktopOwnsKey(event: KeyboardEvent): boolean {
  const target = event.target as Element | null
  return !!target?.closest?.('[data-remote-keyboard="true"]')
}

/** Menus opened without moving DOM focus still need local keyboard control. */
export function releaseRemoteKeyboardFocus(): void {
  const active = document.activeElement as HTMLElement | null
  if (active?.closest?.('[data-remote-keyboard="true"]')) active.blur()
}

// Panels share one hook. A delayed blur must not disable a newly focused panel.
let captureChange: Promise<unknown> = Promise.resolve()
export function queueRemoteKeyboardChange(change: () => Promise<unknown>): Promise<unknown> {
  const next = captureChange.then(change)
  captureChange = next.catch(() => {})
  return next
}

export type NativeRdpKey = {
  captureID: string
  scancode: number
  down: boolean
  repeat: boolean
  ctrl: boolean
  shift: boolean
  meta: boolean
  reset: boolean
}

export function isRdpPasteKey(scancode: number, ctrl: boolean, meta: boolean, shift: boolean): boolean {
  return ((ctrl || meta) && scancode === 0x2f) || (shift && scancode === 0xe052)
}
