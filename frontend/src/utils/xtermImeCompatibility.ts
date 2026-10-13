import type { Terminal } from '@xterm/xterm'

// IME compatibility patch for xterm.js on macOS (WKWebView).
//
// With an IME active, WKWebView can report ordinary keystrokes as keydown
// keyCode=229 even when no composition is active. xterm then diverts the
// character to its deferred textarea-diff path, which can drop characters.
// Force eligible single-character insertText events through xterm's direct
// path and rewind the textarea so the deferred diff cannot send them twice.
//
// After switching a Chinese IME to English mode with Caps Lock, WKWebView can
// also emit the input event before its corresponding keyCode-229 keydown. If
// that input event was already delivered, the late keydown must not inject a
// fallback. If no usable input event was delivered, the keydown fallback
// keeps the character from being swallowed.
//
// Some IMEs (e.g. Doubao) swallow keypress for Shift+letter without reporting
// keyCode 229, so uppercase had no delivery path left at all. xterm skips its
// direct path whenever `composed && _keyDownSeen`, so that is the condition we
// key off, rather than inferring it from an IME-specific keyCode.
//
// Event-tap IMEs (XKey/EVKey/OpenKey for Vietnamese) never compose at all; they
// backspace and retype, delivering the replacement as a multi-character
// `keydown.key`. That needs intercepting before keypress — see
// deliverKeyTextRewrite below.

export interface Disposable {
  dispose(): void
}

interface XtermCompositionHelperInternals {
  _isComposing?: unknown
  _isSendingComposition?: unknown
  isComposing?: unknown
}

interface XtermCoreInternals {
  _inputEvent?: (this: XtermCoreInternals, ev: InputEvent) => boolean
  _keyDownSeen?: boolean
  _compositionHelper?: XtermCompositionHelperInternals
  textarea?: {
    value: string
    addEventListener: (type: string, listener: EventListener, capture?: boolean) => void
    removeEventListener: (type: string, listener: EventListener, capture?: boolean) => void
  } | null
}

type TerminalWithCore = Terminal & { _core?: XtermCoreInternals }

interface PendingFallback {
  character: string
  textareaValueBeforeKeydown: string
  createdAt: number
  injected: boolean
  timeoutId: ReturnType<typeof setTimeout>
}

interface DeliveredInput {
  character: string
  deliveredAt: number
}

const PRINTABLE_ASCII = /^[\x20-\x7E]$/
// Every multi-character DOM key value is a CamelCase ASCII identifier
// ("Backspace", "ArrowUp", "F11", "Dead", "Process"). Anything else longer
// than one character is literal text the IME wants inserted.
const NAMED_KEY = /^[A-Z][A-Za-z0-9]*$/
const PRINTABLE_TEXT = /^[^\p{C}]+$/u
const LATE_EVENT_WINDOW_MS = 250
const noopDisposable: Disposable = { dispose() {} }

function isMacPlatform(): boolean {
  return /Mac|iPhone|iPad/.test(navigator.userAgent)
}

function isCompositionActive(helper: XtermCompositionHelperInternals): boolean {
  return (
    helper._isComposing === true ||
    helper._isSendingComposition === true ||
    helper.isComposing === true
  )
}

function isSameCharacter(left: string, right: string): boolean {
  return left.toLowerCase() === right.toLowerCase()
}

