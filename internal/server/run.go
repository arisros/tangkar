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
			sendErr := s.sendWA(ctx, s.cfg.WATo, x.Message)
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
		s.deliverNotices(ctx)
		select {
		case <-ctx.Done():
			return
		case <-s.wake:
		case <-t.C:
		}
	}
}

func (s *Server) sendWA(ctx context.Context, to, message string) error {
	if s.cfg.WADryRun || !s.wa.Enabled() || to == "" {
		s.log.Info("wa dry run", "to", to, "message", message)
		return nil
	}
	sctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	return s.wa.Send(sctx, to, message)
}

// deliverNotices sends the personal "sudah diproses" messages. They run after
// the seller's messages so a slow personal send never delays an order.
func (s *Server) deliverNotices(ctx context.Context) {
	ns, err := s.store.PendingNotices(ctx)
	if err != nil {
		s.log.Error("pending notices", "err", err)
		return
	}
	for _, n := range ns {
		err := s.sendWA(ctx, n.To, n.Message)
		if err != nil {
			s.log.Warn("notice failed", "order", n.OrderID, "id", n.ID, "err", err)
		}
		if err := s.store.MarkNotice(ctx, n.ID, err); err != nil {
			s.log.Error("mark notice", "err", err)
		}
	}
}
