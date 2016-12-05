// Copyright 2016 Google Inc.
// 
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
// 
//     http://www.apache.org/licenses/LICENSE-2.0
// 
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// The shenzhen-go binary serves a visual Go environment.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const pingMsg = "Pong!"

var (
	serveAddr = flag.String("addr", "::1", "Address to bind server to")
	servePort = flag.Int("port", 8088, "Port to serve from")

	identifierRE = regexp.MustCompile(`^[_a-zA-Z][_a-zA-Z0-9]*$`)
)

// Graph models a collection of goroutines (nodes) and channels (edges).
type Graph struct {
	Name        string
	PackageName string
	PackagePath string
	Imports     []string
	Nodes       map[string]*Node
	Edges       map[string]*Edge
}

func (g *Graph) renderToDot(dst io.Writer) error { return dotTemplate.Execute(dst, g) }
func (g *Graph) renderToGo(dst io.Writer) error  { return goTemplate.Execute(dst, g) }

func (g *Graph) renderNodeEditor(dst io.Writer, n *Node) error {
	return nodeEditorTemplate.Execute(dst, struct {
		Graph *Graph
		Node  *Node
	}{g, n})
}

func (g *Graph) renderEdgeEditor(dst io.Writer, e *Edge) error {
	return edgeEditorTemplate.Execute(dst, struct {
		Graph *Graph
		Edge  *Edge
	}{g, e})
}

// Node models a goroutine.
type Node struct {
	Name string
	Code string
	Wait bool
}

func (n *Node) String() string { return n.Name }

// Edge models a channel.
type Edge struct {
	Src, Dst string
	Name     string
	Type     string
	Cap      int
}

func open(args ...string) error {
	cmd := exec.Command(`open`, args...)
	return cmd.Run()
}

func pipeThru(dst io.Writer, cmd *exec.Cmd, src io.Reader) error {
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	if _, err := io.Copy(stdin, src); err != nil {
		return err
	}
	if err := stdin.Close(); err != nil {
		return err
	}
	if _, err := io.Copy(dst, stdout); err != nil {
		return err
	}
	return cmd.Wait()
}

func dotToSVG(dst io.Writer, src io.Reader) error {
	return pipeThru(dst, exec.Command(`dot`, `-Tsvg`), src)
}

func gofmt(dst io.Writer, src io.Reader) error {
	return pipeThru(dst, exec.Command(`gofmt`), src)
}

func goimports(dst io.Writer, src io.Reader) error {
	return pipeThru(dst, exec.Command(`goimports`), src)
}

func favIcon(w http.ResponseWriter, r *http.Request) {
	log.Printf("%s %s", r.Method, r.URL)
	w.Write([]byte(faviconSrc))
}

func (g *Graph) handleEdgeRequest(w http.ResponseWriter, r *http.Request) {
	log.Printf("%s %s", r.Method, r.URL)
	nm := strings.TrimPrefix(r.URL.Path, "/edge/")

	e, found := g.Edges[nm]
	if nm != "new" && !found {
		http.Error(w, fmt.Sprintf("Edge %q not found", nm), http.StatusNotFound)
		return
	}

	switch r.Method {
	case "POST":
		if e == nil {
			e = new(Edge)
		}

		// Parse...
		if err := r.ParseForm(); err != nil {
			log.Printf("Could not parse form: %v", err)
			http.Error(w, "Could not parse", http.StatusBadRequest)
			return
		}

		// ...Validate...
		nn := r.FormValue("Name")
		if !identifierRE.MatchString(nn) {
			msg := fmt.Sprintf("Invalid identifier %q !~ %q", nn, identifierRE)
			log.Printf(msg)
			http.Error(w, msg, http.StatusBadRequest)
			return
		}

		s := r.FormValue("Src")
		if _, ok := g.Nodes[s]; !ok {
			log.Printf("Unknown node %v", s)
			http.Error(w, "Unknown node", http.StatusBadRequest)
			return
		}

		d := r.FormValue("Dst")
		if _, ok := g.Nodes[d]; !ok {
			log.Printf("Unknown node %v", d)
			http.Error(w, "Unknown node", http.StatusBadRequest)
			return
		}

		ci, err := strconv.Atoi(r.FormValue("Cap"))
		if err != nil {
			log.Printf("Capacity is not an integer: %v", err)
			http.Error(w, "Capacity is not an integer", http.StatusBadRequest)
			return
		}
		if ci < 0 {
			log.Printf("Must specify nonnegative capacity [%d < 0]", ci)
			http.Error(w, "Capacity must be non-negative", http.StatusBadRequest)
			return
		}

		// ...update...
		e.Src = s
		e.Dst = d
		e.Type = r.FormValue("Type")
		e.Cap = ci

		// Do name changes last since they cause a redirect.
		if nn == e.Name {
			break
		}
		delete(g.Edges, e.Name)
		e.Name = nn
		g.Edges[nn] = e
		http.Redirect(w, r, "/edge/"+nn, http.StatusSeeOther) // should cause GET
		return
	}

	if err := g.renderEdgeEditor(w, e); err != nil {
		log.Printf("Could not render source editor: %v", err)
		http.Error(w, "Could not render source editor", http.StatusInternalServerError)
		return
	}
	return
}

