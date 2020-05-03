// Copyright 2019 The Bazel Authors. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// gopackagesdriver collects metadata, syntax, and type information for
// Go packages built with bazel. It implements the driver interface for
// golang.org/x/tools/go/packages. When gopackagesdriver is installed
// in PATH, tools like gopls written with golang.org/x/tools/go/packages,
// work in bazel workspaces.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/types"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	bespb "github.com/bazelbuild/rules_go/go/tools/gopackagesdriver/proto/build_event_stream"
	"github.com/golang/protobuf/proto"
	"golang.org/x/tools/go/packages"
)

const (
	bazelBin = "bazel"
)

func main() {
	log.SetPrefix("gopackagesdriver: ")
	log.SetFlags(0)
	if err := run(os.Args[1:]); err != nil {
		log.Fatal(err)
	}
}

// driverRequest is a JSON object sent by golang.org/x/tools/go/packages
// on stdin. Keep in sync.
type driverRequest struct {
	Command    string            `json:"command"`
	cMode      packages.LoadMode `json:"mode"`
	Env        []string          `json:"env"`
	BuildFlags []string          `json:"build_flags"`
	Tests      bool              `json:"tests"`
	Overlay    map[string][]byte `json:"overlay"`
}

// driverResponse is a JSON object sent by this program to
// golang.org/x/tools/go/packages on stdout. Keep in sync.
type driverResponse struct {
	// Sizes, if not nil, is the types.Sizes to use when type checking.
	Sizes *types.StdSizes

	// Roots is the set of package IDs that make up the root packages.
	// We have to encode this separately because when we encode a single package
	// we cannot know if it is one of the roots as that requires knowledge of the
	// graph it is part of.
	Roots []string `json:",omitempty"`

	// Packages is the full set of packages in the graph.
	// The packages are not connected into a graph.
	// The Imports if populated will be stubs that only have their ID set.
	// Imports will be connected and then type and syntax information added in a
	// later pass (see refine).
	Packages []*packages.Package
}

