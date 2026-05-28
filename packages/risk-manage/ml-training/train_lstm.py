"""
ml-training/train_lstm.py
=========================

PyTorch LSTM 训行为序列模型 → ONNX export。

模型架构（跟 internal/mlscore/BEHAVIOR_LSTM.md & behavior_lstm.go 严格对齐）
-----------------------------------------------------------------------
    mouse_seq    (B, 200, 3)  →  LSTM(64, 2-layer, dropout=0.3) → last_hidden
    keystroke    (B,  50, 3)  →  LSTM(64, 2-layer, dropout=0.3) → last_hidden
                                                 ↓ concat (128)
                                       Dense(64, ReLU)
                                       Dropout(0.3)
                                       Dense(1, logit)   →  sigmoid → bot prob

* 二分类：y=1 bot / 自动化，y=0 真人
* class_weight 处理 imbalance（bot 通常 <5%）
* BCEWithLogitsLoss（数值稳定，logit→prob 推理时做）
* 早停：patience=5（val_auc 5 个 epoch 没提升）
* ONNX export shape：mouse=[1,200,3], keystroke=[1,50,3]，跟 Go 侧 const 一致
* 输入预处理（serving / training 必须严格一致）：
    - 鼠标：x/screen_w, y/screen_h, t_ms/session_dur_ms → clamp[0,1]
    - 按键：keycode/255, dwell/1000, flight/2000
    - 不足 max_len 左 pad 0；超长截尾保留最近 max_len 点
* 输出：
    - behavior_lstm.onnx       生产 serving
    - metadata.json            input shape、归一化常量、训练超参
    - metrics.json             AUC、loss curve

输入 parquet schema
-------------------
    decision_id           string
    mouse_events          list<struct(x:int, y:int, t:int)>      可空
    keystroke_events      list<struct(keycode:int, dwell:float, flight:float)>  可空
    screen_width          int
    screen_height         int
    session_dur_ms        int
    is_bot                int{0,1}

用法
----
    python train_lstm.py \\
        --input ./data/out/behavior_train.parquet \\
        --output-dir ./out/lstm \\
        --epochs 40 --batch-size 256 --lr 1e-3
"""

from __future__ import annotations

import argparse
import json
import logging
import os
import sys
from dataclasses import dataclass
from datetime import datetime, timezone
from pathlib import Path

import numpy as np
import pandas as pd

# ── Tensor shape contract（不能改：对齐 Go 侧 const）─────────────
MOUSE_SEQ_LEN = 200
MOUSE_FEAT_DIM = 3
KEY_SEQ_LEN = 50
KEY_FEAT_DIM = 3

# 归一化常量（必须跟 internal/mlscore/behavior_lstm.go preprocess 一致）
DEFAULT_SCREEN_W = 1920
DEFAULT_SCREEN_H = 1080
DEFAULT_SESSION_MS = 60_000
KEYCODE_DIVISOR = 255.0
DWELL_DIVISOR = 1000.0
FLIGHT_DIVISOR = 2000.0

logger = logging.getLogger("train_lstm")


def _lazy_torch():
    import torch
    import torch.nn as nn
    from torch.utils.data import Dataset, DataLoader, WeightedRandomSampler
    return torch, nn, Dataset, DataLoader, WeightedRandomSampler


def normalize_mouse(events: list, screen_w: int, screen_h: int, session_ms: int) -> np.ndarray:
    """list[{x,y,t}] → (200, 3) np.float32. 左 pad 0；超长截尾。"""
    if not events:
        return np.zeros((MOUSE_SEQ_LEN, MOUSE_FEAT_DIM), dtype=np.float32)
    sw = max(int(screen_w or DEFAULT_SCREEN_W), 1)
    sh = max(int(screen_h or DEFAULT_SCREEN_H), 1)
    sd = max(int(session_ms or DEFAULT_SESSION_MS), 1)
    arr = np.zeros((len(events), 3), dtype=np.float32)
    for i, e in enumerate(events):
        arr[i, 0] = np.clip(e["x"] / sw, 0.0, 1.0)
        arr[i, 1] = np.clip(e["y"] / sh, 0.0, 1.0)
        arr[i, 2] = np.clip(e["t"] / sd, 0.0, 1.0)
    if len(arr) >= MOUSE_SEQ_LEN:
        return arr[-MOUSE_SEQ_LEN:]
    out = np.zeros((MOUSE_SEQ_LEN, MOUSE_FEAT_DIM), dtype=np.float32)
    out[-len(arr):] = arr  # 左 pad
    return out


