package tracer

import (
	"fmt"
	"go/format"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"

	"github.com/go-delve/delve/service/api"
	"github.com/go-delve/delve/service/debugger"
	"go.temporal.io/sdk/client"
)

type Config struct {
	ClientOptions       client.Options
	WorkflowFuncPackage string
	// TODO(cretz): What about struct functions? Maybe a WorkflowFuncExpr option?
	WorkflowFuncName string
	WorkflowID       string
	WorkflowRunID    string

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
	EventID int64 `json:"eventId"`
}

type EventCode struct {
	File          string         `json:"file,omitempty"`
	Line          int            `json:"line,omitempty"`
	LocalsUpdated []api.Variable `json:"locals_updated,omitempty"`
}

func Trace(config Config) (*Result, error) {
	// Create temp dir
	dir, err := os.MkdirTemp(config.RootDir, "debug-go-trace-")
	if err != nil {
		return nil, fmt.Errorf("failed creating temp dir: %w", err)
	}
	if !config.RetainTempDir {
		defer os.RemoveAll(dir)
	}

	// Create main.go
	if b, err := replayMainCode(config); err != nil {
		return nil, fmt.Errorf("failed building temp main.go: %w", err)
	} else if err = os.WriteFile(filepath.Join(dir, "main.go"), b, 0644); err != nil {
		return nil, fmt.Errorf("failed writing temp main.go: %w", err)
	}

	// Build binary with optimizations disabled (what the delve gobuild does for
	// >= 1.10.0)
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
	debugConfig := &debugger.Config{WorkingDir: config.RootDir}
	debug, err := debugger.New(debugConfig, []string{exe})
	if err != nil {
		return nil, fmt.Errorf("failed creating debugger: %w", err)
	}

	// Close debugger when done ignoring errors
	defer func() {
		if debug.IsRunning() {
			debug.Command(&api.DebuggerCommand{Name: api.Halt}, nil)
		}
		debug.Detach(true)
	}()

	// Local loading details
	// TODO(cretz): Configurable
	localLoadConfig := &api.LoadConfig{
		FollowPointers:     true,
		MaxVariableRecurse: 10,
		MaxStringLen:       255,
		MaxArrayValues:     100,
		MaxStructFields:    -1,
	}

	// Set breakpoint on the workflow function
	_, err = debug.CreateBreakpoint(&api.Breakpoint{
		FunctionName: config.WorkflowFuncPackage + "." + config.WorkflowFuncName,
		LoadLocals:   localLoadConfig,
	})
	if err != nil {
		return nil, fmt.Errorf("failed setting breakpoint at start of workflow: %w", err)
	}

	// TODO(cretz): Keep working
	panic("TODO")
}

func replayMainCode(config Config) ([]byte, error) {
	optionsCode, err := clientOptionsCode(config.ClientOptions)
	if err != nil {
		return nil, fmt.Errorf("invalid client options: %w", err)
	}
	source := `package main

import (
	"log"
	"context"

	fnpkg "` + config.WorkflowFuncPackage + `"
	"go.temporal.io/api/enums/v1"
	"go.temporal.io/api/history/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
)

func main() {
	// Create client
	c, err := client.NewClient(` + optionsCode + `)
	if err != nil {
		log.Fatalf("failed creating client: %w", err)
	}
	defer c.Close()

	// Load history
	var hist history.History
	iter := c.GetWorkflowHistory(
		context.Background(),
		` + strconv.Quote(config.WorkflowID) + `,
		` + strconv.Quote(config.WorkflowRunID) + `,
		false,
		enums.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT,
	)
	for iter.HasNext() {
		event, err := iter.Next()
		if err != nil {
			log.Fatalf("failed reading history: %w", err)
		}
		hist.Events = append(hist.Events, event)
	}

	// Replay
	replayer := sdkworker.NewWorkflowReplayer()
	replayer.RegisterWorkflow(fnpkg.` + config.WorkflowFuncName + `)
	err = replayer.ReplayWorkflowHistory(context.Background(), &hist)
	if err != nil {
		log.Fatalf("failed replaying workflow: %w", err)
	}
}
`
	return format.Source([]byte(source))
}

func clientOptionsCode(options client.Options) (string, error) {
	panic("TODO")
}
