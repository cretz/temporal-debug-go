package tracer

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/gogo/protobuf/jsonpb"
	"go.temporal.io/api/enums/v1"
	"go.temporal.io/api/history/v1"
	"go.temporal.io/sdk/client"
)

type HTMLGeneratorAnnotated struct {
	RetainTempDir bool
}

var htmlAnnotatedProjDir string

func init() {
	_, currFile, _, _ := runtime.Caller(0)
	htmlAnnotatedProjDir = filepath.Join(currFile, "..", "html_annotated_proj")
}

func (h *HTMLGeneratorAnnotated) GenerateHTML(ctx context.Context, t *Tracer, outDir string, res *Result) error {
	// Run NPM in annotated proj dir if no node_modules
	if _, err := os.Stat(filepath.Join(htmlAnnotatedProjDir, "node_modules")); os.IsNotExist(err) {
		t.Log.Debug("Running NPM install", "Dir", htmlAnnotatedProjDir)
		cmd := exec.CommandContext(ctx, "npm", "install")
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		cmd.Dir = htmlAnnotatedProjDir
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("npm install failed: %w", err)
		}
	}

	// Create temp dir
	tmpDir, err := os.MkdirTemp(htmlAnnotatedProjDir, "temp-app-")
	if err != nil {
		return fmt.Errorf("failed creating temp dir: %w", err)
	}
	t.Log.Debug("Building Next project", "Dir", tmpDir)
	if !h.RetainTempDir {
		defer os.RemoveAll(tmpDir)
	}

	// Add pages in a pages subdir
	pagesDir := filepath.Join(tmpDir, "pages")
	if err := os.Mkdir(pagesDir, 0755); err != nil {
		return fmt.Errorf("failed creating pages dir: %w", err)
	}
	err = os.WriteFile(filepath.Join(pagesDir, "_app.js"), []byte(
		`import '../../style.scss'

export default function App({ Component, pageProps }) {
  return <Component {...pageProps} />
}`), 0644)
	if err != nil {
		return fmt.Errorf("failed creating app JS: %w", err)
	}
	var title string
	if t.Execution != nil {
		title = "Workflow " + t.Execution.ID
	} else if t.HistoryFile != "" {
		title = "History " + t.HistoryFile
	}
	err = os.WriteFile(filepath.Join(pagesDir, "index.js"), []byte(
		`import Trace from '../trace.mdx'
import Head from 'next/head'

export default function Page() {
  return (
    <div style={{ margin: '10px' }}>
      <Head><title>`+esc(title)+`</title></Head>
      <Trace />
    </div>
  )
}`), 0644)
	if err != nil {
		return fmt.Errorf("failed creating index JS: %w", err)
	}

	// Create MDX file representing the full trace
	if err := h.writeMDX(ctx, t, tmpDir, res); err != nil {
		return err
	}

	// Run Next build
	t.Log.Debug("Running Next build", "Dir", tmpDir)
	cmd := exec.CommandContext(ctx, "npm", "run", "next", "--", "build", filepath.Base(tmpDir))
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Dir = htmlAnnotatedProjDir
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("next build failed: %w", err)
	}

	// Run Next export
	t.Log.Debug("Running Next export", "Dir", tmpDir)
	if err := os.MkdirAll(outDir, 0755); err != nil {
		return fmt.Errorf("failed creating dir %v: %w", outDir, err)
	}
	ourDirAbs, err := filepath.Abs(outDir)
	if err != nil {
		return err
	}
	cmd = exec.CommandContext(ctx, "npm", "run", "next", "--", "export", "-o", ourDirAbs, filepath.Base(tmpDir))
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Dir = htmlAnnotatedProjDir
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("next export failed: %w", err)
	}

	// Remove the 404 file if present (ignore error)
	os.Remove(filepath.Join(outDir, "404.html"))
	return nil
}

