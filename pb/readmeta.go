package pb

import (
	"bytes"
	"io"
	"os"
	"sync"

	"google.golang.org/protobuf/encoding/prototext"
)

var metaBufPool = sync.Pool{
	New: func() interface{} {
		return &bytes.Buffer{}
	},
}

func ReadMetaFile(path string) (*Meta, error) {
	var meta Meta
	b := metaBufPool.Get().(*bytes.Buffer)
	b.Reset()
	defer metaBufPool.Put(b)
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if _, err := io.Copy(b, f); err != nil {
		return nil, err
	}
	if err := (prototext.UnmarshalOptions{
		// Discarding unknown fields is more robust: when the user runs a
		// different version of distri as FUSE daemon process and install
		// process, installing packages with an unknown field might result
		// in an error.
		DiscardUnknown: true,
	}).Unmarshal(b.Bytes(), &meta); err != nil {
		return nil, err
	}
	return &meta, nil
}
