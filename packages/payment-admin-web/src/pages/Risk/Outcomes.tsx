import { useCallback, useEffect, useState } from 'react'
import {
  Alert, Button, Card, Form, Input, Modal, Select, Space, Switch, Table, Tag,
  Typography, message,
} from 'antd'
import dayjs from 'dayjs'
import { useTranslation } from 'react-i18next'
import { recentOutcomes, recordOutcome } from '../../api/risk'
import type { Outcome, OutcomeSource } from '../../api/risk'

const SOURCE_COLOR: Record<OutcomeSource, string> = {
  review_human: 'blue',
  dispute: 'red',
  merchant_confirm: 'green',
}

export default function Outcomes() {
  const { t } = useTranslation('risk')
  const [loading, setLoading] = useState(false)
  const [rows, setRows] = useState<Outcome[]>([])
  const [open, setOpen] = useState(false)
  const [form] = Form.useForm<{
    decision_id: string; source: OutcomeSource; is_fraud: boolean;
    actor: string; notes: string;
  }>()

  const SOURCE_LABEL: Record<OutcomeSource, string> = {
    review_human: t('outcomes.sourceLabels.review_human'),
    dispute: t('outcomes.sourceLabels.dispute'),
    merchant_confirm: t('outcomes.sourceLabels.merchant_confirm'),
  }

  const load = useCallback(async () => {
    setLoading(true)
    try {
      const r = await recentOutcomes(200)
      setRows(r.items || [])
    } catch (e) { message.error(String(e)) }
    finally { setLoading(false) }
  }, [])

  useEffect(() => { load() }, [load])

  const onSubmit = async (v: Parameters<typeof recordOutcome>[0]) => {
    try {
      await recordOutcome(v)
      message.success(t('outcomes.recorded'))
      setOpen(false)
      form.resetFields()
      load()
    } catch (e) { message.error(String(e)) }
  }

  return (
    <div>
      <Typography.Title level={3}>{t('outcomes.title')}</Typography.Title>
      <Card>
        <Alert
          type="info" showIcon style={{ marginBottom: 16 }}
          message={t('outcomes.infoAlert')}
        />
        <Space style={{ marginBottom: 16 }}>
          <Button type="primary" onClick={() => setOpen(true)}>{t('outcomes.recordButton')}</Button>
          <Button onClick={load}>{t('common:actions.refresh')}</Button>
          <Typography.Text type="secondary">{t('outcomes.recentCount', { count: rows.length })}</Typography.Text>
        </Space>
        <Table<Outcome>
          rowKey={(r) => `${r.decision_id}-${r.at}`} size="small" loading={loading} dataSource={rows}
          pagination={{ pageSize: 25 }}
          columns={[
            {
              title: t('outcomes.columns.decisionId'), dataIndex: 'decision_id', width: 270, ellipsis: true,
              render: (v) => <Typography.Text code copyable>{v}</Typography.Text>,
            },
            {
              title: t('outcomes.columns.time'), dataIndex: 'at', width: 170,
              render: (v: string) => dayjs(v).format('MM-DD HH:mm:ss'),
            },
            {
              title: t('outcomes.columns.source'), dataIndex: 'source', width: 140,
              render: (v: OutcomeSource) => <Tag color={SOURCE_COLOR[v]}>{SOURCE_LABEL[v] || v}</Tag>,
            },
            {
              title: t('outcomes.columns.verdict'), dataIndex: 'is_fraud', width: 90, align: 'center',
              render: (v: boolean) => v
                ? <Tag color="red">FRAUD</Tag>
                : <Tag color="green">LEGIT</Tag>,
            },
            { title: t('outcomes.columns.actor'), dataIndex: 'actor', width: 140, ellipsis: true },
            { title: t('outcomes.columns.notes'), dataIndex: 'notes', ellipsis: true },
          ]}
        />
      </Card>

      <Modal
        title={t('outcomes.modal.title')} open={open}
        onCancel={() => setOpen(false)} onOk={() => form.submit()}
      >
        <Alert
          type="warning" showIcon style={{ marginBottom: 12 }}
          message={t('outcomes.modal.warning')}
        />
        <Form form={form} layout="vertical" onFinish={onSubmit} initialValues={{
          source: 'merchant_confirm', is_fraud: false,
        }}>
          <Form.Item name="decision_id" label={t('outcomes.modal.decisionIdLabel')} rules={[{ required: true }]}>
            <Input placeholder={t('outcomes.modal.decisionIdPlaceholder')} />
          </Form.Item>
          <Form.Item name="source" label={t('outcomes.modal.sourceLabel')} rules={[{ required: true }]}>
            <Select options={Object.entries(SOURCE_LABEL).map(([k, v]) => ({
              value: k, label: v,
            }))} />
          </Form.Item>
          <Form.Item name="is_fraud" label={t('outcomes.modal.isFraudLabel')} valuePropName="checked">
            <Switch checkedChildren={t('outcomes.modal.isFraudYes')} unCheckedChildren={t('outcomes.modal.isFraudNo')} />
          </Form.Item>
          <Form.Item name="actor" label={t('outcomes.modal.actorLabel')}>
            <Input placeholder={t('outcomes.modal.actorPlaceholder')} />
          </Form.Item>
          <Form.Item name="notes" label={t('outcomes.modal.notesLabel')}>
            <Input.TextArea rows={3} placeholder={t('outcomes.modal.notesPlaceholder')} />
          </Form.Item>
        </Form>
      </Modal>
    </div>
  )
}
