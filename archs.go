package distri

import "strings"

// Architectures contains one entry for each known architecture identifier.
var Architectures = map[string]bool{
	"amd64": true,
	"i686":  true,
}

// HasArchSuffix reports whether pkg ends in an architecture identifier
// (e.g. emacs-amd64) and returns the identifier.
func HasArchSuffix(pkg string) (archIdentifier string, ok bool) {
	for a := range Architectures {
		// unversioned, but ending in an architecture already (e.g. emacs-amd64)
		if strings.HasSuffix(pkg, "-"+a) {
			return a, true
		}
	}
	return "", false
}

// LikelyFullySpecified returns true if the provided pkg contains an
// architecture suffix in the middle, e.g. systemd-amd64-239.
func LikelyFullySpecified(pkg string) bool {
	for a := range Architectures {
		if strings.Contains(pkg, "-"+a+"-") {
			return true
		}
	}
	return false
}
