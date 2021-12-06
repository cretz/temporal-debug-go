package tracer

import (
	"bytes"
	"fmt"
	"html"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/alecthomas/chroma"
	chromahtml "github.com/alecthomas/chroma/formatters/html"
	"github.com/alecthomas/chroma/lexers"
	"github.com/alecthomas/chroma/styles"
)

func (tr *Tracer) GenerateHTML(dir string, res *Result) error {
	// Create all the source HTML files and keep map of file path to html path
	var p page
	p.sources = map[string]string{}
	for _, event := range res.Events {
		if event.Code != nil && p.sources[event.Code.File] == "" {
			relFile := path.Join("sources",
				strings.ReplaceAll(event.Code.Package, "/", "__"),
				path.Base(event.Code.File)+".html")
			absFile := filepath.Join(dir, relFile)
			// Create parent dirs
			if err := os.MkdirAll(filepath.Dir(absFile), 0755); err != nil {
				return fmt.Errorf("failed creating dir %v: %w", filepath.Dir(absFile), err)
			}
			// Write
			if err := writeGoHTMLFile(event.Code.File, absFile); err != nil {
				return err
			}
			p.sources[event.Code.File] = relFile
		}
	}

	// Build index page
	p.h("<!DOCTYPE html>")
	p.h("<html>")
	p.h("<head>")
	p.indent()
	p.h(`<meta charset="utf-8">`)
	if tr.Execution != nil {
		p.h("<title>", "Workflow ", esc(tr.Execution.ID), "</title>")
	} else if tr.HistoryFile != "" {
		p.h("<title>", "History ", esc(tr.HistoryFile), "</title>")
	}
	p.dedent()
	p.h("</head>")
	p.h("<body>")
	p.indent()
	// Header
	p.h("<div>")
	p.indent()
	p.h("<h1>Workflow Execution</h1>")
	if tr.Execution != nil {
		p.h("<strong>ID: </strong>", esc(tr.Execution.ID), "<br />")
		if tr.Execution.RunID != "" {
			p.h("<strong>Run ID: </strong>", esc(tr.Execution.RunID), "<br />")
		}
	} else if tr.HistoryFile != "" {
		p.h("<strong>History: </strong>", esc(tr.HistoryFile), "<br />")
	}
	p.h("<strong>Entry Function: </strong>", tr.fnPkg, " - ", tr.fn)
	p.dedent()
	p.h("</div>")

	// Iterate events, keeping like events together
	var pendingEvents []*Event
	for _, event := range res.Events {
		// See if we need to flush pending events
		needsFlush := len(pendingEvents) > 0
		if needsFlush {
			lastEvent := pendingEvents[len(pendingEvents)-1]
			needsFlush = (lastEvent.Server != nil && event.Server == nil) ||
				(lastEvent.Client != nil && event.Client == nil) ||
				(lastEvent.Code != nil && event.Code == nil)
			// If we think we don't need flush due to code, make sure it's an
			// increasing line number of the same file and same coroutine
			if !needsFlush && lastEvent.Code != nil {
				needsFlush = lastEvent.Code.File != event.Code.File ||
					lastEvent.Code.Line > event.Code.Line ||
					lastEvent.Code.Coroutine != event.Code.Coroutine
			}
		}
		if needsFlush {
			p.eventSet(pendingEvents)
			pendingEvents = pendingEvents[:0]
		}
		pendingEvents = append(pendingEvents, event)
	}
	if len(pendingEvents) > 0 {
		p.eventSet(pendingEvents)
	}
	p.dedent()
	p.h("</body>")
	p.h("</html>")

	// Write index page
	return os.WriteFile(filepath.Join(dir, "index.html"), p.Bytes(), 0644)
}

type page struct {
	bytes.Buffer
	indentStr string
	sources   map[string]string
}

func (p *page) h(v ...interface{}) {
	p.WriteString(p.indentStr)
	for _, piece := range v {
		fmt.Fprint(p, piece)
	}
	fmt.Fprintln(p)
}

