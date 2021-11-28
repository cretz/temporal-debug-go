package tracertest

import (
	"strconv"
	"time"

	"go.temporal.io/sdk/workflow"
)

func TestWorkflow(ctx workflow.Context) error {
	marks := []string{"workflow started"}
	defer func() { marks = append(marks, "workflow cleanup") }()

	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 30 * time.Minute,
		HeartbeatTimeout:    5 * time.Second,
		WaitForCancellation: true,
	})

	// Add query handler for marks
	workflow.SetQueryHandler(ctx, "marks-query", func() ([]string, error) { return marks, nil })

	// Run something async
	workflow.Go(ctx, func(ctx workflow.Context) {
		marks = append(marks, "coroutine 1 started")
		workflow.Sleep(ctx, 100*time.Millisecond)
		marks = append(marks, "coroutine 1 ended")
	})

	// Run something async on named coroutine
	workflow.GoNamed(ctx, "my-coroutine", func(ctx workflow.Context) {
		marks = append(marks, "coroutine 2 started")
		workflow.Sleep(ctx, 100*time.Millisecond)
		marks = append(marks, "coroutine 2 ended")
	})

	// Wait for asyncs to be done
	err := workflow.Await(ctx, func() bool {
		var coroutine1Ended, coroutine2Ended bool
		for _, mark := range marks {
			if mark == "coroutine 1 ended" {
				coroutine1Ended = true
			} else if mark == "coroutine 2 ended" {
				coroutine2Ended = true
			}
		}
		return coroutine1Ended && coroutine2Ended
	})
	if err != nil {
		return err
	}

	// Wait for signal or done on named selector
	marks = append(marks, "workflow signal wait")
	valueSignal := workflow.GetSignalChannel(ctx, "value-signal")
	continueSignal := workflow.GetSignalChannel(ctx, "continue-signal")
	for i, done := 0, false; !done; i++ {
		sel := workflow.NewNamedSelector(ctx, "my-selector-"+strconv.Itoa(i))
		sel.AddReceive(ctx.Done(), func(workflow.ReceiveChannel, bool) {
			marks = append(marks, "workflow context done")
			done = true
		})
		sel.AddReceive(valueSignal, func(c workflow.ReceiveChannel, more bool) {
			var val string
			c.Receive(ctx, &val)
			marks = append(marks, "value signal with "+val)
		})
		sel.AddReceive(continueSignal, func(workflow.ReceiveChannel, bool) {
			marks = append(marks, "continue signal")
			done = true
		})
		sel.Select(ctx)
	}

	marks = append(marks, "workflow ended")
	return nil
}
