import { describe, expect, it } from 'vitest'
import { readFileSync } from 'node:fs'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

const currentDir = dirname(fileURLToPath(import.meta.url))
const accountsViewSource = readFileSync(resolve(currentDir, '../AccountsView.vue'), 'utf8')

describe('AccountsView pool ranking isolation', () => {
  it('keeps the standalone ranking out of the account table and edit workflow', () => {
    expect(accountsViewSource).not.toContain('PoolAutoPriorityLeaderboard')
    expect(accountsViewSource).not.toContain('auto_priority_rank')
    expect(accountsViewSource).not.toContain('available_balance')
    expect(accountsViewSource).toContain('EditAccountModal')
    expect(accountsViewSource).toContain('AccountActionMenu')
  })
})
