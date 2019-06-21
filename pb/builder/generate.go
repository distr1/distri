// Package builder defines a gRPC protocol to leverage remote compute resources
// in a distri build.
package builder

//go:generate protoc --go_out=plugins=grpc:. builder.proto
