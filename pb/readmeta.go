package pb

import (
	"bytes"
	"io"
	"os"
	"sync"

	"github.com/golang/protobuf/proto"
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
	if err := proto.UnmarshalText(b.String(), &meta); err != nil {
		return nil, err
	}
	return &meta, nil
}