export function installImeCompatibilityPatch(terminal: Terminal): Disposable {
  if (!isMacPlatform()) {
    return noopDisposable
  }

  const core = (terminal as TerminalWithCore)._core
  const helper = core?._compositionHelper
  if (
    !core ||
    !helper ||
    typeof core._inputEvent !== 'function' ||
    typeof core._keyDownSeen !== 'boolean' ||
    !core.textarea ||
    typeof core.textarea.addEventListener !== 'function'
  ) {
    return noopDisposable
  }

  let latestKeydownWas229 = false
  let forcedSinceKeydown = false
  let textareaValueBefore229Keydown = ''
  const pendingFallbacks: PendingFallback[] = []
  const deliveredInputs: DeliveredInput[] = []

  const removePendingFallback = (pendingFallback: PendingFallback) => {
    const index = pendingFallbacks.indexOf(pendingFallback)
    if (index >= 0) pendingFallbacks.splice(index, 1)
  }

  const pruneLateEventRecords = () => {
    const now = Date.now()
    for (let index = pendingFallbacks.length - 1; index >= 0; index -= 1) {
      const pendingFallback = pendingFallbacks[index]
      if (now - pendingFallback.createdAt <= LATE_EVENT_WINDOW_MS) continue
      if (!pendingFallback.injected) clearTimeout(pendingFallback.timeoutId)
      pendingFallbacks.splice(index, 1)
    }
    for (let index = deliveredInputs.length - 1; index >= 0; index -= 1) {
      if (now - deliveredInputs[index].deliveredAt <= LATE_EVENT_WINDOW_MS) continue
      deliveredInputs.splice(index, 1)
    }
  }

  // Event-tap Vietnamese IMEs (XKey, EVKey, OpenKey) edit the text themselves
  // instead of composing: they post real Backspace key events, then ONE keydown
  // whose `key` holds the entire replacement run. Typing "tieengs" ends with
  // Backspace x3 followed by `key="iếng"` (code=KeyA, keyCode=73, charCode=105).
  //
  // xterm has no path for that. evaluateKeyboardEvent only accepts
  // `key.length === 1`, so _keyDown bails at `if (!result.key) return true`
  // without sending or cancelling. _keyPress then runs and sends
  // String.fromCharCode(ev.charCode) — just the FIRST character, "i" — and sets
  // _keyPressHandled, which makes _inputEvent discard the full "iếng" at its
  // first line. The backspaces land, the replacement does not: "tiêng" becomes
  // "ti". Only letters carrying a second diacritic hit this, because those are
  // the ones whose tone mark lands on a vowel that is no longer last; "ê", "ô",
  // "đ" arrive as a single-character `key` that xterm handles on its own.
  //
  // Deliver the run here. preventDefault suppresses both the keypress that
  // would leak the first character and the input event that would double-send.
  const deliverKeyTextRewrite = (keyboardEvent: KeyboardEvent): boolean => {
    if (
      keyboardEvent.key.length < 2 ||
      NAMED_KEY.test(keyboardEvent.key) ||
      !PRINTABLE_TEXT.test(keyboardEvent.key) ||
      keyboardEvent.keyCode === 229 ||
      keyboardEvent.isComposing ||
      keyboardEvent.ctrlKey ||
      keyboardEvent.metaKey ||
      keyboardEvent.altKey ||
      isCompositionActive(helper)
    ) {
      return false
    }
    keyboardEvent.preventDefault()
    terminal.input(keyboardEvent.key)
    return true
  }

  const onKeyDown = (event: Event) => {
    const keyboardEvent = event as KeyboardEvent
    pruneLateEventRecords()

    latestKeydownWas229 = keyboardEvent.keyCode === 229
    forcedSinceKeydown = false
    const textareaValueBeforeKeydown = core.textarea?.value ?? ''
    if (latestKeydownWas229) {
      textareaValueBefore229Keydown = textareaValueBeforeKeydown
    }

    if (deliverKeyTextRewrite(keyboardEvent)) {
      return
    }

    if (
      keyboardEvent.keyCode !== 229 ||
      keyboardEvent.isComposing ||
      !keyboardEvent.getModifierState('CapsLock') ||
      !/^[A-Za-z]$/.test(keyboardEvent.key) ||
      keyboardEvent.ctrlKey ||
      keyboardEvent.metaKey ||
      keyboardEvent.altKey ||
      isCompositionActive(helper)
    ) {
      return
    }

    const character = keyboardEvent.shiftKey
      ? keyboardEvent.key
      : keyboardEvent.key.toLowerCase()
    const deliveredIndex = deliveredInputs.findIndex(deliveredInput =>
      isSameCharacter(deliveredInput.character, character),
    )

    // WKWebView can emit insertText before the corresponding 229 keydown. If
    // xterm already delivered that input, only cancel the late keydown.
    if (deliveredIndex >= 0) {
      deliveredInputs.splice(deliveredIndex, 1)
      keyboardEvent.preventDefault()
      return
    }

    keyboardEvent.preventDefault()
    const pendingFallback: PendingFallback = {
      character,
      textareaValueBeforeKeydown,
      createdAt: Date.now(),
      injected: false,
      timeoutId: setTimeout(() => {
        // xterm queues a textarea-diff timeout on the same keydown. Wait one
        // more turn so that diff can run before the fallback injects.
        pendingFallback.timeoutId = setTimeout(() => {
          removePendingFallback(pendingFallback)
          if (
            isCompositionActive(helper) ||
            (core.textarea && core.textarea.value !== textareaValueBeforeKeydown)
          ) {
            return
          }
          pendingFallback.injected = true
          pendingFallbacks.push(pendingFallback)
          terminal.input(character)
        }, 0)
      }, 0),
    }
    pendingFallbacks.push(pendingFallback)
  }

  core.textarea.addEventListener('keydown', onKeyDown, true)

  const originalInputEvent = core._inputEvent
  const patchedInputEvent = function patchedInputEvent(
    this: XtermCoreInternals,
    ev: InputEvent,
  ): boolean {
    pruneLateEventRecords()

    let matchingIndex = -1
    if (ev.data) {
      for (let index = pendingFallbacks.length - 1; index >= 0; index -= 1) {
        if (isSameCharacter(pendingFallbacks[index].character, ev.data)) {
          matchingIndex = index
          break
        }
      }
    }
    const matchingFallback = matchingIndex >= 0
      ? pendingFallbacks[matchingIndex]
      : null

    // A fallback already delivered this character; suppress the late input.
    if (matchingFallback?.injected) {
      if (this.textarea) {
        this.textarea.value = matchingFallback.textareaValueBeforeKeydown
      }
      removePendingFallback(matchingFallback)
      return true
    }

    if (ev.isComposing || isCompositionActive(helper)) {
      for (let index = pendingFallbacks.length - 1; index >= 0; index -= 1) {
        const pendingFallback = pendingFallbacks[index]
        if (pendingFallback.injected) continue
        clearTimeout(pendingFallback.timeoutId)
        pendingFallbacks.splice(index, 1)
      }
    }

    const was229 = latestKeydownWas229
    const shouldForceDirectPath =
      ev.inputType === 'insertText' &&
      Boolean(ev.data) &&
      ev.data!.length === 1 &&
      PRINTABLE_ASCII.test(ev.data!) &&
      !ev.isComposing &&
      !isCompositionActive(helper) &&
      this._keyDownSeen === true &&
      (was229 ? !forcedSinceKeydown : ev.composed)

    let result: boolean
    if (shouldForceDirectPath) {
      forcedSinceKeydown = true
      const savedKeyDownSeen = this._keyDownSeen
      this._keyDownSeen = false
      try {
        result = originalInputEvent!.call(this, ev)
      } finally {
        // Only the 229 path has a deferred diff to neutralise; rewinding on the
        // composed path would discard text xterm never queued.
        if (was229 && this.textarea) {
          this.textarea.value = textareaValueBefore229Keydown
        }
        this._keyDownSeen = savedKeyDownSeen
      }
    } else {
      result = originalInputEvent!.call(this, ev)
    }

    // If xterm delivered this input, remember it briefly. WKWebView may send
    // the matching 229 keydown afterward, and that keydown must not inject a
    // second copy of the character.
    if (
      result &&
      ev.inputType === 'insertText' &&
      ev.data &&
      PRINTABLE_ASCII.test(ev.data) &&
      !ev.isComposing &&
      !isCompositionActive(helper)
    ) {
      deliveredInputs.push({ character: ev.data, deliveredAt: Date.now() })
      if (matchingFallback && !matchingFallback.injected) {
        clearTimeout(matchingFallback.timeoutId)
        removePendingFallback(matchingFallback)
      }
    }

    return result
  }

  core._inputEvent = patchedInputEvent

  return {
    dispose() {
      for (const pendingFallback of pendingFallbacks) {
        if (!pendingFallback.injected) clearTimeout(pendingFallback.timeoutId)
      }
      pendingFallbacks.length = 0
      deliveredInputs.length = 0
      core.textarea?.removeEventListener('keydown', onKeyDown, true)
      if (core._inputEvent === patchedInputEvent) {
        core._inputEvent = originalInputEvent
      }
    },
  }
}
