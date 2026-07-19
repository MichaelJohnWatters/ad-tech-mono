// cmd/devconsole is the HOST-side dev loop UI — the deliberate replacement
// for Tilt's dashboard, kept thin on purpose: it is a face on the exact
// commands you'd type (`make deploy SVC=x`, `make stack-up`, …), not an
// orchestrator. It must run on the HOST because image builds need the local
// Go toolchain and docker socket — no in-cluster UI can do this part (the
// staff portal's Ops console covers everything cluster-side).
//
//	make devconsole   →  http://localhost:8099
//
// One action runs at a time (build output is interleaved-hostile); output
// streams live into the page. Binds 127.0.0.1 only — this thing shells out.
package main

import (
	_ "embed"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os/exec"
	"sync"
)

const listenAddr = "127.0.0.1:8099"

// services mirrors scripts/stack-images.sh: everything with a Build+Deploy
// button. "dsp" fans out to all three DSP deployments (make deploy handles it).
var services = []string{
	"dsp", "ssp", "exchange", "adserver", "publisher-adserver", "tracker",
	"pipeline", "webhooks", "identity-consumer", "report-runner", "ssai",
	"batch-conductor", "dayboundary", "gateway", "transcoder", "reporting",
}

// stackActions are whole-stack buttons, mapped to their make targets.
var stackActions = map[string][]string{
	"stack-up":     {"make", "stack-up"},
	"stack-images": {"make", "stack-images"},
	"seed":         {"go", "run", "./cmd/seed", "--profile", "standard"},
	"demo":         {"make", "demo"},
}

//go:embed console.html
var pageHTML string

type console struct {
	mu      sync.Mutex // one action at a time
	running string
}

func main() {
	c := &console{}
	tmpl := template.Must(template.New("page").Parse(pageHTML))

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		_ = tmpl.Execute(w, map[string]any{"Services": services})
	})
	mux.HandleFunc("POST /run/deploy/{svc}", func(w http.ResponseWriter, r *http.Request) {
		svc := r.PathValue("svc")
		if !validService(svc) {
			http.Error(w, "unknown service", http.StatusBadRequest)
			return
		}
		c.stream(w, r, "deploy "+svc, "make", "deploy", "SVC="+svc)
	})
	mux.HandleFunc("POST /run/action/{name}", func(w http.ResponseWriter, r *http.Request) {
		argv, ok := stackActions[r.PathValue("name")]
		if !ok {
			http.Error(w, "unknown action", http.StatusBadRequest)
			return
		}
		c.stream(w, r, r.PathValue("name"), argv[0], argv[1:]...)
	})

	log.Printf("devconsole: http://%s (host dev loop — cluster-side ops live in the staff portal)", listenAddr)
	log.Fatal(http.ListenAndServe(listenAddr, mux))
}

func validService(s string) bool {
	for _, v := range services {
		if v == s {
			return true
		}
	}
	return false
}

// stream runs one command and streams its combined output as chunked text.
// A second concurrent action gets 409 — build output must never interleave.
func (c *console) stream(w http.ResponseWriter, r *http.Request, label, bin string, args ...string) {
	c.mu.Lock()
	if c.running != "" {
		busy := c.running
		c.mu.Unlock()
		http.Error(w, "busy: "+busy, http.StatusConflict)
		return
	}
	c.running = label
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.running = ""
		c.mu.Unlock()
	}()

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	fl, _ := w.(http.Flusher)

	cmd := exec.CommandContext(r.Context(), bin, args...)
	cmd.Stdout = &flushWriter{w: w, fl: fl}
	cmd.Stderr = cmd.Stdout
	fmt.Fprintf(cmd.Stdout, "$ %s %v\n", bin, args)
	err := cmd.Run()
	if err != nil {
		fmt.Fprintf(cmd.Stdout, "\nFAILED: %v\n", err)
		return
	}
	fmt.Fprintf(cmd.Stdout, "\nOK\n")
}

type flushWriter struct {
	w  http.ResponseWriter
	fl http.Flusher
}

func (f *flushWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if f.fl != nil {
		f.fl.Flush()
	}
	return n, err
}
