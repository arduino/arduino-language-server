// This file is part of arduino-language-server.
//
// Copyright 2024 ARDUINO SA (http://www.arduino.cc/)
//
// This software is released under the GNU Affero General Public License version 3,
// which covers the main part of arduino-language-server.
// The terms of this license can be found at:
// https://www.gnu.org/licenses/agpl-3.0.html
//
// You can be released from the requirements of the above licenses by purchasing
// a commercial license. Buying such a license is mandatory if you want to
// modify or otherwise use the software for commercial activities involving the
// Arduino software without disclosing the source code of your own applications.
// To purchase a commercial license, send an email to license@arduino.cc.

package ls

import (
	"runtime"
	"strings"

	"github.com/arduino/go-paths-helper"
	"go.bug.st/json"
)

// compilationDatabase represents a compile_commands.json content
type compilationDatabase struct {
	Contents []compileCommand
	File     *paths.Path
}

// compileCommand keeps track of a single run of a compile command
type compileCommand struct {
	Directory string   `json:"directory"`
	Command   string   `json:"command,omitempty"`
	Arguments []string `json:"arguments,omitempty"`
	File      string   `json:"file"`
}

// loadCompilationDatabase load a compile_commands.json file into a compilationDatabase structure
func loadCompilationDatabase(file *paths.Path) (*compilationDatabase, error) {
	f, err := file.ReadFile()
	if err != nil {
		return nil, err
	}
	res := &compilationDatabase{
		File:     file,
		Contents: []compileCommand{},
	}
	return res, json.Unmarshal(f, &res.Contents)
}

// SaveToFile save the CompilationDatabase to file as a clangd-compatible compile_commands.json,
// see https://clang.llvm.org/docs/JSONCompilationDatabase.html
func (db *compilationDatabase) save() error {
	if jsonContents, err := json.MarshalIndent(db.Contents, "", " "); err != nil {
		return err
	} else if err := db.File.WriteFile(jsonContents); err != nil {
		return err
	}
	return nil
}

func canonicalizeCompileCommandsJSON(compileCommandsJSONPath *paths.Path) {
	// TODO: do canonicalization directly in `arduino-cli`

	compileCommands, err := loadCompilationDatabase(compileCommandsJSONPath)
	if err != nil {
		panic("could not find compile_commands.json")
	}
	for i, cmd := range compileCommands.Contents {
		if len(cmd.Arguments) == 0 {
			panic("invalid empty argument field in compile_commands.json")
		}

		// clangd requires full path to compiler (including extension .exe on Windows!)
		compilerPath := paths.New(cmd.Arguments[0]).Canonical()
		compiler := compilerPath.String()
		if runtime.GOOS == "windows" && strings.ToLower(compilerPath.Ext()) != ".exe" {
			compiler += ".exe"
		}
		compileCommands.Contents[i].Arguments[0] = compiler
	}

	// Save back compile_commands.json with OS native file separator and extension
	compileCommands.save()
}
