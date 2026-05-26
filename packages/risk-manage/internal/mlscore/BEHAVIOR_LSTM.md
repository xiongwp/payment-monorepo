# Behavior LSTM — 行为生物时序模型接入

## 1. 定位

跟 `OnnxService`（GBDT，tabular fraud probability）并行的一条 ML 通道：
- 输入：用户在 checkout 页的**鼠标轨迹** + **按键节奏**时序
- 输出：`behavior_anomaly_score ∈ [0, 1]`，越高越像 bot / 自动化脚本

集成位置 — `service.Screen` 在 ml_score stage 之后新增一个**可选** behavior_lstm
stage（feature flag `BEHAVIOR_LSTM_ENABLED` 默认 off）。stage 输出写回
`TxnContext.BehaviorAnomalyScore`，规则 `behavior_lstm_threshold` 按阈值打分。

## 2. 模型架构（Python 团队负责）

推荐起点：

```
[mouse_seq (1, 200, 3)]  →  LSTM(64) → Dropout(0.3) → ┐
                                                       ├→ Concat(128) → Dense(64) → Dense(1, sigmoid)
[key_seq   (1, 50,  3)]  →  LSTM(64) → Dropout(0.3) → ┘
```

替代：
- Conv1D + GRU（参数少 30%，推理快但召回略低）
- Transformer encoder（数据 > 100K 再考虑；当前 10K 标注样本不够撑得起）

二分类目标：`y ∈ {0=human, 1=bot}`。验证集 AUC ≥ 0.88 才允许上线。

## 3. 输入预处理 contract（serving / training 必须一致）

### 3.1 鼠标时序 — `(B=1, T=200, F=3)`

`F = [x_norm, y_norm, t_norm]`：

| 维度 | 归一化公式 | 备注 |
|------|-----------|------|
| `x_norm` | `x / screen_width` | 默认 screen_width=1920 |
| `y_norm` | `y / screen_height` | 默认 screen_height=1080 |
| `t_norm` | `t_ms / session_duration_ms` | session 起点对齐到 0 |

- 不足 200 点：**左 padding 0**（数据右对齐，让 LSTM 末态看到最近活动）
- 超长：**保留最后 200 点**（最近活动最有信号）
- 异常坐标（负数 / 越界）：clamp 到 [0, 1]

### 3.2 按键时序 — `(B=1, T=50, F=3)`

`F = [keycode_norm, dwell_norm, flight_norm]`：

| 维度 | 归一化 | 备注 |
|------|-------|------|
| `keycode_norm` | `keycode / 256` | 覆盖 ASCII + 常见控制键 |
| `dwell_norm` | `dwell_ms / 1000` | 按下持续时间，多数 < 200ms |
| `flight_norm` | `flight_ms / 1000` | 与前一次按键间隔，多数 < 500ms |

同样左 padding + 尾截断。

### 3.3 输出 — `(B=1, 1)`

单一浮点 ∈ [0,1]。clamp + NaN/Inf → 0.5 中性兜底。

## 4. 训练流程（Python 团队 TODO）

### 4.1 数据来源 — `audit_replay`

我们生产环境从 P0 开始已经在落 `audit replay`（每条 Screen 把入参/出参/SDK 原始
信号 dump 到 S3）。Python 团队回放：

```python
from risk_replay import iter_screens

# 拉过去 30 天的 audit；正样本：客户标注 bot / chargeback；负样本：>30 天无 dispute
for screen in iter_screens(start='2024-09-01', end='2024-09-30'):
    mouse = screen.signals.get('mouse_events', [])  # [{x, y, t}, ...]
    keys  = screen.signals.get('keystrokes', [])    # [{keycode, dwell, flight}, ...]
    label = derive_label(screen.decision_id)        # 0 / 1
    yield mouse, keys, label
```

初版目标：**10K 标注样本**（5K human + 5K bot）。

### 4.2 训练 — PyTorch / Keras

