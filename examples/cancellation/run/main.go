package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/cretz/temporal-debug-go/examples/cancellation"
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
	w.RegisterWorkflow(cancellation.MyWorkflow)
	w.RegisterActivity(&cancellation.Activities{})
	if err := w.Start(); err != nil {
		return fmt.Errorf("failed starting worker: %w", err)
	}
	defer w.Stop()

	// Start workflow
	startOpts := client.StartWorkflowOptions{ID: "my-workflow-" + uuid.NewString(), TaskQueue: taskQueue}
	log.Printf("Starting workflow with ID %v", startOpts.ID)
	run, err := c.ExecuteWorkflow(ctx, startOpts, cancellation.MyWorkflow)
	if err != nil {
		return fmt.Errorf("failed starting workflow: %w", err)
	}

	// Cancel after three seconds
	log.Printf("Waiting 3 seconds then cancelling")
	time.Sleep(3 * time.Second)
	if err := c.CancelWorkflow(ctx, run.GetID(), run.GetRunID()); err != nil {
		return fmt.Errorf("failed cancelling workflow: %w", err)
	}

	log.Printf("Waiting on workflow to complete")
	if err = run.Get(ctx, nil); err != nil {
		return fmt.Errorf("failed running workflow: %w", err)
	}

	log.Printf("Workflow complete")
	return nil
}
