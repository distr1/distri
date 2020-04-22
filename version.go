package distri

import (
	"strconv"
	"strings"
)

// PackageVersion describes one released version of a package. It is assumed
// that files never change in the archive, but may become unavailable.
type PackageVersion struct {
	Pkg  string
	Arch string

	// Upstream is the upstream version number. It is never parsed or compared,
	// and is meant for human consumption only.
	Upstream string

	// DistriRevision is an incrementing integer starting at 1. Every time the
	// package is changed, it must be increased by 1 so that e.g. distri update
	// will see the package. Even if upstream versions change, the revision does
	// not reset. E.g., 8.2.0-3 could be followed by 8.3.0-4.
	//
	// If the version could not be parsed, DistriRevision is 0.
	DistriRevision int64
}

func (pv PackageVersion) String() string {
	return pv.Pkg + "-" + pv.Arch + "-" + pv.Upstream + "-" + strconv.FormatInt(pv.DistriRevision, 10)
}

var fileExtensions = map[string]bool{
	"squashfs":       true,
	"meta.textproto": true,
	"log":            true,
}

func buildFile(filename, full string) bool {
	parts := strings.Split(filename, "/")
	return parts[len(parts)-1] == "build" && strings.HasSuffix(full, ".log")
}

func anyFullySpecified(filename string) string {
	// zero in on the correct path component first, if we can identify it
	var offset int
	idx := strings.IndexByte(filename, '/')
	var component string
	for {
		if idx == -1 {
			component = filename[offset:]
		} else {
			component = filename[offset : offset+idx]
		}
		if LikelyFullySpecified(component) {
			return component
		}
		if idx == -1 {
			break
		}
		offset += idx + 1
		idx = strings.IndexByte(filename[offset:], '/')
	}
	return filename
}

// ParseVersion constructs a PackageVersion from filename,
// e.g. glibc-amd64-2.31-4, which parses into PackageVersion{Upstream: "2.31",
// DistriRevision: 4}.
func ParseVersion(filename string) PackageVersion {
	filename = anyFullySpecified(filename)
	var pkg, arch string
	parts := strings.Split(filename, "-")
	// Locate the last architecture specifier. Any previous ones might be part
	// of the package name (e.g. glibc-i686-host).
	for i := len(parts) - 1; i > 0; i-- {
		if !Architectures[parts[i]] {
			continue
		}
		pkg = strings.Join(parts[:i], "-")
		if idx := strings.LastIndexByte(pkg, '/'); idx > -1 {
			pkg = pkg[idx+1:]
		}
		arch = parts[i]
		parts = parts[i+1:]
		break
	}
	if len(parts) == 0 {
		return PackageVersion{Pkg: pkg, Arch: arch}
	}
	// TODO: make build log files contain the architecture and delete this conditional:
	if buildFile(parts[0], filename) {
		parts = parts[1:]
	}
	upstream := strings.Join(parts, "-")
	for ext := range fileExtensions {
		upstream = strings.TrimSuffix(upstream, "."+ext)
	}
	if idx := strings.IndexByte(upstream, '/'); idx > -1 {
		upstream = upstream[:idx] // constrain ourselves to this path component
	}
	if len(parts) <= 1 {
		return PackageVersion{Pkg: pkg, Arch: arch, Upstream: upstream}
	}
	rev := parts[len(parts)-1]
	if idx := strings.IndexByte(rev, '.'); idx > -1 {
		rev = rev[:idx] // strip any file extensions
	}
	if idx := strings.IndexByte(rev, '/'); idx > -1 {
		rev = rev[:idx] // constrain ourselves to this path component
	}
	revision, _ := strconv.ParseInt(rev, 0, 64)
	if revision > 0 {
		upstream = strings.Join(parts[:len(parts)-1], "-")
	}
	return PackageVersion{
		Pkg:            pkg,
		Arch:           arch,
		Upstream:       upstream,
		DistriRevision: revision,
	}
}

// PackageRevisionLess returns true if the distri package revision extracted
// from filenameA is less than those extracted from filenameB. This can be used
// with sort.Sort.
func PackageRevisionLess(filenameA, filenameB string) bool {
	versionA := ParseVersion(filenameA).DistriRevision
	versionB := ParseVersion(filenameB).DistriRevision
	return versionA < versionB
}
