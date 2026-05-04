import { useState } from 'react'
import {
  Tabs,
  Input,
  Button,
  Table,
  Tag,
  Space,
  InputNumber,
  Descriptions,
  Alert,
  Modal,
  message,
} from 'antd'
import {
  SearchOutlined,
  ReloadOutlined,
  StopOutlined,
} from '@ant-design/icons'
import type { ColumnsType } from 'antd/es/table'
import {
  getTccStatus,
  listStuckTcc,
  cancelTcc,
  cancelTccBranch,
  tccRetryConfirmNow,
} from '../../api/accounting'
import {
  TccStatus,
  TCC_STATUS_COLOR,
  TCC_STATUS_LABEL,
} from '../../types/accounting'
import type { TccBranch, TccStatusResponse } from '../../types/accounting'
import { display as displayMoney } from '../../utils/money'

// TccBranch has no currency field on the response (balance_delta & frozen_amount
// are storage int64 strings). System default is PHP.
const FALLBACK_CURRENCY = 'PHP'

// ─── TCC 分支表格列定义 ───────────────────────────────────────────────────────

function branchColumns(
  onCancelBranch?: (b: TccBranch) => void,
): ColumnsType<TccBranch> {
  return [
    { title: 'Branch ID', dataIndex: 'branch_id', key: 'branch_id', ellipsis: true, width: 200 },
    { title: '账户号', dataIndex: 'account_no', key: 'account_no', width: 180 },
    {
      title: '余额增量',
      dataIndex: 'balance_delta',
      key: 'balance_delta',
      width: 140,
      render: (v: string) => {
        // balance_delta may be a storage int64 string; compare via BigInt to
        // avoid losing precision when the value exceeds 2^53.
        let isPositive = true
        try { isPositive = BigInt(v || '0') >= 0n } catch { /* fall back to string sign */ isPositive = !String(v).startsWith('-') }
        const formatted = displayMoney(v, FALLBACK_CURRENCY)
        return <span style={{ color: isPositive ? '#3f8600' : '#cf1322' }}>{isPositive && !formatted.startsWith('-') ? `+${formatted}` : formatted}</span>
      },
    },
    {
      title: '冻结金额',
      dataIndex: 'frozen_amount',
      key: 'frozen_amount',
      width: 140,
      render: (v: string) => displayMoney(v, FALLBACK_CURRENCY),
    },
    {
      title: '状态',
      dataIndex: 'status',
      key: 'status',
      width: 110,
      render: (s: TccStatus) => (
        <Tag color={TCC_STATUS_COLOR[s]}>{TCC_STATUS_LABEL[s] ?? s}</Tag>
      ),
    },
    { title: 'DB', dataIndex: 'db_index', key: 'db_index', width: 60 },
    { title: 'Table', dataIndex: 'table_index', key: 'table_index', width: 70 },
    { title: '创建时间', dataIndex: 'created_at', key: 'created_at', width: 180 },
    {
      title: 'TCC ID',
      dataIndex: 'tcc_id',
      key: 'tcc_id',
      ellipsis: true,
      width: 200,
    },
    ...(onCancelBranch
      ? [
          {
            title: '操作',
            key: 'action',
            width: 90,
            render: (_: unknown, record: TccBranch) =>
              record.status === TccStatus.TRYING ? (
                <Button
                  size="small"
                  danger
                  icon={<StopOutlined />}
                  onClick={() => onCancelBranch(record)}
                >
                  取消
                </Button>
              ) : null,
          },
        ]
      : []),
  ]
}

// ─── Tab1: 按 TCC ID 查询 ─────────────────────────────────────────────────────

function TccIdSearch() {
  const [tccId, setTccId] = useState('')
  const [loading, setLoading] = useState(false)
  const [result, setResult] = useState<TccStatusResponse | null>(null)

  const handleSearch = async () => {
    if (!tccId.trim()) { message.warning('请输入 TCC ID'); return }
    setLoading(true)
    try {
      const data = await getTccStatus(tccId.trim())
      setResult(data)
    } catch (e: unknown) {
      message.error(e instanceof Error ? e.message : '查询失败')
      setResult(null)
    } finally {
      setLoading(false)
    }
  }

  const handleCancelTcc = () => {
    if (!result) return
    Modal.confirm({
      title: `取消 TCC 事务`,
      content: `将取消 ${result.tcc_id} 下所有 TRYING 分支，释放冻结资金。确认？`,
      okText: '确认取消',
      okType: 'danger',
      onOk: async () => {
        try {
          await cancelTcc(result.tcc_id)
          message.success('取消成功')
          handleSearch()
        } catch (e: unknown) {
          message.error(e instanceof Error ? e.message : '取消失败')
        }
      },
    })
  }

  const overallColor: Record<string, string> = {
    CONFIRMED: 'success',
    CANCELLED: 'info',
    TRYING: 'warning',
    PARTIAL: 'error',
  }

  return (
    <div>
      <Space style={{ marginBottom: 16 }}>
        <Input
          placeholder="输入 TCC ID（voucherNo）"
          value={tccId}
          onChange={(e) => setTccId(e.target.value)}
          onPressEnter={handleSearch}
          style={{ width: 340 }}
          prefix={<SearchOutlined />}
        />
        <Button type="primary" loading={loading} onClick={handleSearch}>查询</Button>
      </Space>

      {result && (
        <>
          <Alert
            style={{ marginBottom: 16 }}
            type={overallColor[result.overall_status] as 'success' | 'info' | 'warning' | 'error'}
            message={
              <Descriptions size="small" column={4}>
                <Descriptions.Item label="TCC ID">{result.tcc_id}</Descriptions.Item>
                <Descriptions.Item label="整体状态">
                  <Tag color={overallColor[result.overall_status]}>{result.overall_status}</Tag>
                </Descriptions.Item>
                <Descriptions.Item label="分支数">{result.branch_count}</Descriptions.Item>
                <Descriptions.Item label="">
                  {result.overall_status === 'TRYING' || result.overall_status === 'PARTIAL' ? (
                    <Button size="small" danger onClick={handleCancelTcc}>取消整笔 TCC</Button>
                  ) : null}
                </Descriptions.Item>
              </Descriptions>
            }
          />
          <Table
            size="small"
            rowKey="branch_id"
            columns={branchColumns()}
            dataSource={result.branches}
            pagination={false}
            scroll={{ x: 1200 }}
          />
        </>
      )}
    </div>
  )
}

