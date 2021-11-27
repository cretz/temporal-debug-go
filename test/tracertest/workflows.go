package tracertest

import (
	"time"

	"go.temporal.io/sdk/workflow"
)

func TestWorkflow(ctx workflow.Context) error {
	marks := []string{"workflow started"}

	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 30 * time.Minute,
		HeartbeatTimeout:    5 * time.Second,
		WaitForCancellation: true,
	})

	// Add query handler for marks
	workflow.SetQueryHandler(ctx, "marks", func() ([]string, error) { return marks, nil })

	// Run something async
	workflow.Go(ctx, func(ctx workflow.Context) {
		marks = append(marks, "coroutine started")
		time.Sleep(100 * time.Millisecond)
		marks = append(marks, "coroutine ended")
	})

	// Wait for a signal
	marks = append(marks, "workflow signal wait")
	workflow.GetSignalChannel(ctx, "continue").Receive(ctx, nil)

	marks = append(marks, "workflow ended")
	return nil
}
