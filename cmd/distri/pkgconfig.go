package main

import (
	"strings"
	"unicode"
)

// pkgConfigFilesFromRequires returns file names (e.g. atk.pc) from a Requires
// or Requires.private value (e.g. atk >= 2.15.1).
func pkgConfigFilesFromRequires(requires string) []string {
	const operators = "<>!="

	fields := strings.FieldsFunc(requires, func(r rune) bool {
		// modules are separated by space or comma
		return r == ',' || unicode.IsSpace(r)
	})

	var modules []string
	for i := 0; i < len(fields); i++ {
		f := fields[i]
		if strings.IndexAny(f, operators) == 0 {
			i++      // skip the operand
			continue // skip the operator
		}
		if strings.TrimSpace(f) == "" {
			continue
		}
		modules = append(modules, f)
	}
	return modules
}
