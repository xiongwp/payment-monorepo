import { Routes, Route } from 'react-router-dom'
import AppLayout from './layouts/AppLayout'
import Dashboard from './pages/Dashboard/Dashboard'
import OrderList from './pages/Orders/OrderList'
import OrderDetail from './pages/Orders/OrderDetail'
import ChannelProbe from './pages/Channels/ChannelProbe'
import WebhookTest from './pages/Channels/WebhookTest'
import PHChannelTester from './pages/Channels/PHChannelTester'
import KMSPanel from './pages/KMS/KMSPanel'
import AppSimulator from './pages/App/AppSimulator'
import OpsCenter from './pages/Ops/OpsCenter'
import MerchantList from './pages/Merchants/MerchantList'
import MerchantDetail from './pages/Merchants/MerchantDetail'
import MerchantSecrets from './pages/Merchants/MerchantSecrets'
import AuditLog from './pages/Audit/AuditLog'
import UserMerchantAuditLog from './pages/Audit/UserMerchantAuditLog'
import WebhookDeliveries from './pages/Webhooks/WebhookDeliveries'
import Ledger from './pages/Ledger/Ledger'
import DisputeList from './pages/Disputes/DisputeList'
import RiskDashboard from './pages/Risk/Dashboard'
import RiskReviews from './pages/Risk/Reviews'
import RiskWorkbench from './pages/Risk/Workbench'
import RiskDecisions from './pages/Risk/Decisions'
import RiskOutcomes from './pages/Risk/Outcomes'
import RiskRules from './pages/Risk/Rules'
import RiskCohort from './pages/Risk/Cohort'
import RiskABTest from './pages/Risk/ABTest'

function App() {
  return (
    <Routes>
      <Route path="/" element={<AppLayout />}>
        <Route index element={<Dashboard />} />
        <Route path="app" element={<AppSimulator />} />
        <Route path="orders" element={<OrderList />} />
        <Route path="orders/:id" element={<OrderDetail />} />
        <Route path="merchants" element={<MerchantList />} />
        <Route path="merchants/:id" element={<MerchantDetail />} />
        <Route path="merchants/:id/secrets" element={<MerchantSecrets />} />
        <Route path="audit" element={<AuditLog />} />
        <Route path="audit/user-merchant" element={<UserMerchantAuditLog />} />
        <Route path="webhooks/deliveries" element={<WebhookDeliveries />} />
        <Route path="ledger" element={<Ledger />} />
        <Route path="disputes" element={<DisputeList />} />
        <Route path="channels/probe" element={<ChannelProbe />} />
        <Route path="channels/webhook" element={<WebhookTest />} />
        <Route path="channels/ph-tester" element={<PHChannelTester />} />
        <Route path="kms" element={<KMSPanel />} />
        <Route path="ops" element={<OpsCenter />} />
        <Route path="risk" element={<RiskDashboard />} />
        <Route path="risk/reviews" element={<RiskReviews />} />
        <Route path="risk/workbench" element={<RiskWorkbench />} />
        <Route path="risk/decisions" element={<RiskDecisions />} />
        <Route path="risk/outcomes" element={<RiskOutcomes />} />
        <Route path="risk/rules" element={<RiskRules />} />
        <Route path="risk/cohort" element={<RiskCohort />} />
        <Route path="risk/abtest" element={<RiskABTest />} />
      </Route>
    </Routes>
  )
}

export default App