def normalize_key(events: list) -> np.ndarray:
    """list[{keycode,dwell,flight}] → (50, 3) np.float32"""
    if not events:
        return np.zeros((KEY_SEQ_LEN, KEY_FEAT_DIM), dtype=np.float32)
    arr = np.zeros((len(events), 3), dtype=np.float32)
    for i, e in enumerate(events):
        arr[i, 0] = np.clip(e["keycode"] / KEYCODE_DIVISOR, 0.0, 1.0)
        arr[i, 1] = np.clip(e["dwell"] / DWELL_DIVISOR, 0.0, 1.0)
        arr[i, 2] = np.clip(e["flight"] / FLIGHT_DIVISOR, 0.0, 1.0)
    if len(arr) >= KEY_SEQ_LEN:
        return arr[-KEY_SEQ_LEN:]
    out = np.zeros((KEY_SEQ_LEN, KEY_FEAT_DIM), dtype=np.float32)
    out[-len(arr):] = arr
    return out


@dataclass
class BehaviorSample:
    mouse: np.ndarray   # (200, 3)
    key: np.ndarray     # (50, 3)
    label: int


def df_to_samples(df: pd.DataFrame) -> list[BehaviorSample]:
    out = []
    for _, row in df.iterrows():
        m = normalize_mouse(
            row.get("mouse_events") or [],
            row.get("screen_width", DEFAULT_SCREEN_W),
            row.get("screen_height", DEFAULT_SCREEN_H),
            row.get("session_dur_ms", DEFAULT_SESSION_MS),
        )
        k = normalize_key(row.get("keystroke_events") or [])
        out.append(BehaviorSample(m, k, int(row["is_bot"])))
    return out


def build_torch_dataset(samples: list[BehaviorSample]):
    torch, nn, Dataset, DataLoader, _ = _lazy_torch()

    class _DS(Dataset):
        def __init__(self, s): self.s = s
        def __len__(self): return len(self.s)
        def __getitem__(self, i):
            x = self.s[i]
            return (torch.from_numpy(x.mouse), torch.from_numpy(x.key),
                    torch.tensor(x.label, dtype=torch.float32))

    return _DS(samples)


def build_model():
    torch, nn, *_ = _lazy_torch()

    class BehaviorLSTM(nn.Module):
        def __init__(self):
            super().__init__()
            self.mouse_lstm = nn.LSTM(
                input_size=MOUSE_FEAT_DIM, hidden_size=64,
                num_layers=2, dropout=0.3, batch_first=True,
            )
            self.key_lstm = nn.LSTM(
                input_size=KEY_FEAT_DIM, hidden_size=64,
                num_layers=2, dropout=0.3, batch_first=True,
            )
            self.head = nn.Sequential(
                nn.Linear(128, 64),
                nn.ReLU(),
                nn.Dropout(0.3),
                nn.Linear(64, 1),
            )

        def forward(self, mouse, key):
            # mouse: (B, 200, 3), key: (B, 50, 3)
            _, (mh, _) = self.mouse_lstm(mouse)
            _, (kh, _) = self.key_lstm(key)
            # 取最后一层 hidden
            m_last = mh[-1]  # (B, 64)
            k_last = kh[-1]  # (B, 64)
            cat = torch.cat([m_last, k_last], dim=-1)  # (B, 128)
            return self.head(cat).squeeze(-1)  # (B,) logit

    return BehaviorLSTM()


