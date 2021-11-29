package cancellation

import (
	"context"
	"errors"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/workflow"
)

func MyWorkflow(ctx workflow.Context) error {
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 30 * time.Minute,
		HeartbeatTimeout:    5 * time.Second,
		WaitForCancellation: true,
	})
	log := workflow.GetLogger(ctx)
	log.Info("Cancel workflow started")

	// Used to call activities by function pointer
	var a *Activities
	defer func() {

		if !errors.Is(ctx.Err(), workflow.ErrCanceled) {
			return
		}

		// When the workflow is canceled, it has to get a new disconnected context
		newCtx, _ := workflow.NewDisconnectedContext(ctx)
		err := workflow.ExecuteActivity(newCtx, a.CleanupActivity).Get(ctx, nil)
		if err != nil {
			log.Error("CleanupActivity failed", "Error", err)
		}
	}()

	var result string
	err := workflow.ExecuteActivity(ctx, a.ActivityToBeCanceled).Get(ctx, &result)
	log.Info("ActivityToBeCanceled returned", "Result", result, "Error", result, err)

	err = workflow.ExecuteActivity(ctx, a.ActivityToBeSkipped).Get(ctx, nil)
	log.Error("Error from ActivityToBeSkipped", "Error", err)

	log.Info("Workflow Execution complete")

	return nil
}

type Activities struct{}

func (a *Activities) ActivityToBeCanceled(ctx context.Context) (string, error) {
	logger := activity.GetLogger(ctx)
	logger.Info("activity started")
	for {
		select {
		case <-time.After(1 * time.Second):
			logger.Info("heartbeating...")
			activity.RecordHeartbeat(ctx, "")
		case <-ctx.Done():
			logger.Info("context is cancelled")
			return "I am canceled by Done", nil
		}
	}
}

func (a *Activities) CleanupActivity(ctx context.Context) error {
	logger := activity.GetLogger(ctx)
	logger.Info("Cleanup Activity started")
	return nil
}

func (a *Activities) ActivityToBeSkipped(ctx context.Context) error {
	logger := activity.GetLogger(ctx)
	logger.Info("this Activity will be skipped due to cancellation")
	return nil
}
