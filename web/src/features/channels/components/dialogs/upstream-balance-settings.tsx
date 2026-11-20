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
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useEffect, useId, useState } from 'react'
import { useTranslation } from 'react-i18next'

import { ConfirmDialog } from '@/components/confirm-dialog'
import { PasswordInput } from '@/components/password-input'
import { Button } from '@/components/ui/button'
import { DialogFooter } from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'
import { Switch } from '@/components/ui/switch'
import { toIntlLocale } from '@/i18n/languages'
import { api } from '@/lib/api'
import { formatCurrencyFromUSD } from '@/lib/currency'
import { formatTimestamp } from '@/lib/format'
import {
  getServerErrorMessage,
  requireServerSuccess,
} from '@/lib/server-error-message'

import { queueUpstreamBalance, upstreamBalanceQueryKey } from '../../api'
import { channelsQueryKeys } from '../../lib/channel-actions'

type BalanceConfig = {
  provider: 'new-api' | 'sub2api'
  base_url: string
  upstream_user_id: number
  query_minutes: number
  refresh_minutes: number
  alert_enabled: boolean
  threshold_usd: number
  phone: string
}

type Monitor = BalanceConfig & {
  balance_usd: number | null
  checked_at: number
  access_expires_at: number
  query_pending: boolean
  query_error?: string
  refresh_error?: string
  alert_state?: string
  alert_call_id?: string
}

function upstreamBalancePath(id: number) {
  return `/api/channel/${id}/upstream_balance`
}

export function UpstreamBalanceSettings(props: {
  channelId: number
  canQuery: boolean
}) {
  const { t, i18n } = useTranslation()
  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language)
  const client = useQueryClient()
  const queryKey = upstreamBalanceQueryKey(props.channelId)
  const [testStatus, setTestStatus] = useState<
    'idle' | 'testing' | 'success' | 'failure'
  >('idle')
  const result = useQuery({
    queryKey,
    queryFn: async () => {
      const response = await api.get<{
        success: boolean
        data: Monitor | null
      }>(upstreamBalancePath(props.channelId), { disableDuplicate: true })
      return requireServerSuccess(response.data).data
    },
    refetchInterval: (query) =>
      query.state.data?.query_pending ? 2500 : false,
  })
  useEffect(() => {
    if (testStatus !== 'testing' || result.isFetching) return
    if (result.isError) {
      setTestStatus('failure')
      return
    }
    if (!result.data || result.data.query_pending) return
    setTestStatus(result.data.query_error ? 'failure' : 'success')
    void client.invalidateQueries({ queryKey: channelsQueryKeys.lists() })
  }, [testStatus, result.isFetching, result.isError, result.data, client])
  if (result.isPending) return <p role='status'>{t('Loading...')}</p>
  if (result.isError) {
    return (
      <p role='alert'>
        {getServerErrorMessage(
          result.error,
          t('Unable to load upstream balance settings')
        )}
      </p>
    )
  }
  let testMessage = ''
  if (testStatus === 'testing') testMessage = t('Testing...')
  if (testStatus === 'success') testMessage = t('Balance query succeeded')
  if (testStatus === 'failure') {
    testMessage =
      result.data?.query_error === 'credentials'
        ? t('Upstream credentials were rejected.')
        : t('Failed to query the upstream balance.')
  }
  return (
    <section className='space-y-4' aria-label={t('Upstream account balance')}>
      <h3 className='font-medium'>{t('Upstream account balance')}</h3>
      <p className='text-muted-foreground text-sm'>
        {t('Use a dedicated upstream login session for automatic renewal.')}
      </p>
      {testStatus !== 'idle' && (
        <div
          role='status'
          aria-label={t('Balance test result')}
          className='rounded-md border p-3 text-sm'
        >
          <p>{testMessage}</p>
          {testStatus === 'success' && result.data?.balance_usd != null && (
            <p>{formatCurrencyFromUSD(result.data.balance_usd, { locale })}</p>
          )}
        </div>
      )}
      <UpstreamBalanceForm
        key={result.data ? 'configured' : 'new'}
        channelId={props.channelId}
        canQuery={props.canQuery}
        monitor={result.data}
        testing={testStatus === 'testing'}
        onTestQueued={() => setTestStatus('testing')}
        onChanged={async () => {
          await client.invalidateQueries({ queryKey })
          await client.invalidateQueries({
            queryKey: channelsQueryKeys.lists(),
          })
        }}
      />
    </section>
  )
}

