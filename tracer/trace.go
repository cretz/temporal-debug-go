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
	*Tracer
	result       Result
	debug        *debugger.Debugger
	state        *api.DebuggerState
	sourceCache  map[string]string
	packageFiles map[string]string
	breakpoints  map[int]*breakpoint
	// Key is goroutine ID
	coroutineNames map[int]string
}

type breakpoint struct {
	*api.Breakpoint
	// Can be nil
	handler func() error
}

func (tr *Tracer) newTrace(dir, exe string) (*trace, error) {
	t := &trace{
		Tracer:         tr,
		sourceCache:    map[string]string{},
		packageFiles:   map[string]string{},
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
		matchInternalTaskHandlers  = matchInternalPkg + `internal_task_handlers\.go`
		matchInternalWorkflow      = matchInternalPkg + `internal_workflow\.go`
	)

	// Add breakpoint for workflow start
	err = t.addFuncBreakpoint(t.fnPkg+"."+t.fn, nil)
	// Add breakpoint for obtaining the event
	if err == nil {
		err = t.addFileLineBreakpoint(matchInternalEventHandlers, "\tif event == nil {", t.onProcessEvent)
	}
	// Add breakpoint for obtaining the commands
	if err == nil {
		err = t.addFileLineBreakpoint(matchInternalTaskHandlers, "if len(eventCommands) > 0 && !skipReplayCheck {",
			t.onReplayCommands)
	}
	// Add breakpoint for coroutine spawning
	if err == nil {
		err = t.addFileLineBreakpoint(matchInternalWorkflow, "\t\tf(spawned)", t.populateCoroutineName)
	}
	// Add breakpoint for end of initial yield
	if err == nil {
		err = t.addFileLineBreakpoint(matchInternalWorkflow, "\ts.blocked.Swap(false)", nil)
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
	for !t.state.Exited {

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

		// If there is a next in progress, it means a breakpoint was hit while
		// stepping from another. Since we have logic to step out where we want and
		// return from yields, we just cancel all next's.
		if t.state.NextInProgress {
			if err := t.debug.CancelNext(); err != nil {
				return fmt.Errorf("failed cancelling next: %w", err)
			}
		}

		// If the file is in the GOROOT or part of Temporal inner code, we step out
		goStdOrTemporal := strings.HasPrefix(filepath.ToSlash(t.state.CurrentThread.File), goRootSrc) ||
			strings.HasPrefix(t.state.CurrentThread.Function.Name(), "go.temporal.io/sdk/") ||
			strings.HasPrefix(t.state.CurrentThread.Function.Name(), "go.uber.org/atomic.") ||
			strings.HasPrefix(t.state.CurrentThread.Function.Name(), "runtime.") ||
			t.state.CurrentThread.Function.Name() == "main.main"
		if goStdOrTemporal {
			// If the function is runtime.goexit, we cannot step out because there is
			// nothing to step out to
			if strings.HasPrefix(t.state.CurrentThread.Function.Name(), "runtime.goexit") {
				t.state, err = t.debug.Command(&api.DebuggerCommand{Name: api.Step}, nil)
			} else {
				t.state, err = t.debug.Command(&api.DebuggerCommand{Name: api.StepOut}, nil)
			}
			if err != nil {
				return fmt.Errorf("failed stepping out: %w", err)
			}
			continue
		}

		// This is a line that represents an event if not Go stdlib or temporal
		if t.state.CurrentThread.File != "" && !goStdOrTemporal {
			pkg, _ := t.debug.CurrentPackage()
			t.result.Events = append(t.result.Events, &Event{Code: &EventCode{
				Package:   pkg,
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

// Breakpoint created for last line of code to match
func (t *trace) addFileLineBreakpoint(fileRegex string, codeToMatch string, handler func() error) error {
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
	t.breakpoints[bp.ID] = &breakpoint{Breakpoint: bp, handler: handler}
	return nil
}

func (t *trace) addFuncBreakpoint(fn string, handler func() error) error {
	bp, err := t.debug.CreateBreakpoint(&api.Breakpoint{FunctionName: fn})
	if err != nil {
		return err
	}
	t.breakpoints[bp.ID] = &breakpoint{Breakpoint: bp, handler: handler}
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
					i, err := intInTrailingParens(child.Value)
					if err != nil {
						return fmt.Errorf("invalid event type %q: %w", child.Value, err)
					}
					event.Type = EventServerType(i)
				}
			}
		}
	}
	t.result.Events = append(t.result.Events, &Event{Server: &event})
	return nil
}

func (t *trace) onReplayCommands() error {
	// Get "completedRequest" function arg which has "commands" array
	vars, err := t.debug.LocalVariables(t.state.CurrentThread.GoroutineID, 0, 0, proc.LoadConfig{
		FollowPointers: true, MaxStringLen: 200, MaxArrayValues: 100, MaxStructFields: -1, MaxVariableRecurse: 3,
	})
	if err != nil {
		return fmt.Errorf("failed loading vars: %w", err)
	}
	// TODO(cretz): This is expensive!
	var commands []EventClientCommandType
	for _, arg := range api.ConvertVars(vars) {
		if arg.Name == "eventCommands" {
			for _, command := range arg.Children {
				val := command.Children[0].Children[0].Value
				i, err := intInTrailingParens(val)
				if err != nil {
					return fmt.Errorf("invalid command type %q: %w", val, err)
				}
				commands = append(commands, EventClientCommandType(i))
			}
		}
	}
	if len(commands) > 0 {
		t.result.Events = append(t.result.Events, &Event{Client: &EventClient{Commands: commands}})
	}
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

func intInTrailingParens(str string) (int, error) {
	beginParens := strings.Index(str, "(")
	if beginParens < 0 || !strings.HasSuffix(str, ")") {
		return 0, fmt.Errorf("expected int in parens at the end")
	}
	intStr := str[beginParens+1 : len(str)-1]
	return strconv.Atoi(intStr)
}
