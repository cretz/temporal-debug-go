package tracertest_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/DataDog/temporalite"
	"github.com/cretz/temporal-debug-go/test/tracertest"
	"github.com/cretz/temporal-debug-go/tracer"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
	"go.temporal.io/server/common/log"
)

func TestTracer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start server
	t.Log("Starting server")
	const namespace = "my-namespace"
	srv, err := temporalite.NewServer(
		temporalite.WithNamespaces(namespace),
		temporalite.WithPersistenceDisabled(),
		temporalite.WithDynamicPorts(),
		temporalite.WithLogger(log.NewNoopLogger()),
	)
	require.NoError(t, err)
	require.NoError(t, srv.Start())
	defer srv.Stop()

	// Connect client
	cl, err := srv.NewClient(ctx, namespace)
	require.NoError(t, err)
	defer cl.Close()

	// Start worker with workflow registered
	const taskQueue = "my-task-queue"
	wrk := worker.New(cl, taskQueue, worker.Options{WorkflowPanicPolicy: worker.FailWorkflow})
	wrk.RegisterWorkflow(tracertest.TestWorkflow)
	require.NoError(t, wrk.Start())
	defer wrk.Stop()

	// Start workflow
	t.Log("Starting workflow")
	startOpts := client.StartWorkflowOptions{ID: "my-workflow-" + uuid.NewString(), TaskQueue: taskQueue}
	run, err := cl.ExecuteWorkflow(ctx, startOpts, tracertest.TestWorkflow)
	require.NoError(t, err)

	// Wait for the coroutine to end
	require.NoError(t, waitForMark(ctx, cl, run, "coroutine ended"))

	// Send signal to continue and wait for workflow to end
	require.NoError(t, cl.SignalWorkflow(ctx, run.GetID(), run.GetRunID(), "continue", nil))
	require.NoError(t, run.Get(ctx, nil))

	// Trace the execution
	t.Log("Running trace")
	_, currFile, _, _ := runtime.Caller(0)
	tr, err := tracer.New(tracer.Config{
		ClientOptions: client.Options{HostPort: srv.FrontendHostPort(), Namespace: namespace},
		WorkflowFuncs: []string{"github.com/cretz/temporal-debug-go/test/tracertest.TestWorkflow"},
		Execution:     &workflow.Execution{ID: run.GetID(), RunID: run.GetRunID()},
		RootDir:       filepath.Dir(currFile),
	})
	require.NoError(t, err)
	res, err := tr.Trace(ctx)
	require.NoError(t, err)

	j, _ := json.MarshalIndent(res, "", " ")
	fmt.Printf("JSON: %s\n", j)
}

func waitForMark(ctx context.Context, c client.Client, run client.WorkflowRun, mark string) error {
	// Try every so often for a few seconds
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	deadlineCh := time.After(3 * time.Second)
	for {
		select {
		case <-ticker.C:
			val, err := c.QueryWorkflow(ctx, run.GetID(), run.GetRunID(), "marks")
			// Query can not be set yet which is ok
			var queryFailed *serviceerror.QueryFailed
			if errors.As(err, &queryFailed) {
				continue
			} else if err != nil {
				return err
			}
			var marks []string
			if err := val.Get(&marks); err != nil {
				return err
			}
			for _, maybeMark := range marks {
				if mark == maybeMark {
					return nil
				}
			}
		case <-deadlineCh:
			return fmt.Errorf("deadline exceeded")
		}
	}
}