func (g *Graph) handleNodeRequest(w http.ResponseWriter, r *http.Request) {
	log.Printf("%s %s", r.Method, r.URL)
	nm := strings.TrimPrefix(r.URL.Path, "/node/")
	n, found := g.Nodes[nm]
	if !found {
		http.Error(w, fmt.Sprintf("Node %q not found", nm), http.StatusNotFound)
		return
	}

	switch r.Method {
	case "POST":
		// Update the node.
		if err := r.ParseForm(); err != nil {
			log.Printf("Could not parse form: %v", err)
			http.Error(w, "Could not parse", http.StatusBadRequest)
			return
		}

		nm := strings.TrimSpace(r.FormValue("Name"))
		if nm == "" {
			log.Printf("Name invalid [%q == \"\"]", nm)
			http.Error(w, "Name invalid", http.StatusBadRequest)
			return
		}

		n.Wait = (r.FormValue("Wait") == "on")
		n.Code = r.FormValue("Code")

		if nm == n.Name {
			break
		}

		for _, e := range g.Edges {
			if e.Src == n.Name {
				e.Src = nm
			}
			if e.Dst == n.Name {
				e.Dst = nm
			}
		}

		delete(g.Nodes, n.Name)
		n.Name = nm
		g.Nodes[nm] = n

		http.Redirect(w, r, "/node/"+nm, http.StatusSeeOther) // should cause GET
		return
	}

	if err := g.renderNodeEditor(w, n); err != nil {
		log.Printf("Could not render source editor: %v", err)
		http.Error(w, "Could not render source editor", http.StatusInternalServerError)
		return
	}
	return
}

func (g *Graph) outputDotSrc(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", "text/vnd.graphviz")
	if err := g.renderToDot(w); err != nil {
		log.Printf("Could not render to dot: %v", err)
		http.Error(w, "Could not render to dot", http.StatusInternalServerError)
	}
}

func (g *Graph) writeGoSrc(w io.Writer) error {
	buf := &bytes.Buffer{}
	if err := g.renderToGo(buf); err != nil {
		return err
	}
	return goimports(w, buf)
}

func (g *Graph) outputGoSrc(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", "text/golang")
	if err := g.writeGoSrc(w); err != nil {
		log.Printf("Could not render to Go: %v", err)
		http.Error(w, "Could not render to Go", http.StatusInternalServerError)
	}
}

func (g *Graph) outputJSON(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(g); err != nil {
		log.Printf("Could not encode JSON: %v", err)
		http.Error(w, "Could not encode JSON", http.StatusInternalServerError)
		return
	}
}

func (g *Graph) handleRootRequest(w http.ResponseWriter, r *http.Request) {
	log.Printf("%s %s", r.Method, r.URL)
	q := r.URL.Query()
	if _, t := q["dot"]; t {
		g.outputDotSrc(w)
		return
	}
	if _, t := q["go"]; t {
		g.outputGoSrc(w)
		return
	}
	if _, t := q["json"]; t {
		g.outputJSON(w)
		return
	}
	if _, t := q["run"]; t {
		if err := g.saveBuildAndRun(); err != nil {
			log.Printf("Failed to save, build, run: %v", err)
		}
	}

	var dot, svg bytes.Buffer
	if err := g.renderToDot(&dot); err != nil {
		log.Printf("Could not render to dot: %v", err)
		http.Error(w, "Could not render to dot", http.StatusInternalServerError)
	}
	if err := dotToSVG(&svg, &dot); err != nil {
		log.Printf("Could not render dot to SVG: %v", err)
		http.Error(w, "Could not render dot to SVG", http.StatusInternalServerError)
	}
	if err := rootTemplate.Execute(w, template.HTML(svg.String())); err != nil {
		log.Printf("Could not execute root template: %v", err)
		http.Error(w, "Could not execute root template", http.StatusInternalServerError)
	}
}

