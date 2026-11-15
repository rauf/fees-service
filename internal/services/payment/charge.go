package payment

import (
	"context"
	"fmt"
	"strings"
)

const declinePrefix = "cus_decline"

func (*service) Charge(_ context.Context, req ChargeRequest) error {
	if strings.HasPrefix(req.CustomerID, declinePrefix) {
		return fmt.Errorf("%w: customer %s", ErrDeclined, req.CustomerID)
	}

	return nil
}
