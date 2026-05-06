// fxprovider.go：统一 fx Provider，让 14 服务接 config-center 一行接入。
//
// 用法（main.go）：
//
//	app := fx.New(
//	    fx.Provide(
//	        configcenter.FxProvider("api-gateway"), // namespace
//	        ...
//	    ),
//	    ...
//	)
//
// 然后业务 service 直接 inject `*configcenter.Client`：
//
//	type RiskService struct { cli *configcenter.Client }
//	func NewRiskService(cli *configcenter.Client, ...) *RiskService { ... }
//
// **prod fail-fast / dev 兜底**：
//
//	env=prod / production：config-center 不可达 → 启动失败（防服务带空 cache 上线）
//	其它 env：返 nil；service 层 nil-check 走 def 兜底
//
// 配置（yaml / env，env 前缀由 caller 控制如 APIGW_*）：
//
//	configcenter.endpoint    = "http://config-center:9691"
//	configcenter.namespace   = <由 FxProvider 入参提供，env 可覆盖>
//	configcenter.instance_id = HOSTNAME / POD_NAME（留空自动）
package configcenter

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/viper"
	"go.uber.org/zap"
)

// FxProvider 返一个 fx 兼容的 provider 函数。
//
// defaultNamespace 当 yaml / env 没显式设 configcenter.namespace 时用。一般传服务名。
func FxProvider(defaultNamespace string) interface{} {
	return func(v *viper.Viper, logger *zap.Logger) (*Client, error) {
		return NewFromViper(v, defaultNamespace, logger)
	}
}

// AssertProdMandatory 在 env=prod / production 时校验 configcenter.endpoint 必填。
//
// 各服务的 assertProdSafety 末尾调一次：
//
//	if err := configcenter.AssertProdMandatory(v); err != nil {
//	    return err
//	}
//
// 这把 config-center 提升为生产级强依赖：无 endpoint → 启动失败，不允许
// 服务带 yaml-only 配置上 prod。
func AssertProdMandatory(v *viper.Viper) error {
	env := strings.ToLower(strings.TrimSpace(v.GetString("env")))
	if env != "prod" && env != "production" {
		return nil
	}
	if strings.TrimSpace(v.GetString("configcenter.endpoint")) == "" {
		return fmt.Errorf("PROD-SAFETY: configcenter.endpoint must be configured in env=prod (config-center 是全平台动态配置强依赖)")
	}
	return nil
}

// NewFromViper 从 viper 读 endpoint / namespace / instance_id 构造 Client。
//
// 不能用 fx 的服务（如 cmd/script）也可手动调本函数。
func NewFromViper(v *viper.Viper, defaultNamespace string, logger *zap.Logger) (*Client, error) {
	if logger == nil {
		logger = zap.NewNop()
	}
	endpoint := v.GetString("configcenter.endpoint")
	if endpoint == "" {
		endpoint = "http://config-center:9691"
	}
	namespace := v.GetString("configcenter.namespace")
	if namespace == "" {
		namespace = defaultNamespace
	}
	if namespace == "" {
		return nil, fmt.Errorf("configcenter.namespace required (yaml / env or default)")
	}
	instanceID := v.GetString("configcenter.instance_id")
	if instanceID == "" {
		instanceID, _ = os.Hostname()
	}
	if instanceID == "" {
		instanceID = "anon-" + fmt.Sprint(time.Now().UnixNano())
	}

	// 启动期重试：业务服务可能比 config-center 早起来 (deploy 顺序 / 重启风暴 /
	// 容器编排时序)。每次失败退避 2s, 4s, 8s ... 最长 60s，总等待最多 5 分钟，
	// 给 config-center pod 足够时间 init MySQL + 监听端口。
	//
	// 5 分钟还连不上 → prod fail-fast 启动失败（K8s 会重启），dev 返 nil 兜底。
	rpc := NewHTTPClient(endpoint, nil)
	var (
		cli      *Client
		err      error
		backoff  = 2 * time.Second
		maxWait  = 5 * time.Minute
		started  = time.Now()
	)
	for {
		cli, err = NewWithRPC(rpc, Config{
			Namespace:        namespace,
			InstanceID:       instanceID,
			ReconnectBackoff: 1 * time.Second,
			InitTimeout:      10 * time.Second,
			Logger:           logger,
		})
		if err == nil {
			break
		}
		if time.Since(started) >= maxWait {
			break
		}
		logger.Warn("config-center not ready yet; retrying",
			zap.String("endpoint", endpoint),
			zap.Duration("backoff", backoff),
			zap.Duration("elapsed", time.Since(started)),
			zap.Error(err))
		time.Sleep(backoff)
		backoff *= 2
		if backoff > 60*time.Second {
			backoff = 60 * time.Second
		}
	}
	if err != nil {
		env := strings.ToLower(strings.TrimSpace(v.GetString("env")))
		if env == "prod" || env == "production" {
			return nil, fmt.Errorf("config-center init failed after %s (prod fail-fast): %w", maxWait, err)
		}
		logger.Warn("config-center init failed after retry; non-prod degrades to all-defaults",
			zap.String("namespace", namespace),
			zap.Error(err))
		return nil, nil
	}
	logger.Info("config-center connected",
		zap.String("endpoint", endpoint),
		zap.String("namespace", namespace),
		zap.String("instance_id", instanceID))
	return cli, nil
}
