import { useState } from 'react'
import {
  Alert,
  Button,
  Card,
  Form,
  Input,
  Radio,
  Space,
  Statistic,
  Switch,
  Table,
  Tag,
  Tooltip,
  Typography,
  message,
} from 'antd'
import { ReloadOutlined } from '@ant-design/icons'
import type { ColumnsType } from 'antd/es/table'
import { useTranslation, Trans } from 'react-i18next'
import { rebuildHotAccounts } from '../../api/accounting'
import type { RebuildReport, RebuildEntry } from '../../api/accounting'

const { Title, Paragraph, Text } = Typography

type AsOfMode = 'now' | 'duration' | 'timestamp'

interface FormValues {
  asOfMode: AsOfMode
  asOfDuration?: string
  asOfTimestamp?: string
  accountNos?: string
  dryRun: boolean
}

export default function RedisRebuildPage() {
  const { t } = useTranslation('redis')
  const [form] = Form.useForm<FormValues>()
  const [loading, setLoading] = useState(false)
  const [report, setReport] = useState<RebuildReport | null>(null)
  const [error, setError] = useState<string | null>(null)

  const onFinish = async (values: FormValues) => {
    setLoading(true)
    setError(null)
    try {
      let asOf = ''
      switch (values.asOfMode) {
        case 'duration':
          asOf = values.asOfDuration?.trim() ?? ''
          break
        case 'timestamp':
          asOf = values.asOfTimestamp?.trim() ?? ''
          break
        case 'now':
        default:
          asOf = ''
      }
      const accountNos = (values.accountNos ?? '')
        .split(/[,\n]/)
        .map((s) => s.trim())
        .filter(Boolean)
      const r = await rebuildHotAccounts({
        as_of: asOf,
        account_nos: accountNos.length > 0 ? accountNos : undefined,
        dry_run: !!values.dryRun,
      })
      setReport(r)
      const verb = values.dryRun ? t('verbs.dryRun') : t('verbs.executed')
      message.success(t('messages.result', {
        verb,
        updated: r.updated,
        skipped: r.skipped,
        failed: r.failed,
      }))
    } catch (err: unknown) {
      const msg = (err as { message?: string })?.message ?? t('messages.callFailed')
      setError(msg)
      message.error(msg)
    } finally {
      setLoading(false)
    }
  }

  const columns: ColumnsType<RebuildEntry> = [
    { title: t('columns.accountNo'), dataIndex: 'account_no', key: 'account_no', width: 240 },
    {
      title: t('columns.redisCurrent'),
      dataIndex: 'balance_before',
      key: 'balance_before',
      render: (v: string) => (v === '' || v == null ? <Tag color="default">{t('tags.cacheMiss')}</Tag> : <Text code>{v}</Text>),
    },
    {
      title: t('columns.targetBalance'),
      dataIndex: 'balance_after',
      key: 'balance_after',
      render: (v: string) => <Text code>{v}</Text>,
    },
    {
      title: t('columns.delta'),
      key: 'delta',
      render: (_: unknown, r: RebuildEntry) => {
        if (r.balance_before === '' || r.balance_before == null) return <Tag color="orange">{t('tags.na')}</Tag>
        try {
          const before = BigInt(r.balance_before)
          const after = BigInt(r.balance_after)
          const diff = after - before
          if (diff === 0n) return <Tag color="green">{t('tags.consistent')}</Tag>
          const sign = diff > 0n ? '+' : ''
          return <Tag color={diff > 0n ? 'blue' : 'red'}>{sign}{diff.toString()}</Tag>
        } catch {
          return <Tag color="default">-</Tag>
        }
      },
    },
    {
      title: t('columns.source'),
      dataIndex: 'source',
      key: 'source',
      render: (s: string) => {
        if (s === 'transaction_journal') return <Tag color="purple">{t('tags.sourceJournal')}</Tag>
        if (s === 'account') return <Tag color="blue">{t('tags.sourceAccount')}</Tag>
        return s
      },
    },
    {
      title: t('columns.status'),
      key: 'status',
      render: (_: unknown, r: RebuildEntry) => {
        if (r.reason && !r.skipped) return <Tag color="red">{t('tags.failed')}</Tag>
        if (r.skipped) return <Tag color="default">{t('tags.skipped')}</Tag>
        return <Tag color="green">{t('tags.written')}</Tag>
      },
    },
    {
      title: t('columns.reason'),
      dataIndex: 'reason',
      key: 'reason',
      ellipsis: true,
    },
  ]

  return (
    <div>
      <Title level={3}>{t('title')}</Title>
      <Paragraph type="secondary">
        <Trans i18nKey="description" ns="redis" components={{ code: <Text code />, b: <b /> }} />
      </Paragraph>

      <Card style={{ marginBottom: 16 }}>
        <Form<FormValues>
          form={form}
          layout="vertical"
          initialValues={{ asOfMode: 'now', dryRun: true }}
          onFinish={onFinish}
        >
          <Form.Item label={t('form.asOfModeLabel')} name="asOfMode">
            <Radio.Group>
              <Radio value="now">{t('form.asOfModeNow')}</Radio>
              <Radio value="duration">{t('form.asOfModeDuration')}</Radio>
              <Radio value="timestamp">{t('form.asOfModeTimestamp')}</Radio>
            </Radio.Group>
          </Form.Item>

          <Form.Item
            shouldUpdate={(prev, cur) => prev.asOfMode !== cur.asOfMode}
            noStyle
          >
            {() => {
              const mode = form.getFieldValue('asOfMode') as AsOfMode
              if (mode === 'duration') {
                return (
                  <Form.Item
                    label={t('form.durationLabel')}
                    name="asOfDuration"
                    rules={[{ required: true, message: t('form.durationRequired') }]}
                  >
                    <Input style={{ width: 200 }} placeholder={t('form.durationPlaceholder')} />
                  </Form.Item>
                )
              }
              if (mode === 'timestamp') {
                return (
                  <Form.Item
                    label={t('form.timestampLabel')}
                    name="asOfTimestamp"
                    rules={[{ required: true, message: t('form.timestampRequired') }]}
                  >
                    <Input style={{ width: 320 }} placeholder={t('form.timestampPlaceholder')} />
                  </Form.Item>
                )
              }
              return null
            }}
          </Form.Item>

          <Form.Item
            label={
              <Space>
                <span>{t('form.accountNosLabel')}</span>
                <Tooltip title={t('form.accountNosTooltip')}>
                  <span style={{ color: '#888' }}>?</span>
                </Tooltip>
              </Space>
            }
            name="accountNos"
          >
            <Input.TextArea rows={3} placeholder={t('form.accountNosPlaceholder')} />
          </Form.Item>

          <Form.Item label={t('form.dryRunLabel')} name="dryRun" valuePropName="checked">
            <Switch checkedChildren={t('form.dryRunOn')} unCheckedChildren={t('form.dryRunOff')} />
          </Form.Item>

          <Form.Item>
            <Button
              type="primary"
              htmlType="submit"
              loading={loading}
              icon={<ReloadOutlined />}
              danger={!form.getFieldValue('dryRun')}
            >
              {form.getFieldValue('dryRun') ? t('submit.dryRun') : t('submit.execute')}
            </Button>
          </Form.Item>
        </Form>
      </Card>

      {error && <Alert type="error" message={error} style={{ marginBottom: 16 }} />}

      {report && (
        <>
          <Card style={{ marginBottom: 16 }}>
            <Space size="large">
              <Statistic title={t('stats.total')} value={report.total} />
              <Statistic title={t('stats.updated')} value={report.updated} valueStyle={{ color: '#3f8600' }} />
              <Statistic title={t('stats.skipped')} value={report.skipped} />
              <Statistic title={t('stats.failed')} value={report.failed} valueStyle={{ color: report.failed > 0 ? '#cf1322' : undefined }} />
              <Statistic title={t('stats.duration')} value={report.duration} />
              {report.dry_run && <Tag color="orange">{t('stats.dryRunTag')}</Tag>}
              {report.as_of && <Tag color="purple">{t('stats.asOfTag', { value: report.as_of })}</Tag>}
            </Space>
          </Card>

          <Table<RebuildEntry>
            columns={columns}
            dataSource={report.entries ?? []}
            rowKey={(r) => r.account_no}
            size="small"
            scroll={{ x: 1200 }}
            pagination={{ pageSize: 50, showSizeChanger: true }}
          />
        </>
      )}
    </div>
  )
}
