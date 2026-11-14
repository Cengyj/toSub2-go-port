package protocol

import (
	"context"
	"time"
)

// Local durability may finish after network cancellation, within a fixed bound.
func privateStateSaveContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 10*time.Second)
}
