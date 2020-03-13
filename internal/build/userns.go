package build

import (
	"io/ioutil"
	"strings"
)

// usernsError examines the system and returns suggestions for users to try and
// enable user namespaces.
func usernsError() string {
	// Check if we are running in Docker and adjust the error message to clarify
	// to run these commands on the host:
	var runningInDocker bool
	if b, err := ioutil.ReadFile("/proc/1/cgroup"); err == nil {
		if strings.Contains(string(b), "docker") {
			runningInDocker = true
		}
	}

	var fixes []string

	// Check if kernel.unprivileged_userns_clone (Debian, Arch) is off:
	if b, err := ioutil.ReadFile("/proc/sys/kernel/unprivileged_userns_clone"); err == nil {
		if val := strings.TrimSpace(string(b)); val != "1" {
			fixes = append(fixes, "sysctl -w kernel.unprivileged_userns_clone=1")
		}
	}

	// Check if user.max_user_namespaces (introduced in Linux 4.9) is non-zero
	// (defaults to zero on RHEL):
	if b, err := ioutil.ReadFile("/proc/sys/user/max_user_namespaces"); err == nil {
		if val := strings.TrimSpace(string(b)); val == "0" {
			fixes = append(fixes, "sysctl -w user.max_user_namespaces=1000")
		}
	}

	if len(fixes) == 0 {
		return ""
	}

	suggestion := strings.Join(fixes, "\n")

	if runningInDocker {
		return "On your Docker host (not in the container), try:\n" + suggestion
	}
	return "try:\n" + suggestion
}