func run(args []string) error {
	ctx := context.Background()
	// Parse command line arguments and driver request sent on stdin.
	fs := flag.NewFlagSet("gopackagesdriver", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	directAndIndirectTargets, err := parseTargetsAndQueries(fs.Args())
	if err != nil {
		return err
	}
	targets, err := resolveTargets(ctx, directAndIndirectTargets)
	if err != nil {
		return err
	}

	reqData, err := ioutil.ReadAll(os.Stdin)
	if err != nil {
		return err
	}
	var req driverRequest
	if err := json.Unmarshal(reqData, &req); err != nil {
		return fmt.Errorf("could not unmarshal driver request: %v", err)
	}

	// Build package data files using bazel. We use one of several aspects
	// (depending on what mode we're in). The aspect produces .json and source
	// files in an output group. Each .json file contains a serialized
	// *packages.Package object.
	outputGroup := "gopackagesdriver_data"
	aspect := "gopackagesdriver_todo"

	// We ask bazel to write build event protos to a binary file, which
	// we read to find the output files.
	eventFile, err := ioutil.TempFile("", "gopackagesdriver-bazel-bep-*.bin")
	if err != nil {
		return err
	}
	eventFileName := eventFile.Name()
	defer func() {
		if eventFile != nil {
			eventFile.Close()
		}
		os.Remove(eventFileName)
	}()

	cmd := bazelCmd("build")
	if aspect == "FIXMEDONOTSUBMIT" {
		cmd.Args = append(cmd.Args, "--aspects="+aspect)
	}
	cmd.Args = append(cmd.Args, "--output_groups="+outputGroup)
	cmd.Args = append(cmd.Args, "--build_event_binary_file="+eventFile.Name())
	cmd.Args = append(cmd.Args, req.BuildFlags...)
	cmd.Args = append(cmd.Args, "--")
	for _, target := range targets {
		cmd.Args = append(cmd.Args, target...)
	}
	cmd.Stdout = os.Stderr // sic
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("error running bazel: %v", err)
	}

	eventData, err := ioutil.ReadAll(eventFile)
	if err != nil {
		return fmt.Errorf("could not read bazel build event file: %v", err)
	}
	eventFile.Close()

	var rootSets []namedSetOfFilesID
	setToFiles := make(map[namedSetOfFilesID][]string)
	setToSets := make(map[namedSetOfFilesID][]namedSetOfFilesID)
	pbuf := proto.NewBuffer(eventData)
	var event bespb.BuildEvent
	eventCount, targetCompletedCount := 0, 0
	for !event.GetLastMessage() {
		if err := pbuf.DecodeMessage(&event); err != nil {
			return err
		}
		eventCount++
		if id := event.GetId().GetTargetCompleted(); id != nil {
			targetCompletedCount++
			completed := event.GetCompleted()
			if !completed.GetSuccess() {
				return fmt.Errorf("%s: target did not build successfully", id.GetLabel())
			}
			for _, g := range completed.GetOutputGroup() {
				for _, s := range g.GetFileSets() {
					if setID := makeNamedNamedSetOfFilesID(s); setID != "" {
						rootSets = append(rootSets, setID)
					}
				}
			}
		}

		id := makeNamedNamedSetOfFilesID(event.GetId().GetNamedSet())
		if id == "" {
			continue
		}
		files := event.GetNamedSetOfFiles().GetFiles()
		fileNames := make([]string, len(files))
		for i, f := range files {
			fileNames[i] = f.GetName()
		}
		setToFiles[id] = fileNames
		sets := event.GetNamedSetOfFiles().GetFileSets()
		setIds := make([]namedSetOfFilesID, len(sets))
		for i, s := range sets {
			setIds[i] = makeNamedNamedSetOfFilesID(s)
		}
		setToSets[id] = setIds
	}

	var visit func(namedSetOfFilesID, map[string]bool, map[namedSetOfFilesID]bool)
	visit = func(setID namedSetOfFilesID, files map[string]bool, visited map[namedSetOfFilesID]bool) {
		if visited[setID] {
			return
		}
		visited[setID] = true
		for _, f := range setToFiles[setID] {
			files[f] = true
		}
		for _, s := range setToSets[setID] {
			visit(s, files, visited)
		}
	}

	files := make(map[string]bool)
	for _, s := range rootSets {
		visit(s, files, map[namedSetOfFilesID]bool{})
	}
	sortedFiles := make([]string, 0, len(files))
	for f := range files {
		sortedFiles = append(sortedFiles, f)
	}
	sort.Strings(sortedFiles)

	// Load data files referenced on the command line.
	pkgs := make(map[string]*packages.Package)
	roots := make(map[string]bool)
	for _, target := range targets {
		return (fmt.Errorf("JSON processing not implemented: %s; rootSets = %v; eventCount = %d, tcc = %d", target, rootSets, eventCount, targetCompletedCount))
	}

	sortedRoots := make([]string, 0, len(roots))
	for root := range roots {
		sortedRoots = append(sortedRoots, root)
	}
	sort.Strings(sortedRoots)

	sortedPkgs := make([]*packages.Package, 0, len(pkgs))
	for _, pkg := range pkgs {
		sortedPkgs = append(sortedPkgs, pkg)
	}
	sort.Slice(sortedPkgs, func(i, j int) bool {
		return sortedPkgs[i].ID < sortedPkgs[j].ID
	})

	resp := driverResponse{
		Sizes:    nil, // TODO
		Roots:    sortedRoots,
		Packages: sortedPkgs,
	}
	respData, err := json.Marshal(resp)
	if err != nil {
		return fmt.Errorf("could not marshal driver response: %v", err)
	}
	_, err = os.Stdout.Write(respData)
	if err != nil {
		return err
	}

	return errors.New("not implemented")
}

