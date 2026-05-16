// page_incident.go — /admin/incidents/{id} 单笔 diff 全景.
//
// 内容:
//   - 顶部 summary 卡: rule_name / severity / state / opened_at / SLA
//   - 5 态状态机 timeline (NEW → ACK → INVESTIGATING → RESOLVED / DISMISSED)
//   - Cytoscape 全屏跨服务事件关联图
//   - Detail JSON pretty-print
//   - 关联 diff 列表 (同 idempotency_key / pi_id 的其它 diff)
//   - 评论 + 操作日志
//
// /admin/incidents (无 id) 显示最近 100 条 incident 列表.
package api

import (
	"net/http"
	"strings"
)

func (s *Server) pageIncident(w http.ResponseWriter, r *http.Request) {
	// 提 id (/admin/incidents/<id> or /admin/incidents)
	path := strings.TrimPrefix(r.URL.Path, "/admin/incidents")
	path = strings.Trim(path, "/")

	if path == "" {
		s.pageIncidentList(w, r)
		return
	}
	s.pageIncidentDetail(w, r, path)
}

// pageIncidentList — incident 列表
func (s *Server) pageIncidentList(w http.ResponseWriter, _ *http.Request) {
	body := `
<div x-data="incidentList()" x-init="load()" class="space-y-4">
  <!-- 工具条 -->
  <div class="flex items-center gap-3">
    <input type="search" x-model="q" placeholder="搜索 key / rule / state..."
           class="pl-9 pr-3 py-1.5 text-sm border border-slate-300 rounded-md w-72 relative"
           @keyup.enter="load()">
    <select x-model="state" class="text-sm border border-slate-300 rounded-md px-2 py-1.5">
      <option value="">全部状态</option>
      <option>NEW</option>
      <option>ACK</option>
      <option>INVESTIGATING</option>
      <option>RESOLVED</option>
      <option>DISMISSED</option>
    </select>
    <button @click="load()" class="btn btn-outline text-sm">
      <i data-lucide="refresh-cw" class="icon"></i> 刷新
    </button>
  </div>

  <!-- 列表 -->
  <div class="bg-white rounded-lg border border-slate-200 overflow-hidden">
    <table class="w-full">
      <thead class="bg-slate-50">
        <tr>
          <th class="table-th">ID</th>
          <th class="table-th">Rule</th>
          <th class="table-th">Key</th>
          <th class="table-th">Severity</th>
          <th class="table-th">State</th>
          <th class="table-th">Age</th>
        </tr>
      </thead>
      <tbody class="divide-y divide-slate-100">
        <template x-for="d in items" :key="d.id">
          <tr class="hover:bg-slate-50 cursor-pointer" @click="open(d.id)">
            <td class="table-td mono text-xs" x-text="d.id"></td>
            <td class="table-td font-medium" x-text="d.rule_name"></td>
            <td class="table-td mono text-xs text-slate-500" x-text="d.key"></td>
            <td class="table-td">
              <span class="badge"
                    :class="d.severity==='critical' ? 'badge-critical' :
                            d.severity==='warning' ? 'badge-warning' : 'badge-info'"
                    x-text="d.severity"></span>
            </td>
            <td class="table-td">
              <span class="badge" :class="stateClass(d.state)" x-text="d.state || 'NEW'"></span>
            </td>
            <td class="table-td text-xs text-slate-500" x-text="d.age || '-'"></td>
          </tr>
        </template>
        <tr x-show="items.length === 0">
          <td colspan="6" class="table-td text-center text-slate-400">无 incident</td>
        </tr>
      </tbody>
    </table>
  </div>
</div>
`
	script := `
<script>
function incidentList() {
  return {
    items: [], q: '', state: '',
    async load() {
      const url = '/api/v1/diffs?limit=100'
        + (this.q ? '&q=' + encodeURIComponent(this.q) : '')
        + (this.state ? '&state=' + encodeURIComponent(this.state) : '');
      const r = await fetch(url).then(r => r.json()).catch(() => []);
      this.items = Array.isArray(r) ? r : [];
    },
    open(id) { window.location = '/admin/incidents/' + encodeURIComponent(id); },
    stateClass(s) {
      return ({
        'NEW': 'badge-critical', 'ACK': 'badge-warning',
        'INVESTIGATING': 'badge-info', 'RESOLVED': 'badge-ok',
        'DISMISSED': 'bg-slate-200 text-slate-700',
      })[s] || 'badge-info';
    },
  };
}
</script>
`
	adminPage(w, "Incidents", "incidents", body, "", script)
}

