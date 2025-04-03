/*
 * Copyright 2025 The Kythe Authors. All rights reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *   http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"kythe.io/kythe/go/platform/kzip"
	"kythe.io/kythe/go/platform/vfs"
	apb "kythe.io/kythe/proto/analysis_go_proto"
	spb "kythe.io/kythe/proto/storage_go_proto"
)

type Source struct {
	IncludeDirs []string `json:"include_dirs"`
	ExcludeDirs []string `json:"exclude_dirs"`
}

type Dep struct {
	CrateId uint32 `json:"crate"`
	Name    string `json:"name"`
}

type Crate struct {
	RootModule   string   `json:"root_module"`
	Edition      string   `json:"edition"`
	Deps         []Dep    `json:"deps"`
	Cfg          []string `json:"cfg"`
	Files        []string
	CompilerArgs []string `json:"compiler_args"`
	CrateId      uint32   `json:"crate_id"`
	Label        string   `json:"label"`
	Target       string   `json:"target"`
	Source       Source   `json:"source"`
}

type RustProject struct {
	Crates []Crate `json:"crates"`
}

func removeProjectRoot(path string, projectRoot string) string {
	if len(path) >= len(projectRoot) && path[:len(projectRoot)] == projectRoot {
		return path[len(projectRoot):]
	}
	return path
}

// todo:refactor
func getSourceFiles(ctx context.Context, crate Crate, include_dirs []string, exclude_dirs []string, projectRoot string, required_inputs *[]*apb.CompilationUnit_FileInput, kzip_writer *kzip.Writer) ([]string, error) {
	var source_files []string
	for _, include_dir := range include_dirs {

		err := vfs.Walk(ctx, include_dir, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() {
				return nil
			}

			for _, exclude_dir := range exclude_dirs {
				if filepath.HasPrefix(path, exclude_dir) {
					return nil
				}
			}

			// only index .rs files for now. It's possible that some rust files depend on non rust files, like if they use include_bytes, but I'd prefer to find that out later rather than index a million .deps and .rmeta files.
			if filepath.Ext(path) != ".rs" {
				return nil
			}

			input, err := vfs.Open(ctx, path)
			if err != nil {
				return err
			}
			defer input.Close()

			digest, err := kzip_writer.AddFile(input)
			if err != nil {
				return err
			}

			path = removeProjectRoot(path, projectRoot)
			vname := &spb.VName{
				Corpus:   "fuchsia",
				Language: "rust",
				Path:     path,
			}

			source_files = append(source_files, path)
			*required_inputs = append(*required_inputs, &apb.CompilationUnit_FileInput{
				VName: vname,
				Info: &apb.FileInfo{
					Path:   path,
					Digest: digest,
				},
			})
			return nil
		})
		if err != nil {
			//fmt.Printf("Error walking directory %s: %v\n", include_dir, err)
			continue
		}
	}

	return source_files, nil
}

func getSourceDirs(crates []Crate, transitiveDeps map[uint32][]uint32) (map[uint32][]string, map[uint32][]string) {
	source_include_dirs := make(map[uint32][]string)
	source_exclude_dirs := make(map[uint32][]string)
	for _, crate := range crates {

		//remove
		/*
			if crate.CrateId != 3021 { // pkg-resolver
				continue
			}
		*/

		source_include_dirs[crate.CrateId] = append(source_include_dirs[crate.CrateId], crate.Source.IncludeDirs...)
		source_exclude_dirs[crate.CrateId] = append(source_exclude_dirs[crate.CrateId], crate.Source.ExcludeDirs...)
		for _, dep := range transitiveDeps[crate.CrateId] {
			if crates[dep].Source.IncludeDirs != nil {
				source_include_dirs[crate.CrateId] = append(source_include_dirs[crate.CrateId], crates[dep].Source.IncludeDirs...)
				source_exclude_dirs[crate.CrateId] = append(source_exclude_dirs[crate.CrateId], crates[dep].Source.ExcludeDirs...)
			}
		}
	}
	return source_include_dirs, source_exclude_dirs
}

func getTransitiveDependencies(crate Crate, crate_deps map[uint32][]uint32) []uint32 {
	// breadth first search over dependencies
	queue := []uint32{crate.CrateId}
	visited := make(map[uint32]bool)
	visited[crate.CrateId] = true
	for len(queue) > 0 {
		currentCrateId := queue[0]
		queue = queue[1:] // Dequeue

		// Iterate over the direct dependencies of the current crate
		for _, dep := range crate_deps[currentCrateId] {
			// If the dependency hasn't been visited, add it to the queue and mark it as visited
			if _, ok := visited[dep]; !ok {
				queue = append(queue, dep)
				visited[dep] = true
			}
		}
	}
	var transitiveDeps []uint32
	for k := range visited {
		if k != crate.CrateId {
			transitiveDeps = append(transitiveDeps, k)
		}
	}
	return transitiveDeps
}

