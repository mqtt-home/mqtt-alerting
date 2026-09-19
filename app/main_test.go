package main

import (
	"testing"
	"time"

	"github.com/mqtt-home/mqtt-mail/mail"
)

// publishStatus is reachable from the MQTT message path. It must return even
// when nobody drains the queue (broker gone, publisher blocked), or the client
// deadlocks — which is how v0.2.0 died.
func TestPublishStatusNeverBlocks(t *testing.T) {
	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			publishStatus(mail.Status{Messages: int64(i)})
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("publishStatus blocked without a consumer")
	}

	// Only the newest status is worth publishing.
	if got := (<-statusQueue).Messages; got != 999 {
		t.Fatalf("queue held status %d, want the latest (999)", got)
	}
}
