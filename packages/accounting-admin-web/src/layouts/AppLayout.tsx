import { useState } from 'react'
import { Outlet, useNavigate, useLocation } from 'react-router-dom'
import { Layout, Menu, theme } from 'antd'
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

const items: MenuItem[] = [
  getItem('工作台', '/', <DashboardOutlined />),
  getItem('账户管理', '/accounts', <WalletOutlined />),
  getItem('交易流水', '/transactions', <TransactionOutlined />),
  getItem('余额快照', '/snapshots', <CameraOutlined />),
  getItem('调账管理', '/adjustment', <ToolOutlined />),
  getItem('日切管理', '/daycut', <ScissorOutlined />),
  getItem('TCC 监控', '/tcc', <BranchesOutlined />),
  getItem('试算平衡', '/trial-balance', <AuditOutlined />),
  getItem('热点账户', '/hot-accounts', <FireOutlined />),
  getItem('缓冲记账', '/buffer-accounts', <DatabaseOutlined />),
  getItem('服务实例', '/instances', <ClusterOutlined />),
  getItem('系统账户', '/platform-accounts', <BankOutlined />),
  getItem('业务类型', '/business-types', <ProfileOutlined />),
  getItem('平台账户余额', '/platform-balances', <PieChartOutlined />),
  getItem('平台账户快照', '/platform-snapshots', <AreaChartOutlined />),
  getItem('账户轮换管理', 'rotation-group', <SwapOutlined />, [
    getItem('LA 列表', '/rotation', <UnorderedListOutlined />),
    getItem('Fleet 路由测试', '/rotation/fleet-test', <ThunderboltOutlined />),
  ]),
  getItem('TCC 归档', '/tcc-archive', <DeleteOutlined />),
  getItem('系统配置', '/system-config', <SettingOutlined />),
  getItem('Redis 重建', '/redis-rebuild', <ReloadOutlined />),
]

export default function AppLayout() {
  const [collapsed, setCollapsed] = useState(false)
  const navigate = useNavigate()
  const location = useLocation()
  const {
    token: { colorBgContainer, borderRadiusLG },
  } = theme.useToken()

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
        <Header style={{ padding: 0, background: colorBgContainer }}>
          <div style={{ padding: '0 24px', fontSize: 20, fontWeight: 'bold' }}>
            复式记账管理系统
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