func (h *HTMLGeneratorAnnotated) writeMDX(ctx context.Context, t *Tracer, dir string, res *Result) error {
	var s simpleStringBuilder
	// Heading
	s.line("# Workflow Execution").line()
	if t.Execution != nil {
		s.linef("**ID:** `%v`", t.Execution.ID).line()
		if t.Execution.RunID != "" {
			s.linef("**Run ID:** `%v`", t.Execution.RunID).line()
		}
	} else if t.HistoryFile != "" {
		s.linef("**History:** `%v`", t.HistoryFile)
	}

	// Open scrollycoding
	s.line("<CH.Scrollycoding>").line()

	// Load the history and convert to indented JSON
	hist, err := h.loadHistory(ctx, t)
	if err != nil {
		return err
	}
	histJSON, err := (&jsonpb.Marshaler{Indent: "  "}).MarshalToString(hist)
	if err != nil {
		return err
	}

	// Go over each event, writing the step
	seenFiles := map[string]bool{}
	for i := 0; i < len(res.Events); i++ {
		event := res.Events[i]
		// Add separator after first
		if i > 0 {
			s.line("---").line()
		}

		switch {
		case event.Server != nil:
			// Put the event as a heading
			s.linef("### %v", event.Server.Type).line()
			focus := ""
			// Find the event ID in the history JSON
			eventIDIndex := strings.Index(histJSON, "\n      \"eventId\": \""+strconv.FormatInt(event.Server.ID, 10)+`"`)
			if eventIDIndex > 0 {
				// Find the opening and closing brace indexes
				openBraceIndex := strings.LastIndex(histJSON[:eventIDIndex], "\n    {")
				closeBraceIndex := strings.Index(histJSON[eventIDIndex:], "\n    }")
				if openBraceIndex > 0 && closeBraceIndex > 0 {
					startLine := strings.Count(histJSON[:openBraceIndex+1], "\n") + 1
					endLine := strings.Count(histJSON[:eventIDIndex+closeBraceIndex+1], "\n") + 1
					focus = fmt.Sprintf(" focus=%v:%v", startLine, endLine)
				}
			}

			// Make code block
			s.linef("```json history.json%v", focus)
			// Add actual code if first time seeing
			if !seenFiles["history.json"] {
				seenFiles["history.json"] = true
				s.line(histJSON)
			}
			s.line("```").line()

		case event.Client != nil:
			// Put command count as the heading
			plural := "s"
			if len(event.Client.Commands) == 1 {
				plural = ""
			}
			s.linef("* %v command%v to server", len(event.Client.Commands), plural).line()

			// Make code block as JSON set of commands
			commandStrs := make([]string, len(event.Client.Commands))
			for i, c := range event.Client.Commands {
				commandStrs[i] = c.String()
			}
			commandsJSON, err := json.MarshalIndent(map[string][]string{"commands": commandStrs}, "", "  ")
			if err != nil {
				return err
			}
			s.line("```json commands.json").line(string(commandsJSON)).line("```").line()

		case event.Code != nil:
			// Get line numbers for all subsequent code events that have the same
			// file, coroutine, and increasing line
			lineNums := []string{strconv.Itoa(event.Code.Line)}
			for i+1 < len(res.Events) {
				curr, next := res.Events[i], res.Events[i+1]
				sameBlock := next.Code != nil &&
					next.Code.File == curr.Code.File &&
					next.Code.Coroutine == curr.Code.Coroutine &&
					next.Code.Line >= curr.Code.Line
				if !sameBlock {
					break
				} else if next.Code.Line > curr.Code.Line {
					lineNums = append(lineNums, strconv.Itoa(next.Code.Line))
				}
				i++
			}

			// Put file and coroutine as heading
			file := filepath.Base(event.Code.File)
			s.linef("* Package: %v", event.Code.Package)
			s.linef("* File: %v", file)
			s.linef("* Coroutine: %v", event.Code.Coroutine)

			// Put code block, including code if first time seeing
			s.linef("```go %v&nbsp;-&nbsp;%v focus=%v", file, event.Code.Package, strings.Join(lineNums, ","))
			if !seenFiles[event.Code.File] {
				seenFiles[event.Code.File] = true
				b, err := os.ReadFile(event.Code.File)
				if err != nil {
					return fmt.Errorf("failed reading %v: %w", event.Code.File, err)
				}
				s.line(string(b))
			}
			s.line("```").line()
		}
	}

	// End scrollycoding and write
	s.line("</CH.Scrollycoding>").line()
	return os.WriteFile(filepath.Join(dir, "trace.mdx"), []byte(s.String()), 0644)
}

func (h *HTMLGeneratorAnnotated) loadHistory(ctx context.Context, t *Tracer) (*history.History, error) {
	// If the history file is present, unmarshal from it. Otherwise load from
	// execution.
	var hist history.History
	if t.HistoryFile != "" {
		if b, err := os.ReadFile(t.HistoryFile); err != nil {
			return nil, fmt.Errorf("failed loading history file: %w", err)
		} else if err = jsonpb.UnmarshalString(string(b), &hist); err != nil {
			return nil, fmt.Errorf("failed unmarshaling history file: %w", err)
		}
	} else if t.Execution != nil {
		// We have to connect to server to obtain history
		c, err := client.NewClient(t.ClientOptions)
		if err != nil {
			return nil, fmt.Errorf("failed connecting to server: %w", err)
		}
		defer c.Close()

		// Iterate and build events
		iter := c.GetWorkflowHistory(ctx, t.Execution.ID, t.Execution.RunID, false, enums.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)
		for iter.HasNext() {
			event, err := iter.Next()
			if err != nil {
				return nil, fmt.Errorf("failed fetching history: %w", err)
			}
			hist.Events = append(hist.Events, event)
		}
	} else {
		return nil, fmt.Errorf("must have execution or history file")
	}
	return &hist, nil
}

type simpleStringBuilder struct{ strings.Builder }

func (s *simpleStringBuilder) linef(f string, v ...interface{}) *simpleStringBuilder {
	s.WriteString(fmt.Sprintf(f, v...) + "\n")
	return s
}

func (s *simpleStringBuilder) line(v ...interface{}) *simpleStringBuilder {
	s.WriteString(fmt.Sprintln(v...))
	return s
}
