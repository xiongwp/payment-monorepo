import { useEffect, useState } from 'react'
import { useTranslation } from 'react-i18next'
import type { TFunction } from 'i18next'
import { Card, Descriptions, Button, Typography, Alert, Space, message, Spin } from 'antd'
import { PlayCircleOutlined, ReloadOutlined } from '@ant-design/icons'
import {
  getTccArchiveConfig,
  runTccArchiveNow,
  type TccArchiveConfig,
} from '../../api/accounting'

const { Title, Paragraph, Text } = Typography

function formatInterval(seconds: number, t: TFunction): string {
  if (seconds <= 0) return t('interval.none')
  if (seconds % 3600 === 0) return t('interval.hours', { value: seconds / 3600 })
  if (seconds % 60 === 0) return t('interval.minutes', { value: seconds / 60 })
  return t('interval.seconds', { value: seconds })
}

export default function TccArchive() {
  const { t } = useTranslation('tcc')
  const [loading, setLoading] = useState(false)
  const [running, setRunning] = useState(false)
  const [cfg, setCfg] = useState<TccArchiveConfig | null>(null)

  const load = () => {
    setLoading(true)
    getTccArchiveConfig()
      .then(setCfg)
      .catch((e) => message.error(e instanceof Error ? e.message : t('archive.loadFailed')))
      .finally(() => setLoading(false))
  }
  useEffect(load, [])

  const handleRunNow = async () => {
    setRunning(true)
    try {
      const resp = await runTccArchiveNow()
      message.success(resp.message || t('archive.runNowSuccess'))
    } catch (e) {
      message.error(e instanceof Error ? e.message : t('archive.runNowFailed'))
    } finally {
      setRunning(false)
    }
  }

  return (
    <div>
      <Title level={3}>{t('archive.title')}</Title>
      <Paragraph type="secondary">
        {t('archive.intro')}
      </Paragraph>

      <Alert
        type="info"
        showIcon
        style={{ marginBottom: 16 }}
        message={t('archive.runModeTitle')}
        description={
          <>
            {t('archive.runModeDescPrefix')}<b>{t('archive.intervalSecondsTag')}</b>{t('archive.runModeDescMiddle')}<b>{t('archive.batchSizeTag')}</b>{t('archive.runModeDescSuffix')}
            <br />
            {t('archive.metricsLine')}<Text code>{t('archive.metricArchived')}</Text>{t('archive.metricArchivedDesc')}
            <Text code>{t('archive.metricErrors')}</Text>{t('archive.metricErrorsDesc')}
          </>
        }
      />

      <Card
        title={t('archive.configTitle')}
        extra={<Button icon={<ReloadOutlined />} onClick={load}>{t('archive.refresh')}</Button>}
        style={{ marginBottom: 16 }}
      >
        {loading ? (
          <Spin />
        ) : cfg ? (
          <Descriptions bordered column={1} size="middle">
            <Descriptions.Item label={t('archive.intervalLabel')}>
              {t('archive.intervalValue', { seconds: cfg.interval_seconds, display: formatInterval(cfg.interval_seconds, t) })}
            </Descriptions.Item>
            <Descriptions.Item label={t('archive.retentionLabel')}>
              {t('archive.retentionValue', { days: cfg.retention_days })}
            </Descriptions.Item>
            <Descriptions.Item label={t('archive.batchSizeLabel')}>
              {t('archive.batchSizeValue', { rows: cfg.batch_size })}
            </Descriptions.Item>
          </Descriptions>
        ) : (
          <Alert type="warning" message={t('archive.configMissing')} />
        )}
      </Card>

      <Card title={t('archive.runNowTitle')}>
        <Paragraph>
          {t('archive.runNowDesc', { batch: cfg?.batch_size ?? 1000 })}
        </Paragraph>
        <Space>
          <Button
            type="primary"
            icon={<PlayCircleOutlined />}
            onClick={handleRunNow}
            loading={running}
          >
            {t('archive.runNowButton')}
          </Button>
        </Space>
      </Card>
    </div>
  )
}
