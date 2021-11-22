package tracer

import (
	"context"
	"fmt"
	"go/format"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/go-delve/delve/pkg/proc"
	"github.com/go-delve/delve/service/api"
	"github.com/go-delve/delve/service/debugger"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/log"
	"go.temporal.io/sdk/workflow"
)

type Config struct {
	ClientOptions client.Options
	Log           log.Logger
	// Qualified by package up to last dot. Only one supported for now
	// TODO(cretz): Support multiple for child workflows?
	WorkflowFuncs []string

	// One and only one required
	Execution   *workflow.Execution
	HistoryFile string

	// Temp dir created under this, usually the current working dir so func
	// package works properly
	RootDir       string
	RetainTempDir bool
}

type Result struct {
	Events []Event `json:"events"`
}

type Event struct {
	// Only one of these is present
	Server *EventServer `json:"server,omitempty"`
	Code   *EventCode   `json:"code,omitempty"`
}

type EventServer struct {
	ID   int64  `json:"eventId"`
	Type string `json:"eventType"`
}

type EventCode struct {
	File string `json:"file,omitempty"`
	Line int    `json:"line,omitempty"`
	// TODO(cretz): Locals
	// LocalsUpdated []api.Variable `json:"locals_updated,omitempty"`
}

func Trace(ctx context.Context, config Config) (*Result, error) {
	if config.Execution == nil && config.HistoryFile == "" {
		return nil, fmt.Errorf("must have existing execution or history file")
	} else if config.Execution != nil && config.HistoryFile != "" {
		return nil, fmt.Errorf("cannot have both execution and history file")
	}
	if config.Log == nil {
		config.Log = DefaultLogger
	}

	// Split function and package
	// TODO(cretz): Support multiple workflows
	if len(config.WorkflowFuncs) != 1 {
		return nil, fmt.Errorf("single workflow function required")
	}
	// TODO(cretz): Support struct-based workflows
	lastDot := strings.LastIndex(config.WorkflowFuncs[0], ".")
	if lastDot == -1 {
		return nil, fmt.Errorf("workflow function missing dot")
	}
	fnPkg, fn := config.WorkflowFuncs[0][:lastDot], config.WorkflowFuncs[0][lastDot+1:]

	// Create temp dir
	dir, err := os.MkdirTemp(config.RootDir, "debug-go-trace-")
	if err != nil {
		return nil, fmt.Errorf("failed creating temp dir: %w", err)
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("could not turn dir absolute: %w", err)
	}
	config.Log.Debug("Created temp dir", "Dir", dir)
	if !config.RetainTempDir {
		defer os.RemoveAll(dir)
	}

	// Create main.go
	config.Log.Debug("Creating temp main.go")
	if b, err := replayMainCode(config, fnPkg, fn); err != nil {
		return nil, fmt.Errorf("failed building temp main.go: %w", err)
	} else if err = os.WriteFile(filepath.Join(dir, "main.go"), b, 0644); err != nil {
		return nil, fmt.Errorf("failed writing temp main.go: %w", err)
	}

	// Build binary with optimizations disabled (what the delve gobuild does for
	// >= 1.10.0)
	config.Log.Debug("Building temp main.go")
	exe := filepath.Join(dir, "main")
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	cmd := exec.Command("go", "build", "-o", exe, "-gcflags", "all=-N -l", "main.go")
	cmd.Dir = dir
	cmd.Stderr, cmd.Stdout = os.Stderr, os.Stdout
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("failed building main exe: %w", err)
	}

	// Create debugger
	config.Log.Debug("Starting debugger")
	debugConfig := &debugger.Config{
		WorkingDir: dir,
		Backend:    "default",
	}
	debug, err := debugger.New(debugConfig, []string{exe})
	if err != nil {
		return nil, fmt.Errorf("failed creating debugger: %w", err)
	}

	// Close debugger when done ignoring errors
	defer func() {
		config.Log.Debug("Stopping debugger")
		if debug.IsRunning() {
			debug.Command(&api.DebuggerCommand{Name: api.Halt}, nil)
		}
		debug.Detach(true)
	}()

	const (
		breakpointWorkflowStart = iota + 1
		breakpointProcessEventOuter
		breakpointProcessEventInner
	)

	// Set breakpoint on the workflow function
	config.Log.Debug("Setting breakpoint on workflow function")
	_, err = debug.CreateBreakpoint(&api.Breakpoint{
		ID:           breakpointWorkflowStart,
		FunctionName: fnPkg + "." + fn,
	})
	if err != nil {
		return nil, fmt.Errorf("failed setting breakpoint at start of workflow: %w", err)
	}

	// Set a breakpoint on the process event function
	_, err = debug.CreateBreakpoint(&api.Breakpoint{
		ID:           breakpointProcessEventOuter,
		FunctionName: "go.temporal.io/sdk/internal.(*workflowExecutionEventHandlerImpl).ProcessEvent",
	})
	if err != nil {
		return nil, fmt.Errorf("failed setting breakpoint for event processing: %w", err)
	}

	// Continue until the breakpoint is hit
	config.Log.Debug("Starting execution")
	state, err := debug.Command(&api.DebuggerCommand{Name: api.Continue}, nil)
	if err != nil {
		return nil, fmt.Errorf("failed starting execution: %w", err)
	}

	// Step until runtime exit
	goRootSrc := filepath.ToSlash(filepath.Join(runtime.GOROOT(), "src"))
	var res Result
	for state.CurrentThread.Function.Name() != "runtime.goexit" {

		// If we have hit process event breakpoint
		if state.CurrentThread.Breakpoint != nil && state.CurrentThread.Breakpoint.ID >= breakpointProcessEventOuter {
			// If we have hit the outer, that does not have the params yet, so we must
			// go to the first line via a step. Since we can't do this everytime
			// (sometimes we're NextInProgress), we instead clear this breakpoint and
			// set a new one at that first line. Alternatively we could try to find
			// what the actual first executable line of the function is ahead of time
			// instead of runtime, but this is fine for now.
			if state.CurrentThread.Breakpoint.ID == breakpointProcessEventOuter {
				// Remove this breakpoint
				if _, err := debug.ClearBreakpoint(state.CurrentThread.Breakpoint); err != nil {
					return nil, fmt.Errorf("failed clearing outer breakpoint: %w", err)
				}
				// Step
				state, err = debug.Command(&api.DebuggerCommand{Name: api.Step}, nil)
				if err != nil {
					return nil, fmt.Errorf("failed stepping: %w", err)
				}
				// Create the new breakpoint at this stepped location
				_, err = debug.CreateBreakpoint(&api.Breakpoint{
					ID:   breakpointProcessEventInner,
					File: state.CurrentThread.File,
					Line: state.CurrentThread.Line,
				})
				if err != nil {
					return nil, fmt.Errorf("failed creating inner breakpoint: %w", err)
				}
			}

			// Obtain the event ID and type
			vars, err := debug.FunctionArguments(state.CurrentThread.GoroutineID, 0, 0, proc.LoadConfig{
				FollowPointers: true, MaxStringLen: 200, MaxArrayValues: 1, MaxStructFields: -1,
			})
			if err != nil {
				return nil, fmt.Errorf("failed loading vars: %w", err)
			}
			var event EventServer
			for _, arg := range api.ConvertVars(vars) {
				if arg.Name == "event" {
					for _, child := range arg.Children[0].Children {
						if child.Name == "EventId" {
							if event.ID, err = strconv.ParseInt(child.Value, 10, 64); err != nil {
								return nil, fmt.Errorf("invalid event ID %v: %w", child.Value, err)
							}
						} else if child.Name == "EventType" {
							event.Type = child.Value
						}
					}
				}
			}
			res.Events = append(res.Events, Event{Server: &event})
		}

		// This means we have hit a breakpoint while stepping from another
		// breakpoint. Our only options are to "cancel next" which means to ignore
		// what we were doing and step here, or "continue" which means go back to
		// what we were doing before this breakpoint. We want to "cancel next" if
		// the breakpoint is our workflow breakpoint (i.e. resume stepping here), or
		// "continue" if it is not (so it'd likely be the ProcessEvent breakpoint
		// which we're only capturing some data from).
		if state.NextInProgress {
			if state.CurrentThread.Breakpoint != nil && state.CurrentThread.Breakpoint.ID == breakpointWorkflowStart {
				if err := debug.CancelNext(); err != nil {
					return nil, fmt.Errorf("failed cancelling next: %w", err)
				}
			} else {
				state, err = debug.Command(&api.DebuggerCommand{Name: api.Continue}, nil)
				if err != nil {
					return nil, fmt.Errorf("failed continuing while next in progress: %w", err)
				}
				continue
			}
		}

		// If it's part of temporal SDK or the file is prefixed for GOROOT, step out
		// and continue
		shouldStepOut :=
			strings.HasPrefix(state.CurrentThread.Function.Name(), "go.temporal.io/sdk/") ||
				strings.HasPrefix(filepath.ToSlash(state.CurrentThread.File), goRootSrc)
		if shouldStepOut {
			state, err = debug.Command(&api.DebuggerCommand{Name: api.StepOut}, nil)
			if err != nil {
				return nil, fmt.Errorf("failed stepping out: %w", err)
			}
			continue
		}

		// This is a line that represents an event
		if state.CurrentThread.File != "" {
			res.Events = append(res.Events, Event{Code: &EventCode{
				File: state.CurrentThread.File,
				Line: state.CurrentThread.Line,
			}})
		}

		// Do a normal step
		state, err = debug.Command(&api.DebuggerCommand{Name: api.Step}, nil)
		if err != nil {
			return nil, fmt.Errorf("failed stepping: %w", err)
		}
	}

	return &res, nil
}

