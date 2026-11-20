/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For commercial licensing, please contact support@quantumnous.com
*/

import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import type { Row } from '@tanstack/react-table'
import {
  cleanup,
  render,
  screen,
  waitFor,
  within,
} from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, expect, test, vi } from 'vitest'

import { TooltipProvider } from '@/components/ui/tooltip'
import { api } from '@/lib/api'
import { ROLE } from '@/lib/roles'
import { useAuthStore } from '@/stores/auth-store'

import { upstreamBalanceQueryKey } from '../../api'
import { channelSchema, type Channel } from '../../types'
import { ChannelsProvider, useChannels } from '../channels-provider'
import { DataTableRowActions } from '../data-table-row-actions'
import { BalanceQueryDialog } from '../dialogs/balance-query-dialog'

const originalAuth = useAuthStore.getState().auth
const path = '/api/channel/3/upstream_balance'
const saved = {
  provider: 'new-api',
  base_url: 'https://upstream.example',
  upstream_user_id: 0,
  query_minutes: 15,
  refresh_minutes: 0,
  alert_enabled: false,
  threshold_usd: 0,
  phone: '',
  balance_usd: null,
  checked_at: 0,
  access_expires_at: 0,
  query_pending: false,
  query_error: '',
}
let monitor: Record<string, unknown> | null
let client: QueryClient

function BalanceDialog() {
  const channels = useChannels()
  return (
    <BalanceQueryDialog
      open={channels.open === 'balance-query'}
      onOpenChange={(open) => !open && channels.setOpen(null)}
    />
  )
}

async function openBalance() {
  const channel = channelSchema.parse({
    id: 3,
    key: '',
    name: 'Upstream channel',
    type: 1,
    status: 1,
    created_time: 1,
    test_time: 0,
    response_time: 0,
    balance_updated_time: 0,
  })
  render(
    <QueryClientProvider client={client}>
      <ChannelsProvider>
        <TooltipProvider>
          <DataTableRowActions row={{ original: channel } as Row<Channel>} />
          <BalanceDialog />
        </TooltipProvider>
      </ChannelsProvider>
    </QueryClientProvider>
  )
  const user = userEvent.setup()
  await user.click(screen.getByRole('button', { name: 'Open menu' }))
  await user.click(
    await screen.findByRole('menuitem', { name: 'Query Balance' })
  )
  return user
}

beforeEach(() => {
  client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
  monitor = null
  useAuthStore.setState({
    auth: {
      ...originalAuth,
      user: { id: 1, username: 'root', role: ROLE.SUPER_ADMIN },
    },
  })
  vi.spyOn(api, 'get').mockImplementation(async (url) => {
    if (url === path) return { data: { success: true, data: monitor } }
    throw new Error(`Unexpected GET ${url}`)
  })
  vi.spyOn(api, 'put').mockImplementation(async () => {
    monitor = { ...saved }
    return { data: { success: true } }
  })
  vi.spyOn(api, 'delete').mockImplementation(async () => {
    monitor = null
    return { data: { success: true } }
  })
})

afterEach(() => {
  cleanup()
  client.clear()
  useAuthStore.setState({ auth: originalAuth })
  vi.restoreAllMocks()
})

test('Query Balance opens account settings and Save persists the entered credentials', async () => {
  const user = await openBalance()
  await user.type(await screen.findByLabelText('Upstream URL'), saved.base_url)
  await user.type(
    screen.getByLabelText('Access token', { exact: true }),
    'test-access'
  )
  await user.click(screen.getByRole('button', { name: 'Save' }))
  await waitFor(() =>
    expect(api.put).toHaveBeenCalledWith(
      path,
      expect.objectContaining({
        base_url: saved.base_url,
        access_token: 'test-access',
      })
    )
  )
  await waitFor(() =>
    expect(screen.getByLabelText('Access token', { exact: true })).toHaveValue(
      ''
    )
  )
})

