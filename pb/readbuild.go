package pb

import (
	"bytes"
	"io"
	"os"
	"sync"

	"google.golang.org/protobuf/encoding/prototext"
)

var buildBufPool = sync.Pool{
	New: func() interface{} {
		return &bytes.Buffer{}
	},
}

func ReadBuildFile(path string) (*Build, error) {
	var build Build
	b := buildBufPool.Get().(*bytes.Buffer)
	b.Reset()
	defer buildBufPool.Put(b)
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if _, err := io.Copy(b, f); err != nil {
		return nil, err
	}
	if err := prototext.Unmarshal(b.Bytes(), &build); err != nil {
		return nil, err
	}
	return &build, nil
}
