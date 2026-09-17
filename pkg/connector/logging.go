package connector

import (
	"context"
	"sync/atomic"

	"github.com/grpc-ecosystem/go-grpc-middleware/logging/zap/ctxzap"
	"go.uber.org/zap"
)

type sampledWarn struct {
	n atomic.Uint64
}

func (s *sampledWarn) log(ctx context.Context, msg string, fields ...zap.Field) {
	n := s.n.Add(1)
	if !shouldLogSample(n) {
		return
	}
	ctxzap.Extract(ctx).Warn(msg, append(fields, zap.Uint64("total_occurrences", n))...)
}

// shouldLogSample reports whether the nth occurrence should be logged.
func shouldLogSample(n uint64) bool {
	switch {
	case n <= 1, n == 10, n == 100:
		return true
	default:
		return n%1000 == 0
	}
}
