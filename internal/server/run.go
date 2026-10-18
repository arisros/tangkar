package server

import (
	"context"
	"time"
)

// Run drives the clock: every second it locks, cuts late batches and closes
// orders, then delivers whatever sends are due. It lives in the same process as
// the API, so state on disk is the only thing a restart needs.
func (s *Server) Run(ctx context.Context) {
	go s.deliver(ctx)
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		touched, err := s.store.Tick(ctx)
		if err != nil {
			s.log.Error("tick", "err", err)
		}
		for _, id := range touched {
			s.changed(id)
		}
		if len(touched) > 0 {
			s.kick()
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Server) kick() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Server) deliver(ctx context.Context) {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		out, err := s.store.PendingSends(ctx)
		if err != nil {
			s.log.Error("pending sends", "err", err)
		}
		for _, x := range out {
			var sendErr error
			if s.cfg.WADryRun || !s.wa.Enabled() || s.cfg.WATo == "" {
				s.log.Info("wa dry run", "order", x.OrderID, "batch", x.Batch, "message", x.Message)
			} else {
				sctx, cancel := context.WithTimeout(ctx, 20*time.Second)
				sendErr = s.wa.Send(sctx, s.cfg.WATo, x.Message)
				cancel()
			}
			if sendErr != nil {
				s.log.Warn("wa send failed", "order", x.OrderID, "batch", x.Batch, "err", sendErr)
			} else {
				s.log.Info("wa sent", "order", x.OrderID, "batch", x.Batch)
			}
			if err := s.store.MarkSend(ctx, x.OrderID, x.Batch, sendErr); err != nil {
				s.log.Error("mark send", "err", err)
			}
			s.changed(x.OrderID)
		}
		select {
		case <-ctx.Done():
			return
		case <-s.wake:
		case <-t.C:
		}
	}
}