// pageIncidentDetail — 单笔 incident 详情
func (s *Server) pageIncidentDetail(w http.ResponseWriter, _ *http.Request, id string) {
	body := `
<div x-data="incidentDetail('` + id + `')" x-init="load()" class="grid grid-cols-1 lg:grid-cols-3 gap-4">

  <!-- 主区: summary + state machine + cytoscape -->
  <div class="lg:col-span-2 space-y-4">

    <!-- summary -->
    <div class="stat-card">
      <div class="flex items-start justify-between mb-3">
        <div>
          <div class="flex items-center gap-2 mb-1">
            <h2 class="text-base font-semibold" x-text="diff.rule_name || '...'"></h2>
            <span class="badge" :class="severityClass(diff.severity)" x-text="diff.severity || 'info'"></span>
          </div>
          <div class="mono text-xs text-slate-500" x-text="diff.key"></div>
        </div>
        <div class="flex items-center gap-1.5">
          <button @click="ack()" class="btn btn-outline text-xs" x-show="diff.state === 'NEW'">
            <i data-lucide="check" class="icon w-3 h-3"></i> Ack
          </button>
          <button @click="resolve()" class="btn btn-primary text-xs" x-show="['ACK','INVESTIGATING'].includes(diff.state)">
            <i data-lucide="check-circle" class="icon w-3 h-3"></i> Resolve
          </button>
          <button @click="dismiss()" class="btn btn-outline text-xs">
            <i data-lucide="x" class="icon w-3 h-3"></i> Dismiss
          </button>
        </div>
      </div>
      <!-- 5 态状态机 -->
      <div class="flex items-center gap-1 text-xs">
        <template x-for="(s, i) in stages" :key="s">
          <div class="flex items-center gap-1">
            <span class="w-6 h-6 rounded-full flex items-center justify-center"
                  :class="stageActive(s) ? 'bg-brand-600 text-white' : 'bg-slate-200 text-slate-500'">
              <span x-text="i+1"></span>
            </span>
            <span :class="stageActive(s) ? 'text-slate-900 font-medium' : 'text-slate-400'" x-text="s"></span>
            <i x-show="i < stages.length - 1" data-lucide="chevron-right" class="icon text-slate-300"></i>
          </div>
        </template>
      </div>
    </div>

    <!-- Cytoscape 跨服务事件图 -->
    <div class="stat-card">
      <div class="flex items-center justify-between mb-3">
        <h3 class="text-sm font-semibold">跨服务事件关联图</h3>
        <button @click="fullscreenGraph()" class="btn btn-ghost text-xs">
          <i data-lucide="maximize" class="icon w-3 h-3"></i> 全屏
        </button>
      </div>
      <div id="cy" class="w-full bg-slate-50 rounded border border-slate-200" style="height: 380px"></div>
    </div>

    <!-- Detail JSON -->
    <div class="stat-card">
      <h3 class="text-sm font-semibold mb-2">Detail</h3>
      <pre class="text-xs mono bg-slate-50 p-3 rounded border border-slate-200 overflow-x-auto"
           x-text="JSON.stringify(diff.detail || {}, null, 2)"></pre>
    </div>
  </div>

  <!-- 侧栏: 关联 diff + 评论 + 操作历史 -->
  <div class="space-y-4">

    <div class="stat-card">
      <h3 class="text-sm font-semibold mb-2">关联 Diff</h3>
      <ul class="space-y-1">
        <template x-for="r in related" :key="r.id">
          <li>
            <a :href="'/admin/incidents/' + r.id"
               class="block text-xs hover:bg-slate-50 -mx-2 px-2 py-1.5 rounded">
              <div class="font-medium" x-text="r.rule_name"></div>
              <div class="mono text-slate-400 truncate" x-text="r.id"></div>
            </a>
          </li>
        </template>
        <li x-show="related.length === 0" class="text-xs text-slate-400 py-2">无</li>
      </ul>
    </div>

    <div class="stat-card">
      <h3 class="text-sm font-semibold mb-2">评论</h3>
      <div class="space-y-3 mb-3 max-h-60 overflow-y-auto">
        <template x-for="c in comments" :key="c.id">
          <div class="text-xs border-l-2 border-slate-200 pl-2">
            <div class="flex items-center gap-2">
              <span class="font-medium" x-text="c.author"></span>
              <span class="text-slate-400" x-text="c.at"></span>
            </div>
            <div class="mt-0.5 text-slate-700" x-text="c.text"></div>
          </div>
        </template>
        <div x-show="comments.length===0" class="text-xs text-slate-400">无评论</div>
      </div>
      <textarea x-model="newComment" placeholder="写评论..." rows="2"
                class="w-full text-xs border border-slate-300 rounded p-2"></textarea>
      <button @click="addComment()" class="btn btn-primary text-xs mt-2 w-full">
        <i data-lucide="message-square-plus" class="icon w-3 h-3"></i> 发布
      </button>
    </div>

    <div class="stat-card">
      <h3 class="text-sm font-semibold mb-2">操作历史</h3>
      <ul class="space-y-2 text-xs">
        <template x-for="h in history" :key="h.id">
          <li class="flex items-start gap-2">
            <i data-lucide="dot" class="icon text-slate-300 mt-0.5"></i>
            <div>
              <span class="font-medium" x-text="h.actor"></span>
              <span class="text-slate-500" x-text="h.action"></span>
              <span class="text-slate-400 ml-1" x-text="h.at"></span>
            </div>
          </li>
        </template>
      </ul>
    </div>
  </div>
</div>
`
	extraHead := `<script src="https://cdn.jsdelivr.net/npm/cytoscape@3.28.1/dist/cytoscape.min.js"></script>`

	script := `
<script>
function incidentDetail(id) {
  return {
    id,
    diff: {},
    related: [],
    comments: [],
    history: [],
    newComment: '',
    stages: ['NEW', 'ACK', 'INVESTIGATING', 'RESOLVED', 'DISMISSED'],

    async load() {
      try {
        const r = await fetch('/api/v1/diffs/' + encodeURIComponent(this.id)).then(r => r.json());
        this.diff     = r || {};
        this.related  = r.related  || [];
        this.comments = r.comments || [];
        this.history  = r.history  || [];
        this.renderGraph(r.graph);
      } catch (e) { console.error(e); }
    },

    stageActive(s) {
      const cur = this.diff.state || 'NEW';
      const idx = this.stages.indexOf(cur);
      return this.stages.indexOf(s) <= idx;
    },

    severityClass(s) {
      return ({
        'critical': 'badge-critical',
        'warning':  'badge-warning',
        'info':     'badge-info',
      })[s] || 'badge-info';
    },

    renderGraph(graph) {
      const nodes = (graph && graph.nodes) || [
        { data: { id: 'svc:order-core', label: 'order-core' } },
        { data: { id: 'svc:payment-channel', label: 'payment-channel' } },
        { data: { id: 'svc:accounting-system', label: 'accounting-system' } },
      ];
      const edges = (graph && graph.edges) || [
        { data: { source: 'svc:order-core', target: 'svc:payment-channel', label: this.id } },
        { data: { source: 'svc:payment-channel', target: 'svc:accounting-system', label: this.id } },
      ];
      cytoscape({
        container: document.getElementById('cy'),
        elements: { nodes, edges },
        style: [
          { selector: 'node', style: {
            'background-color': '#3b82f6', 'label': 'data(label)',
            'color': '#fff', 'text-valign': 'center', 'text-halign': 'center',
            'font-size': 11, 'width': 92, 'height': 42, 'shape': 'roundrectangle',
          }},
          { selector: 'edge', style: {
            'width': 2, 'line-color': '#94a3b8',
            'target-arrow-shape': 'triangle', 'target-arrow-color': '#94a3b8',
            'curve-style': 'bezier', 'label': 'data(label)',
            'font-size': 9, 'color': '#64748b',
          }},
        ],
        layout: { name: 'cose' },
      });
    },

    fullscreenGraph() {
      const el = document.getElementById('cy');
      if (el.requestFullscreen) el.requestFullscreen();
    },

    async ack()      { await this.transition('ACK'); },
    async resolve()  { await this.transition('RESOLVED'); },
    async dismiss()  { await this.transition('DISMISSED'); },

    async transition(state) {
      await fetch('/api/v1/diffs/' + this.id, {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ state }),
      });
      this.load();
    },

    async addComment() {
      if (!this.newComment.trim()) return;
      await fetch('/api/v1/diffs/' + this.id + '/comments', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ text: this.newComment }),
      });
      this.newComment = '';
      this.load();
    },
  };
}
</script>
`
	adminPage(w, "Incident #"+id, "incidents", body, extraHead, script)
}