// namedSetOfFilesID is based on build_event_stream.BuildEvent.NamedSetOfFilesId
// and exists keep operations more typesafe than if we were to use the
// underlying string.
//
// corresponds to
// https://cs.opensource.google/bazel/bazel/+/master:src/main/java/com/google/devtools/build/lib/buildeventstream/proto/build_event_stream.proto;l=108
type namedSetOfFilesID string

func makeNamedNamedSetOfFilesID(x *bespb.BuildEventId_NamedSetOfFilesId) namedSetOfFilesID {
	return namedSetOfFilesID(x.GetId())
}

const (
	fileQueryPrefix    = "file="
	patternQueryPrefix = "query="
)

type targetOrQuery string

func (t targetOrQuery) String() string {
	return string(t)
}

func (t targetOrQuery) isBazelTarget() bool {
	return t.fileQuery() == "" && t.patternQuery() == ""
}

// fileQuery returns the value to the right of '=' in a 'file=...' argument.
//
// file queries should be resolved using something like
// https://docs.bazel.build/versions/master/query-how-to.html#What_build_rule_contains_file_ja
func (t targetOrQuery) fileQuery() string {
	if trimmed := strings.TrimPrefix(t.String(), fileQueryPrefix); trimmed != t.String() {
		return trimmed
	}
	return ""
}

func (t targetOrQuery) patternQuery() string {
	if trimmed := strings.TrimPrefix(t.String(), patternQueryPrefix); trimmed != t.String() {
		return trimmed
	}
	return ""
}

func parseTargetsAndQueries(args []string) ([]targetOrQuery, error) {
	if len(args) == 0 {
		return nil, errors.New("no targets specified")
	}
	var out []targetOrQuery
	for _, a := range args {
		out = append(out, targetOrQuery(a))
	}
	return out, nil
}

func resolveTargets(ctx context.Context, args []targetOrQuery) ([][]string, error) {
	resolvedLabels := make([][]string, len(args))
	for i, a := range args {
		if a.isBazelTarget() {
			resolvedLabels[i] = []string{a.String()}
			continue
		}
		if a.patternQuery() != "" {
			return nil, fmt.Errorf("don't know how to handle pattern query argument %q", a)
		}
		targets, err := targetsWithSrcFile(ctx, a.fileQuery())
		if err != nil {
			return nil, err
		}
		resolvedLabels[i] = targets
	}
	return resolvedLabels, nil
}

func targetsWithSrcFile(ctx context.Context, sourceFile string) ([]string, error) {
	if !strings.HasSuffix(sourceFile, ".go") {
		return nil, fmt.Errorf("don't know how to handle non-go file %q", sourceFile)
	}

	// file queries should be resolved using something like
	// https://docs.bazel.build/versions/master/query-how-to.html#What_build_rule_contains_file_ja
	sourceFileTarget, err := srcFileTarget(ctx, sourceFile)
	if err != nil {
		return nil, fmt.Errorf("UNIMPLEMENTED handling of src files with no labels: gopackagesdriver could not find a corresponding bazel label for file=%s: %w", sourceFile, err)
	}
	sourceFileBazelPackage := strings.TrimSuffix(sourceFileTarget, fmt.Sprintf(":%s", filepath.Base(sourceFile)))

	return []string{fmt.Sprintf("%s:all", sourceFileBazelPackage)}, nil
}

func srcFileTarget(ctx context.Context, src string) (string, error) {
	// file queries should be resolved using something like
	// https://docs.bazel.build/versions/master/query-how-to.html#What_build_rule_contains_file_ja
	c := bazelCmd("query", src)
	c.Stderr = os.Stderr
	got, err := c.Output()
	if err != nil {
		return "", fmt.Errorf("unable to map source file %q to bazel label: %w", src, err)
	}
	lines := strings.Split(strings.TrimRight(string(got), "\n"), "\n")
	if got, want := len(lines), 1; got != want {
		return "", fmt.Errorf("maping source file %q to bazel label got %d label(s), want %d: %v", src, got, want, lines)
	}
	return lines[0], nil
}

func bazelCmd(arg ...string) *exec.Cmd {
	return exec.Command(bazelBin, arg...)
}
