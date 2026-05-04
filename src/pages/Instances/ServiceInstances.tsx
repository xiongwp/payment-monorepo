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
import {
  listServiceInstances,
  reloadBufferAccountsToInstance,
  reloadHotAccountsToInstance,
} from '../../api/accounting'
import type { ServiceInstance } from '../../types/accounting'

export default function ServiceInstancesPage() {
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
      message.error((err as { message?: string })?.message ?? '加载失败')
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => { fetchList() }, [fetchList])

  const handleReloadHot = async (instanceId?: string) => {
    const key = instanceId ?? '__all__'
    setReloadingHot(prev => ({ ...prev, [key]: true }))
    try {
      const res = await reloadHotAccountsToInstance(instanceId)
      message.success(`热点账户热重载完成，共 ${res.count} 个账户`)
    } catch (err: unknown) {
      message.error((err as { message?: string })?.message ?? '热重载失败')
    } finally {
      setReloadingHot(prev => ({ ...prev, [key]: false }))
    }
  }

  const handleReloadBuffer = async (instanceId?: string) => {
    const key = instanceId ?? '__all__'
    setReloadingBuffer(prev => ({ ...prev, [key]: true }))
    try {
      const res = await reloadBufferAccountsToInstance(instanceId)
      message.success(`缓冲记账配置热重载完成，共 ${res.count} 个账户`)
    } catch (err: unknown) {
      message.error((err as { message?: string })?.message ?? '热重载失败')
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
      title: '状态',
      width: 70,
      render: (_: unknown, record: ServiceInstance) =>
        isAlive(record)
          ? <Badge status="success" text="活跃" />
          : <Badge status="error" text="失联" />,
    },
    {
      title: '实例 ID',
      dataIndex: 'instance_id',
      width: 200,
    },
    {
      title: 'Host',
      dataIndex: 'host',
      width: 160,
    },
    {
      title: 'HTTP Admin 端口',
      dataIndex: 'http_admin_port',
      width: 130,
    },
    {
      title: 'gRPC 端口',
      dataIndex: 'grpc_port',
      width: 100,
    },
    {
      title: '最后心跳',
      dataIndex: 'last_heartbeat',
      width: 180,
      render: (v: string) => v ? new Date(v).toLocaleString('zh-CN') : '-',
    },
    {
      title: '启动时间',
      dataIndex: 'started_at',
      width: 180,
      render: (v: string) => v ? new Date(v).toLocaleString('zh-CN') : '-',
    },
    {
      title: '操作',
      width: 220,
      render: (_: unknown, record: ServiceInstance) => (
        <Space>
          <Tooltip title="热重载热点账户白名单到此实例">
            <Button
              size="small"
              loading={reloadingHot[record.instance_id]}
              icon={<SyncOutlined />}
              onClick={() => handleReloadHot(record.instance_id)}
            >
              热点账户
            </Button>
          </Tooltip>
          <Tooltip title="热重载缓冲记账配置到此实例">
            <Button
              size="small"
              loading={reloadingBuffer[record.instance_id]}
              icon={<SyncOutlined />}
              onClick={() => handleReloadBuffer(record.instance_id)}
            >
              缓冲记账
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
          <Tooltip title="向所有活跃实例推送热点账户白名单">
            <Button
              type="primary"
              icon={<SyncOutlined spin={reloadingHot['__all__']} />}
              loading={reloadingHot['__all__']}
              onClick={() => handleReloadHot()}
            >
              全量推送热点账户
            </Button>
          </Tooltip>
          <Tooltip title="向所有活跃实例推送缓冲记账配置">
            <Button
              icon={<SyncOutlined spin={reloadingBuffer['__all__']} />}
              loading={reloadingBuffer['__all__']}
              onClick={() => handleReloadBuffer()}
            >
              全量推送缓冲记账
            </Button>
          </Tooltip>
        </Space>
        <Button icon={<ReloadOutlined />} onClick={fetchList} loading={loading}>
          刷新
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
