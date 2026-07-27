import { mount } from '@vue/test-utils'
import { defineComponent, h } from 'vue'
import { afterEach, describe, expect, it, vi } from 'vitest'

import { useAutoRefresh } from './useAutoRefresh'

afterEach(() => {
  vi.useRealTimers()
  localStorage.clear()
})

describe('useAutoRefresh fixed cadence', () => {
  it('fires exactly after 60 seconds', async () => {
    vi.useFakeTimers()
    const onRefresh = vi.fn()

    const Host = defineComponent({
      setup() {
        const refresh = useAutoRefresh({
          storageKey: 'channel-status-test',
          intervals: [60] as const,
          defaultInterval: 60,
          onRefresh,
        })
        refresh.setEnabled(true)
        return () => h('div', String(refresh.countdown.value))
      },
    })

    const wrapper = mount(Host)
    await vi.advanceTimersByTimeAsync(59_000)
    expect(onRefresh).not.toHaveBeenCalled()

    await vi.advanceTimersByTimeAsync(1_000)
    expect(onRefresh).toHaveBeenCalledTimes(1)
    expect(wrapper.text()).toBe('60')
    wrapper.unmount()
  })
})
