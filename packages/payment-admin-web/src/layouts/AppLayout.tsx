import { useEffect, useState } from 'react'
import { Outlet, useNavigate, useLocation } from 'react-router-dom'
import { Button, Input, Layout, Menu, Modal, Space, Tag, Typography, theme } from 'antd'
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

const { Header, Content, Sider } = Layout

type MenuItem = Required<MenuProps>['items'][number]

const items: MenuItem[] = [
  { key: '/', icon: <DashboardOutlined />, label: '工作台' },
  { key: '/app', icon: <MobileOutlined />, label: '模拟下单（App）' },
  { key: '/orders', icon: <ShoppingCartOutlined />, label: '订单管理' },
  { key: '/merchants', icon: <ShopOutlined />, label: '商户管理' },
  { key: '/ledger', icon: <BookOutlined />, label: '总账' },
  { key: '/disputes', icon: <WarningOutlined />, label: 'Disputes' },
  {
    key: 'risk',
    icon: <AlertOutlined />,
    label: '风控运营',
    children: [
      { key: '/risk',           label: '风控总览' },
      { key: '/risk/workbench', label: 'Case 工作台' },
      { key: '/risk/reviews',   label: '所有审核' },
      { key: '/risk/decisions', label: '决策审计' },
      { key: '/risk/outcomes',  label: '反馈记录' },
      { key: '/risk/rules',     label: '规则管理' },
      { key: '/risk/cohort',    label: 'Cohort 分析' },
      { key: '/risk/abtest',    label: 'ML A/B 检验' },
    ],
  },
  {
    key: 'channels',
    icon: <ApartmentOutlined />,
    label: '渠道',
    children: [
      { key: '/channels/probe', label: '路由探测' },
      { key: '/channels/webhook', label: 'Webhook 调试' },
      { key: '/channels/ph-tester', label: 'PH 渠道联测' },
      { key: '/webhooks/deliveries', label: '出站 Webhook' },
    ],
  },
  { key: '/kms', icon: <SafetyCertificateOutlined />, label: '密钥管理' },
  { key: '/ops', icon: <ControlOutlined />, label: '运营中心' },
  {
    key: 'audit-group',
    icon: <AuditOutlined />,
    label: '审计日志',
    children: [
      { key: '/audit',                label: '订单审计（order-core）' },
      { key: '/audit/user-merchant',  label: '商户审计（链式签名）' },
    ],
  },
]

const ROLE_COLOR: Record<string, string> = {
  read: 'blue',
  write: 'orange',
  danger: 'red',
}

export default function AppLayout() {
  const [collapsed, setCollapsed] = useState(false)
  const navigate = useNavigate()
  const location = useLocation()
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
            支付平台管理后台
          </div>
          <Space style={{ paddingRight: 24 }}>
            {role && (
              <Tag color={ROLE_COLOR[role] || 'default'}>
                角色: {role}
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
              {token ? '切换 Token' : '登录'}
            </Button>
          </Space>
        </Header>
        <Modal
          open={tokenModalOpen}
          title="设置 Admin Token"
          onCancel={() => setTokenModalOpen(false)}
          onOk={onSaveToken}
          okText="保存"
          cancelText="取消"
        >
          <Typography.Paragraph type="secondary">
            填入运维分配的 admin token (read / write / danger 三级 RBAC)。
            空 token = 退出登录 / dev 模式。
          </Typography.Paragraph>
          <Input.Password
            value={tokenInput}
            onChange={(e) => setTokenInput(e.target.value)}
            placeholder="rsk_admin_xxx"
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
