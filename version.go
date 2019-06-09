package distri

import (
	"strconv"
	"strings"
)

// extractPackageRevision returns the distri package revision (e.g. 37 for
// glibc-amd64-2.27-37) from the specified filename. When no package revision
// can be parsed, 0 is returned.
func extractPackageRevision(filename string) int64 {
	parts := strings.Split(filename, "-")
	// Discard everything up to the architecture identifier, including the first
	// minus-separated part following it (the upstream version).
	for i := 1; i < len(parts); i++ {
		if Architectures[parts[i]] {
			// TODO: bounds check
			parts = parts[i+2:]
			break
		}
	}
	if len(parts) == 0 {
		return 0
	}
	rev := parts[len(parts)-1]
	if idx := strings.IndexByte(rev, '.'); idx > -1 {
		rev = rev[:idx] // strip any file extensions
	}
	if idx := strings.IndexByte(rev, '/'); idx > -1 {
		rev = rev[:idx] // constrain ourselves to this path component
	}
	revision, _ := strconv.ParseInt(rev, 0, 64)
	return revision
}

// PackageRevisionLess returns true if the distri package revision extracted
// from filenameA is less than those extracted from filenameB. This can be used
// with sort.Sort.
func PackageRevisionLess(filenameA, filenameB string) bool {
	versionA := extractPackageRevision(filenameA)
	versionB := extractPackageRevision(filenameB)
	return versionA < versionB
}
