// internal/mxcli/describe.go
package mxcli

import (
	"context"
	"time"
)

type DescribeRequest struct {
	MprPath       string
	UnitType      string
	QualifiedName string
}

func Describe(ctx context.Context, req DescribeRequest) (string, error) {
	return run(ctx, 60*time.Second, "describe", "-p", req.MprPath, req.UnitType, req.QualifiedName)
}