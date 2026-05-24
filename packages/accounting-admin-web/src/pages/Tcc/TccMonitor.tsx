import { useState } from 'react'
import { useTranslation } from 'react-i18next'
import type { TFunction } from 'i18next'
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
  t: TFunction,
  onCancelBranch?: (b: TccBranch) => void,
): ColumnsType<TccBranch> {
  return [
    { title: t('branch.id'), dataIndex: 'branch_id', key: 'branch_id', ellipsis: true, width: 200 },
    { title: t('branch.accountNo'), dataIndex: 'account_no', key: 'account_no', width: 180 },
    {
      title: t('branch.balanceDelta'),
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
      title: t('branch.frozenAmount'),
      dataIndex: 'frozen_amount',
      key: 'frozen_amount',
      width: 140,
      render: (v: string) => displayMoney(v, FALLBACK_CURRENCY),
    },
    {
      title: t('branch.status'),
      dataIndex: 'status',
      key: 'status',
      width: 110,
      render: (s: TccStatus) => (
        <Tag color={TCC_STATUS_COLOR[s]}>{TCC_STATUS_LABEL[s] ?? s}</Tag>
      ),
    },
    { title: t('branch.db'), dataIndex: 'db_index', key: 'db_index', width: 60 },
    { title: t('branch.table'), dataIndex: 'table_index', key: 'table_index', width: 70 },
    { title: t('branch.createdAt'), dataIndex: 'created_at', key: 'created_at', width: 180 },
    {
      title: t('branch.tccId'),
      dataIndex: 'tcc_id',
      key: 'tcc_id',
      ellipsis: true,
      width: 200,
    },
    ...(onCancelBranch
      ? [
          {
            title: t('branch.action'),
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
                  {t('branch.cancel')}
                </Button>
              ) : null,
          },
        ]
      : []),
  ]
}

// ─── Tab1: 按 TCC ID 查询 ─────────────────────────────────────────────────────

