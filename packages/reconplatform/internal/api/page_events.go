// page_events.go — /admin/events 实时 binlog 事件流 + 搜索 + CDC 状态.
//
// 拆 SPA 时遗漏的关键功能恢复:
//   - 顶部:CDC runner 状态 (per-service 位点 / lag / 健康)
//   - 左侧:实时 SSE 流 (从 /api/v1/events/stream 拉,新事件 prepend 到列表)
//   - 右侧:业务 key 搜索 (输入 pi_id=xxx → /api/v1/search 立即返跨服务关联事件)
//   - 多维过滤 (service / table / op),暂停 / 清屏,点开事件展开 JSON / 跳关联 incident
//
// 这是 oncall 排查 "为啥 diff 出现 / 上游数据有没有到" 的核心入口.
package api

import "net/http"

func (s *Server) pageEvents(w http.ResponseWriter, _ *http.Request) {
	body := `
<div x-data="eventsPage()" x-init="boot()" class="grid grid-cols-12 gap-4 h-[calc(100vh-7rem)]">

  <!-- 左:实时流 -->
  <section class="col-span-7 flex flex-col bg-white rounded-lg border border-slate-200 overflow-hidden">

    <!-- 工具条 -->
    <div class="px-3 py-2 border-b border-slate-200 bg-slate-50 flex items-center gap-2 shrink-0">
      <button @click="togglePause()" class="btn btn-outline text-xs"
              :class="paused && 'bg-amber-50 border-amber-300 text-amber-700'">
        <i :data-lucide="paused ? 'play' : 'pause'" class="icon w-3 h-3"></i>
        <span x-text="paused ? 'Resume' : 'Pause'"></span>
      </button>
      <button @click="clearStream()" class="btn btn-outline text-xs">
        <i data-lucide="eraser" class="icon w-3 h-3"></i> Clear
      </button>
      <button @click="injectTestEvent()" class="btn btn-outline text-xs"
              title="dev: 注入一条模拟事件验证渲染">
        <i data-lucide="zap" class="icon w-3 h-3"></i> 注入测试
      </button>
      <div class="h-5 w-px bg-slate-200 mx-1"></div>
      <select x-model="filterSvc" class="text-xs border border-slate-300 rounded px-2 py-1">
        <option value="">所有 service</option>
        <template x-for="s in services" :key="s"><option :value="s" x-text="s"></option></template>
      </select>
      <select x-model="filterTable" class="text-xs border border-slate-300 rounded px-2 py-1">
        <option value="">所有 table</option>
        <template x-for="t in tables" :key="t"><option :value="t" x-text="t"></option></template>
      </select>
      <select x-model="filterOp" class="text-xs border border-slate-300 rounded px-2 py-1">
        <option value="">所有 op</option>
        <option>INSERT</option><option>UPDATE</option><option>DELETE</option>
      </select>
      <div class="flex-1"></div>
      <div class="text-xs text-slate-500 flex items-center gap-3">
        <span class="flex items-center gap-1">
          <span class="w-2 h-2 rounded-full"
                :class="sseStatus === 'open' ? 'bg-emerald-500 animate-pulse'
                       : sseStatus === 'err' ? 'bg-red-500' : 'bg-slate-300'"></span>
          <span x-text="sseStatus === 'open' ? 'streaming' : sseStatus === 'err' ? 'disconnected' : 'connecting...'"></span>
        </span>
        <span x-text="filtered.length + ' / ' + buffer.length + ' events'"></span>
      </div>
    </div>

    <!-- 事件流 -->
    <ul class="flex-1 overflow-y-auto divide-y divide-slate-100" x-ref="stream">
      <template x-for="e in filtered" :key="e._uid">
        <li class="px-3 py-2 hover:bg-slate-50 cursor-pointer transition-colors"
            :class="e._highlight && 'bg-amber-50'"
            @click="openDetail(e)">
          <div class="flex items-center gap-2">
            <span class="badge"
                  :class="e.op === 'INSERT' ? 'badge-ok' :
                          e.op === 'UPDATE' ? 'badge-info' : 'badge-critical'"
                  x-text="e.op || '?'"></span>
            <span class="mono text-xs font-medium" x-text="e.svc + ':' + e.table"></span>
            <span class="mono text-xs text-slate-500 truncate flex-1" x-text="e.pk"></span>
            <span class="text-xs text-slate-400" x-text="formatTs(e.ts)"></span>
          </div>
          <div class="mt-1 text-xs text-slate-600 mono truncate" x-show="e.indexes && Object.keys(e.indexes).length">
            <template x-for="(v,k) in e.indexes" :key="k">
              <span class="inline-block bg-slate-100 rounded px-1.5 py-0.5 mr-1 mb-0.5">
                <span class="text-slate-500" x-text="k"></span>
                <span class="text-slate-700">=</span>
                <span class="text-slate-700" x-text="v"></span>
              </span>
            </template>
          </div>
        </li>
      </template>
      <li x-show="filtered.length === 0 && sseStatus === 'open'"
          class="px-3 py-8 text-center text-sm text-slate-400">
        <i data-lucide="hourglass" class="w-6 h-6 mx-auto mb-2 opacity-50"></i>
        <div>等待事件...</div>
        <div class="text-xs mt-1">CDC 已连接,binlog 静止时间窗可能没行变更</div>
      </li>
      <li x-show="sseStatus === 'err'" class="px-3 py-4 text-center text-sm text-red-600">
        SSE 连接断开。<button @click="connectSSE()" class="underline">重连</button>
      </li>
    </ul>
  </section>

  <!-- 右:搜索 + CDC 状态 -->
  <section class="col-span-5 flex flex-col gap-4">

    <!-- 搜索面板 -->
    <div class="bg-white rounded-lg border border-slate-200 p-4 shrink-0">
      <div class="flex items-center justify-between mb-3">
        <h3 class="text-sm font-semibold">业务键搜索</h3>
        <span class="text-xs text-slate-400">跨服务关联</span>
      </div>
      <div class="flex items-center gap-2 mb-3">
        <select x-model="searchIdx" class="text-sm border border-slate-300 rounded px-2 py-1.5">
          <template x-for="i in indexKeys" :key="i"><option :value="i" x-text="i"></option></template>
        </select>
        <input type="text" x-model="searchVal" placeholder="value (e.g. pi_abc123)"
               @keyup.enter="runSearch()"
               class="flex-1 text-sm border border-slate-300 rounded px-3 py-1.5 mono">
        <button @click="runSearch()" class="btn btn-primary text-sm">
          <i data-lucide="search" class="icon w-3 h-3"></i>
        </button>
      </div>
      <div class="text-xs text-slate-500 mb-2" x-show="searchResults.length > 0"
           x-text="'命中 ' + searchResults.length + ' 条事件:'"></div>
      <ul class="space-y-1 max-h-64 overflow-y-auto">
        <template x-for="r in searchResults" :key="r._uid">
          <li class="text-xs border border-slate-200 rounded p-2 hover:bg-slate-50">
            <div class="flex items-center justify-between mb-1">
              <span class="mono font-medium" x-text="r.svc + ':' + r.table"></span>
              <span class="text-slate-400" x-text="formatTs(r.ts)"></span>
            </div>
            <div class="mono text-slate-500" x-text="r.pk"></div>
          </li>
        </template>
        <li x-show="searchResults.length === 0 && searchAttempted" class="text-xs text-slate-400 py-3 text-center">
          未命中
        </li>
      </ul>
    </div>

    <!-- CDC runner 状态 -->
    <div class="bg-white rounded-lg border border-slate-200 p-4 flex-1 overflow-hidden flex flex-col">
      <div class="flex items-center justify-between mb-3 shrink-0">
        <h3 class="text-sm font-semibold">CDC Runner 状态</h3>
        <button @click="loadCDC()" class="btn btn-ghost text-xs">
          <i data-lucide="refresh-cw" class="icon w-3 h-3"></i>
        </button>
      </div>
      <ul class="space-y-2 overflow-y-auto">
        <template x-for="r in cdcRunners" :key="r.service + ':' + r.shard">
          <li class="border border-slate-200 rounded p-2 text-xs">
            <div class="flex items-center justify-between mb-1">
              <span class="font-medium" x-text="r.service"></span>
              <span class="badge"
                    :class="r.healthy ? 'badge-ok' : 'badge-critical'"
                    x-text="r.healthy ? 'healthy' : 'lagging'"></span>
            </div>
            <div class="mono text-[10px] text-slate-500">
              shard <span x-text="r.shard"></span> · pos <span x-text="r.binlog_pos"></span>
            </div>
            <div class="mt-1 text-[10px] text-slate-500" x-show="r.lag_ms !== undefined">
              lag <span class="mono" x-text="r.lag_ms + 'ms'"></span>
              · events/s <span class="mono" x-text="r.events_per_sec || 0"></span>
            </div>
          </li>
        </template>
        <li x-show="cdcRunners.length === 0" class="text-xs text-slate-400 py-3 text-center">
          CDC 未启动 / 无 status
        </li>
      </ul>
    </div>
  </section>

  <!-- 事件详情 modal (Alpine x-show) -->
  <div x-show="detail" x-cloak class="fixed inset-0 bg-black/30 z-50 flex items-center justify-center p-4"
       @click="detail = null">
    <div class="bg-white rounded-lg shadow-xl max-w-3xl w-full max-h-[90vh] overflow-hidden flex flex-col"
         @click.stop>
      <div class="px-4 py-3 border-b border-slate-200 flex items-center justify-between">
        <div class="flex items-center gap-2">
          <span class="badge"
                :class="detail?.op === 'INSERT' ? 'badge-ok' :
                        detail?.op === 'UPDATE' ? 'badge-info' : 'badge-critical'"
                x-text="detail?.op"></span>
          <span class="mono text-sm font-semibold"
                x-text="(detail?.svc || '') + ':' + (detail?.table || '') + ':' + (detail?.pk || '')"></span>
        </div>
        <button @click="detail = null" class="btn btn-ghost text-sm">
          <i data-lucide="x" class="icon"></i>
        </button>
      </div>
      <div class="flex-1 overflow-auto p-4 space-y-3">
        <div x-show="detail?.indexes">
          <h4 class="text-xs font-semibold uppercase text-slate-500 mb-1">Indexes</h4>
          <div class="flex flex-wrap gap-1">
            <template x-for="(v,k) in (detail?.indexes || {})" :key="k">
              <a :href="'/admin/events?idx=' + k + '&val=' + v"
                 class="badge badge-info mono">
                <span x-text="k"></span>=<span x-text="v"></span>
              </a>
            </template>
          </div>
        </div>
        <div>
          <h4 class="text-xs font-semibold uppercase text-slate-500 mb-1">Row data</h4>
          <pre class="mono text-xs bg-slate-50 p-3 rounded border border-slate-200 overflow-x-auto"
               x-text="JSON.stringify({ before: detail?.before, after: detail?.after }, null, 2)"></pre>
        </div>
        <div class="text-xs text-slate-500">
          binlog <span class="mono" x-text="detail?.binlog_file + ':' + detail?.binlog_pos"></span>
          · gtid <span class="mono" x-text="detail?.gtid || '-'"></span>
        </div>
        <div class="flex items-center gap-2 pt-2 border-t border-slate-100">
          <button x-show="detail?.indexes" @click="findRelated()"
                  class="btn btn-outline text-xs">
            <i data-lucide="git-fork" class="icon w-3 h-3"></i> 找跨服务关联
          </button>
          <button x-show="detail?.related_incident" @click="goToIncident()"
                  class="btn btn-outline text-xs">
            <i data-lucide="search-code" class="icon w-3 h-3"></i> 跳到 incident
          </button>
        </div>
      </div>
    </div>
  </div>
</div>
`
	script := `
<script>
function eventsPage() {
  return {
    // SSE / 缓冲
    sseStatus: 'init',
    sse: null,
    buffer: [],       // 全部收到的事件 (最多 1000 条)
    paused: false,

    // 过滤
    filterSvc: '',
    filterTable: '',
    filterOp: '',
    services: [],
    tables: [],

    // 搜索
    searchIdx: 'pi_id',
    searchVal: '',
    indexKeys: ['pi_id', 'order_id', 'merchant_id', 'idempotency_key', 'transaction_id'],
    searchResults: [],
    searchAttempted: false,

    // CDC 状态
    cdcRunners: [],

    // detail modal
    detail: null,

    // URL 预填 (从 /admin/events?idx=pi_id&val=pi_xxx 跳转过来)
    initFromQuery() {
      const u = new URL(location.href);
      const idx = u.searchParams.get('idx');
      const val = u.searchParams.get('val');
      if (idx) this.searchIdx = idx;
      if (val) { this.searchVal = val; this.runSearch(); }
    },

    async boot() {
      this.initFromQuery();
      this.loadIndexKeys();
      this.loadCDC();
      this.connectSSE();
      // CDC 状态每 10s 刷新
      setInterval(() => this.loadCDC(), 10_000);
    },

    connectSSE() {
      try {
        this.sseStatus = 'init';
        if (this.sse) this.sse.close();
        this.sse = new EventSource('/api/v1/events/stream');
        this.sse.onopen = () => { this.sseStatus = 'open'; };
        this.sse.onerror = () => {
          this.sseStatus = 'err';
          // 5s 后自动重连
          setTimeout(() => this.connectSSE(), 5000);
        };
        // 服务器 connect 事件 (握手)
        this.sse.addEventListener('connect', (m) => {
          this.sseStatus = 'open';
          console.log('[SSE] connected:', m.data);
        });
        // 真正的 binlog 事件 — 关键修复: 必须用 addEventListener('binlog'),
        // 因为 server 端用了 "event: binlog" 命名事件,onmessage 收不到!
        const onBinlog = (m) => {
          if (this.paused) return;
          try {
            const e = JSON.parse(m.data);
            // 服务器把 Redis Stream 的 XADD 值平铺成 {key: value} 字典,
            // 字段名是 svc/table/pk/op/ts/binlog_pos 等
            e._uid = (e.svc || '?') + ':' + (e.table || '?') + ':' + (e.pk || '?') + ':' + (e.binlog_pos || Date.now());
            e._highlight = true;
            this.buffer.unshift(e);
            if (this.buffer.length > 1000) this.buffer.length = 1000;
            setTimeout(() => { e._highlight = false; }, 1200);
            if (e.svc && !this.services.includes(e.svc)) this.services.push(e.svc);
            if (e.table && !this.tables.includes(e.table)) this.tables.push(e.table);
          } catch (err) { console.warn('[SSE] bad msg:', err); }
        };
        this.sse.addEventListener('binlog', onBinlog);
        // 兜底: 部分 server 路径走默认 message (无 event: 头)
        this.sse.onmessage = onBinlog;
      } catch (e) {
        this.sseStatus = 'err';
      }
    },

    // 注入测试事件 (dev 用,绕过 binlog 真实数据流)
    async injectTestEvent() {
      const sample = {
        svc: ['order-core', 'payment-channel', 'accounting-system'][Math.floor(Math.random()*3)],
        table: ['payment_intent', 'acquirer_tx', 'ledger_entry'][Math.floor(Math.random()*3)],
        pk: 'test_' + Math.floor(Math.random()*1e6),
        op: ['INSERT', 'UPDATE', 'DELETE'][Math.floor(Math.random()*3)],
        ts: new Date().toISOString(),
        binlog_pos: Date.now(),
        indexes: { pi_id: 'pi_test_' + Math.floor(Math.random()*100) },
        after: { id: 'test', amount: Math.floor(Math.random()*10000) },
      };
      try {
        const r = await fetch('/api/v1/events/stream/_inject', {
          method: 'POST', headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(sample),
        });
        if (!r.ok) {
          // 后端没接 inject endpoint → 本地直接 push 到 buffer 模拟
          sample._uid = sample.svc + ':' + sample.table + ':' + sample.pk;
          sample._highlight = true;
          this.buffer.unshift(sample);
          if (!this.services.includes(sample.svc)) this.services.push(sample.svc);
          if (!this.tables.includes(sample.table)) this.tables.push(sample.table);
          setTimeout(() => { sample._highlight = false; }, 1200);
        }
      } catch (_) {
        // 网络挂了也本地模拟
      }
    },

    get filtered() {
      return this.buffer.filter(e =>
        (!this.filterSvc   || e.svc   === this.filterSvc)   &&
        (!this.filterTable || e.table === this.filterTable) &&
        (!this.filterOp    || e.op    === this.filterOp)
      );
    },

    togglePause() {
      this.paused = !this.paused;
    },
    clearStream() {
      this.buffer = [];
    },

    formatTs(ts) {
      if (!ts) return '';
      try {
        const d = new Date(ts);
        return d.toTimeString().slice(0, 8) + '.' + String(d.getMilliseconds()).padStart(3, '0');
      } catch (_) { return String(ts).slice(11, 23); }
    },

    async runSearch() {
      this.searchAttempted = true;
      if (!this.searchVal) { this.searchResults = []; return; }
      try {
        const r = await fetch('/api/v1/search?index=' + encodeURIComponent(this.searchIdx) +
                              '&value=' + encodeURIComponent(this.searchVal) + '&limit=50')
          .then(r => r.json()).catch(() => []);
        const list = Array.isArray(r) ? r : (r.events || []);
        list.forEach(e => {
          e._uid = (e.svc || '?') + ':' + (e.table || '?') + ':' + (e.pk || '?');
        });
        this.searchResults = list;
      } catch (e) { console.error(e); this.searchResults = []; }
    },

    async loadIndexKeys() {
      try {
        const r = await fetch('/api/v1/meta/idx_keys').then(r => r.json()).catch(() => null);
        if (Array.isArray(r) && r.length > 0) this.indexKeys = r;
      } catch (_) {}
    },

    async loadCDC() {
      try {
        const r = await fetch('/api/v1/cdc/status').then(r => r.json()).catch(() => null);
        const list = Array.isArray(r) ? r : (r && r.runners) || [];
        // 标准化字段
        this.cdcRunners = list.map(x => ({
          service: x.service || x.svc || 'unknown',
          shard: x.shard !== undefined ? x.shard : (x.idx !== undefined ? x.idx : 0),
          healthy: x.healthy !== false && (x.lag_ms === undefined || x.lag_ms < 5000),
          binlog_pos: (x.binlog_file || x.last_file || '') + ':' + (x.binlog_pos || x.last_pos || 0),
          lag_ms: x.lag_ms,
          events_per_sec: x.events_per_sec,
        }));
      } catch (_) {}
    },

    openDetail(e) {
      this.detail = e;
      this.$nextTick(() => { if (window.lucide) lucide.createIcons(); });
    },

    findRelated() {
      if (!this.detail || !this.detail.indexes) return;
      const [k, v] = Object.entries(this.detail.indexes)[0] || [];
      if (!k) return;
      this.searchIdx = k; this.searchVal = v;
      this.detail = null;
      this.runSearch();
    },

    goToIncident() {
      if (this.detail && this.detail.related_incident) {
        location.href = '/admin/incidents/' + encodeURIComponent(this.detail.related_incident);
      }
    },
  };
}
</script>
<style>
  [x-cloak] { display: none !important; }
</style>
`
	adminPage(w, "Binlog Live Events", "events", body, "", script)
}
