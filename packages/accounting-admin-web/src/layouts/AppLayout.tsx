import { useState, useMemo } from 'react'
import { Outlet, useNavigate, useLocation } from 'react-router-dom'
import { Layout, Menu, theme } from 'antd'
import { useTranslation } from 'react-i18next'
import {
  DashboardOutlined,
  WalletOutlined,
  TransactionOutlined,
  CameraOutlined,
  ToolOutlined,
  ScissorOutlined,
  BranchesOutlined,
  AuditOutlined,
  FireOutlined,
  DatabaseOutlined,
  ClusterOutlined,
  BankOutlined,
  ProfileOutlined,
  PieChartOutlined,
  AreaChartOutlined,
  DeleteOutlined,
  SettingOutlined,
  ReloadOutlined,
  SwapOutlined,
  ThunderboltOutlined,
  UnorderedListOutlined,
} from '@ant-design/icons'
import type { MenuProps } from 'antd'
import LanguageSwitcher from '../i18n/LanguageSwitcher'

const { Header, Content, Sider } = Layout

type MenuItem = Required<MenuProps>['items'][number]

function getItem(
  label: React.ReactNode,
  key: React.Key,
  icon?: React.ReactNode,
  children?: MenuItem[],
): MenuItem {
  return {
    key,
    icon,
    children,
    label,
  } as MenuItem
}

export default function AppLayout() {
  const [collapsed, setCollapsed] = useState(false)
  const navigate = useNavigate()
  const location = useLocation()
  const { t } = useTranslation('menu')
  const {
    token: { colorBgContainer, borderRadiusLG },
  } = theme.useToken()

  // useMemo + t() 依赖：i18n.language 变化时，t 函数引用会变，items 自动刷新。
  const items: MenuItem[] = useMemo(
    () => [
      getItem(t('dashboard'), '/', <DashboardOutlined />),
      getItem(t('accounts'), '/accounts', <WalletOutlined />),
      getItem(t('transactions'), '/transactions', <TransactionOutlined />),
      getItem(t('snapshots'), '/snapshots', <CameraOutlined />),
      getItem(t('adjustment'), '/adjustment', <ToolOutlined />),
      getItem(t('daycut'), '/daycut', <ScissorOutlined />),
      getItem(t('tcc'), '/tcc', <BranchesOutlined />),
      getItem(t('trialBalance'), '/trial-balance', <AuditOutlined />),
      getItem(t('hotAccounts'), '/hot-accounts', <FireOutlined />),
      getItem(t('bufferAccounts'), '/buffer-accounts', <DatabaseOutlined />),
      getItem(t('instances'), '/instances', <ClusterOutlined />),
      getItem(t('platformAccounts'), '/platform-accounts', <BankOutlined />),
      getItem(t('businessTypes'), '/business-types', <ProfileOutlined />),
      getItem(t('platformBalances'), '/platform-balances', <PieChartOutlined />),
      getItem(t('platformSnapshots'), '/platform-snapshots', <AreaChartOutlined />),
      getItem(t('rotationGroup'), 'rotation-group', <SwapOutlined />, [
        getItem(t('rotationList'), '/rotation', <UnorderedListOutlined />),
        getItem(t('rotationFleetTest'), '/rotation/fleet-test', <ThunderboltOutlined />),
      ]),
      getItem(t('tccArchive'), '/tcc-archive', <DeleteOutlined />),
      getItem(t('systemConfig'), '/system-config', <SettingOutlined />),
      getItem(t('redisRebuild'), '/redis-rebuild', <ReloadOutlined />),
    ],
    [t],
  )

  const handleMenuClick: MenuProps['onClick'] = (e) => {
    navigate(e.key)
  }

  return (
    <Layout style={{ minHeight: '100vh' }}>
      <Sider collapsible collapsed={collapsed} onCollapse={(value) => setCollapsed(value)}>
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
          {collapsed ? 'AS' : 'Accounting System'}
        </div>
        <Menu
          theme="dark"
          // 详情页（/rotation/:key, /accounts/:accountNo）也要让父菜单高亮
          // /rotation/fleet-test 是子菜单项之一，要保留完整 key
          selectedKeys={[
            location.pathname === '/rotation/fleet-test'
              ? '/rotation/fleet-test'
              : location.pathname.startsWith('/rotation/')
              ? '/rotation'
              : location.pathname.startsWith('/accounts/')
              ? '/accounts'
              : location.pathname,
          ]}
          defaultOpenKeys={['rotation-group']}
          mode="inline"
          items={items}
          onClick={handleMenuClick}
        />
      </Sider>
      <Layout>
        <Header
          style={{
            padding: 0,
            background: colorBgContainer,
            display: 'flex',
            alignItems: 'center',
            justifyContent: 'space-between',
          }}
        >
          <div style={{ padding: '0 24px', fontSize: 20, fontWeight: 'bold' }}>
            {t('headerTitle')}
          </div>
          <div style={{ padding: '0 24px' }}>
            <LanguageSwitcher />
          </div>
        </Header>
        <Content style={{ margin: '16px' }}>
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