```python
import torch.nn as nn

class BehaviorLSTM(nn.Module):
    def __init__(self):
        super().__init__()
        self.mouse_lstm = nn.LSTM(3, 64, batch_first=True)
        self.key_lstm   = nn.LSTM(3, 64, batch_first=True)
        self.head = nn.Sequential(
            nn.Linear(128, 64), nn.ReLU(),
            nn.Linear(64, 1), nn.Sigmoid(),
        )

    def forward(self, mouse, keys):
        _, (hm, _) = self.mouse_lstm(mouse)
        _, (hk, _) = self.key_lstm(keys)
        h = torch.cat([hm[-1], hk[-1]], dim=-1)
        return self.head(h)
```

训练：BCE loss + Adam(lr=1e-3) + early stopping(patience=5)。

### 4.3 ONNX 导出

```python
mouse = torch.zeros(1, 200, 3)
keys  = torch.zeros(1, 50, 3)
torch.onnx.export(
    model, (mouse, keys),
    "behavior_lstm_v1_<gitsha>.onnx",
    input_names  = ["mouse_seq", "keystroke_seq"],
    output_names = ["anomaly_score"],
    opset_version=15,
    # 重要：不开 dynamic_axes —— Go 侧 batch=1 硬编码，动态 batch 会让
    # session.GetInputs() 返 shape=[-1,…]，绕开 shape 校验埋坑
)
```

Keras 用 `tf2onnx.convert` 一样的产物。

### 4.4 上线流程

```
1. 模型上传 S3：s3://risk-models/behavior_lstm/behavior_lstm_v1_<gitsha>.onnx
2. 运维 admin endpoint:
     POST /admin/ml/behavior_lstm/reload
     {"model_path": "/var/models/behavior_lstm_v1_a1b2c3d.onnx"}
3. champion-challenger 灰度：1% → 10% → 100%（复用 mlscore.RolloutController）
4. 7 天滚动 AUC 监控；阈值 0.85 下打告警
```

## 5. Go 侧 API 一览

```go
// build with: go build -tags onnx ./cmd/server

svc, err := mlscore.NewBehaviorLSTMService("/var/models/behavior_lstm_v1.onnx")
if err != nil {
    // fallback: nil svc，Score 仍返 neutralScore (0.5)
    log.Warn("behavior_lstm disabled", zap.Error(err))
    svc = nil
}

res, err := svc.Score(ctx, mlscore.BehaviorInput{
    Mouse:        events,
    Keystrokes:   keys,
    ScreenW:      1920, ScreenH: 1080,
    SessionDurMs: 60000,
})
// res.AnomalyScore ∈ [0, 1]; res.ModelVer = derive from filename
```

stub build（默认）：`Score` 永远返 `0.5`；不依赖 onnxruntime 库。

## 6. 跟 OnnxService 的对比表

| 维度 | `OnnxService`（GBDT） | `BehaviorLSTMService`（本文档） |
|------|----------------------|--------------------------------|
| 输入 | tabular features `[F]` | sequence `[T, F]` × 2 |
| 输出语义 | fraud probability | bot likelihood |
| Fallback | 返 0（fail-open，rule 不命中） | 返 0.5（中性，rule 默认不命中） |
| Reload | 共享 `atomic.Pointer` 模式 | 同 |
| Admin endpoint | `/admin/ml/onnx/*` | `/admin/ml/behavior_lstm/*`（未来加） |
| 训练 cadence | weekly retrain | bi-weekly（行为漂移慢） |

## 7. 已知风险 & 跟 ML team 对齐项

- **train-serve skew**：Python pad 用 `pad_sequences(padding='pre')` 跟 Go 左
  padding 一致？双方验证 hash(预处理后 tensor) 一致才能上线
- **session_duration_ms**：训练样本里普遍 < 60s，但生产偶有 5min+ 退出再来；
  > 60s 全归一化成 1.0 → LSTM 末几步看不到有效信号；考虑分桶
- **adversarial robustness**：FGSM 攻击者可能用真人轨迹 + 微噪声绕过；
  红队 lab `redteam/04-fake-mouse.js` 覆盖直线轨迹场景，未覆盖噪声攻击
