// Command kagent-dsh runs the DSH Harness runtime adapter: it serves kagent's
// private A2A (gRPC) endpoint on :80 (and /readyz on :8081) and bridges each
// turn to the local DSH web profile over HTTP.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	a2atype "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/kagent-dev/kagent/go/adk/pkg/app"
	"github.com/kagent-dev/kagent/go/adk/pkg/bootstrap"
	"github.com/kagent-dev/kagent/go/harness/dsh/executor"
	"github.com/kagent-dev/kagent/go/pkg/logging"
	"github.com/kagent-dev/kagent/go/pkg/tracing"
)

const privatePort = "80"

// mark 把关键步骤写到 durable /data，crash 后可从 node 侧读取。
func mark(s string) {
	for _, p := range []string{"/data/dsh-harness.log", "/tmp/dsh-harness.log"} {
		f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			continue
		}
		_, _ = f.WriteString(s + "\n")
		_ = f.Close()
	}
}

// writableDir 选一个真正可写的目录：golden 阶段 /data 可能是只读的。
func writableDir(candidates ...string) string {
	for _, d := range candidates {
		if err := os.MkdirAll(d, 0o700); err != nil {
			continue
		}
		f := filepath.Join(d, ".write-probe")
		if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
			continue
		}
		_ = os.Remove(f)
		return d
	}
	return candidates[len(candidates)-1]
}

func main() {
	mark("start")
	defer func() {
		if r := recover(); r != nil {
			mark(fmt.Sprintf("PANIC: %v", r))
			fmt.Fprintln(os.Stderr, "PANIC:", r)
			os.Exit(2)
		}
	}()
	if err := run(); err != nil {
		mark("fatal: " + err.Error())
		fmt.Fprintln(os.Stderr, "kagent-dsh stopped:", err)
		os.Exit(1)
	}
}

func run() error {
	ctx := context.Background()
	getenv := os.Getenv
	dataDir := writableDir("/data", "/tmp/dsh-data")
	mark("dataDir=" + dataDir)

	if getenv("DSH_DEV_CONFIG") != "" {
		// 本地/调试：跳过控制面 bootstrap，用最小配置直接起服务
		return serve(ctx, getenv, dataDir,
			[]byte(`{"control_plane_url":"http://127.0.0.1:8083","max_content_length":1048576}`),
			[]byte(`{"name":"dsh-dev","description":"dsh dev","version":"0.1.0"}`))
	}

	bundle, err := bootstrap.FromEnvironment(ctx, getenv)
	if err != nil {
		// /data 不可写时（golden 阶段常见）退回 tmpfs，避免直接退出
		mark("bootstrap via /data failed: " + err.Error())
		if mkErr := os.MkdirAll("/tmp/dsh-bootstrap", 0o700); mkErr == nil {
			if alt, altErr := bootstrap.Load(ctx, getenv("KAGENT_BOOTSTRAP_JSON"), "/tmp/dsh-bootstrap", getenv("SSL_CERT_FILE")); altErr == nil {
				return serve(ctx, getenv, "/tmp/dsh", alt.ConfigJson, alt.AgentCardJson)
			} else {
				return fmt.Errorf("%v; fallback: %w", err, altErr)
			}
		}
		return err
	}
	return serve(ctx, getenv, dataDir, bundle.ConfigJson, bundle.AgentCardJson)
}

func serve(ctx context.Context, getenv func(string) string, dataDir string, cfgJSON, cardJSON []byte) error {
	var card a2atype.AgentCard
	if err := json.Unmarshal(cardJSON, &card); err != nil {
		return fmt.Errorf("decode agent card: %w", err)
	}
	if strings.TrimSpace(card.Name) == "" {
		return fmt.Errorf("agent card name is required")
	}
	mark("card=" + card.Name)

	var runtimeCfg struct {
		ControlPlaneURL  string `json:"control_plane_url"`
		MaxContentLength *int64 `json:"max_content_length"`
	}
	if len(cfgJSON) > 0 {
		if err := json.Unmarshal(cfgJSON, &runtimeCfg); err != nil {
			return fmt.Errorf("decode runtime config: %w", err)
		}
	}
	if runtimeCfg.ControlPlaneURL == "" {
		var desc struct {
			ControlPlaneURL string `json:"control_plane_url"`
		}
		if err := json.Unmarshal([]byte(getenv("KAGENT_BOOTSTRAP_JSON")), &desc); err == nil {
			runtimeCfg.ControlPlaneURL = desc.ControlPlaneURL
		}
	}
	if runtimeCfg.MaxContentLength == nil {
		def := int64(1 << 20)
		runtimeCfg.MaxContentLength = &def
	}

	logger, err := logging.New(os.Stderr, "info")
	if err != nil {
		return err
	}
	ctx = logging.IntoContext(ctx, logger)

	endpoint := getenv("DSH_ENDPOINT")
	if endpoint == "" {
		endpoint = "http://127.0.0.1:18790"
	}
	mark("executor.New...")
	exec, closer, err := executor.New(ctx, executor.Config{
		DataDir: dataDir, DSHEndpoint: endpoint, AgentName: card.Name,
	})
	if err != nil {
		return err
	}
	defer closer.Close()
	mark("executor ok")

	port := getenv("DSH_PORT")
	if port == "" {
		port = privatePort
	}
	application, err := app.New(app.AppConfig{
		AgentCard:        card,
		Port:             port,
		AppName:          card.Name,
		ControlPlaneURL:  runtimeCfg.ControlPlaneURL,
		MaxContentLength: runtimeCfg.MaxContentLength,
		Logger:           logging.FromContext(ctx),
		Telemetry:        tracing.RuntimeTelemetry{},
		Flush:            func(context.Context) error { return nil },
	}, exec)
	if err != nil {
		return fmt.Errorf("construct private A2A app: %w", err)
	}
	mark("app created, port=" + port)
	return application.Run()
}
