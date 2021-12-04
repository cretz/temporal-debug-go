package nondeterminism

import (
	"context"
	"log"
	"net/http"
	"time"

	"github.com/google/uuid"
	"go.temporal.io/sdk/workflow"
)

func MyWorkflow(ctx workflow.Context) error {
	// Use the Go logger which uses non-deterministic timing
	log.Printf("Some log!")

	// Create a context with a deadline
	timeoutCtx, _ := context.WithTimeout(context.Background(), 200*time.Millisecond)
	<-timeoutCtx.Done()

	// Create a UUID
	uuid.NewString()

	// Iterate over a map inside a goroutine
	go func() {
		for k, v := range map[string]string{"foo": "bar", "baz": "qux"} {
			workflow.GetLogger(ctx).Info("KeyVal", "Key", k, "Val", v)
		}
	}()

	// Make an HTTP call
	http.Get("http://example.com")

	return nil
}