// ─── Tab2: Stuck 分支监控 ─────────────────────────────────────────────────────

function StuckBranches() {
  const [timeout, setTimeout] = useState(5)
  const [limit, setLimit] = useState(50)
  const [loading, setLoading] = useState(false)
  const [branches, setBranches] = useState<TccBranch[]>([])

  const handleLoad = async () => {
    setLoading(true)
    try {
      const data = await listStuckTcc(timeout, limit)
      setBranches(data.branches ?? [])
      if ((data.branches ?? []).length === 0) message.info('暂无 stuck 分支')
    } catch (e: unknown) {
      message.error(e instanceof Error ? e.message : '查询失败')
    } finally {
      setLoading(false)
    }
  }

  const handleCancelBranch = (branch: TccBranch) => {
    Modal.confirm({
      title: '取消分支',
      content: (
        <div>
          <p>Branch ID: <b>{branch.branch_id}</b></p>
          <p>账户号: <b>{branch.account_no}</b></p>
          <p>冻结金额: <b>{displayMoney(branch.frozen_amount, FALLBACK_CURRENCY)}</b></p>
          <p>将释放冻结资金，确认取消？</p>
        </div>
      ),
      okText: '确认取消',
      okType: 'danger',
      onOk: async () => {
        try {
          await cancelTccBranch(branch.branch_id, branch.account_no)
          message.success('分支已取消')
          setBranches((prev) => prev.filter((b) => b.branch_id !== branch.branch_id))
        } catch (e: unknown) {
          message.error(e instanceof Error ? e.message : '取消失败')
        }
      },
    })
  }

  return (
    <div>
      <Space style={{ marginBottom: 16 }}>
        <span>超时阈值：</span>
        <InputNumber
          min={1}
          max={1440}
          value={timeout}
          onChange={(v) => setTimeout(v ?? 5)}
          addonAfter="分钟"
          style={{ width: 130 }}
        />
        <span>最多返回：</span>
        <InputNumber
          min={1}
          max={200}
          value={limit}
          onChange={(v) => setLimit(v ?? 50)}
          addonAfter="条"
          style={{ width: 110 }}
        />
        <Button type="primary" icon={<ReloadOutlined />} loading={loading} onClick={handleLoad}>
          查询
        </Button>
        <Button
          danger
          onClick={async () => {
            try {
              const resp = await tccRetryConfirmNow(0)
              message.success(`立即恢复：${resp.recovered} 个 CONFIRMING 半挂起 TCC 已处理`)
            } catch (e) {
              message.error(e instanceof Error ? e.message : '恢复失败')
            }
          }}
        >
          立即 Retry Confirming（0 阈值）
        </Button>
      </Space>

      <Alert
        type="info"
        style={{ marginBottom: 12 }}
        showIcon
        message="Stuck TRYING → 由 TccRecoveryWorker 每 30s 扫一次并 cancel；CONFIRMING 半挂起 → 同 worker 调 RetryStuckConfirmingTcc 补 confirm（需 updated_at > 5min 才触发）。loadtest 后想立即恢复可点上方「立即 Retry Confirming」按钮。"
      />

      {branches.length > 0 && (
        <Alert
          type="warning"
          style={{ marginBottom: 12 }}
          message={`发现 ${branches.length} 条 stuck 分支，请及时处理`}
        />
      )}

      <Table
        size="small"
        rowKey="branch_id"
        columns={branchColumns(handleCancelBranch)}
        dataSource={branches}
        pagination={{ pageSize: 20, showTotal: (t) => `共 ${t} 条` }}
        scroll={{ x: 1300 }}
        locale={{ emptyText: '暂无数据，点击"查询"加载' }}
      />
    </div>
  )
}

// ─── 主页面 ───────────────────────────────────────────────────────────────────

export default function TccMonitor() {
  return (
    <div>
      <div style={{ marginBottom: 16, fontWeight: 'bold', fontSize: 16 }}>TCC 分布式事务监控</div>
      <Tabs
        items={[
          { key: 'by-id',  label: '按 TCC ID 查询', children: <TccIdSearch /> },
          { key: 'stuck',  label: 'Stuck 分支监控', children: <StuckBranches /> },
        ]}
      />
    </div>
  )
}
