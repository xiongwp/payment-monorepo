import { useEffect, useMemo, useState } from 'react'
import { Outlet, useNavigate, useLocation } from 'react-router-dom'
import { Button, Input, Layout, Menu, Modal, Space, Tag, Typography, theme } from 'antd'
import { useTranslation } from 'react-i18next'
import {
  DashboardOutlined,
  ShoppingCartOutlined,
  ApartmentOutlined,
  SafetyCertificateOutlined,
  MobileOutlined,
  ControlOutlined,
  ShopOutlined,
  AuditOutlined,
  BookOutlined,
  WarningOutlined,
  AlertOutlined,
  UserOutlined,
} from '@ant-design/icons'
import type { MenuProps } from 'antd'
import { useAuth } from '../stores/auth'
import LanguageSwitcher from '../i18n/LanguageSwitcher'

const { Header, Content, Sider } = Layout

type MenuItem = Required<MenuProps>['items'][number]

const ROLE_COLOR: Record<string, string> = {
  read: 'blue',
  write: 'orange',
  danger: 'red',
}

export default function AppLayout() {
  const [collapsed, setCollapsed] = useState(false)
  const navigate = useNavigate()
  const location = useLocation()
  const { t } = useTranslation('menu')
  const {
    token: { colorBgContainer, borderRadiusLG },
  } = theme.useToken()

  // 启动时拉一次 whoami 显示当前 role；设新 token 也重拉
  const { token, role, keyID, setToken, refresh } = useAuth()
  useEffect(() => { void refresh() }, [refresh])

  const [tokenModalOpen, setTokenModalOpen] = useState(false)
  const [tokenInput, setTokenInput] = useState('')

  const onSaveToken = () => {
    setToken(tokenInput.trim())
    setTokenModalOpen(false)
    setTokenInput('')
  }

  const handleMenuClick: MenuProps['onClick'] = (e) => {
    navigate(e.key)
  }

  // 依赖 t：i18n.language 变化时 items 自动重生成
  const items: MenuItem[] = useMemo(
    () => [
      { key: '/', icon: <DashboardOutlined />, label: t('dashboard') },
      { key: '/app', icon: <MobileOutlined />, label: t('appSimulator') },
      { key: '/orders', icon: <ShoppingCartOutlined />, label: t('orders') },
      { key: '/merchants', icon: <ShopOutlined />, label: t('merchants') },
      { key: '/ledger', icon: <BookOutlined />, label: t('ledger') },
      { key: '/disputes', icon: <WarningOutlined />, label: t('disputes') },
      {
        key: 'risk',
        icon: <AlertOutlined />,
        label: t('riskGroup'),
        children: [
          { key: '/risk',           label: t('riskOverview') },
          { key: '/risk/workbench', label: t('riskWorkbench') },
          { key: '/risk/reviews',   label: t('riskReviews') },
          { key: '/risk/decisions', label: t('riskDecisions') },
          { key: '/risk/outcomes',  label: t('riskOutcomes') },
          { key: '/risk/rules',     label: t('riskRules') },
          { key: '/risk/cohort',    label: t('riskCohort') },
          { key: '/risk/abtest',    label: t('riskAbTest') },
        ],
      },
      {
        key: 'channels',
        icon: <ApartmentOutlined />,
        label: t('channelGroup'),
        children: [
          { key: '/channels/probe', label: t('channelProbe') },
          { key: '/channels/webhook', label: t('channelWebhookTest') },
          { key: '/channels/ph-tester', label: t('channelPhTester') },
          { key: '/webhooks/deliveries', label: t('channelOutboundWebhook') },
        ],
      },
      { key: '/kms', icon: <SafetyCertificateOutlined />, label: t('kms') },
      { key: '/ops', icon: <ControlOutlined />, label: t('ops') },
      {
        key: 'audit-group',
        icon: <AuditOutlined />,
        label: t('auditGroup'),
        children: [
          { key: '/audit',                label: t('auditOrder') },
          { key: '/audit/user-merchant',  label: t('auditUserMerchant') },
        ],
      },
    ],
    [t],
  )

  return (
    <Layout style={{ minHeight: '100vh' }}>
      <Sider collapsible collapsed={collapsed} onCollapse={setCollapsed}>
        <div
          style={{
            height: 32,
            margin: 16,
            color: 'white',
            fontSize: 18,
            fontWeight: 'bold',
            textAlign: 'center',
          }}
        >
          {collapsed ? 'PA' : 'Payment Admin'}
        </div>
        <Menu
          theme="dark"
          selectedKeys={[location.pathname]}
          defaultOpenKeys={['channels']}
          mode="inline"
          items={items}
          onClick={handleMenuClick}
        />
      </Sider>
      <Layout>
        <Header style={{ padding: 0, background: colorBgContainer, display: 'flex', alignItems: 'center', justifyContent: 'space-between' }}>
          <div style={{ padding: '0 24px', fontSize: 20, fontWeight: 'bold' }}>
            {t('headerTitle')}
          </div>
          <Space style={{ paddingRight: 24 }}>
            {role && (
              <Tag color={ROLE_COLOR[role] || 'default'}>
                {t('tokenModal.roleLabel', { role })}
              </Tag>
            )}
            {keyID && (
              <Typography.Text type="secondary" style={{ fontSize: 12 }}>
                {keyID}
              </Typography.Text>
            )}
            <Button
              size="small" icon={<UserOutlined />}
              onClick={() => { setTokenInput(token); setTokenModalOpen(true) }}
            >
              {token ? t('tokenModal.switchToken') : t('tokenModal.login')}
            </Button>
            <LanguageSwitcher />
          </Space>
        </Header>
        <Modal
          open={tokenModalOpen}
          title={t('tokenModal.title')}
          onCancel={() => setTokenModalOpen(false)}
          onOk={onSaveToken}
          okText={t('tokenModal.okText')}
          cancelText={t('tokenModal.cancelText')}
        >
          <Typography.Paragraph type="secondary">
            {t('tokenModal.description')}
          </Typography.Paragraph>
          <Input.Password
            value={tokenInput}
            onChange={(e) => setTokenInput(e.target.value)}
            placeholder={t('tokenModal.placeholder')}
            autoComplete="off"
          />
        </Modal>
        <Content style={{ margin: 16 }}>
          <div
            style={{
              padding: 24,
              minHeight: 360,
              background: colorBgContainer,
              borderRadius: borderRadiusLG,
            }}
          >
            <Outlet />
          </div>
        </Content>
      </Layout>
    </Layout>
  )
}
