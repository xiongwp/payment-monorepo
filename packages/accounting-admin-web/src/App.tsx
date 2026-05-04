import { Routes, Route } from 'react-router-dom'
import AppLayout from './layouts/AppLayout'
import Dashboard from './pages/Dashboard'
import AccountList from './pages/Account/AccountList'
import AccountDetail from './pages/Account/AccountDetail'
import TransactionList from './pages/Transaction/TransactionList'
import SnapshotList from './pages/Snapshot/SnapshotList'
import Adjustment from './pages/Adjustment/Adjustment'
import DayCut from './pages/DayCut/DayCut'
import TccMonitor from './pages/Tcc/TccMonitor'
import TrialBalance from './pages/TrialBalance/TrialBalance'
import HotAccountConfig from './pages/HotAccount/HotAccountConfig'
import BufferAccountConfig from './pages/BufferAccount/BufferAccountConfig'
import ServiceInstances from './pages/Instances/ServiceInstances'
import PlatformAccounts from './pages/PlatformAccount/PlatformAccounts'
import PlatformBalances from './pages/PlatformAccount/PlatformBalances'
import PlatformSnapshotsPage from './pages/PlatformAccount/PlatformSnapshotsPage'
import BusinessTypes from './pages/BusinessType/BusinessTypes'
import TccArchive from './pages/TccArchive/TccArchive'
import SystemConfig from './pages/SystemConfig/SystemConfig'
import RedisRebuild from './pages/RedisRebuild/RedisRebuild'
import { loadAccountTypeRegistry } from './api/accounting'
import { useEffect } from 'react'
import './App.css'

function App() {
  // 启动时从后端拉 account_type_info，用真实的 is_platform 覆盖前端 fallback 常量。
  // 失败则静默退回 fallback（硬编码 4-9 为平台类型）。
  useEffect(() => {
    loadAccountTypeRegistry().catch(() => { /* fallback OK */ })
  }, [])

  return (
    <Routes>
      <Route path="/" element={<AppLayout />}>
        <Route index element={<Dashboard />} />
        <Route path="accounts" element={<AccountList />} />
        <Route path="accounts/:accountNo" element={<AccountDetail />} />
        <Route path="transactions" element={<TransactionList />} />
        <Route path="snapshots" element={<SnapshotList />} />
        <Route path="adjustment" element={<Adjustment />} />
        <Route path="daycut" element={<DayCut />} />
        <Route path="tcc" element={<TccMonitor />} />
        <Route path="trial-balance" element={<TrialBalance />} />
        <Route path="hot-accounts" element={<HotAccountConfig />} />
        <Route path="buffer-accounts" element={<BufferAccountConfig />} />
        <Route path="instances" element={<ServiceInstances />} />
        <Route path="platform-accounts" element={<PlatformAccounts />} />
        <Route path="platform-balances" element={<PlatformBalances />} />
        <Route path="platform-snapshots" element={<PlatformSnapshotsPage />} />
        <Route path="business-types" element={<BusinessTypes />} />
        <Route path="tcc-archive" element={<TccArchive />} />
        <Route path="system-config" element={<SystemConfig />} />
        <Route path="redis-rebuild" element={<RedisRebuild />} />
      </Route>
    </Routes>
  )
}

export default App
