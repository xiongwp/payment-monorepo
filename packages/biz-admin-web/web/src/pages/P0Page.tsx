// P0Page — 简化版替代 p0-services.html. 真生产实现完整版的 4 tab.

export default function P0Page() {
  return (
    <div className="max-w-6xl mx-auto p-6">
      <h1 className="text-xl font-semibold mb-3">🛡 P0 服务复核</h1>
      <p className="text-sm text-gray-600 mb-4">
        AML 命中 / Vault token / Tax 表 / Data Rights 工单复核. 真完整版见{' '}
        <code className="text-xs bg-gray-100 px-1 rounded">/static/p0-services.html</code> (Alpine.js 版).
      </p>
      <div className="grid grid-cols-2 gap-4">
        <Card title="AML 命中" desc="OFAC / EU / PEP 制裁筛查命中, 待复核" link="/api/aml/admin/hits/pending" />
        <Card title="Vault" desc="token 状态 + provider 健康" link="/api/vault/admin/health/providers" />
        <Card title="Tax" desc="1099-K / W-9 / VAT 表" link="/api/tax/admin/eligible/2026" />
        <Card title="Data Rights" desc="DSAR / RTBF 工单 (30d 法定 SLA)" link="/api/dr/admin/requests" />
      </div>
    </div>
  );
}

function Card({ title, desc, link }: { title: string; desc: string; link: string }) {
  return (
    <div className="bg-white rounded shadow-sm border p-4">
      <div className="font-medium">{title}</div>
      <div className="text-sm text-gray-600 mt-1">{desc}</div>
      <a
        className="text-xs text-blue-600 mt-2 inline-block hover:underline"
        href={link}
        target="_blank"
        rel="noopener"
      >
        backend API →
      </a>
    </div>
  );
}
