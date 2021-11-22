package cmd

import (
	"context"
	"fmt"

	"github.com/cretz/temporal-debug-go/tracer"
	"github.com/urfave/cli/v2"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/workflow"
)

func traceCmd() *cli.Command {
	var config TraceConfig
	return &cli.Command{
		Name:  "trace",
		Usage: "Replay an existing run",
		Flags: config.flags(),
		Action: func(ctx *cli.Context) error {
			return trace(ctx.Context, config)
		},
	}
}

type TraceConfig struct {
	Address       string
	Namespace     string
	WorkflowID    string
	RunID         string
	HistoryFile   string
	Func          string
	RootDir       string
	RetainTempDir bool
}

func (t *TraceConfig) flags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{
			Name:        "address",
			Aliases:     []string{"ad"},
			Usage:       "Server host:port",
			Value:       client.DefaultHostPort,
			Destination: &t.Address,
		},
		&cli.StringFlag{
			Name:        "namespace",
			Aliases:     []string{"ns"},
			Usage:       "Namespace",
			Value:       client.DefaultNamespace,
			Destination: &t.Namespace,
		},
		&cli.StringFlag{
			Name:        "workflow_id",
			Aliases:     []string{"wid", "w"},
			Usage:       "Workflow ID, required if history file not set",
			Destination: &t.WorkflowID,
		},
		&cli.StringFlag{
			Name:        "run_id",
			Aliases:     []string{"rid", "r"},
			Usage:       "Run ID, can only be set if workflow ID is",
			Destination: &t.RunID,
		},
		&cli.StringFlag{
			Name:        "history",
			Aliases:     []string{"hist"},
			Usage:       "History file, required if workflow ID not set",
			Destination: &t.HistoryFile,
		},
		// TODO(cretz): Support multiple workflow functions
		&cli.StringFlag{
			Name:    "func",
			Aliases: []string{"fn"},
			// TODO(cretz): Support expressions for struct-based functions
			Usage:       "Workflow function, qualified with package up to last dot",
			Required:    true,
			Destination: &t.Func,
		},
		&cli.StringFlag{
			Name:        "root",
			Usage:       "Root directory of the module containing the package for the workflow",
			Value:       ".",
			Destination: &t.RootDir,
		},
		&cli.BoolFlag{
			Name:        "retain_temp",
			Usage:       "Retain the temporary directory created for running",
			Destination: &t.RetainTempDir,
		},
	}
}

func trace(ctx context.Context, config TraceConfig) error {
	// Build config
	tracerConfig := tracer.Config{
		ClientOptions: client.Options{
			HostPort:  config.Address,
			Namespace: config.Namespace,
		},
		WorkflowFuncs: []string{config.Func},
		RootDir:       config.RootDir,
		RetainTempDir: config.RetainTempDir,
	}
	if config.WorkflowID != "" {
		if config.HistoryFile != "" {
			return fmt.Errorf("cannot have both workflow ID and history file")
		}
		tracerConfig.Execution = &workflow.Execution{ID: config.WorkflowID, RunID: config.RunID}
	} else if config.RunID != "" {
		return fmt.Errorf("cannot have run ID without workflow ID")
	} else if config.HistoryFile == "" {
		return fmt.Errorf("must have either workflow ID or history file")
	} else {
		tracerConfig.HistoryFile = config.HistoryFile
	}

	// Do trace
	res, err := tracer.Trace(ctx, tracerConfig)
	if err != nil {
		return err
	}

	// Dump result
	fmt.Printf("------ TRACE ------\n")
	lastFile, lastLine := "", -1
	for _, event := range res.Events {
		if event.Server != nil {
			fmt.Printf("Event %v - %v\n", event.Server.ID, event.Server.Type)
			lastFile, lastLine = "", -1
		} else if event.Code != nil {
			// Ignore if matches last file and line
			if lastFile == event.Code.File && lastLine == event.Code.Line {
				continue
			}
			// TODO(cretz): Attempt to relativize file?
			fmt.Printf("\t%v:%v\n", event.Code.File, event.Code.Line)
			lastFile, lastLine = event.Code.File, event.Code.Line
		}
	}
	return nil
}
