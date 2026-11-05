// Parses pack template fragments the way gc's loadSharedTemplates does
// (one template.Template, each file Parsed in turn, missingkey=zero), then
// renders a prompt that composes the named fragments, and reports the
// defined template names so a duplicate define would show.
package main

import (
	"fmt"
	"os"
	"strings"
	"text/template"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: fragparse <compose-names-comma-separated> <fragment files...>")
		os.Exit(2)
	}
	names := strings.Split(os.Args[1], ",")
	tmpl := template.New("prompt").Option("missingkey=zero")
	seen := map[string]string{}
	for _, f := range os.Args[2:] {
		data, err := os.ReadFile(f)
		if err != nil {
			fmt.Println("READ FAIL", f, err)
			os.Exit(1)
		}
		before := map[string]bool{}
		for _, t := range tmpl.Templates() {
			before[t.Name()] = true
		}
		probe := template.Must(template.New("probe").Parse(""))
		if _, err := probe.Parse(string(data)); err != nil {
			fmt.Println("PARSE FAIL (alone)", f, err)
			os.Exit(1)
		}
		for _, t := range probe.Templates() {
			if t.Name() == "probe" {
				continue
			}
			if prev, dup := seen[t.Name()]; dup {
				fmt.Printf("DUPLICATE define %q in %s (first in %s)\n", t.Name(), f, prev)
				os.Exit(1)
			}
			seen[t.Name()] = f
		}
		if _, err := tmpl.Parse(string(data)); err != nil {
			fmt.Println("PARSE FAIL", f, err)
			os.Exit(1)
		}
		fmt.Printf("parsed %s defines=%v\n", f, keys(probe))
	}
	body := ""
	for _, n := range names {
		body += fmt.Sprintf("{{ template %q . }}\n\n", n)
	}
	if _, err := tmpl.Parse(body); err != nil {
		fmt.Println("PARSE FAIL prompt", err)
		os.Exit(1)
	}
	var sb strings.Builder
	data := map[string]any{"TemplateName": "mayor"}
	if err := tmpl.ExecuteTemplate(&sb, "prompt", data); err != nil {
		fmt.Println("EXEC FAIL", err)
		os.Exit(1)
	}
	out := sb.String()
	fmt.Printf("rendered %d bytes composing %v\n", len(out), names)
	if strings.Contains(out, "{{") || strings.Contains(out, "<no value>") {
		fmt.Println("RENDER LEAK: template syntax or <no value> in output")
		os.Exit(1)
	}
	if path := os.Getenv("FRAGPARSE_OUT"); path != "" {
		if err := os.WriteFile(path, []byte(out), 0o644); err != nil {
			fmt.Println("WRITE FAIL", err)
			os.Exit(1)
		}
	}
	// Headings shared by two fragments would read as one section twice.
	heads := map[string]int{}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "#") {
			heads[line]++
		}
	}
	for h, c := range heads {
		if c > 1 {
			fmt.Printf("DUPLICATE heading %q x%d\n", h, c)
			os.Exit(1)
		}
	}
	fmt.Println("OK: unique defines, unique headings, renders clean")
}

func keys(t *template.Template) []string {
	var out []string
	for _, x := range t.Templates() {
		if x.Name() != "probe" {
			out = append(out, x.Name())
		}
	}
	return out
}
