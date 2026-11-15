// Package bill is the fees HTTP API. Each bill is a Temporal workflow; this service starts,
// updates, signals and queries them, and runs the worker that executes them.
package bill

import (
	"context"
	"fmt"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"

	"github.com/rauf/fees-service/internal/services/bill/workflow"
	"github.com/rauf/fees-service/internal/services/fx"
	"github.com/rauf/fees-service/internal/services/payment"
)

// Service holds the Temporal client and the in-process worker.
//
//encore:service
type Service struct {
	client client.Client
	worker worker.Worker
}

func initService() (*Service, error) {
	c, err := client.Dial(client.Options{HostPort: temporalAddress()})
	if err != nil {
		return nil, fmt.Errorf("dial temporal at %s (is it running? make up): %w", temporalAddress(), err)
	}

	w := worker.New(c, workflow.TaskQueue, worker.Options{})
	w.RegisterWorkflow(workflow.BillWorkflow)
	w.RegisterActivity(&workflow.Activities{Rates: fx.NewService(), Payments: payment.NewService()})
	if err := w.Start(); err != nil {
		c.Close()

		return nil, fmt.Errorf("start worker: %w", err)
	}

	return &Service{client: c, worker: w}, nil
}

// Shutdown stops the worker before closing the client it polls through.
func (s *Service) Shutdown(_ context.Context) {
	s.worker.Stop()
	s.client.Close()
}
