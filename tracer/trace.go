package tracer

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"

	"github.com/go-delve/delve/pkg/proc"
	"github.com/go-delve/delve/service/api"
	"github.com/go-delve/delve/service/debugger"
)

type trace struct {
	*tracer
	result      Result
	debug       *debugger.Debugger
	state       *api.DebuggerState
	sourceCache map[string]string
	breakpoints map[int]*breakpoint
	// Key is goroutine ID
	coroutineNames map[int]string
}

type breakpoint struct {
	*api.Breakpoint
	// If true, this cancels next and prevents a step out
	forStepping bool
	// Can be nil
	handler func() error
}

func (tr *tracer) newTrace(dir, exe string) (*trace, error) {
	t := &trace{
		tracer:         tr,
		sourceCache:    map[string]string{},
		breakpoints:    map[int]*breakpoint{},
		coroutineNames: map[int]string{},
	}

	// Create debugger
	t.Log.Debug("Starting debugger")
	var err error
	t.debug, err = debugger.New(&debugger.Config{WorkingDir: dir, Backend: "default"}, []string{exe})
	if err != nil {
		return nil, fmt.Errorf("failed creating debugger: %w", err)
	}
	// Close if not successful here
	success := false
	defer func() {
		if !success {
			t.close()
		}
	}()

	t.Log.Debug("Setting breakpoints")
	const (
		matchInternalPkg           = `.*/go\.temporal\.io/sdk.*/internal/`
		matchInternalEventHandlers = matchInternalPkg + `internal_event_handlers\.go`
		matchInternalWorkflow      = matchInternalPkg + `internal_workflow\.go`
	)

	// Add breakpoint for workflow start
	err = t.addFuncBreakpoint(t.fnPkg+"."+t.fn, true, nil)
	// Add non-stepping breakpoint for obtaining the event
	if err == nil {
		err = t.addFileLineBreakpoint(matchInternalEventHandlers, "\tif event == nil {", false, t.onProcessEvent)
	}
	// Add stepping breakpoint for coroutine spawning
	if err == nil {
		err = t.addFileLineBreakpoint(matchInternalWorkflow, "\t\tf(spawned)", true, t.populateCoroutineName)
	}
	if err != nil {
		return nil, err
	}

	success = true
	return t, nil
}

func (t *trace) close() {
	t.Log.Debug("Halting debugger")
	if _, err := t.debug.Command(&api.DebuggerCommand{Name: api.Halt}, nil); err != nil {
		t.Log.Warn("Failed halting", "Error", err)
	}
	t.Log.Debug("Detaching debugger")
	if err := t.debug.Detach(true); err != nil {
		t.Log.Warn("Failed detaching", "Error", err)
	}
}

func (t *trace) run() error {
	// Continue until the breakpoint is hit
	t.Log.Debug("Starting execution")
	var err error
	t.state, err = t.debug.Command(&api.DebuggerCommand{Name: api.Continue}, nil)
	if err != nil {
		return fmt.Errorf("failed starting execution: %w", err)
	}

	// Step until runtime exit
	goRootSrc := filepath.ToSlash(filepath.Join(runtime.GOROOT(), "src"))
	for !t.state.Exited && t.state.CurrentThread != nil && t.state.CurrentThread.Function.Name() != "runtime.goexit" {

		// If we have hit a breakpoint, capture it
		var bp *breakpoint
		if t.state.CurrentThread.Breakpoint != nil {
			bp = t.breakpoints[t.state.CurrentThread.Breakpoint.ID]
			// Run handler if set
			if bp.handler != nil {
				if err := bp.handler(); err != nil {
					return err
				}
			}
		}

		// This means we have hit a breakpoint while stepping from another
		// breakpoint. Our only options are to "cancel next" which means to ignore
		// what we were doing and step here, or "continue" which means go back to
		// what we were doing before this breakpoint. We want to "cancel next" if
		// the breakpoint is for stepping (i.e. resume stepping here), or "continue"
		// if it is not.
		if t.state.NextInProgress {
			// Continue if not a for-stepping breakpoint
			if bp == nil || !bp.forStepping {
				t.state, err = t.debug.Command(&api.DebuggerCommand{Name: api.Continue}, nil)
				if err != nil {
					return fmt.Errorf("failed continuing while next in progress: %w", err)
				}
				continue
			}
			// Otherwise, just cancel next and move on
			if err := t.debug.CancelNext(); err != nil {
				return fmt.Errorf("failed cancelling next: %w", err)
			}
		}

		// Unless we're at a for-stepping breakpoint, we step out if the file is
		// prefixed with GOROOT or it is part of the temporal SDK or it is main
		goStdOrTemporal := strings.HasPrefix(filepath.ToSlash(t.state.CurrentThread.File), goRootSrc) ||
			strings.HasPrefix(t.state.CurrentThread.Function.Name(), "go.temporal.io/sdk/") ||
			t.state.CurrentThread.Function.Name() == "main.main"
		if (bp == nil || !bp.forStepping) && goStdOrTemporal {
			t.state, err = t.debug.Command(&api.DebuggerCommand{Name: api.StepOut}, nil)
			if err != nil {
				return fmt.Errorf("failed stepping out: %w", err)
			}
			continue
		}

		// This is a line that represents an event if not Go stdlib or temporal
		if t.state.CurrentThread.File != "" && !goStdOrTemporal {
			t.result.Events = append(t.result.Events, Event{Code: &EventCode{
				File:      t.state.CurrentThread.File,
				Line:      t.state.CurrentThread.Line,
				Coroutine: t.coroutineNames[t.state.CurrentThread.GoroutineID],
			}})
		}

		// Do a normal step
		t.state, err = t.debug.Command(&api.DebuggerCommand{Name: api.Step}, nil)
		if err != nil {
			return fmt.Errorf("failed stepping: %w", err)
		}
	}

	// If there was a failure, fail
	if t.state.Exited && t.state.ExitStatus != 0 {
		return fmt.Errorf("failed with exit status: %v", t.state.ExitStatus)
	}

	return nil
}

