package main

import "testing"

func TestSkipDeviceMapper(t *testing.T) {
	if !skipDeviceMapper("7208960") {
		t.Errorf("skipDeviceManager(): cookie unexpectedly not skipped")
	}
	if skipDeviceMapper("6291456") {
		t.Errorf("skipDeviceManager(): cookie unexpectedly skipped")
	}
}
