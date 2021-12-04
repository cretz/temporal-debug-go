package main

import (
	"context"
	"fmt"
	"log"

	"github.com/cretz/temporal-debug-go/examples/nondeterminism"
	"github.com/google/uuid"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Create client
	c, err := client.NewClient(client.Options{})
	if err != nil {
		return fmt.Errorf("failed starting client: %w", err)
	}
	defer c.Close()

	// Start worker
	taskQueue := "test-task-queue-" + uuid.NewString()
	w := worker.New(c, taskQueue, worker.Options{})
	w.RegisterWorkflow(nondeterminism.MyWorkflow)
	if err := w.Start(); err != nil {
		return fmt.Errorf("failed starting worker: %w", err)
	}
	defer w.Stop()

	// Start workflow
	startOpts := client.StartWorkflowOptions{ID: "my-workflow-" + uuid.NewString(), TaskQueue: taskQueue}
	log.Printf("Starting workflow with ID %v", startOpts.ID)
	run, err := c.ExecuteWorkflow(ctx, startOpts, nondeterminism.MyWorkflow)
	if err != nil {
		return fmt.Errorf("failed starting workflow: %w", err)
	}

	log.Printf("Waiting on workflow to complete")
	if err = run.Get(ctx, nil); err != nil {
		return fmt.Errorf("failed running workflow: %w", err)
	}

	log.Printf("Workflow complete")
	return nil
}
