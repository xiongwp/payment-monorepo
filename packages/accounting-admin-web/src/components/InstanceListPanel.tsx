import { useCallback, useEffect, useState } from 'react'
import {
  Alert,
  Badge,
  Button,
  Card,
  message,
  Space,
  Table,
  Tooltip,
} from 'antd'
import { ReloadOutlined, SyncOutlined, WarningOutlined } from '@ant-design/icons'
import type { ColumnsType } from 'antd/es/table'
import {
  listServiceInstances,
  reloadBufferAccountsToInstance,
  reloadHotAccountsToInstance,
} from '../api/accounting'
import type { ServiceInstance } from '../types/accounting'

interface Props {
  type: 'hot' | 'buffer'
}

const LABEL = {
  hot: '热点账户',
  buffer: '缓冲记账配置',
}

export default function InstanceListPanel({ type }: Props) {
  const [instances, setInstances] = useState<ServiceInstance[]>([])
  const [loading, setLoading] = useState(false)
  const [fetchError, setFetchError] = useState<string | null>(null)
  const [reloading, setReloading] = useState<Record<string, boolean>>({})

  const fetchInstances = useCallback(async () => {
    setLoading(true)
    setFetchError(null)
    try {
      const items = await listServiceInstances()
      setInstances(items ?? [])
    } catch (err: unknown) {
      setInstances([])
      setFetchError((err as { message?: string })?.message ?? '获取实例列表失败')
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => { fetchInstances() }, [fetchInstances])

  const doReload = async (instanceId?: string) => {
    const key = instanceId ?? '__all__'
    setReloading(prev => ({ ...prev, [key]: true }))
    try {
      const res = type === 'hot'
        ? await reloadHotAccountsToInstance(instanceId)
        : await reloadBufferAccountsToInstance(instanceId)
      const target = instanceId ?? '所有实例'
      message.success(`${LABEL[type]}已推送到 ${target}，共 ${res.count} 个账户`)
    } catch (err: unknown) {
      message.error((err as { message?: string })?.message ?? '推送失败')
    } finally {
      setReloading(prev => ({ ...prev, [key]: false }))
    }
  }

  const isAlive = (inst: ServiceInstance) =>
    Date.now() - new Date(inst.last_heartbeat).getTime() < 60_000

  const columns: ColumnsType<ServiceInstance> = [
    {
      title: '状态',
      width: 72,
      render: (_: unknown, record: ServiceInstance) =>
        isAlive(record)
          ? <Badge status="success" text="活跃" />
          : <Badge status="error" text="失联" />,
    },
    {
      title: '实例 ID',
      dataIndex: 'instance_id',
    },
    {
      title: '最后心跳',
      dataIndex: 'last_heartbeat',
      width: 170,
      render: (v: string) => v ? new Date(v).toLocaleString('zh-CN') : '-',
    },
    {
      title: '操作',
      width: 100,
      render: (_: unknown, record: ServiceInstance) => (
        <Tooltip title={`将${LABEL[type]}推送到该实例`}>
          <Button
            size="small"
            icon={<SyncOutlined />}
            loading={reloading[record.instance_id]}
            onClick={() => doReload(record.instance_id)}
          >
            推送
          </Button>
        </Tooltip>
      ),
    },
  ]

  const allLoading = reloading['__all__']

  return (
    <Card
      size="small"
      title={`服务实例列表（${instances.length} 个）`}
      style={{ marginTop: 24 }}
      extra={
        <Space>
          <Tooltip title={`向所有活跃实例推送${LABEL[type]}`}>
            <Button
              type="primary"
              size="small"
              icon={<SyncOutlined spin={allLoading} />}
              loading={allLoading}
              disabled={!!fetchError}
              onClick={() => doReload()}
            >
              全量推送
            </Button>
          </Tooltip>
          <Button
            size="small"
            icon={<ReloadOutlined />}
            onClick={fetchInstances}
            loading={loading}
          >
            刷新
          </Button>
        </Space>
      }
    >
      {fetchError ? (
        <Alert
          type="warning"
          icon={<WarningOutlined />}
          showIcon
          message="无法获取实例列表"
          description={fetchError}
          action={
            <Button size="small" onClick={fetchInstances} loading={loading}>
              重试
            </Button>
          }
        />
      ) : (
        <Table
          rowKey="instance_id"
          columns={columns}
          dataSource={instances}
          loading={loading}
          pagination={false}
          size="small"
        />
      )}
    </Card>
  )
}
