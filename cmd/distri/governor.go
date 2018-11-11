package main

import (
	"io/ioutil"
	"path/filepath"
)

func setGovernor(governor string) (cleanup func(), _ error) {
	const pattern = "/sys/devices/system/cpu/cpu*/cpufreq/scaling_governor"
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil, err
	}
	nonperf := false
	old := make([][]byte, len(matches))
	for idx, m := range matches {
		b, err := ioutil.ReadFile(m)
		if err != nil {
			return nil, err
		}
		old[idx] = b
		if string(b) == governor {
			nonperf = true
			continue
		}
		if err := ioutil.WriteFile(m, []byte(governor), 0644); err != nil {
			return nil, err
		}
	}
	if !nonperf {
		return func() {}, nil // nothing changed, nothing to clean up
	}
	return func() {
		for idx, m := range matches {
			if err := ioutil.WriteFile(m, old[idx], 0644); err != nil {
				return
			}
		}
	}, nil
}
