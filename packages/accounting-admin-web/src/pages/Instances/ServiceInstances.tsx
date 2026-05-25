import { useCallback, useEffect, useState } from 'react'
import {
  Badge,
  Button,
  message,
  Space,
  Table,
  Tooltip,
} from 'antd'
import {
  ReloadOutlined,
  SyncOutlined,
} from '@ant-design/icons'
import type { ColumnsType } from 'antd/es/table'
import { useTranslation } from 'react-i18next'
import {
  listServiceInstances,
  reloadBufferAccountsToInstance,
  reloadHotAccountsToInstance,
} from '../../api/accounting'
import type { ServiceInstance } from '../../types/accounting'

export default function ServiceInstancesPage() {
  const { t } = useTranslation('instances')
  const [data, setData] = useState<ServiceInstance[]>([])
  const [loading, setLoading] = useState(false)
  const [reloadingHot, setReloadingHot] = useState<Record<string, boolean>>({})
  const [reloadingBuffer, setReloadingBuffer] = useState<Record<string, boolean>>({})

  const fetchList = useCallback(async () => {
    setLoading(true)
    try {
      const items = await listServiceInstances()
      setData(items ?? [])
    } catch (err: unknown) {
      message.error((err as { message?: string })?.message ?? t('messages.loadFailed'))
    } finally {
      setLoading(false)
    }
  }, [t])

  useEffect(() => { fetchList() }, [fetchList])

  const handleReloadHot = async (instanceId?: string) => {
    const key = instanceId ?? '__all__'
    setReloadingHot(prev => ({ ...prev, [key]: true }))
    try {
      const res = await reloadHotAccountsToInstance(instanceId)
      message.success(t('messages.hotReloadSuccess', { count: res.count }))
    } catch (err: unknown) {
      message.error((err as { message?: string })?.message ?? t('messages.reloadFailed'))
    } finally {
      setReloadingHot(prev => ({ ...prev, [key]: false }))
    }
  }

  const handleReloadBuffer = async (instanceId?: string) => {
    const key = instanceId ?? '__all__'
    setReloadingBuffer(prev => ({ ...prev, [key]: true }))
    try {
      const res = await reloadBufferAccountsToInstance(instanceId)
      message.success(t('messages.bufferReloadSuccess', { count: res.count }))
    } catch (err: unknown) {
      message.error((err as { message?: string })?.message ?? t('messages.reloadFailed'))
    } finally {
      setReloadingBuffer(prev => ({ ...prev, [key]: false }))
    }
  }

  const isAlive = (inst: ServiceInstance) => {
    const threshold = 60 * 1000 // 60s
    return Date.now() - new Date(inst.last_heartbeat).getTime() < threshold
  }

  const columns: ColumnsType<ServiceInstance> = [
    {
      title: t('columns.status'),
      width: 70,
      render: (_: unknown, record: ServiceInstance) =>
        isAlive(record)
          ? <Badge status="success" text={t('statusTags.alive')} />
          : <Badge status="error" text={t('statusTags.dead')} />,
    },
    {
      title: t('columns.instanceId'),
      dataIndex: 'instance_id',
      width: 200,
    },
    {
      title: t('columns.host'),
      dataIndex: 'host',
      width: 160,
    },
    {
      title: t('columns.httpAdminPort'),
      dataIndex: 'http_admin_port',
      width: 130,
    },
    {
      title: t('columns.grpcPort'),
      dataIndex: 'grpc_port',
      width: 100,
    },
    {
      title: t('columns.lastHeartbeat'),
      dataIndex: 'last_heartbeat',
      width: 180,
      render: (v: string) => v ? new Date(v).toLocaleString('zh-CN') : '-',
    },
    {
      title: t('columns.startedAt'),
      dataIndex: 'started_at',
      width: 180,
      render: (v: string) => v ? new Date(v).toLocaleString('zh-CN') : '-',
    },
    {
      title: t('columns.actions'),
      width: 220,
      render: (_: unknown, record: ServiceInstance) => (
        <Space>
          <Tooltip title={t('rowActions.hotTooltip')}>
            <Button
              size="small"
              loading={reloadingHot[record.instance_id]}
              icon={<SyncOutlined />}
              onClick={() => handleReloadHot(record.instance_id)}
            >
              {t('rowActions.hot')}
            </Button>
          </Tooltip>
          <Tooltip title={t('rowActions.bufferTooltip')}>
            <Button
              size="small"
              loading={reloadingBuffer[record.instance_id]}
              icon={<SyncOutlined />}
              onClick={() => handleReloadBuffer(record.instance_id)}
            >
              {t('rowActions.buffer')}
            </Button>
          </Tooltip>
        </Space>
      ),
    },
  ]

  return (
    <div>
      <div style={{ display: 'flex', justifyContent: 'space-between', marginBottom: 16 }}>
        <Space>
          <Tooltip title={t('toolbar.pushAllHotTooltip')}>
            <Button
              type="primary"
              icon={<SyncOutlined spin={reloadingHot['__all__']} />}
              loading={reloadingHot['__all__']}
              onClick={() => handleReloadHot()}
            >
              {t('toolbar.pushAllHot')}
            </Button>
          </Tooltip>
          <Tooltip title={t('toolbar.pushAllBufferTooltip')}>
            <Button
              icon={<SyncOutlined spin={reloadingBuffer['__all__']} />}
              loading={reloadingBuffer['__all__']}
              onClick={() => handleReloadBuffer()}
            >
              {t('toolbar.pushAllBuffer')}
            </Button>
          </Tooltip>
        </Space>
        <Button icon={<ReloadOutlined />} onClick={fetchList} loading={loading}>
          {t('common:actions.refresh')}
        </Button>
      </div>

      <Table
        rowKey="instance_id"
        columns={columns}
        dataSource={data}
        loading={loading}
        pagination={false}
        size="middle"
      />
    </div>
  )
}
