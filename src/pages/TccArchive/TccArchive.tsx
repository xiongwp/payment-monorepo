import { useEffect, useState } from 'react'
import { Card, Descriptions, Button, Typography, Alert, Space, message, Spin } from 'antd'
import { PlayCircleOutlined, ReloadOutlined } from '@ant-design/icons'
import {
  getTccArchiveConfig,
  runTccArchiveNow,
  type TccArchiveConfig,
} from '../../api/accounting'

const { Title, Paragraph, Text } = Typography

function formatInterval(seconds: number): string {
  if (seconds <= 0) return '-'
  if (seconds % 3600 === 0) return `${seconds / 3600} 小时`
  if (seconds % 60 === 0) return `${seconds / 60} 分钟`
  return `${seconds} 秒`
}

export default function TccArchive() {
  const [loading, setLoading] = useState(false)
  const [running, setRunning] = useState(false)
  const [cfg, setCfg] = useState<TccArchiveConfig | null>(null)

  const load = () => {
    setLoading(true)
    getTccArchiveConfig()
      .then(setCfg)
      .catch((e) => message.error(e instanceof Error ? e.message : '加载失败'))
      .finally(() => setLoading(false))
  }
  useEffect(load, [])

  const handleRunNow = async () => {
    setRunning(true)
    try {
      const resp = await runTccArchiveNow()
      message.success(resp.message || '已触发归档（异步执行）')
    } catch (e) {
      message.error(e instanceof Error ? e.message : '触发失败')
    } finally {
      setRunning(false)
    }
  }

  return (
    <div>
      <Title level={3}>TCC 归档</Title>
      <Paragraph type="secondary">
        定期清理 <Text code>tcc_transaction</Text> 表中已终态（CONFIRMED / CANCELLED）且超过保留期的
        分支记录，防止表无限增长。业务流水、凭证、日切快照都在独立表长期保存，归档 tcc_transaction
        不影响审计。
      </Paragraph>

      <Alert
        type="info"
        showIcon
        style={{ marginBottom: 16 }}
        message="运行方式"
        description={
          <>
            后台 worker 每 <b>interval_seconds</b> 自动扫一遍所有 100 个分片，按 <b>batch_size</b> 分批
            DELETE 超期终态记录；手动点击"立即归档"会额外触发一轮扫描（两者并发安全）。
            <br />
            运行成效观察 Prometheus 指标：<Text code>accounting_tcc_archived_total</Text>（删除行数累计）、
            <Text code>accounting_tcc_archive_errors_total</Text>（失败次数）。
          </>
        }
      />

      <Card
        title="当前归档配置"
        extra={<Button icon={<ReloadOutlined />} onClick={load}>刷新</Button>}
        style={{ marginBottom: 16 }}
      >
        {loading ? (
          <Spin />
        ) : cfg ? (
          <Descriptions bordered column={1} size="middle">
            <Descriptions.Item label="扫描间隔 (interval_seconds)">
              {cfg.interval_seconds} 秒 · 约 {formatInterval(cfg.interval_seconds)}
            </Descriptions.Item>
            <Descriptions.Item label="保留天数 (retention_days)">
              {cfg.retention_days} 天（updated_at 早于此时限的终态分支会被删除）
            </Descriptions.Item>
            <Descriptions.Item label="单分片批次大小 (batch_size)">
              {cfg.batch_size} 行 / 分片 / 轮
            </Descriptions.Item>
          </Descriptions>
        ) : (
          <Alert type="warning" message="未能加载配置" />
        )}
      </Card>

      <Card title="立即归档">
        <Paragraph>
          立即触发一轮归档扫描（每分片 {cfg?.batch_size ?? 1000} 行上限，所有分片串行）。
          接口立即返回；真正的 DELETE 在后台 goroutine 中执行，需查指标看进度。
        </Paragraph>
        <Space>
          <Button
            type="primary"
            icon={<PlayCircleOutlined />}
            onClick={handleRunNow}
            loading={running}
          >
            立即归档
          </Button>
        </Space>
      </Card>
    </div>
  )
}
