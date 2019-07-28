module github.com/distr1/distri

require (
	github.com/golang/gddo v0.0.0-20180911175731-8b031907f29f // indirect
	github.com/golang/protobuf v1.3.1
	github.com/google/go-cmp v0.3.0
	github.com/google/renameio v0.0.0-20181108174601-76365acd908f
	github.com/jacobsa/fuse v0.0.0-20180417054321-cd3959611bcb
	github.com/kylelemons/godebug v1.1.0 // indirect
	github.com/lpar/gzipped v1.1.0
	github.com/orcaman/writerseeker v0.0.0-20180723184025-774071c66cec
	golang.org/x/exp v0.0.0-20190221220918-438050ddec5e
	golang.org/x/net v0.0.0-20190724013045-ca1201d0de80 // indirect
	golang.org/x/sync v0.0.0-20190423024810-112230192c58
	golang.org/x/sys v0.0.0-20190507160741-ecd444e8653b
	golang.org/x/text v0.3.2 // indirect
	golang.org/x/xerrors v0.0.0-20190717185122-a985d3407aa7
	gonum.org/v1/gonum v0.0.0-20181012194325-406984d37414
	gonum.org/v1/netlib v0.0.0-20181018051557-57e1e4db57a7 // indirect
	google.golang.org/appengine v1.4.0 // indirect
	google.golang.org/genproto v0.0.0-20190502173448-54afdca5d873 // indirect
	google.golang.org/grpc v1.20.1
)

replace github.com/jacobsa/fuse => ./fuse

go 1.13
