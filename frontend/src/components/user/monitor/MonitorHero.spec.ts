import { mount } from '@vue/test-utils'
import { describe, expect, it, vi } from 'vitest'

import MonitorHero from './MonitorHero.vue'

vi.mock('vue-i18n', () => ({
  useI18n: () => ({ t: (key: string) => key }),
}))

describe('MonitorHero fixed window controls', () => {
  it('shows the one-hour window and readonly refresh countdown without window tabs', () => {
    const wrapper = mount(MonitorHero, {
      props: {
        overallStatus: 'operational',
        loading: false,
        countdownSeconds: 60,
      },
    })

    expect(wrapper.find('[role="tablist"]').exists()).toBe(false)
    expect(wrapper.text()).toContain('channelStatus.windowLabel')
    expect(wrapper.text()).toContain('common.autoRefresh.countdown')
    expect(wrapper.findAll('button')).toHaveLength(1)
  })
})
