import { beforeEach, describe, expect, it, vi } from 'vitest'
import { createPinia, setActivePinia } from 'pinia'

vi.mock('../../bindings/github.com/ys-ll/uniterm/app', () => ({ LoadLocalState: vi.fn(), SaveLocalState: vi.fn() }))
import { LoadLocalState, SaveLocalState } from '../../bindings/github.com/ys-ll/uniterm/app'
import { useLocalStateStore } from './localStateStore'

beforeEach(() => {
  setActivePinia(createPinia())
  vi.resetAllMocks()
  vi.mocked(LoadLocalState).mockResolvedValue({ sidebarVisible: false } as any)
  vi.mocked(SaveLocalState).mockResolvedValue(undefined)
})

describe('local AI preferences', () => {
  it('migrates the legacy model once while preserving existing local fields', async () => {
    const store = useLocalStateStore()
    await store.ensureAISelection(['a', 'b'], 'b')
    expect(store.state.ai?.activeModelId).toBe('b')
    expect(store.state.sidebarVisible).toBe(false)
    await store.ensureAISelection(['a', 'b'], 'a')
    expect(store.state.ai?.activeModelId).toBe('b')
    expect(SaveLocalState).toHaveBeenCalledTimes(1)
  })

  it('keeps the local choice ahead of a stale settings selection', async () => {
    vi.mocked(LoadLocalState).mockResolvedValue({ ai: { activeModelId: 'a', modelPreferences: {} } } as any)
    const store = useLocalStateStore()
    await store.ensureAISelection(['a', 'b'], 'b')
    expect(store.state.ai?.activeModelId).toBe('a')
    expect(SaveLocalState).not.toHaveBeenCalled()
    await store.ensureAISelection(['b'], 'a')
    expect(store.state.ai?.activeModelId).toBe('b')
  })

  it('remembers independent model choices after restart', async () => {
    const store = useLocalStateStore()
    await store.ensureAISelection(['a', 'b'])
    await Promise.all([
      store.setAIReasoning('a', { reasoningEffort: 'high' }),
      store.setAIReasoning('b', { reasoningEffort: 'default' }),
    ])
    await store.setActiveAIModel('b')
    const saved = vi.mocked(SaveLocalState).mock.calls.at(-1)![0]
    vi.mocked(LoadLocalState).mockResolvedValue(saved)
    setActivePinia(createPinia())
    const restarted = useLocalStateStore()
    await restarted.init()
    expect(restarted.state.ai).toEqual({ activeModelId: 'b', modelPreferences: {
      a: { reasoningEffort: 'high' },
      b: { reasoningEffort: 'default' },
    } })
  })

  it('serializes writes so an old completion cannot overwrite a newer choice', async () => {
    const store = useLocalStateStore()
    await store.init()
    let finishFirst!: () => void
    vi.mocked(SaveLocalState).mockImplementationOnce(() => new Promise(resolve => { finishFirst = () => resolve(undefined) }))
    const first = store.setActiveAIModel('a')
    const second = store.setActiveAIModel('b')
    await vi.waitFor(() => expect(SaveLocalState).toHaveBeenCalledTimes(1))
    finishFirst()
    await Promise.all([first, second])
    expect(vi.mocked(SaveLocalState).mock.calls.map((call: unknown[]) => (call[0] as any).ai.activeModelId)).toEqual(['a', 'b'])
    expect(store.state.ai?.activeModelId).toBe('b')
  })

  it('reports migration failure instead of allowing legacy selection deletion', async () => {
    vi.mocked(SaveLocalState).mockRejectedValueOnce(new Error('disk full'))
    await expect(useLocalStateStore().ensureAISelection(['a'], 'a')).rejects.toThrow('Could not save')
    await useLocalStateStore().ensureAISelection(['a'], 'a')
    expect(SaveLocalState).toHaveBeenCalledTimes(2)
  })

  it('removes obsolete thinking switches while retaining model and effort choices', async () => {
    vi.mocked(LoadLocalState).mockResolvedValue({ ai: { activeModelId: 'a', modelPreferences: {
      a: { thinkingMode: 'disabled', reasoningEffort: 'high' },
      b: { thinkingMode: 'enabled', reasoningEffort: 'default' },
    } } } as any)
    const store = useLocalStateStore()
    await store.ensureAISelection(['a', 'b'])
    expect(store.state.ai).toEqual({ activeModelId: 'a', modelPreferences: {
      a: { reasoningEffort: 'high' },
      b: { reasoningEffort: 'default' },
    } })
    expect(vi.mocked(SaveLocalState).mock.calls.at(-1)![0].ai).toEqual(store.state.ai)
    await store.setAIReasoning('a', { thinkingMode: 'disabled', reasoningEffort: 'low' } as any)
    expect(vi.mocked(SaveLocalState).mock.calls.at(-1)![0].ai.modelPreferences.a).toEqual({ reasoningEffort: 'low' })
  })
})