def train_epoch(model, loader, opt, loss_fn, torch_mod):
    model.train()
    total = 0.0
    for mouse, key, y in loader:
        opt.zero_grad()
        logit = model(mouse, key)
        loss = loss_fn(logit, y)
        loss.backward()
        opt.step()
        total += float(loss.item()) * len(y)
    return total / len(loader.dataset)


def eval_epoch(model, loader, loss_fn, torch_mod):
    from sklearn.metrics import roc_auc_score
    model.eval()
    losses, scores, ys = [], [], []
    with torch_mod.no_grad():
        for mouse, key, y in loader:
            logit = model(mouse, key)
            loss = loss_fn(logit, y)
            losses.append(float(loss.item()) * len(y))
            scores.append(torch_mod.sigmoid(logit).cpu().numpy())
            ys.append(y.cpu().numpy())
    y_all = np.concatenate(ys)
    s_all = np.concatenate(scores)
    auc = float(roc_auc_score(y_all, s_all)) if len(set(y_all)) > 1 else 0.5
    return sum(losses) / max(len(loader.dataset), 1), auc


def export_onnx(model, out_path: Path):
    """torch.onnx.export — 必须 dynamic_axes={batch}，让 serving 跑 batch>1。

    input shape 严格 [1, 200, 3] / [1, 50, 3]，跟 internal/mlscore/onnx_service.go
    & behavior_lstm.go 的 input shape 契约一致。
    """
    torch, *_ = _lazy_torch()
    model.eval()
    dummy_mouse = torch.zeros(1, MOUSE_SEQ_LEN, MOUSE_FEAT_DIM, dtype=torch.float32)
    dummy_key = torch.zeros(1, KEY_SEQ_LEN, KEY_FEAT_DIM, dtype=torch.float32)
    torch.onnx.export(
        model,
        (dummy_mouse, dummy_key),
        str(out_path),
        input_names=["mouse", "keystroke"],
        output_names=["logit"],
        dynamic_axes={
            "mouse": {0: "batch"},
            "keystroke": {0: "batch"},
            "logit": {0: "batch"},
        },
        opset_version=15,
        do_constant_folding=True,
    )
    logger.info("ONNX exported: %s (%.2f KB)", out_path, out_path.stat().st_size / 1024)


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--input", required=True, help="behavior_train.parquet")
    ap.add_argument("--output-dir", required=True)
    ap.add_argument("--model-ver", default="")
    ap.add_argument("--epochs", type=int, default=40)
    ap.add_argument("--batch-size", type=int, default=256)
    ap.add_argument("--lr", type=float, default=1e-3)
    ap.add_argument("--val-split", type=float, default=0.15)
    ap.add_argument("--patience", type=int, default=5, help="early-stopping patience（epochs）")
    ap.add_argument("--seed", type=int, default=42)
    ap.add_argument("-v", "--verbose", action="store_true")
    args = ap.parse_args()

    logging.basicConfig(level=logging.DEBUG if args.verbose else logging.INFO,
                        format="%(asctime)s %(levelname)s %(message)s")

    torch, nn, Dataset, DataLoader, WeightedRandomSampler = _lazy_torch()
    torch.manual_seed(args.seed)
    np.random.seed(args.seed)

    out_dir = Path(args.output_dir)
    out_dir.mkdir(parents=True, exist_ok=True)
    model_ver = args.model_ver or f"lstm_v{datetime.now(timezone.utc).strftime('%Y%m%d')}"

    # ── load + preprocess ─────────────────────────────────
    df = pd.read_parquet(args.input)
    logger.info("loaded %d rows", len(df))
    samples = df_to_samples(df)
    if len(samples) < 100:
        logger.error("too few samples: %d", len(samples))
        sys.exit(2)

    # shuffle + split
    rs = np.random.RandomState(args.seed)
    idx = rs.permutation(len(samples))
    n_val = int(len(samples) * args.val_split)
    train_s = [samples[i] for i in idx[n_val:]]
    val_s = [samples[i] for i in idx[:n_val]]
    logger.info("train=%d val=%d", len(train_s), len(val_s))

    # class weight for imbalance
    labels = np.array([s.label for s in train_s])
    n_pos = max(int(labels.sum()), 1)
    n_neg = len(labels) - n_pos
    pos_weight = torch.tensor([n_neg / n_pos], dtype=torch.float32)
    logger.info("pos_weight=%.2f (pos=%d neg=%d)", float(pos_weight), n_pos, n_neg)

    train_loader = DataLoader(build_torch_dataset(train_s), batch_size=args.batch_size, shuffle=True)
    val_loader = DataLoader(build_torch_dataset(val_s), batch_size=args.batch_size, shuffle=False)

    # ── train ─────────────────────────────────────────────
    model = build_model()
    opt = torch.optim.Adam(model.parameters(), lr=args.lr)
    loss_fn = nn.BCEWithLogitsLoss(pos_weight=pos_weight)

    best_auc = -1.0
    best_epoch = -1
    history = []
    bad_epochs = 0

    for epoch in range(args.epochs):
        tr_loss = train_epoch(model, train_loader, opt, loss_fn, torch)
        val_loss, val_auc = eval_epoch(model, val_loader, loss_fn, torch)
        history.append({"epoch": epoch, "train_loss": tr_loss,
                        "val_loss": val_loss, "val_auc": val_auc})
        logger.info("epoch %02d: train_loss=%.4f val_loss=%.4f val_auc=%.4f",
                    epoch, tr_loss, val_loss, val_auc)
        if val_auc > best_auc + 1e-4:
            best_auc = val_auc
            best_epoch = epoch
            bad_epochs = 0
            torch.save(model.state_dict(), out_dir / "best.pt")
        else:
            bad_epochs += 1
            if bad_epochs >= args.patience:
                logger.info("early stopping at epoch %d (best=%d auc=%.4f)",
                            epoch, best_epoch, best_auc)
                break

    # ── reload best + ONNX export ─────────────────────────
    model.load_state_dict(torch.load(out_dir / "best.pt"))
    export_onnx(model, out_dir / "behavior_lstm.onnx")

    # ── artifacts ─────────────────────────────────────────
    with open(out_dir / "metadata.json", "w") as f:
        json.dump({
            "model_ver": model_ver,
            "model_type": "behavior_lstm",
            "input_shapes": {
                "mouse": [1, MOUSE_SEQ_LEN, MOUSE_FEAT_DIM],
                "keystroke": [1, KEY_SEQ_LEN, KEY_FEAT_DIM],
            },
            "normalization": {
                "screen_w_default": DEFAULT_SCREEN_W,
                "screen_h_default": DEFAULT_SCREEN_H,
                "session_ms_default": DEFAULT_SESSION_MS,
                "keycode_divisor": KEYCODE_DIVISOR,
                "dwell_divisor": DWELL_DIVISOR,
                "flight_divisor": FLIGHT_DIVISOR,
            },
            "hyperparams": {
                "epochs": args.epochs,
                "batch_size": args.batch_size,
                "lr": args.lr,
                "patience": args.patience,
                "lstm_hidden": 64,
                "lstm_layers": 2,
                "dropout": 0.3,
            },
        }, f, indent=2)

    with open(out_dir / "metrics.json", "w") as f:
        json.dump({
            "best_epoch": best_epoch,
            "best_val_auc": best_auc,
            "n_train": len(train_s),
            "n_val": len(val_s),
            "history": history,
        }, f, indent=2)

    logger.info("done. best val AUC=%.4f at epoch %d", best_auc, best_epoch)
    if best_auc < 0.88:
        logger.warning("val AUC=%.4f < 0.88 — 不达标，不建议上线（见 BEHAVIOR_LSTM.md §2）",
                       best_auc)


if __name__ == "__main__":
    main()
