package repo

import (
	"context"

	"github.com/xiongwp/payment-channel/internal/idgen"
	"github.com/xiongwp/payment-channel/internal/sharding"
)

// IDIssuer 把 idgen.IDGenerator + sharding.Router 包成 `aq_<db><tbl><seq>` 形态的
// 稳定 ID，供 service 层无关心地拿。
type IDIssuer interface {
	Next(prefix string, piID string) (string, error)
}

type idIssuer struct {
	gen    idgen.IDGenerator
	router *sharding.Router
}

func NewIDIssuer(gen idgen.IDGenerator, r *sharding.Router) IDIssuer {
	return &idIssuer{gen: gen, router: r}
}

func (i *idIssuer) Next(prefix, piID string) (string, error) {
	db, tbl := i.router.RouteByPrefixedID(piID)
	// biz_tag 直接用 prefix —— "aq" / "wh" / "tok" 任选；这里固定走 aq_id。
	seq, err := i.gen.NextID(context.Background(), idgen.BizTagAcquirerTx)
	if err != nil {
		return "", err
	}
	return i.router.FormatID(prefix, db, tbl, seq), nil
}
