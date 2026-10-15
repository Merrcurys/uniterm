import { describe, expect, it } from 'vitest'
import { withDefaultTermType } from './terminalOptions'

describe('withDefaultTermType', () => {
  it('leaves an explicit per-connection termType untouched', () => {
    const config: any = { termType: 'vt100' }
    expect(withDefaultTermType(config, 'xterm').termType).toBe('vt100')
  })

  it('fills the app default when the connection has none', () => {
    const config: any = {}
    expect(withDefaultTermType(config, 'vt320').termType).toBe('vt320')
  })

  it('keeps the config unchanged when no app default is set', () => {
    const config: any = { host: 'h' }
    const out = withDefaultTermType(config, '')
    expect(out.termType).toBeUndefined()
  })

  it('does not mutate the input config', () => {
    const config: any = {}
    withDefaultTermType(config, 'vt100')
    expect(config.termType).toBeUndefined()
  })
})
