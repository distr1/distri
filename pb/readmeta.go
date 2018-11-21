package pb

import (
	"io/ioutil"

	"github.com/golang/protobuf/proto"
)

func ReadMetaFile(path string) (*Meta, error) {
	var meta Meta
	b, err := ioutil.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if err := proto.UnmarshalText(string(b), &meta); err != nil {
		return nil, err
	}
	return &meta, nil
}