function TccIdSearch() {
  const { t } = useTranslation('tcc')
  const [tccId, setTccId] = useState('')
  const [loading, setLoading] = useState(false)
  const [result, setResult] = useState<TccStatusResponse | null>(null)

  const handleSearch = async () => {
    if (!tccId.trim()) { message.warning(t('search.emptyInput')); return }
    setLoading(true)
    try {
      const data = await getTccStatus(tccId.trim())
      setResult(data)
    } catch (e: unknown) {
      message.error(e instanceof Error ? e.message : t('search.queryFailed'))
      setResult(null)
    } finally {
      setLoading(false)
    }
  }

  const handleCancelTcc = () => {
    if (!result) return
    Modal.confirm({
      title: t('cancel.tccTitle'),
      content: t('cancel.tccContent', { tccId: result.tcc_id }),
      okText: t('cancel.confirmOk'),
      okType: 'danger',
      onOk: async () => {
        try {
          await cancelTcc(result.tcc_id)
          message.success(t('cancel.success'))
          handleSearch()
        } catch (e: unknown) {
          message.error(e instanceof Error ? e.message : t('cancel.failed'))
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
          placeholder={t('search.placeholder')}
          value={tccId}
          onChange={(e) => setTccId(e.target.value)}
          onPressEnter={handleSearch}
          style={{ width: 340 }}
          prefix={<SearchOutlined />}
        />
        <Button type="primary" loading={loading} onClick={handleSearch}>{t('stuck.query')}</Button>
      </Space>

      {result && (
        <>
          <Alert
            style={{ marginBottom: 16 }}
            type={overallColor[result.overall_status] as 'success' | 'info' | 'warning' | 'error'}
            message={
              <Descriptions size="small" column={4}>
                <Descriptions.Item label={t('overall.tccId')}>{result.tcc_id}</Descriptions.Item>
                <Descriptions.Item label={t('overall.status')}>
                  <Tag color={overallColor[result.overall_status]}>{result.overall_status}</Tag>
                </Descriptions.Item>
                <Descriptions.Item label={t('overall.branchCount')}>{result.branch_count}</Descriptions.Item>
                <Descriptions.Item label="">
                  {result.overall_status === 'TRYING' || result.overall_status === 'PARTIAL' ? (
                    <Button size="small" danger onClick={handleCancelTcc}>{t('overall.cancelWhole')}</Button>
                  ) : null}
                </Descriptions.Item>
              </Descriptions>
            }
          />
          <Table
            size="small"
            rowKey="branch_id"
            columns={branchColumns(t)}
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
  const { t } = useTranslation('tcc')
  const [timeout, setTimeout] = useState(5)
  const [limit, setLimit] = useState(50)
  const [loading, setLoading] = useState(false)
  const [branches, setBranches] = useState<TccBranch[]>([])

  const handleLoad = async () => {
    setLoading(true)
    try {
      const data = await listStuckTcc(timeout, limit)
      setBranches(data.branches ?? [])
      if ((data.branches ?? []).length === 0) message.info(t('stuck.noStuck'))
    } catch (e: unknown) {
      message.error(e instanceof Error ? e.message : t('search.queryFailed'))
    } finally {
      setLoading(false)
    }
  }

  const handleCancelBranch = (branch: TccBranch) => {
    Modal.confirm({
      title: t('cancel.branchTitle'),
      content: (
        <div>
          <p>{t('branch.id')}: <b>{branch.branch_id}</b></p>
          <p>{t('cancel.branchAccount')}: <b>{branch.account_no}</b></p>
          <p>{t('cancel.branchFrozen')}: <b>{displayMoney(branch.frozen_amount, FALLBACK_CURRENCY)}</b></p>
          <p>{t('cancel.branchConfirm')}</p>
        </div>
      ),
      okText: t('cancel.confirmOk'),
      okType: 'danger',
      onOk: async () => {
        try {
          await cancelTccBranch(branch.branch_id, branch.account_no)
          message.success(t('cancel.branchSuccess'))
          setBranches((prev) => prev.filter((b) => b.branch_id !== branch.branch_id))
        } catch (e: unknown) {
          message.error(e instanceof Error ? e.message : t('cancel.failed'))
        }
      },
    })
  }

  return (
    <div>
      <Space style={{ marginBottom: 16 }}>
        <span>{t('stuck.timeoutLabel')}</span>
        <InputNumber
          min={1}
          max={1440}
          value={timeout}
          onChange={(v) => setTimeout(v ?? 5)}
          addonAfter={t('stuck.timeoutUnit')}
          style={{ width: 130 }}
        />
        <span>{t('stuck.limitLabel')}</span>
        <InputNumber
          min={1}
          max={200}
          value={limit}
          onChange={(v) => setLimit(v ?? 50)}
          addonAfter={t('stuck.limitUnit')}
          style={{ width: 110 }}
        />
        <Button type="primary" icon={<ReloadOutlined />} loading={loading} onClick={handleLoad}>
          {t('stuck.query')}
        </Button>
        <Button
          danger
          onClick={async () => {
            try {
              const resp = await tccRetryConfirmNow(0)
              message.success(t('stuck.retrySuccess', { recovered: resp.recovered }))
            } catch (e) {
              message.error(e instanceof Error ? e.message : t('stuck.retryFailed'))
            }
          }}
        >
          {t('stuck.retryButton')}
        </Button>
      </Space>

      <Alert
        type="info"
        style={{ marginBottom: 12 }}
        showIcon
        message={t('stuck.infoBanner')}
      />

      {branches.length > 0 && (
        <Alert
          type="warning"
          style={{ marginBottom: 12 }}
          message={t('stuck.warningCount', { count: branches.length })}
        />
      )}

      <Table
        size="small"
        rowKey="branch_id"
        columns={branchColumns(t, handleCancelBranch)}
        dataSource={branches}
        pagination={{ pageSize: 20, showTotal: (total) => t('stuck.totalSuffix', { count: total }) }}
        scroll={{ x: 1300 }}
        locale={{ emptyText: t('stuck.emptyText') }}
      />
    </div>
  )
}

// ─── 主页面 ───────────────────────────────────────────────────────────────────

export default function TccMonitor() {
  const { t } = useTranslation('tcc')
  return (
    <div>
      <div style={{ marginBottom: 16, fontWeight: 'bold', fontSize: 16 }}>{t('monitor.title')}</div>
      <Tabs
        items={[
          { key: 'by-id',  label: t('monitor.tabs.byId'), children: <TccIdSearch /> },
          { key: 'stuck',  label: t('monitor.tabs.stuck'), children: <StuckBranches /> },
        ]}
      />
    </div>
  )
}
