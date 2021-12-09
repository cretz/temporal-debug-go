# Temporal Debug Go

This repository, and accompanying CLI `temporal-debug-go`, houses experiments in debugging and visualizing Temporal Go
code.

These are just experiments and proofs-of-concepts, often still under development, and may not work from version to
version.

## Building

With this repository cloned and [Go](https://golang.org/) installed and on the `PATH`, simply run `go build` at the top
directory. This will create a `temporal-debug-go` CLI binary.

Users can also just use `go run .` from the root of this repository to run instead of the two-step build-then-run.

## Code Tracing

Workflows executions are essentially single-threaded code executions that are backed by events coming from the Temporal
server. However, sometimes when developing in Go at a high-level, it is sometimes not obvious which lines of code run in
which order and as a response to which events. This can make understanding what happened on a past workflow execution
difficult to follow, even in a debugger. This is especially true using `workflow.Go`, `workflow.Channel`, signals, etc.

This code tracing tool helps clarify the execution by listing exactly which lines of code execute(d) on a past workflow
execution and in response to which events. These are listed in a single path of execution regardless of how the code
actually appears. This helps people:

* Understand why one piece of code may run before another when it may not otherwise be clear
* Debug which pieces of code ran in a past workflow execution without manually stepping through a debugger
* See how new workflow function alterations may run past executions

### Usage

To trace a past workflow execution, run:

    temporal-debug-go trace --wid MY_WF_ID --fn mydomain.com/pkg/path.WorkflowFunction

This example, if run within a directory that has a `go.mod`, and has a past workflow execution for workflow ID
`MY_WF_ID` on the localhost server, will replay the steps on the top-level package function `WorkflowFunction`, and dump
the events and the lines of code executed in the exact order.

Instead of dumping to stdout, `--json` can be used to set a JSON output file or `--html` can be used to set an HTML
output directory. Even if the replay of the workflow fails, output will still be performed.

There are other settings/approaches that can be used. Run `temporal-debug-go help trace` for more details.

The `github.com/cretz/temporal-debug-go/tracer` package can also be used as a library to run programmatically.

#### HTML Generation

When `--html DIR` is set, a static HTML site is generated in `DIR` representing the execution. `--html_theme THEME` can
be provided with one of the following values for `THEME`:

**simple-linear**

This is the default that just generates a simple set of linear steps with code shown highlighted in iframes.

[See an example here](https://cretz.github.io/temporal-debug-go/examples/cancellation/html-linear/)

**annotated**

This theme uses [Code Hike](https://codehike.org/) and [Next.js](https://nextjs.org/) to generate a step-based
visualization. Node must be installed to run this.

Note: The current version suffers some known scroll jank.

[See an example here](https://cretz.github.io/temporal-debug-go/examples/cancellation/html-annotated/)

#### Slow Execution

If the tracer is too slow and going through too much code, you may get something like:

    Error Potential deadlock detected: workflow goroutine "root" didn't yield for over a second

This is because the debugger is going so slow stepping so many lines. The debugger can be sped up significantly by
stepping out of files/functions eagerly. Use the `--exclude_func` and `--exclude_file` options to add regular expression
patterns for functions and files to step out of when they are reached. This means that code events will not be captured
for them _or for any code that is executed by them_ since this literally does a step-out debugger command. Note, files
are normalized to use the `/` separator before matched on all platforms.

The deadlock timeout can be removed altogether by setting the `TEMPORAL_DEBUG` environment variable to any value.

### Example

For example, at [examples/cancellation/workflow.go](examples/cancellation/workflow.go) there is a workflow and set of
activities that have a cancellation path. Specifically, the workflow executes one long-running activity that waits for
cancel, then once cancelled externally, there is an activity that is skipped (because workflow is cancelling), and
another activity that does cleanup. This is basically taken directly from the
[samples-go repo](https://github.com/temporalio/samples-go/tree/main/cancellation).

First, the workflow must _actually_ be executed against a server to have a recorded execution. Given a
[locally running Temporal server](https://docs.temporal.io/docs/server/quick-install/), from the root of this repository
run:

    go run ./examples/cancellation/run

This starts the workflow, waits a few seconds, then cancels it. In addition to other log lines, one in particular is
printed early:

    Starting workflow with ID my-workflow-2b38def0-8cb8-4f59-8f99-da52183512c5

That is the workflow ID. Now, we can check its code path:

    temporal-debug-go trace --wid my-workflow-2b38def0-8cb8-4f59-8f99-da52183512c5 --fn github.com/cretz/temporal-debug-go/examples/cancellation.MyWorkflow

This will end with the output:

    ------ TRACE ------
    Event 1 - WorkflowExecutionStarted
    Event 3 - WorkflowTaskStarted
            github.com/cretz/temporal-debug-go/examples/cancellation - workflow.go:12
            github.com/cretz/temporal-debug-go/examples/cancellation - workflow.go:13
            github.com/cretz/temporal-debug-go/examples/cancellation - workflow.go:18
            github.com/cretz/temporal-debug-go/examples/cancellation - workflow.go:19
            github.com/cretz/temporal-debug-go/examples/cancellation - workflow.go:22
            github.com/cretz/temporal-debug-go/examples/cancellation - workflow.go:23
            github.com/cretz/temporal-debug-go/examples/cancellation - workflow.go:37
            github.com/cretz/temporal-debug-go/examples/cancellation - workflow.go:38
            Command - ScheduleActivityTask
    Event 4 - WorkflowTaskCompleted
    Event 5 - ActivityTaskScheduled
    Event 6 - WorkflowExecutionCancelRequested
    Event 8 - WorkflowTaskStarted
            Command - RequestCancelActivityTask
    Event 9 - WorkflowTaskCompleted
    Event 10 - ActivityTaskCancelRequested
    Event 11 - ActivityTaskStarted
    Event 12 - ActivityTaskCompleted
    Event 14 - WorkflowTaskStarted
            github.com/cretz/temporal-debug-go/examples/cancellation - workflow.go:38
            github.com/cretz/temporal-debug-go/examples/cancellation - workflow.go:39
            github.com/cretz/temporal-debug-go/examples/cancellation - workflow.go:41
            Command - ScheduleActivityTask
            Command - RequestCancelActivityTask
    Event 15 - WorkflowTaskCompleted
    Event 16 - ActivityTaskScheduled
    Event 17 - ActivityTaskCancelRequested
    Event 18 - ActivityTaskCanceled
    Event 20 - WorkflowTaskStarted
            github.com/cretz/temporal-debug-go/examples/cancellation - workflow.go:41
            github.com/cretz/temporal-debug-go/examples/cancellation - workflow.go:42
            github.com/cretz/temporal-debug-go/examples/cancellation - workflow.go:44
            github.com/cretz/temporal-debug-go/examples/cancellation - workflow.go:46
            github.com/cretz/temporal-debug-go/examples/cancellation - workflow.go:23
            github.com/cretz/temporal-debug-go/examples/cancellation - workflow.go:25
            github.com/cretz/temporal-debug-go/examples/cancellation - workflow.go:30
            github.com/cretz/temporal-debug-go/examples/cancellation - workflow.go:31
            Command - ScheduleActivityTask
    Event 21 - WorkflowTaskCompleted
    Event 22 - ActivityTaskScheduled
    Event 23 - ActivityTaskStarted
    Event 24 - ActivityTaskCompleted
    Event 26 - WorkflowTaskStarted
            github.com/cretz/temporal-debug-go/examples/cancellation - workflow.go:31
            github.com/cretz/temporal-debug-go/examples/cancellation - workflow.go:32
            github.com/cretz/temporal-debug-go/examples/cancellation - workflow.go:35
            github.com/cretz/temporal-debug-go/examples/cancellation - workflow.go:46

Extra visualization options could be given such as:

* `--json examples/cancellation/trace.json` - output will look like [this JSON file](examples/cancellation/trace.json)
* `--html examples/cancellation/html-linear` - output will look like
  [this page](https://cretz.github.io/temporal-debug-go/examples/cancellation/html-linear/)
* `--html examples/cancellation/html-annotated --html_theme annotated` - output will look like
  [this page](https://cretz.github.io/temporal-debug-go/examples/cancellation/html-annotated/)


### How

This creates a temporary directory and dynamically creates and compiles a Go binary that starts the replayer using the
history of the given workflow ID. The embedded https://github.com/go-delve/delve debugger is used to execute the binary
and set breakpoints at both the top of the workflow and where events are processed internally. Then code is stepped
capturing events and code execution lines, filtering out any lines that are Go stdlib or Temporal SDK code.

### TODO

* Multiple workflow support for child workflows
* Expression-based workflow function creation for advanced initialization needs
* Include local variable values (and their changing) as part of the output
* Ability to serve tracer web server
  * Has config that has host, cache dir, code dir, and fn options
  * When no `wid` query param present, page has form for accepting workflow ID
  * When `wid` query param present, use cache if present (offer rebuild option) or live run if not

## Time-travelling Debugger

TODO: Still in the design stage. Essentially this will have a single command that:

* Start pre-built docker container that has this tool and its prereqs in the image
* With code parts as volumes, take a past execution and replay it with https://github.com/rr-debugger/rr in the
  container (rr is needed for the ability to step backwards)
* Take the rr dump and start a remote delve debugger w/ exposed port and provide a launch.json for VS code
* Optionally also start VS code itself via https://github.com/cdr/code-server connected to remote debugger, expose port
  to see vscode on, set breakpoint at the start of the workflow, launch, open VS code in web browser

So basically when done, you can do something like:

    temporal-debug-go rr-debug --wid MY_WF_ID --fn mydomain.com/pkg/path.WorkflowFunction

And in a second or two, a browser will open with a full-featured VS code instance letting you immediately step through
the workflow in a normal debugger. And you'll be able to step backwards _and_ forwards. We may even be able to provide
a place via delve to show the current event that is being handled and all past events too as variables.