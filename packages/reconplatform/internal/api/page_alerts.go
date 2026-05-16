// page_alerts.go — /admin/alerts 告警配置页 + /api/v1/alerts/* (FEAT-2).
//
// 端点:
//   GET    /api/v1/alerts           列表
//   PUT    /api/v1/alerts           创建 / 更新 (body: AlertConfig)
//   DELETE /api/v1/alerts?rule=xxx  删除一条
//
// 前端 (单页):
//   - 表格: rule_id / threshold / window / cooldown / webhook / enabled
//   - 新增 / 编辑行内表单 (一行一条)
//   - 触发测试按钮 (调 _test_fire)
package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"reconcile-system/internal/alerter"
)

// alertsAPI GET / PUT / DELETE /api/v1/alerts.
func (s *Server) alertsAPI(w http.ResponseWriter, r *http.Request) {
	if s.alertStore == nil {
		writeErr(w, http.StatusServiceUnavailable, fmt.Errorf("alerter not configured"))
		return
	}
	switch r.Method {
	case http.MethodGet:
		list, err := s.alertStore.List(r.Context())
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, list)
	case http.MethodPut, http.MethodPost:
		var c alerter.AlertConfig
		if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		if c.RuleID == "" {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("rule_id required"))
			return
		}
		if err := s.alertStore.Set(r.Context(), c); err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, c)
	case http.MethodDelete:
		ruleID := strings.TrimPrefix(r.URL.Path, "/api/v1/alerts/")
		if ruleID == "" {
			ruleID = r.URL.Query().Get("rule")
		}
		if ruleID == "" {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("rule_id required"))
			return
		}
		if err := s.alertStore.Delete(r.Context(), ruleID); err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"deleted": true})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// pageAlerts /admin/alerts.
func (s *Server) pageAlerts(w http.ResponseWriter, _ *http.Request) {
	body := `
<div x-data="alertsModel()" x-init="load()" class="space-y-6">
  <div class="flex items-center justify-between">
    <div>
      <h1 class="text-xl font-semibold text-slate-900">告警</h1>
      <p class="text-sm text-slate-500 mt-1">规则 diff 数超阈值时发 Slack-compatible webhook.</p>
    </div>
    <button @click="newRow()" class="btn btn-primary text-sm">
      <i data-lucide="plus" class="icon w-3 h-3"></i> 新增告警
    </button>
  </div>

  <div class="stat-card">
    <div class="overflow-x-auto">
      <table class="min-w-full text-sm">
        <thead class="text-xs text-slate-500 border-b border-slate-200">
          <tr>
            <th class="text-left py-2 pr-2">规则 ID</th>
            <th class="text-right py-2 px-2">阈值</th>
            <th class="text-right py-2 px-2">窗口(min)</th>
            <th class="text-right py-2 px-2">冷却(min)</th>
            <th class="text-left py-2 px-2">Webhook URL</th>
            <th class="text-center py-2 px-2">启用</th>
            <th class="py-2 pl-2"></th>
          </tr>
        </thead>
        <tbody>
          <template x-for="(a, i) in alerts" :key="a.rule_id + '_' + i">
            <tr class="border-b border-slate-100">
              <td class="py-2 pr-2 mono text-xs">
                <input type="text" x-model="a.rule_id" :disabled="!a._isNew"
                       class="w-full text-xs border border-slate-200 rounded px-1 py-0.5 mono disabled:bg-slate-50">
              </td>
              <td class="py-2 px-2 text-right">
                <input type="number" x-model.number="a.threshold" min="1"
                       class="w-16 text-right text-xs border border-slate-200 rounded px-1 py-0.5">
              </td>
              <td class="py-2 px-2 text-right">
                <input type="number" x-model.number="a.window_min" min="1"
                       class="w-16 text-right text-xs border border-slate-200 rounded px-1 py-0.5">
              </td>
              <td class="py-2 px-2 text-right">
                <input type="number" x-model.number="a.cooldown_min" min="1"
                       class="w-16 text-right text-xs border border-slate-200 rounded px-1 py-0.5">
              </td>
              <td class="py-2 px-2">
                <input type="url" x-model="a.webhook_url" placeholder="https://hooks.slack.com/services/..."
                       class="w-full text-xs border border-slate-200 rounded px-1 py-0.5 mono">
              </td>
              <td class="py-2 px-2 text-center">
                <input type="checkbox" x-model="a.enabled">
              </td>
              <td class="py-2 pl-2 flex gap-1">
                <button @click="save(a)" class="text-emerald-600 hover:text-emerald-700"
                        title="保存">
                  <i data-lucide="save" class="icon w-3 h-3"></i>
                </button>
                <button @click="del(a, i)" class="text-red-600 hover:text-red-700"
                        title="删除">
                  <i data-lucide="trash-2" class="icon w-3 h-3"></i>
                </button>
              </td>
            </tr>
          </template>
          <tr x-show="alerts.length === 0">
            <td colspan="7" class="py-8 text-center text-slate-400 text-sm">
              暂无告警配置 · 点 "新增告警" 添加
            </td>
          </tr>
        </tbody>
      </table>
    </div>
  </div>

  <div class="text-xs text-slate-500 leading-relaxed">
    <p>说明: 当规则在 N 分钟内累计 mismatched / orphan / error 类型的 diff 达到阈值,会向 webhook 发一次 POST.</p>
    <p>Slack incoming webhook: https://api.slack.com/messaging/webhooks ; 通用 webhook 也 OK (body 是 JSON).</p>
  </div>
</div>

<script>
function alertsModel() {
  return {
    alerts: [],
    async load() {
      try {
        const r = await fetch('/api/v1/alerts');
        if (!r.ok) { this.alerts = []; return; }
        this.alerts = (await r.json()) || [];
        if (window.lucide) lucide.createIcons();
      } catch (e) { console.warn('alerts load', e); }
    },
    newRow() {
      this.alerts.unshift({
        rule_id: '', threshold: 10, window_min: 5, cooldown_min: 30,
        webhook_url: '', enabled: true, _isNew: true,
      });
    },
    async save(a) {
      if (!a.rule_id) { alert('rule_id 必填'); return; }
      const r = await fetch('/api/v1/alerts', {
        method: 'PUT', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          rule_id: a.rule_id, threshold: a.threshold,
          window_min: a.window_min, cooldown_min: a.cooldown_min,
          webhook_url: a.webhook_url, enabled: !!a.enabled,
        }),
      });
      if (r.ok) {
        a._isNew = false;
        toast2('已保存');
      } else {
        const j = await r.json().catch(()=>({}));
        alert('保存失败: ' + (j.error || r.status));
      }
    },
    async del(a, i) {
      if (!confirm('删除告警 ' + a.rule_id + ' ?')) return;
      if (a._isNew) {
        this.alerts.splice(i, 1);
        return;
      }
      const r = await fetch('/api/v1/alerts?rule=' + encodeURIComponent(a.rule_id), { method: 'DELETE' });
      if (r.ok) this.alerts.splice(i, 1);
    },
  };
}
function toast2(msg) {
  const t = document.createElement('div');
  t.textContent = msg;
  t.className = 'fixed bottom-4 right-4 bg-slate-900 text-white px-3 py-1.5 rounded text-sm shadow';
  document.body.appendChild(t);
  setTimeout(()=>t.remove(), 1500);
}
</script>
`
	adminPage(w, "告警", "alerts", body, "", "")
}
