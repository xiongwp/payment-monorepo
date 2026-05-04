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
      const verb = values.dryRun ? '已模拟（dry-run）' : '已重建'
      message.success(`${verb}：updated=${r.updated} skipped=${r.skipped} failed=${r.failed}`)
    } catch (err: unknown) {
      const msg = (err as { message?: string })?.message ?? '调用失败'
      setError(msg)
      message.error(msg)
    } finally {
      setLoading(false)
    }
  }

  const columns: ColumnsType<RebuildEntry> = [
    { title: '账户号', dataIndex: 'account_no', key: 'account_no', width: 240 },
    {
      title: 'Redis 当前',
      dataIndex: 'balance_before',
      key: 'balance_before',
      render: (v: string) => (v === '' || v == null ? <Tag color="default">cache miss</Tag> : <Text code>{v}</Text>),
    },
    {
      title: '目标余额',
      dataIndex: 'balance_after',
      key: 'balance_after',
      render: (v: string) => <Text code>{v}</Text>,
    },
    {
      title: '差量',
      key: 'delta',
      render: (_: unknown, r: RebuildEntry) => {
        if (r.balance_before === '' || r.balance_before == null) return <Tag color="orange">N/A</Tag>
        try {
          const before = BigInt(r.balance_before)
          const after = BigInt(r.balance_after)
          const diff = after - before
          if (diff === 0n) return <Tag color="green">一致</Tag>
          const sign = diff > 0n ? '+' : ''
          return <Tag color={diff > 0n ? 'blue' : 'red'}>{sign}{diff.toString()}</Tag>
        } catch {
          return <Tag color="default">-</Tag>
        }
      },
    },
    {
      title: '来源',
      dataIndex: 'source',
      key: 'source',
      render: (s: string) => {
        if (s === 'transaction_journal') return <Tag color="purple">流水回放</Tag>
        if (s === 'account') return <Tag color="blue">account 表</Tag>
        return s
      },
    },
    {
      title: '状态',
      key: 'status',
      render: (_: unknown, r: RebuildEntry) => {
        if (r.reason && !r.skipped) return <Tag color="red">失败</Tag>
        if (r.skipped) return <Tag color="default">skipped</Tag>
        return <Tag color="green">已写</Tag>
      },
    },
    {
      title: '说明',
      dataIndex: 'reason',
      key: 'reason',
      ellipsis: true,
    },
  ]

  return (
    <div>
      <Title level={3}>Redis 热账户重建</Title>
      <Paragraph type="secondary">
        从 MySQL 重建 Redis 热账户余额（仅 <Text code>hot_account_config</Text> 中 enabled=1 的账户）。
        典型场景：Redis 数据丢失 / 损坏 / 怀疑被脏写。<b>建议先 dry-run 看 diff 再正式执行。</b>
      </Paragraph>

      <Card style={{ marginBottom: 16 }}>
        <Form<FormValues>
          form={form}
          layout="vertical"
          initialValues={{ asOfMode: 'now', dryRun: true }}
          onFinish={onFinish}
        >
          <Form.Item label="时间点" name="asOfMode">
            <Radio.Group>
              <Radio value="now">当前 MySQL 余额（最常用，Redis 重启后恢复）</Radio>
              <Radio value="duration">回滚到 N 分钟前（怀疑最近写入有问题）</Radio>
              <Radio value="timestamp">回滚到指定时间戳</Radio>
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
                    label="时长（如 5m / 1h / 24h）"
                    name="asOfDuration"
                    rules={[{ required: true, message: '请输入时长' }]}
                  >
                    <Input style={{ width: 200 }} placeholder="5m" />
                  </Form.Item>
                )
              }
              if (mode === 'timestamp') {
                return (
                  <Form.Item
                    label="RFC3339 时间戳（如 2026-04-24T15:00:00Z）"
                    name="asOfTimestamp"
                    rules={[{ required: true, message: '请输入时间戳' }]}
                  >
                    <Input style={{ width: 320 }} placeholder="2026-04-24T15:00:00Z" />
                  </Form.Item>
                )
              }
              return null
            }}
          </Form.Item>

          <Form.Item
            label={
              <Space>
                <span>账户号过滤</span>
                <Tooltip title="留空 = 重建全部启用的热账户。多个账户用逗号或换行分隔。账户必须仍在 hot_account_config 中。">
                  <span style={{ color: '#888' }}>?</span>
                </Tooltip>
              </Space>
            }
            name="accountNos"
          >
            <Input.TextArea rows={3} placeholder="010100001-001, 010100002-002 ..." />
          </Form.Item>

          <Form.Item label="Dry run（只算 diff 不写 Redis）" name="dryRun" valuePropName="checked">
            <Switch checkedChildren="dry-run" unCheckedChildren="正式执行" />
          </Form.Item>

          <Form.Item>
            <Button
              type="primary"
              htmlType="submit"
              loading={loading}
              icon={<ReloadOutlined />}
              danger={!form.getFieldValue('dryRun')}
            >
              {form.getFieldValue('dryRun') ? '模拟重建' : '执行重建'}
            </Button>
          </Form.Item>
        </Form>
      </Card>

      {error && <Alert type="error" message={error} style={{ marginBottom: 16 }} />}

      {report && (
        <>
          <Card style={{ marginBottom: 16 }}>
            <Space size="large">
              <Statistic title="总数" value={report.total} />
              <Statistic title="已写入" value={report.updated} valueStyle={{ color: '#3f8600' }} />
              <Statistic title="跳过" value={report.skipped} />
              <Statistic title="失败" value={report.failed} valueStyle={{ color: report.failed > 0 ? '#cf1322' : undefined }} />
              <Statistic title="耗时" value={report.duration} />
              {report.dry_run && <Tag color="orange">dry-run</Tag>}
              {report.as_of && <Tag color="purple">as_of: {report.as_of}</Tag>}
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
