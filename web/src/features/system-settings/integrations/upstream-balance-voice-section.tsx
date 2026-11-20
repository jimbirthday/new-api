import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
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
import { useId, useRef } from 'react'
import { useTranslation } from 'react-i18next'

import { PasswordInput } from '@/components/password-input'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { api } from '@/lib/api'
import {
  getServerErrorMessage,
  requireServerSuccess,
} from '@/lib/server-error-message'

import { SettingsForm } from '../components/settings-form-layout'
import { SettingsSection } from '../components/settings-section'

const endpoint = '/api/option/upstream_balance_voice'

type VoiceConfig = {
  app_id: string
  template_id: string
  configured: boolean
}

export function UpstreamBalanceVoiceSection() {
  const { t } = useTranslation()
  const id = useId()
  const client = useQueryClient()
  const result = useQuery({
    queryKey: [endpoint],
    queryFn: async () => {
      const response = await api.get<{ success: boolean; data: VoiceConfig }>(
        endpoint,
        { disableDuplicate: true }
      )
      return requireServerSuccess(response.data).data
    },
  })
  const formRef = useRef<HTMLFormElement>(null)
  const mutation = useMutation({
    mutationFn: async () => {
      if (!formRef.current) return
      try {
        const data = new FormData(formRef.current)
        requireServerSuccess(
          (await api.put(endpoint, Object.fromEntries(data))).data
        )
      } catch (error) {
        throw new Error(
          getServerErrorMessage(error, t('Unable to save voice alert settings'))
        )
      }
    },
    onSuccess: async () => {
      for (const name of ['secret_id', 'secret_key']) {
        const input = formRef.current?.elements.namedItem(name)
        if (input instanceof HTMLInputElement) input.value = ''
      }
      await client.invalidateQueries({ queryKey: [endpoint] })
    },
  })
  return (
    <SettingsSection title={t('Tencent Cloud voice alerts')}>
      {result.isPending && <p role='status'>{t('Loading...')}</p>}
      {result.isError && (
        <p role='alert'>
          {getServerErrorMessage(
            result.error,
            t('Unable to load voice alert settings')
          )}
        </p>
      )}
      {result.data && (
        <SettingsForm
          key={JSON.stringify(result.data)}
          ref={formRef}
          onSubmit={(event) => {
            event.preventDefault()
            mutation.mutate()
          }}
        >
          <p className='text-muted-foreground text-sm'>
            {t(
              'Use an approved Tencent Cloud voice template without parameters.'
            )}
          </p>
          <div data-slot='form-item' className='grid gap-1.5'>
            <Label htmlFor={`${id}-app`}>{t('Voice app ID')}</Label>
            <Input
              id={`${id}-app`}
              name='app_id'
              inputMode='numeric'
              pattern='[0-9]{1,32}'
              required
              defaultValue={result.data.app_id}
            />
          </div>
          <div data-slot='form-item' className='grid gap-1.5'>
            <Label htmlFor={`${id}-template`}>{t('Voice template ID')}</Label>
            <Input
              id={`${id}-template`}
              name='template_id'
              inputMode='numeric'
              pattern='[0-9]{1,32}'
              required
              defaultValue={result.data.template_id}
            />
          </div>
          <div data-slot='form-item' className='grid gap-1.5'>
            <Label htmlFor={`${id}-secret-id`}>SecretId</Label>
            <PasswordInput
              id={`${id}-secret-id`}
              name='secret_id'
              autoComplete='off'
              maxLength={256}
              required={!result.data.configured}
            />
          </div>
          <div data-slot='form-item' className='grid gap-1.5'>
            <Label htmlFor={`${id}-secret-key`}>SecretKey</Label>
            <PasswordInput
              id={`${id}-secret-key`}
              name='secret_key'
              autoComplete='off'
              maxLength={256}
              required={!result.data.configured}
            />
          </div>
          {result.data.configured && (
            <p className='text-muted-foreground text-sm'>
              {t(
                'Leave both credential fields blank to keep the saved credentials.'
              )}
            </p>
          )}
          {mutation.isError && (
            <p className='text-destructive' role='alert'>
              {getServerErrorMessage(
                mutation.error,
                t('Unable to save voice alert settings')
              )}
            </p>
          )}
          {mutation.isSuccess && (
            <p role='status'>{t('Settings saved successfully')}</p>
          )}
          <div>
            <Button type='submit' disabled={mutation.isPending}>
              {t('Save')}
            </Button>
          </div>
        </SettingsForm>
      )}
    </SettingsSection>
  )
}
