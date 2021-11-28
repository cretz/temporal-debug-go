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
	require := require.New(t)
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
	require.NoError(err)
	require.NoError(srv.Start())
	defer srv.Stop()

	// Connect client
	cl, err := srv.NewClient(ctx, namespace)
	require.NoError(err)
	defer cl.Close()

	// Start worker with workflow registered
	const taskQueue = "my-task-queue"
	wrk := worker.New(cl, taskQueue, worker.Options{WorkflowPanicPolicy: worker.FailWorkflow})
	wrk.RegisterWorkflow(tracertest.TestWorkflow)
	require.NoError(wrk.Start())
	defer wrk.Stop()

	// Start workflow
	t.Log("Starting workflow")
	startOpts := client.StartWorkflowOptions{ID: "my-workflow-" + uuid.NewString(), TaskQueue: taskQueue}
	run, err := cl.ExecuteWorkflow(ctx, startOpts, tracertest.TestWorkflow)
	require.NoError(err)

	// Send a value signal and wait until received
	require.NoError(cl.SignalWorkflow(ctx, run.GetID(), run.GetRunID(), "value-signal", "value1"))
	require.NoError(waitForMark(ctx, cl, run, "value signal with value1"))

	// Do it again
	require.NoError(cl.SignalWorkflow(ctx, run.GetID(), run.GetRunID(), "value-signal", "value2"))
	require.NoError(waitForMark(ctx, cl, run, "value signal with value2"))

	// Send a continue to finish the workflow
	require.NoError(cl.SignalWorkflow(ctx, run.GetID(), run.GetRunID(), "continue-signal", nil))
	require.NoError(run.Get(ctx, nil))

	// Trace the execution
	t.Log("Running trace")
	_, currFile, _, _ := runtime.Caller(0)
	tr, err := tracer.New(tracer.Config{
		ClientOptions: client.Options{HostPort: srv.FrontendHostPort(), Namespace: namespace},
		WorkflowFuncs: []string{"github.com/cretz/temporal-debug-go/test/tracertest.TestWorkflow"},
		Execution:     &workflow.Execution{ID: run.GetID(), RunID: run.GetRunID()},
		RootDir:       filepath.Dir(currFile),
	})
	require.NoError(err)
	res, err := tr.Trace(ctx)
	require.NoError(err)

	// TODO(cretz): Assert actual values
	j, err := json.MarshalIndent(res, "", " ")
	require.NoError(err)
	t.Logf("JSON: %s", j)

	marks, err := marks(ctx, cl, run)
	require.NoError(err)
	t.Logf("Marks: %v", marks)
}

func waitForMark(ctx context.Context, c client.Client, run client.WorkflowRun, mark string) error {
	// Try every so often for a few seconds
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	deadlineCh := time.After(3 * time.Second)
	for {
		select {
		case <-ticker.C:
			marks, err := marks(ctx, c, run)
			if err != nil {
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

func marks(ctx context.Context, c client.Client, run client.WorkflowRun) ([]string, error) {
	val, err := c.QueryWorkflow(ctx, run.GetID(), run.GetRunID(), "marks-query")
	// Query can not be set yet which is ok
	var queryFailed *serviceerror.QueryFailed
	if errors.As(err, &queryFailed) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var marks []string
	if err := val.Get(&marks); err != nil {
		return nil, err
	}
	return marks, nil
}
