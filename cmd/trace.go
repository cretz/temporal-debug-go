package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"

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
	Address        string
	Namespace      string
	WorkflowID     string
	RunID          string
	HistoryFile    string
	Func           string
	OutputStdout   bool
	OutputJSONFile string
	OutputHTMLDir  string
	RootDir        string
	RetainTempDir  bool
	ExcludeFuncs   cli.StringSlice
	ExcludeFiles   cli.StringSlice
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
		&cli.BoolFlag{
			Name:        "stdout",
			Usage:       "Dump trace to stdout (default true if no other output)",
			Destination: &t.OutputStdout,
		},
		&cli.StringFlag{
			Name:        "json",
			Usage:       "File to output JSON trace to",
			Destination: &t.OutputJSONFile,
		},
		&cli.StringFlag{
			Name:        "html",
			Usage:       "Directory to output HTML to",
			Destination: &t.OutputHTMLDir,
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
		&cli.StringSliceFlag{
			Name:        "exclude_func",
			Usage:       "Regex patterns for functions to not step through",
			Destination: &t.ExcludeFuncs,
		},
		&cli.StringSliceFlag{
			Name:        "exclude_file",
			Usage:       "Regex patterns for files to not step through",
			Destination: &t.ExcludeFiles,
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
	var err error
	if tracerConfig.ExcludeFuncs, err = stringsToRegexps(config.ExcludeFuncs.Value()); err != nil {
		return err
	} else if tracerConfig.ExcludeFiles, err = stringsToRegexps(config.ExcludeFiles.Value()); err != nil {
		return err
	}

	// Do trace
	t, err := tracer.New(tracerConfig)
	if err != nil {
		return err
	}
	res, traceErr := t.Trace(ctx)

	// Dump if there is a result
	if res == nil || len(res.Events) == 0 {
		fmt.Println("No events recorded")
	} else {
		// Dump result to stdout
		if config.OutputStdout || (config.OutputJSONFile == "" && config.OutputHTMLDir == "") {
			fmt.Printf("------ TRACE ------\n")
			lastFile, lastLine := "", -1
			for _, event := range res.Events {
				if event.Server != nil {
					fmt.Printf("Event %v - %v\n", event.Server.ID, event.Server.Type)
					lastFile, lastLine = "", -1
				} else if event.Client != nil {
					for _, command := range event.Client.Commands {
						fmt.Printf("\tCommand - %v\n", command)
					}
					lastFile, lastLine = "", -1
				} else if event.Code != nil {
					// Ignore if matches last file and line
					if lastFile == event.Code.File && lastLine == event.Code.Line {
						continue
					}
					fmt.Printf("\t%v - %v:%v\n", event.Code.Package, filepath.Base(event.Code.File), event.Code.Line)
					lastFile, lastLine = event.Code.File, event.Code.Line
				}
			}
		}

		// Dump result to JSON if requested
		if config.OutputJSONFile != "" {
			if j, err := json.MarshalIndent(res, "", "  "); err != nil {
				return fmt.Errorf("failed marshaling JSON: %w", err)
			} else if err = os.WriteFile(config.OutputJSONFile, j, 0644); err != nil {
				return fmt.Errorf("failed writing %v: %w", config.OutputJSONFile, err)
			}
			fmt.Printf("Wrote JSON to %v\n", config.OutputJSONFile)
		}

		// Dump result to HTML if requested
		if config.OutputHTMLDir != "" {
			if err := t.GenerateHTML(config.OutputHTMLDir, res); err != nil {
				return fmt.Errorf("failed generating HTML: %w", err)
			}
			fmt.Printf("Wrote HTML to %v\n", config.OutputHTMLDir)
		}
	}

	if traceErr != nil {
		return fmt.Errorf("trace failed: %w", traceErr)
	}
	return nil
}

func stringsToRegexps(strs []string) ([]*regexp.Regexp, error) {
	ret := make([]*regexp.Regexp, len(strs))
	for i, str := range strs {
		var err error
		if ret[i], err = regexp.Compile(str); err != nil {
			return nil, fmt.Errorf("invalid regex %v: %w", str, err)
		}
	}
	return ret, nil
}