function UpstreamBalanceForm(props: {
  channelId: number
  canQuery: boolean
  monitor: Monitor | null
  testing: boolean
  onTestQueued: () => void
  onChanged: () => Promise<void>
}) {
  const { t, i18n } = useTranslation()
  const id = useId()
  const [draft, setDraft] = useState<BalanceConfig>(
    props.monitor ?? {
      provider: 'new-api',
      base_url: '',
      upstream_user_id: 0,
      query_minutes: 15,
      refresh_minutes: 0,
      alert_enabled: false,
      threshold_usd: 0,
      phone: '',
    }
  )
  const [access, setAccess] = useState('')
  const [refresh, setRefresh] = useState('')
  const [confirm, setConfirm] = useState(false)
  const [feedback, setFeedback] = useState('')
  const monitor = props.monitor
  const mutation = useMutation({
    mutationFn: async (operation: 'save' | 'test' | 'query' | 'remove') => {
      const path = upstreamBalancePath(props.channelId)
      try {
        if (operation === 'remove') {
          requireServerSuccess((await api.delete(path)).data)
        } else if (operation === 'query') {
          await queueUpstreamBalance(props.channelId)
        } else {
          requireServerSuccess(
            (
              await api.put(path, {
                provider: draft.provider,
                base_url: draft.base_url,
                upstream_user_id: draft.upstream_user_id,
                query_minutes: draft.query_minutes,
                refresh_minutes: draft.refresh_minutes,
                alert_enabled: draft.alert_enabled,
                threshold_usd: draft.threshold_usd,
                phone: draft.phone,
                access_token: access,
                refresh_token: refresh,
              })
            ).data
          )
          if (operation === 'test') await queueUpstreamBalance(props.channelId)
        }
      } catch (error) {
        throw new Error(
          getServerErrorMessage(
            error,
            t('Unable to save upstream balance settings')
          )
        )
      }
      return operation
    },
    onSuccess: async (operation) => {
      if (operation === 'save' || operation === 'test') {
        setAccess('')
        setRefresh('')
        setFeedback(t('Settings saved successfully'))
      }
      if (operation === 'test' || operation === 'query') props.onTestQueued()
      setConfirm(false)
      await props.onChanged()
    },
  })
  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language)
  let renewalStatus = ''
  if (
    monitor?.refresh_error === 'credentials' ||
    monitor?.refresh_error === 'refresh_unknown'
  ) {
    renewalStatus = t('Renewal failed. Replace the upstream credentials.')
  } else if (monitor?.refresh_error === 'storage') {
    renewalStatus = t('Unable to read the saved credentials.')
  } else if (monitor?.access_expires_at) {
    renewalStatus = t('Access token expires at {{time}}', {
      time: formatTimestamp(monitor.access_expires_at),
    })
  }
  let alertStatus = ''
  if (monitor?.alert_state === 'submitted') {
    alertStatus = t('Voice alert submitted')
  } else if (
    monitor?.alert_state === 'sending' ||
    monitor?.alert_state === 'unknown'
  ) {
    alertStatus = t('Voice alert delivery is unconfirmed')
  } else if (monitor?.alert_state === 'rejected') {
    alertStatus = t('Voice alert rejected by Tencent Cloud')
  } else if (monitor?.alert_state === 'unconfigured') {
    alertStatus = t('Configure Tencent Cloud voice alerts in System Settings.')
  }

  return (
    <>
      <form
        className='grid gap-4'
        onSubmit={(event) => {
          event.preventDefault()
          event.stopPropagation()
          setFeedback('')
          const submitter = (event.nativeEvent as SubmitEvent).submitter
          mutation.mutate(
            submitter?.getAttribute('value') === 'test' ? 'test' : 'save'
          )
        }}
      >
        {monitor && (
          <div
            aria-live='polite'
            className='bg-muted/50 grid gap-1 rounded-md p-3 text-sm'
          >
            <p className='font-medium'>
              {monitor.balance_usd === null
                ? t('No successful balance query yet')
                : formatCurrencyFromUSD(monitor.balance_usd, { locale })}
            </p>
            {monitor.checked_at > 0 && (
              <p>
                {t('Last successful query at {{time}}', {
                  time: formatTimestamp(monitor.checked_at),
                })}
              </p>
            )}
            {monitor.query_pending && <p>{t('Balance query queued')}</p>}
            {monitor.query_error && (
              <p>
                {monitor.query_error === 'credentials'
                  ? t('Upstream credentials were rejected.')
                  : t('Failed to query the upstream balance.')}
              </p>
            )}
            {renewalStatus && <p>{renewalStatus}</p>}
            {alertStatus && <p>{alertStatus}</p>}
          </div>
        )}
        <fieldset
          className='grid gap-3'
          disabled={mutation.isPending || props.testing}
        >
          <div className='grid gap-1.5'>
            <Label id={`${id}-provider`}>{t('Upstream platform')}</Label>
            <Select
              value={draft.provider}
              onValueChange={(value) => {
                if (value === 'new-api' || value === 'sub2api') {
                  setDraft({ ...draft, provider: value })
                }
              }}
            >
              <SelectTrigger
                aria-labelledby={`${id}-provider`}
                className='w-full'
              >
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value='new-api'>new-api</SelectItem>
                <SelectItem value='sub2api'>Sub2API</SelectItem>
              </SelectContent>
            </Select>
          </div>
          <div className='grid gap-1.5'>
            <Label htmlFor={`${id}-url`}>{t('Upstream URL')}</Label>
            <Input
              id={`${id}-url`}
              type='url'
              placeholder='https://'
              required
              value={draft.base_url}
              onChange={(e) => setDraft({ ...draft, base_url: e.target.value })}
            />
          </div>
          {draft.provider === 'new-api' && (
            <div className='grid gap-1.5'>
              <Label htmlFor={`${id}-user`}>
                {t('Legacy new-api user ID')}
              </Label>
              <Input
                id={`${id}-user`}
                type='number'
                min={0}
                value={draft.upstream_user_id || ''}
                onChange={(e) =>
                  setDraft({
                    ...draft,
                    upstream_user_id: Number(e.target.value),
                  })
                }
              />
            </div>
          )}
          <div className='grid gap-1.5'>
            <Label htmlFor={`${id}-access`}>{t('Access token')}</Label>
            <PasswordInput
              id={`${id}-access`}
              autoComplete='off'
              maxLength={8192}
              value={access}
              onChange={(e) => setAccess(e.target.value)}
            />
          </div>
          <div className='grid gap-1.5'>
            <Label htmlFor={`${id}-refresh`}>
              {draft.provider === 'new-api'
                ? t('Refresh cookie (new_api_refresh)')
                : t('Refresh token')}
            </Label>
            <PasswordInput
              id={`${id}-refresh`}
              autoComplete='off'
              maxLength={8192}
              value={refresh}
              onChange={(e) => setRefresh(e.target.value)}
            />
          </div>
          {monitor && (
            <p className='text-muted-foreground text-xs'>
              {t(
                'Leave both credential fields blank to keep the saved credentials.'
              )}
            </p>
          )}
          <div className='grid gap-3 sm:grid-cols-2'>
            <div className='grid gap-1.5'>
              <Label htmlFor={`${id}-query`}>
                {t('Balance query interval (minutes)')}
              </Label>
              <Input
                id={`${id}-query`}
                type='number'
                min={0}
                max={1440}
                required
                value={draft.query_minutes}
                onChange={(e) =>
                  setDraft({ ...draft, query_minutes: Number(e.target.value) })
                }
              />
            </div>
            <div className='grid gap-1.5'>
              <Label htmlFor={`${id}-renew`}>
                {t('Renewal check interval (minutes)')}
              </Label>
              <Input
                id={`${id}-renew`}
                type='number'
                min={0}
                max={1440}
                required
                value={draft.refresh_minutes}
                onChange={(e) =>
                  setDraft({
                    ...draft,
                    refresh_minutes: Number(e.target.value),
                  })
                }
              />
            </div>
          </div>
          <p className='text-muted-foreground text-xs'>
            {t('Set an interval to 0 to turn off that schedule.')}
          </p>
          <div className='flex items-center gap-2'>
            <Switch
              id={`${id}-alert`}
              checked={draft.alert_enabled}
              onCheckedChange={(checked) =>
                setDraft({ ...draft, alert_enabled: checked })
              }
            />
            <Label htmlFor={`${id}-alert`}>
              {t('Low balance phone alert')}
            </Label>
          </div>
          {draft.alert_enabled && (
            <div className='grid gap-3 sm:grid-cols-2'>
              <div className='grid gap-1.5'>
                <Label htmlFor={`${id}-threshold`}>
                  {t('Alert threshold (USD)')}
                </Label>
                <Input
                  id={`${id}-threshold`}
                  type='number'
                  min={0}
                  step='any'
                  required
                  value={draft.threshold_usd}
                  onChange={(e) =>
                    setDraft({
                      ...draft,
                      threshold_usd: Number(e.target.value),
                    })
                  }
                />
              </div>
              <div className='grid gap-1.5'>
                <Label htmlFor={`${id}-phone`}>{t('Phone number')}</Label>
                <Input
                  id={`${id}-phone`}
                  type='tel'
                  required
                  placeholder='+8613800000000'
                  value={draft.phone}
                  onChange={(e) =>
                    setDraft({ ...draft, phone: e.target.value })
                  }
                />
              </div>
            </div>
          )}
        </fieldset>
        {mutation.isError && (
          <p className='text-destructive' role='alert'>
            {getServerErrorMessage(
              mutation.error,
              t('Unable to save upstream balance settings')
            )}
          </p>
        )}
        {feedback && <p role='status'>{feedback}</p>}
        <DialogFooter>
          {monitor && (
            <Button
              type='button'
              variant='outline'
              disabled={mutation.isPending || props.testing}
              onClick={() => setConfirm(true)}
            >
              {t('Remove')}
            </Button>
          )}
          {monitor && props.canQuery && (
            <Button
              type='button'
              variant='outline'
              disabled={
                mutation.isPending || props.testing || monitor.query_pending
              }
              onClick={() => mutation.mutate('query')}
            >
              {t('Query now')}
            </Button>
          )}
          {props.canQuery && (
            <Button
              type='submit'
              value='test'
              variant='outline'
              disabled={
                mutation.isPending || props.testing || monitor?.query_pending
              }
            >
              {t('Save and test')}
            </Button>
          )}
          <Button type='submit' disabled={mutation.isPending || props.testing}>
            {t('Save')}
          </Button>
        </DialogFooter>
      </form>
      <ConfirmDialog
        open={confirm}
        onOpenChange={setConfirm}
        title={t('Remove upstream balance monitoring?')}
        desc={t(
          'This removes the saved credentials and stops the balance tasks and phone alerts.'
        )}
        confirmText={t('Remove')}
        destructive
        isLoading={mutation.isPending}
        handleConfirm={() => mutation.mutate('remove')}
      />
    </>
  )
}