func main() {
	if len(os.Args) < 4 {
		fmt.Println("Usage: go run main.go <project_json_path> <output_dir> <project_root>")
		os.Exit(1)
	}

	ctx := context.Background()

	projectJSONPath := os.Args[1]
	outputDir := os.Args[2]
	projectRoot := os.Args[3]

	if projectRoot[len(projectRoot)-1] != '/' {
		projectRoot += "/"
	}

	// ensure the project root exists on the disk and is a directory
	if info, err := os.Stat(projectRoot); os.IsNotExist(err) {
		fmt.Printf("Project root does not exist: %s\n", projectRoot)
		os.Exit(1)
	} else if err == nil && !info.IsDir() {
		fmt.Printf("Project root is not a directory: %s\n", projectRoot)
		os.Exit(1)
	} else if err != nil {
		fmt.Printf("Error checking project root: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("rust-project.json path: %s\n", projectJSONPath)

	projectJSONFile, err := os.Open(projectJSONPath)
	if err != nil {
		fmt.Printf("Error opening rust-project.json: %v\n", err)
		os.Exit(1)
	}
	defer projectJSONFile.Close()

	var projectJSON RustProject
	decoder := json.NewDecoder(projectJSONFile)
	err = decoder.Decode(&projectJSON)
	if err != nil {
		fmt.Printf("Error decoding rust-project.json: %v\n", err)
		os.Exit(1)
	}

	crates := projectJSON.Crates

	firstCrate, err := json.MarshalIndent(crates[0], "", "    ")
	if err != nil {
		fmt.Printf("Error marshalling first crate: %v\n", err)
		os.Exit(1)
	}
	fmt.Println(string(firstCrate))

	// delete outputdir if it already exists
	_ = os.RemoveAll(outputDir)

	crate_deps := make(map[uint32][]uint32)
	for _, crate := range crates {
		for _, dep := range crate.Deps {
			crate_deps[crate.CrateId] = append(crate_deps[crate.CrateId], dep.CrateId)
		}
	}

	// get the transitive dependencies of each crate
	transitiveDeps := make(map[uint32][]uint32)
	for _, crate := range crates {
		transitiveDepsForCrate := getTransitiveDependencies(crate, crate_deps)
		fmt.Printf("crate %d transitive deps: %v\n", crate.CrateId, transitiveDepsForCrate)
		transitiveDeps[crate.CrateId] = transitiveDepsForCrate
	}

	// for each crate, make a list of all source directories that it or its transitive dependencies include
	source_include_dirs, source_exclude_dirs := getSourceDirs(crates, transitiveDeps)

	kzip_options := kzip.WithEncoding(kzip.EncodingJSON)
	out, err := vfs.Create(ctx, outputDir)
	if err != nil {
		fmt.Printf("Error creating output directory: %v\n", err)
		os.Exit(1)

	}

	kzip_writer, err := kzip.NewWriteCloser(out, kzip_options)
	if err != nil {
		fmt.Printf("Error creating kzip writer: %v\n", err)
		os.Exit(1)
	}

	i := 0
	for _, crate := range crates {
		var crate_sources []string
		var required_inputs = []*apb.CompilationUnit_FileInput{}

		fmt.Printf("Adding crate %s...", crate.Label)

		if crate_sources, err = getSourceFiles(ctx, crate, crate.Source.IncludeDirs, crate.Source.ExcludeDirs, projectRoot, &required_inputs, kzip_writer); err != nil {
			fmt.Printf("Error getting source files for crate %s: %v\n", crate.Label, err)
			continue
		}

		// todo: wtf is this function call
		required_inputs = []*apb.CompilationUnit_FileInput{}
		if _, err := getSourceFiles(ctx, crate, source_include_dirs[crate.CrateId], source_exclude_dirs[crate.CrateId], projectRoot, &required_inputs, kzip_writer); err != nil {
			fmt.Printf("Error getting source files for crate %s: %v\n", crate.Label, err)
			continue
		}

		compilation_unit := &apb.CompilationUnit{
			VName: &spb.VName{
				Corpus:   "fuchsia",
				Language: "rust",
				Root:     crate.Label,
			},
			RequiredInput: required_inputs,
			SourceFile:    crate_sources, // TODO(wittrock): this should probably be the path to the root module and other source files should be in required_inputs.
		}

		digest, err := kzip_writer.AddUnit(compilation_unit, nil)
		if err != nil {
			fmt.Printf("Error adding compilation unit to kzip: %v, crate %s, digest: %s\n", err, crate.Label, digest)
			fmt.Printf("required inputs for crate %s on platform %s: %v\n", crate.Label, crate.Target, required_inputs)
			fmt.Printf("source include dirs for crate %s: %v", crate.Label, source_include_dirs[crate.CrateId])
			continue
		}

		fmt.Println(" done.")
		i++
	}

	fmt.Printf("Added %d crates\n", len(crates))
	fmt.Printf("Wrote %d units\n", i)
	kzip_writer.Close()
	fmt.Printf("wrote zip to %s\n", outputDir)
}