test.each([
  { error: '', message: 'Balance query succeeded', balance: 0 },
  {
    error: 'credentials',
    message: 'Upstream credentials were rejected.',
    balance: 12,
  },
])(
  'Save and test reports the actual query result when error is "$error"',
  async (item) => {
    monitor = { ...saved, balance_usd: 12 }
    let queued = false
    vi.spyOn(api, 'post').mockImplementation(async () => {
      queued = true
      monitor = { ...saved, balance_usd: 12, query_pending: true }
      return { data: { success: true } }
    })
    const user = await openBalance()
    await user.click(
      await screen.findByRole('button', { name: 'Save and test' })
    )
    await waitFor(() => expect(api.post).toHaveBeenCalledWith(`${path}/query`))
    expect(queued).toBe(true)
    expect(
      await screen.findByRole('status', { name: 'Balance test result' })
    ).toHaveTextContent('Testing...')
    expect(screen.getByRole('button', { name: 'Save and test' })).toBeDisabled()
    monitor = {
      ...saved,
      checked_at: 1795135559,
      balance_usd: item.balance,
      query_error: item.error,
    }
    await client.invalidateQueries({ queryKey: upstreamBalanceQueryKey(3) })
    await waitFor(() =>
      expect(
        screen.getByRole('status', { name: 'Balance test result' })
      ).toHaveTextContent(item.message)
    )
    if (item.error) {
      expect(
        screen.queryByText('Balance query succeeded')
      ).not.toBeInTheDocument()
    } else {
      expect(
        screen.getByRole('status', { name: 'Balance test result' })
      ).toHaveTextContent('$0')
    }
  }
)

test('Remove deletes the saved monitor after confirmation', async () => {
  monitor = { ...saved }
  const user = await openBalance()
  await user.click(await screen.findByRole('button', { name: 'Remove' }))
  const confirm = await screen.findByRole('alertdialog')
  await user.click(within(confirm).getByRole('button', { name: 'Remove' }))
  await waitFor(() => expect(api.delete).toHaveBeenCalledWith(path))
  expect(api.put).not.toHaveBeenCalled()
})

test('a failed save shows the error and does not start a test', async () => {
  monitor = { ...saved }
  vi.spyOn(api, 'put').mockResolvedValue({
    data: {
      success: false,
      message:
        'Enter valid upstream credentials and a refresh token for automatic renewal',
    },
  })
  const post = vi.spyOn(api, 'post')
  const user = await openBalance()
  await user.click(await screen.findByRole('button', { name: 'Save and test' }))
  expect(await screen.findByRole('alert')).toHaveTextContent(
    'Enter valid upstream credentials and a refresh token for automatic renewal'
  )
  expect(post).not.toHaveBeenCalled()
})

test('an admin without sensitive channel permission cannot view the credential settings', async () => {
  useAuthStore.setState({
    auth: {
      ...originalAuth,
      user: { id: 2, username: 'admin', role: ROLE.ADMIN },
    },
  })
  await openBalance()
  expect(screen.getByRole('button', { name: 'Update Balance' })).toBeVisible()
  expect(screen.queryByLabelText('Upstream URL')).not.toBeInTheDocument()
  expect(api.get).not.toHaveBeenCalledWith(path, expect.anything())
})

test('Save and test saves a new configuration before querying it', async () => {
  vi.spyOn(api, 'post').mockImplementation(async () => {
    expect(api.put).toHaveBeenCalled()
    monitor = { ...saved, balance_usd: 7.5, checked_at: 1795135559 }
    return { data: { success: true } }
  })
  const user = await openBalance()
  await user.type(await screen.findByLabelText('Upstream URL'), saved.base_url)
  await user.type(
    screen.getByLabelText('Access token', { exact: true }),
    'new-access'
  )
  await user.click(screen.getByRole('button', { name: 'Save and test' }))
  await waitFor(() =>
    expect(
      screen.getByRole('status', { name: 'Balance test result' })
    ).toHaveTextContent('Balance query succeeded')
  )
  expect(
    screen.getByRole('status', { name: 'Balance test result' })
  ).toHaveTextContent('$7.5')
  expect(screen.getByLabelText('Access token', { exact: true })).toHaveValue('')
})
