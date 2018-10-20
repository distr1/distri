package main

import "github.com/stapelberg/zi/pb"

func stepsToProto(steps [][]string) []*pb.BuildStep {
	protoSteps := make([]*pb.BuildStep, len(steps))
	for idx, argv := range steps {
		protoSteps[idx] = &pb.BuildStep{Argv: argv}
	}
	return protoSteps
}
