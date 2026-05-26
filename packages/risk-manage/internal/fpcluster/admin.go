// admin.go — fpcluster admin endpoints。
//
//	GET  /admin/fpcluster/{id}           看 cluster 信息
//	GET  /admin/fpcluster/count          当前 cluster 数
//	POST /admin/fpcluster/rebuild        全量 DBSCAN 重建（admin 触发）
package fpcluster

import (
	"context"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"time"
)

// FingerprintProvider 返回 ClusterStore 重建用的全量指纹集合。
// 主流程注入：从 LinkStore / Postgres 拉所有 last_observed 90 天内的 fingerprint。
type FingerprintProvider interface {
	ListFingerprints(ctx context.Context) ([]Fingerprint, error)
}

// AdminHandlers 给主流程挂 routes。
func AdminHandlers(store ClusterStore, provider FingerprintProvider) http.Handler {
	mux := http.NewServeMux()

	// in-flight rebuild 防并发
	var rebuilding int32

	mux.HandleFunc("/admin/fpcluster/count", func(w http.ResponseWriter, r *http.Request) {
		n, err := store.ClusterCount(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]interface{}{"cluster_count": n})
	})

	mux.HandleFunc("/admin/fpcluster/rebuild", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		if !atomic.CompareAndSwapInt32(&rebuilding, 0, 1) {
			http.Error(w, "rebuild already in progress", http.StatusTooManyRequests)
			return
		}
		defer atomic.StoreInt32(&rebuilding, 0)

		if provider == nil {
			http.Error(w, "fingerprint provider not configured", http.StatusServiceUnavailable)
			return
		}

		start := time.Now()
		fps, err := provider.ListFingerprints(r.Context())
		if err != nil {
			http.Error(w, "fetch fingerprints: "+err.Error(), http.StatusInternalServerError)
			return
		}
		eps := DefaultEps
		minPts := DefaultMinPts
		if v := r.URL.Query().Get("eps"); v != "" {
			if n, e := parseIntInRange(v, 1, 32); e == nil {
				eps = n
			}
		}
		if v := r.URL.Query().Get("min_pts"); v != "" {
			if n, e := parseIntInRange(v, 1, 100); e == nil {
				minPts = n
			}
		}

		result := DBSCAN(fps, eps, minPts)
		if err := store.ReplaceAll(r.Context(), result); err != nil {
			http.Error(w, "replace cluster store: "+err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]interface{}{
			"fingerprint_count": len(fps),
			"cluster_count":     len(result.Clusters),
			"noise_count":       len(result.Noise),
			"eps":               eps,
			"min_pts":           minPts,
			"elapsed_ms":        time.Since(start).Milliseconds(),
		})
	})

	mux.HandleFunc("/admin/fpcluster/", func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Path[len("/admin/fpcluster/"):]
		if id == "" || id == "count" || id == "rebuild" {
			http.NotFound(w, r)
			return
		}
		cl, err := store.GetCluster(r.Context(), id)
		if err != nil {
			http.Error(w, "cluster not found", http.StatusNotFound)
			return
		}
		writeJSON(w, cl)
	})
	return mux
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func parseIntInRange(s string, lo, hi int) (int, error) {
	n := 0
	if _, err := jsonNumberScan(s, &n); err != nil {
		return 0, err
	}
	if n < lo || n > hi {
		return 0, errOutOfRange
	}
	return n, nil
}

var errOutOfRange = errString("out of range")

// 简易整数 scan（避免 strconv 在小热路径再次依赖）
func jsonNumberScan(s string, out *int) (int, error) {
	v := 0
	for i, c := range s {
		if c < '0' || c > '9' {
			return i, errOutOfRange
		}
		v = v*10 + int(c-'0')
	}
	*out = v
	return len(s), nil
}

type errString string

func (e errString) Error() string { return string(e) }