func replayMainCode(config Config, fnPkg, fn string) ([]byte, error) {
	optionsCode, err := clientOptionsCode(config.ClientOptions)
	if err != nil {
		return nil, fmt.Errorf("invalid client options: %w", err)
	}
	source := `package main

import (
	"log"
	"context"

	fnpkg "` + fnPkg + `"
	"go.temporal.io/api/enums/v1"
	"go.temporal.io/api/history/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
)

func main() {
	// Create client
	c, err := client.NewClient(` + optionsCode + `)
	if err != nil {
		log.Fatalf("failed creating client: %v", err)
	}
	defer c.Close()

	// Create replayer
	replayer := worker.NewWorkflowReplayer()
	replayer.RegisterWorkflow(fnpkg.` + fn + `)
`
	// Load history if execution, otherwise use file
	if config.Execution != nil {
		source += `
	// Load history
	var hist history.History
	iter := c.GetWorkflowHistory(
		context.Background(),
		` + strconv.Quote(config.Execution.ID) + `,
		` + strconv.Quote(config.Execution.RunID) + `,
		false,
		enums.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT,
	)
	for iter.HasNext() {
		event, err := iter.Next()
		if err != nil {
			log.Fatalf("failed reading history: %v", err)
		}
		hist.Events = append(hist.Events, event)
	}

	// Replay
	err = replayer.ReplayWorkflowHistory(nil, &hist)`
	} else {
		source += `
	// Run from file
	err = replayer.ReplayWorkflowHistoryFromJSONFile(nil, ` + strconv.Quote(config.HistoryFile) + `) error
	`
	}
	source += `
	if err != nil {
		log.Fatalf("failed replaying workflow: %v", err)
	}
}
`
	return format.Source([]byte(source))
}

func clientOptionsCode(options client.Options) (string, error) {
	// For now, only some params required, others disallowed
	if options.HostPort == "" {
		return "", fmt.Errorf("missing host:port")
	} else if options.Namespace == "" {
		return "", fmt.Errorf("missing namespace")
	}
	// TODO(cretz): Validate none of the unsupported values are set
	return fmt.Sprintf("client.Options{HostPort: %q, Namespace: %q}", options.HostPort, options.Namespace), nil
}