func (p *page) indent() {
	p.indentStr += "  "
}

func (p *page) dedent() {
	p.indentStr = p.indentStr[:len(p.indentStr)-2]
}

func (p *page) eventSet(events []*Event) {
	p.h("<hr />")
	// Handle if server or client
	if events[0].Server != nil {
		p.h("<strong>Events from server:</strong><br />")
		p.h("<ul>")
		p.indent()
		for _, event := range events {
			p.h("<li>", event.Server.Type, "</li>")
		}
		p.dedent()
		p.h("</ul>")
		return
	} else if events[0].Client != nil {
		p.h("<strong>Commands to server:</strong><br />")
		p.h("<ul>")
		p.indent()
		for _, event := range events {
			for _, command := range event.Client.Commands {
				p.h("<li>", command, "</li>")
			}
		}
		p.dedent()
		p.h("</ul>")
		return
	}

	// Now we know it's a code event, collect lines to highlight
	var hl []string
	for _, event := range events {
		hl = append(hl, strconv.Itoa(event.Code.Line))
	}
	src := p.sources[events[0].Code.File] + "?hl=" + strings.Join(hl, ",")
	p.h("<strong>Code: </strong>", esc(events[0].Code.Package), ` - <a href="`,
		esc(src), `">`, esc(filepath.Base(events[0].Code.File)), "</a>",
		" (coroutine: ", esc(events[0].Code.Coroutine), ")<br />")
	// We want 2 lines before and 2 lines after
	startLine := events[0].Code.Line - 2
	endLine := events[len(events)-1].Code.Line + 2
	height := (endLine - startLine) * 16
	// Build URL for iframe
	p.h(`<iframe height="`, height, `" src="`, esc(src), `" frameborder="0" style="width: 100%"></iframe>`)
}

func esc(s string) string { return html.EscapeString(s) }

func writeGoHTMLFile(sourceFile, targetFile string) error {
	// Read source
	source, err := os.ReadFile(sourceFile)
	if err != nil {
		return fmt.Errorf("failed reading %v: %w", sourceFile, err)
	}

	// Format
	lexer := chroma.Coalesce(lexers.Get("go"))
	formatter := chromahtml.New(
		chromahtml.Standalone(true),
		chromahtml.WithClasses(true),
		chromahtml.WithLineNumbers(true),
		chromahtml.LinkableLineNumbers(true, ""),
		// We want to act like the entire file is highlighted to get proper wrapping
		// of spans
		chromahtml.HighlightLines([][2]int{{1, bytes.Count(source, []byte{'\n'})}}),
	)
	style := styles.Get("github")
	iter, err := lexer.Tokenise(nil, string(source))
	if err != nil {
		return err
	}
	var target bytes.Buffer
	if err := formatter.Format(&target, style, iter); err != nil {
		return err
	}
	b := target.Bytes()

	// Remove the target highlight
	b = regexp.MustCompile(`/\* LineNumbers.* targeted.*\n`).ReplaceAll(b, nil)
	// Remove the highlight class from all lines
	b = bytes.ReplaceAll(b, []byte(` class="hl"`), nil)
	// Add highlight based on query param
	b = bytes.ReplaceAll(b, []byte("</body>"), []byte(
		`<script>
  const lines = (new URLSearchParams(window.location.search)).get('hl').split(',')

  // Highlight all lines
  lines.forEach(v => document.getElementById(v).parentElement.classList.add('hl'))

  // Due to chrome scrolling the parent when using an anchor, we instead just
  // manually scroll to two before the given line
  if (lines.length > 0) {
    setTimeout(() =>
      scroll({ top: document.getElementById('' + Math.max(1, parseInt(lines[0], 10) - 2)).offsetTop }), 1)
  }
</script>
</body>`))

	// Write
	return os.WriteFile(targetFile, b, 0644)
}
