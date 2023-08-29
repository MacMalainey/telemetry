// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !plan9

// Command esbuild bundles and minifies stylesheet and typescript files.
//
// The command will walk the directory it is run in to gather the set of
// entrypoints and filenames beginning with an underscore are ignored.
//
//	go run golang.org/x/telemetry/godev/devtools/cmd/esbuild
//
// You can also pass a directory as an argument to the command.
//
//	go run golang.org/x/telemetry/godev/devtools/cmd/esbuild directory
//
// By default the command writes the output files to the same directory as
// the entrypoints with .min.css or .min.js extensions for .css and .ts files
// respectively. Override the output directory with a flag.
//
//	go run golang.org/x/telemetry/godev/devtools/cmd/esbuild --outdir static
//
// To watch the entrypoints and rebuild the output files on changes use the
// watch flag.
//
//	go run golang.org/x/telemetry/godev/devtools/cmd/esbuild --watch
package main

import (
	"flag"
	"io/fs"
	"log"
	"os"
	"path"
	"strings"

	"github.com/evanw/esbuild/pkg/api"
)

var (
	outdir = flag.String("outdir", ".", "output directory for the build operation")
	watch  = flag.Bool("watch", false, "listen for changes on the filesystem and automatically rebuild")
)

func main() {
	flag.Parse()
	dirs := []string{"."}
	if len(flag.Args()) > 0 {
		dirs = flag.Args()
	}
	for _, dir := range dirs {
		opts := api.BuildOptions{
			Banner:            map[string]string{"css": "/* Code generated by esbuild. DO NOT EDIT. */", "js": "// Code generated by esbuild. DO NOT EDIT."},
			Bundle:            true,
			EntryPoints:       entrypoints(dir),
			LogLevel:          api.LogLevelInfo,
			MinifyWhitespace:  true,
			MinifyIdentifiers: true,
			MinifySyntax:      true,
			OutExtension:      map[string]string{".css": ".min.css", ".js": ".min.js"},
			Outdir:            *outdir,
			Sourcemap:         api.SourceMapLinked,
			Write:             true,
		}
		if *watch {
			ctx, err := api.Context(opts)
			if err != nil {
				log.Fatal(err)
			}
			if err := ctx.Watch(api.WatchOptions{}); err != nil {
				log.Fatal(err)
			}
			// Returning from main() exits immediately in Go.
			// Block forever so we keep watching and don't exit.
			<-make(chan struct{})
		} else {
			result := api.Build(opts)
			if len(result.Errors) > 0 {
				// esbuild already logs errors
				os.Exit(1)
			}
		}
	}
}

func entrypoints(dir string) []string {
	var e []string
	if err := fs.WalkDir(os.DirFS(dir), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		base := path.Base(p)
		if strings.HasPrefix(base, "_") || strings.HasSuffix(base, ".min.css") || strings.HasSuffix(base, ".min.js") {
			return nil
		}
		switch path.Ext(p) {
		case ".css", ".ts":
			e = append(e, path.Join(dir, p))
		}
		return nil
	}); err != nil {
		log.Fatal(err)
	}
	return e
}
