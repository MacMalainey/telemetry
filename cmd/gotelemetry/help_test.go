// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"flag"
	"fmt"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"strings"
	"testing"
)

var updateDocs = flag.Bool("update", false, "if set, update docs")

func TestMain(m *testing.M) {
	if os.Getenv("GOTELEMETRY_RUN_AS_MAIN") != "" {
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestDocHelp(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "help")
	cmd.Env = append(os.Environ(), "GOTELEMETRY_RUN_AS_MAIN=1")
	help, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}

	if *updateDocs {
		var lines []string
		for _, line := range strings.Split(strings.TrimSpace(string(help)), "\n") {
			if len(line) > 0 {
				lines = append(lines, "// "+line)
			} else {
				lines = append(lines, "//")
			}
		}
		contents := fmt.Sprintf(`// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Code generated by TestDocHelp; DO NOT EDIT.

%s
package main
`, strings.Join(lines, "\n"))

		data, err := format.Source([]byte(contents))
		if err != nil {
			t.Fatalf("formatting content: %v", err)
		}

		if err := os.WriteFile("doc.go", data, 0666); err != nil {
			t.Fatalf("writing doc.go: %v", err)
		}
	}

	f, err := parser.ParseFile(token.NewFileSet(), "doc.go", nil, parser.PackageClauseOnly|parser.ParseComments)
	if err != nil {
		t.Fatalf("parsing doc.go: %v", err)
	}
	doc := f.Doc.Text()
	if got, want := doc, string(help); got != want {
		t.Errorf("doc.go: mismatching content\ngot:\n%s\nwant:\n%s", got, want)
	}
}
