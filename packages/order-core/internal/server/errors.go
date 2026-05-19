package server

import (
	"errors"
	"fmt"

	"github.com/xiongwp/order-core/internal/domain"
)

// grpcErr maps domain-level errors to gRPC status codes so the admin-web gets
// clean 404/400/409 responses instead of opaque Internal.
func grpcErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, domain.ErrPaymentIntentNotFound),
		errors.Is(err, domain.ErrChargeNotFound),
		errors.Is(err, domain.ErrRefundNotFound),
		errors.Is(err, domain.ErrPayActionNotFound),
		errors.Is(err, domain.ErrLedgerAccountNotFound),
		errors.Is(err, domain.ErrDisputeNotFound):
		return fmt.Errorf("%s", err.Error())
	case errors.Is(err, domain.ErrValidation):
		return fmt.Errorf("%s", err.Error())
	case errors.Is(err, domain.ErrInvalidTransition),
		errors.Is(err, domain.ErrPayActionNotPending):
		return fmt.Errorf("%s", err.Error())
	}
	return fmt.Errorf("%s", err.Error())
}
