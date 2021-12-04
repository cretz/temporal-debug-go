package tracer

import (
	"context"
	"fmt"
	"go/format"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/log"
	"go.temporal.io/sdk/workflow"
)

var ImpliedExcludeFuncs = []*regexp.Regexp{
	// Exclude all Temporal internal code
	regexp.MustCompile(`^go\.temporal\.io/sdk/.*`),
	// Exclude all Uber atomic code
	regexp.MustCompile(`^go\.uber\.org/atomic\..*`),
	// Exclude anything in runtime package (this does not appear as part of
	// GOROOT so the file matcher does not apply)
	regexp.MustCompile(`^runtime\..*`),
	// Exclude the main.main function
	regexp.MustCompile(`^main\.main$`),
}

var ImpliedExcludeFiles = []*regexp.Regexp{
	// No files in GOROOT
	regexp.MustCompile("^" + filepath.ToSlash(filepath.Join(runtime.GOROOT(), "src")) + ".*"),
}

type Config struct {
	ClientOptions client.Options

	Log log.Logger
	// Qualified by package up to last dot. Only one supported for now
	// TODO(cretz): Support multiple for child workflows?
	WorkflowFuncs []string

	// One and only one of the next two fields required
	Execution   *workflow.Execution
	HistoryFile string // TODO(cretz): Stop creating client if history file given

	// Temp dir created under this, usually the current working dir so func
	// package works properly
	RootDir       string
	RetainTempDir bool

	// These are stepped out of if reached in any way. ImpliedExcludeFuncs and
	// ImpliedExcludeFiles are automatically assumed.
	ExcludeFuncs []*regexp.Regexp
	ExcludeFiles []*regexp.Regexp

	IncludeTemporalInternal bool
}

type Tracer struct {
	Config
	fnPkg string
	fn    string
}

func New(config Config) (*Tracer, error) {
	t := &Tracer{Config: config}
	if t.Execution == nil && t.HistoryFile == "" {
		return nil, fmt.Errorf("must have existing execution or history file")
	} else if t.Execution != nil && t.HistoryFile != "" {
		return nil, fmt.Errorf("cannot have both execution and history file")
	}
	if t.Log == nil {
		t.Log = DefaultLogger
	}

	// Split function and package
	// TODO(cretz): Support multiple workflows
	if len(t.WorkflowFuncs) != 1 {
		return nil, fmt.Errorf("single workflow function required")
	}
	// TODO(cretz): Support struct-based workflows
	lastDot := strings.LastIndex(config.WorkflowFuncs[0], ".")
	if lastDot == -1 {
		return nil, fmt.Errorf("workflow function missing dot")
	}
	t.fnPkg, t.fn = config.WorkflowFuncs[0][:lastDot], config.WorkflowFuncs[0][lastDot+1:]
	return t, nil
}

// This may still return a result, even if there is an error
func (t *Tracer) Trace(ctx context.Context) (*Result, error) {
	// Create temp dir
	dir, err := os.MkdirTemp(t.RootDir, "debug-go-trace-")
	if err != nil {
		return nil, fmt.Errorf("failed creating temp dir: %w", err)
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("could not turn dir absolute: %w", err)
	}
	t.Log.Debug("Created temp dir", "Dir", dir)
	if !t.RetainTempDir {
		defer func() {
			// We have to try this 20 times on Windows because there is a delay on
			// process exit before we can delete. This mimics what
			// github.com/go-delve/delve/pkg/gobuild.Remove does.
			var err error
			for i := 0; i < 20; i++ {
				err := os.RemoveAll(dir)
				if err == nil || runtime.GOOS != "windows" {
					break
				}
				time.Sleep(1 * time.Millisecond)
			}
			if err != nil {
				t.Log.Warn("Failed deleting temp dir", "Dir", dir, "Error", err)
			}
		}()
	}

	// Create main.go
	t.Log.Debug("Creating temp main.go")
	if b, err := t.buildReplayMainCode(); err != nil {
		return nil, fmt.Errorf("failed building temp main.go: %w", err)
	} else if err = os.WriteFile(filepath.Join(dir, "main.go"), b, 0644); err != nil {
		return nil, fmt.Errorf("failed writing temp main.go: %w", err)
	}

	// Build binary with optimizations disabled (what the delve gobuild does for
	// >= 1.10.0)
	t.Log.Debug("Building temp main.go")
	exe := filepath.Join(dir, "main")
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	cmd := exec.CommandContext(ctx, "go", "build", "-o", exe, "-gcflags=all=-N -l", "main.go")
	cmd.Dir = dir
	cmd.Stderr, cmd.Stdout = os.Stderr, os.Stdout
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("failed building main exe: %w", err)
	}

	// Run trace
	trace, err := t.newTrace(dir, exe)
	if err != nil {
		return nil, err
	}
	defer trace.close()
	// Run and return result even if it errors
	err = trace.run()
	return &trace.result, err
}

func (t *Tracer) buildReplayMainCode() ([]byte, error) {
	optionsCode, err := t.buildClientOptionsCode()
	if err != nil {
		return nil, fmt.Errorf("invalid client options: %w", err)
	}
	source := `package main

import (
	"log"
	"context"

	fnpkg "` + t.fnPkg + `"
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
	replayer.RegisterWorkflow(fnpkg.` + t.fn + `)
`
	// Load history if execution, otherwise use file
	if t.Execution != nil {
		source += `
	// Load history
	var hist history.History
	iter := c.GetWorkflowHistory(
		context.Background(),
		` + strconv.Quote(t.Execution.ID) + `,
		` + strconv.Quote(t.Execution.RunID) + `,
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
	err = replayer.ReplayWorkflowHistoryFromJSONFile(nil, ` + strconv.Quote(t.HistoryFile) + `) error
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

func (t *Tracer) buildClientOptionsCode() (string, error) {
	// For now, only some params required, others disallowed
	if t.ClientOptions.HostPort == "" {
		return "", fmt.Errorf("missing host:port")
	} else if t.ClientOptions.Namespace == "" {
		return "", fmt.Errorf("missing namespace")
	}
	// TODO(cretz): Validate none of the unsupported values are set
	return fmt.Sprintf("client.Options{HostPort: %q, Namespace: %q}",
		t.ClientOptions.HostPort, t.ClientOptions.Namespace), nil
}
