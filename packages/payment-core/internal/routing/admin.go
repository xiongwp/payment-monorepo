// Package routing — admin.go：路由规则的运维 HTTP 端点。
//
//	GET  /admin/routing/rules    返回当前生效的规则集（JSON）
//	POST /admin/routing/reload   重新读 viper 配置 + ReplaceRules 原子替换
//
// 用途：加新渠道 / 调价 / 灰度切量时不用重启 payment-core。在 k8s 下 ConfigMap
// 热挂载 + `curl -X POST .../admin/routing/reload` 即可。
//
// 安全：鉴权由调用方 wrap middleware 注入（与 accounting-system adminhttp
// 共用同样的 X-Admin-Token 协议；为空时不鉴权，开发默认）。
package routing

import (
	"encoding/json"
	"net/http"

	"github.com/spf13/viper"
	"go.uber.org/zap"
)

// RuleLoader 从 viper 抽取规则。独立成函数便于 reload 复用 + 测试 mock。
func RuleLoader(v *viper.Viper) ([]Rule, error) {
	var cfgRules []struct {
		Priority      int    `mapstructure:"priority"`
		Merchant      string `mapstructure:"merchant"`
		Country       string `mapstructure:"country"`
		PaymentMethod string `mapstructure:"payment_method"`
		AmountMin     int64  `mapstructure:"amount_min"`
		AmountMax     int64  `mapstructure:"amount_max"`
		Adapter       string `mapstructure:"adapter"`
	}
	if err := v.UnmarshalKey("routing.rules", &cfgRules); err != nil {
		return nil, err
	}
	rules := make([]Rule, 0, len(cfgRules))
	for _, c := range cfgRules {
		rules = append(rules, Rule{
			Priority:      c.Priority,
			Merchant:      c.Merchant,
			Country:       c.Country,
			PaymentMethod: c.PaymentMethod,
			AmountMin:     c.AmountMin,
			AmountMax:     c.AmountMax,
			Adapter:       c.Adapter,
		})
	}
	return rules, nil
}

// AdminHandler 返回一个 mux，挂到外部 HTTP server 上即可。
func AdminHandler(r *Router, v *viper.Viper, logger *zap.Logger) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/admin/routing/rules", func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"count": len(r.Rules()),
			"rules": r.Rules(),
		})
	})
	mux.HandleFunc("/admin/routing/reload", func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		// ReadInConfig 会重新读磁盘文件；ConfigMap 挂载时 k8s 会把新内容写进来。
		if err := v.ReadInConfig(); err != nil {
			logger.Error("routing reload: re-read config failed", zap.Error(err))
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		rules, err := RuleLoader(v)
		if err != nil {
			logger.Error("routing reload: parse rules failed", zap.Error(err))
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		r.ReplaceRules(rules)
		logger.Info("routing rules reloaded", zap.Int("count", len(rules)))
		writeJSON(w, http.StatusOK, map[string]any{"reloaded": true, "count": len(rules)})
	})
	return mux
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}