func (t *trace) addFileLineBreakpoint(
	fileRegex string,
	// Breakpoint is on last line of code
	codeToMatch string,
	forStepping bool,
	handler func() error,
) error {
	// Find the file name
	// TODO(cretz): Cache this lookup too?
	var file string
	fileRegexp, err := regexp.Compile(fileRegex)
	if err != nil {
		return fmt.Errorf("invalid file regex: %w", err)
	}
	for _, maybeFile := range t.debug.Target().BinInfo().Sources {
		if fileRegexp.MatchString(maybeFile) {
			if file != "" {
				return fmt.Errorf("both %v and %v match %v", file, maybeFile, fileRegex)
			}
			file = maybeFile
		}
	}
	if file == "" {
		return fmt.Errorf("unable to find file matching %v", fileRegex)
	}

	// Get source lines
	source := t.sourceCache[file]
	if source == "" {
		// Read it and split into lines
		b, err := os.ReadFile(file)
		if err != nil {
			return fmt.Errorf("failed reading %v: %w", file, err)
		}
		source = strings.ReplaceAll(string(b), "\r\n", "\n")
		t.sourceCache[file] = source
	}

	// Find line for code to match
	codeIndex := strings.Index(source, codeToMatch)
	if codeIndex == -1 {
		return fmt.Errorf("cannot find matching code on %v", file)
	} else if strings.LastIndex(source, codeToMatch) != codeIndex {
		return fmt.Errorf("code found twice in %v", file)
	}
	line := strings.Count(source[:codeIndex+len(codeToMatch)], "\n") + 1

	// Add the breakpoint
	bp, err := t.debug.CreateBreakpoint(&api.Breakpoint{File: file, Line: line})
	if err != nil {
		return err
	}
	t.breakpoints[bp.ID] = &breakpoint{Breakpoint: bp, forStepping: forStepping, handler: handler}
	return nil
}

func (t *trace) addFuncBreakpoint(fn string, forStepping bool, handler func() error) error {
	bp, err := t.debug.CreateBreakpoint(&api.Breakpoint{FunctionName: fn})
	if err != nil {
		return err
	}
	t.breakpoints[bp.ID] = &breakpoint{Breakpoint: bp, forStepping: forStepping, handler: handler}
	return nil
}

func (t *trace) onProcessEvent() error {
	// Need the event and type from function args
	vars, err := t.debug.FunctionArguments(t.state.CurrentThread.GoroutineID, 0, 0, proc.LoadConfig{
		FollowPointers: true, MaxStringLen: 200, MaxArrayValues: 1, MaxStructFields: -1,
	})
	if err != nil {
		return fmt.Errorf("failed loading vars: %w", err)
	}
	var event EventServer
	for _, arg := range api.ConvertVars(vars) {
		if arg.Name == "event" {
			for _, child := range arg.Children[0].Children {
				if child.Name == "EventId" {
					if event.ID, err = strconv.ParseInt(child.Value, 10, 64); err != nil {
						return fmt.Errorf("invalid event ID %v: %w", child.Value, err)
					}
				} else if child.Name == "EventType" {
					// Take the int in parentheses and convert to type
					beginParens := strings.Index(child.Value, "(")
					if beginParens < 0 || !strings.HasSuffix(child.Value, ")") {
						return fmt.Errorf("invalid event type %q, expected int in parens at the end", child.Value)
					}
					intStr := child.Value[beginParens+1 : len(child.Value)-1]
					i, err := strconv.Atoi(intStr)
					if err != nil {
						return fmt.Errorf("invalid event type %q, failed converting to int: %w", child.Value, err)
					}
					event.Type = EventServerType(i)
				}
			}
		}
	}
	t.result.Events = append(t.result.Events, Event{Server: &event})
	return nil
}

func (t *trace) populateCoroutineName() error {
	// Get function args which has "crt" which has "name"
	vars, err := t.debug.FunctionArguments(t.state.CurrentThread.GoroutineID, 0, 0, proc.LoadConfig{
		FollowPointers: true, MaxStringLen: 200, MaxArrayValues: 1, MaxStructFields: -1, MaxVariableRecurse: 2,
	})
	if err != nil {
		return fmt.Errorf("failed loading vars: %w", err)
	}
	// TODO(cretz): This is expensive!
	for _, arg := range api.ConvertVars(vars) {
		if arg.Name == "crt" {
			for _, child := range arg.Children[0].Children {
				if child.Name == "name" {
					t.coroutineNames[t.state.CurrentThread.GoroutineID] = child.Value
					return nil
				}
			}
		}
	}
	return nil
}