func (g *Graph) saveGoSrc() error {
	gopath, ok := os.LookupEnv("GOPATH")
	if !ok || gopath == "" {
		return errors.New("cannot use $GOPATH; empty or undefined")
	}
	pp := filepath.Join(gopath, "src", g.PackagePath)
	if err := os.Mkdir(pp, os.FileMode(0755)); err != nil {
		log.Printf("Could not make path %q, continuing: %v", pp, err)
	}
	mp := filepath.Join(pp, "main.go")
	f, err := os.Create(mp)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := g.writeGoSrc(f); err != nil {
		return err
	}
	return f.Close()
}

func (g *Graph) build() error {
	return exec.Command(`go`, `build`, g.PackagePath).Run()
}

func (g *Graph) saveBuildAndRun() error {
	if err := g.saveGoSrc(); err != nil {
		return err
	}
	if err := g.build(); err != nil {
		return err
	}
	// TODO: Be less lazy about the output binary path
	return open("./" + g.PackageName)
}

func openWhenUp(addr string) {
	base := fmt.Sprintf("http://%s/", addr)
	t := time.NewTicker(100 * time.Millisecond)
	for range t.C {
		resp, err := http.Get(base + "ping")
		if err != nil {
			continue
		}
		defer resp.Body.Close()
		msg, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			continue
		}
		if string(msg) != pingMsg {
			continue
		}
		open(base)
		t.Stop()
		return
	}
}

func main() {
	flag.Parse()
	addr := net.JoinHostPort(*serveAddr, strconv.Itoa(*servePort))

	exampleGraph := Graph{
		Name:        "Example",
		PackageName: "example",
		PackagePath: "example", // == $GOPATH/src/example
		Imports: []string{
			"fmt",
		},
		Nodes: map[string]*Node{
			"Generate integers ≥ 2": {
				Name: "Generate integers ≥ 2",
				Code: `for i:= 2; i<100; i++ {
	raw <- i
}
close(raw)`,
				Wait: true,
			},
			"Filter divisible by 2": {
				Name: "Filter divisible by 2",
				Code: `for n := range raw {
	if n > 2 && n % 2 == 0 {
		continue
	}
	div2 <- n
}
close(div2)`,
				Wait: true,
			},
			"Filter divisible by 3": {
				Name: "Filter divisible by 3",
				Code: `for n := range div2 {
	if n > 3 && n % 3 == 0 {
		continue
	}
	out <- n
}
close(out)`,
				Wait: true,
			},
			"Print output": {
				Name: "Print output",
				Code: `for n := range out {
	fmt.Println(n)
}`,
				Wait: true,
			},
		},
		Edges: map[string]*Edge{
			"raw": {
				Src:  "Generate integers ≥ 2",
				Dst:  "Filter divisible by 2",
				Name: "raw",
				Type: "int",
				Cap:  0,
			},
			"div2": {
				Src:  "Filter divisible by 2",
				Dst:  "Filter divisible by 3",
				Name: "div2",
				Type: "int",
				Cap:  0,
			},
			"out": {
				Src:  "Filter divisible by 3",
				Dst:  "Print output",
				Name: "out",
				Type: "int",
				Cap:  0,
			},
		},
	}

	http.HandleFunc("/edge/", exampleGraph.handleEdgeRequest)
	http.HandleFunc("/node/", exampleGraph.handleNodeRequest)
	http.HandleFunc("/", exampleGraph.handleRootRequest)
	http.HandleFunc("/ping", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, pingMsg)
	})

	// As soon as we're serving, launch a web browser.
	// Generally expected to work on macOS...
	go openWhenUp(addr)

	if err := http.ListenAndServe(addr, nil); err != nil {
		log.Fatal(err)
	}
}