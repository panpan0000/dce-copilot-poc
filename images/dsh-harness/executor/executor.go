// Package executor builds the DSH A2A executor for kagent-dsh.
package executor

import (
	"context"
	"io"

	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/kagent-dev/kagent/go/harness/dsh/internal/dsh"
	runtimea2a "github.com/kagent-dev/kagent/go/harness/runtime/a2a"
	"github.com/kagent-dev/kagent/go/harness/runtime/continuation"
	"github.com/kagent-dev/kagent/go/pkg/tracing"
)

// Config is the input to New.
type Config struct {
	DataDir     string
	DSHEndpoint string
	AgentName   string
}

// New returns the executor and the resource to close on shutdown.
func New(ctx context.Context, cfg Config) (a2asrv.AgentExecutor, io.Closer, error) {
	runner := dsh.New(cfg.DSHEndpoint, cfg.DataDir+"/workspace")
	store, err := continuation.New(cfg.DataDir+"/adapter", "dsh", validateSessionID)
	if err != nil {
		_ = runner.Close()
		return nil, nil, err
	}
	// 零值 telemetry：部分身份（只有 agent name）会被校验拒绝；
	// 等 kagent 给 DSH 定义 runtime 标识后再补 Runtime/AgentNamespace。
	exec, err := runtimea2a.New(runner, store, tracing.RuntimeTelemetry{})
	if err != nil {
		_ = runner.Close()
		return nil, nil, err
	}
	return exec, runner, nil
}

func validateSessionID(string) error { return nil }
