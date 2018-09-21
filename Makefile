all:
	CGO_ENABLED=0 go install ./cmd/distri && sudo setcap CAP_SYS_ADMIN,CAP_DAC_OVERRIDE=ep ~/go/bin/distri
