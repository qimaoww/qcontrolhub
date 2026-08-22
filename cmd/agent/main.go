//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/qimaoww/qcontrolhub/internal/agent"
	"github.com/qimaoww/qcontrolhub/internal/core"
)

var version = "dev"

func main() {
	hostname, _ := os.Hostname()
	specs := agent.DefaultSpecs()
	specs[core.EngineMihomo] = overrideSpec(specs[core.EngineMihomo], "MIHOMO")
	specs[core.EngineXray] = overrideSpec(specs[core.EngineXray], "XRAY")
	specs[core.EngineSingBox] = overrideSpec(specs[core.EngineSingBox], "SING_BOX")
	specs[core.EngineShadowsocksRust] = overrideSpec(specs[core.EngineShadowsocksRust], "SS_RUST")
	if len(os.Args) > 1 {
		if err := runUtilityCommand(specs, os.Args[1:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	capabilities := parseEngines(env("QCH_AGENT_ENGINES", "mihomo,xray,sing-box,ss-rust"))
	enabledSpecs := make(map[core.Engine]agent.EngineSpec, len(capabilities))
	for _, engine := range capabilities {
		enabledSpecs[engine] = specs[engine]
	}
	statePath := env("QCH_AGENT_STATE", "./data/agent-state.json")
	manualExistingSpecs := make(map[core.Engine]agent.EngineSpec)
	if spec, ok := existingSpec("XRAY"); ok {
		manualExistingSpecs[core.EngineXray] = spec
	}
	if spec, ok := existingSpec("SING_BOX"); ok {
		manualExistingSpecs[core.EngineSingBox] = spec
	}
	discoveryContext, discoveryCancel := context.WithTimeout(context.Background(), 30*time.Second)
	existingSpecs, discoveryIssues, err := agent.RefreshExistingCoreDiscovery(
		discoveryContext,
		statePath+".existing-cores",
		statePath+".core-migration",
		enabledSpecs,
		manualExistingSpecs,
	)
	discoveryCancel()
	if err != nil {
		slog.Error("existing core discovery failed", "error", err)
		os.Exit(1)
	}

	executor := &agent.Executor{
		Specs: enabledSpecs, ExistingSpecs: existingSpecs,
		ExistingDiscoveryIssues: discoveryIssues,
		MigrationMarkerPrefix:   statePath + ".core-migration",
	}
	client, err := agent.NewClient(agent.ClientConfig{
		ServerURL:         env("QCH_SERVER_URL", "http://localhost:8080"),
		EnrollmentToken:   os.Getenv("QCH_ENROLLMENT_TOKEN"),
		StatePath:         statePath,
		Name:              env("QCH_AGENT_NAME", hostname),
		Version:           version,
		Labels:            parseLabels(os.Getenv("QCH_AGENT_LABELS")),
		Capabilities:      capabilities,
		HeartbeatEvery:    envDuration("QCH_HEARTBEAT_INTERVAL", 15*time.Second),
		MetricsEvery:      envDuration("QCH_METRICS_INTERVAL", time.Second),
		AllowHTTP:         envBool("QCH_ALLOW_HTTP", false),
		AllowInsecureLive: envBool("QCH_ALLOW_INSECURE_LIVE", false),
		TLSCAFile:         strings.TrimSpace(os.Getenv("QCH_TLS_CA_FILE")),
	}, executor)
	if err != nil {
		slog.Error("invalid agent configuration", "error", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	slog.Info("QControlHub agent starting", "version", version)
	if err := client.Run(ctx); err != nil {
		if errors.Is(err, agent.ErrIdentityRejected) {
			slog.Error("agent identity is no longer valid; remove the state file and enroll again", "error", err)
			return
		}
		slog.Error("agent stopped", "error", err)
		os.Exit(1)
	}
}

func existingSpec(prefix string) (agent.EngineSpec, bool) {
	binary := strings.TrimSpace(os.Getenv("QCH_EXISTING_" + prefix + "_BINARY"))
	configPath := strings.TrimSpace(os.Getenv("QCH_EXISTING_" + prefix + "_CONFIG"))
	configDirectory := strings.TrimSpace(os.Getenv("QCH_EXISTING_" + prefix + "_CONFIG_DIRECTORY"))
	serviceBinary := strings.TrimSpace(os.Getenv("QCH_EXISTING_" + prefix + "_SERVICE_BINARY"))
	service := strings.TrimSpace(os.Getenv("QCH_EXISTING_" + prefix + "_SERVICE"))
	configured := binary != "" || configPath != "" || configDirectory != "" || serviceBinary != "" || service != ""
	if !configured {
		return agent.EngineSpec{}, false
	}
	if binary == "" || configPath == "" || service == "" {
		slog.Error("existing core mapping requires binary, config, and service", "engine", prefix)
		os.Exit(1)
	}
	return agent.EngineSpec{
		Binary: binary, ConfigPath: configPath, ConfigDirectory: configDirectory,
		ServiceBinary: serviceBinary, Service: service,
	}, true
}

func runUtilityCommand(specs map[core.Engine]agent.EngineSpec, arguments []string) error {
	if len(arguments) != 2 || arguments[0] != "inspect-existing" {
		return errors.New("usage: qagent inspect-existing <xray|sing-box>")
	}
	engine, err := core.ParseEngine(arguments[1])
	if err != nil || (engine != core.EngineXray && engine != core.EngineSingBox) {
		return errors.New("inspect-existing supports only xray and sing-box")
	}
	existing := specs[engine]
	temporaryDirectory, err := os.MkdirTemp("", "qagent-inspect-existing-")
	if err != nil {
		return fmt.Errorf("create protected validation directory: %w", err)
	}
	defer os.RemoveAll(temporaryDirectory)
	managed := existing
	managed.ConfigPath = temporaryDirectory + "/config.json"
	executor := &agent.Executor{
		Specs:         map[core.Engine]agent.EngineSpec{engine: managed},
		ExistingSpecs: map[core.Engine]agent.EngineSpec{engine: existing},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := executor.Execute(ctx, core.Task{Action: core.ActionReadConfig, Engine: engine}); err != nil {
		return fmt.Errorf("existing %s configuration cannot be inspected safely: %w", engine, err)
	}
	return nil
}

func overrideSpec(spec agent.EngineSpec, prefix string) agent.EngineSpec {
	spec.Binary = env("QCH_"+prefix+"_BINARY", spec.Binary)
	spec.ConfigPath = env("QCH_"+prefix+"_CONFIG", spec.ConfigPath)
	spec.ConfigDirectory = strings.TrimSpace(os.Getenv("QCH_" + prefix + "_CONFIG_DIRECTORY"))
	spec.ServiceBinary = strings.TrimSpace(os.Getenv("QCH_" + prefix + "_SERVICE_BINARY"))
	spec.Service = env("QCH_"+prefix+"_SERVICE", spec.Service)
	return spec
}

func parseEngines(value string) []core.Engine {
	result := make([]core.Engine, 0, 4)
	seen := make(map[core.Engine]struct{})
	for _, item := range strings.Split(value, ",") {
		engine, err := core.ParseEngine(item)
		if err != nil {
			slog.Error("invalid QCH_AGENT_ENGINES", "value", item, "error", err)
			os.Exit(1)
		}
		if _, exists := seen[engine]; !exists {
			result = append(result, engine)
			seen[engine] = struct{}{}
		}
	}
	return result
}

func parseLabels(value string) map[string]string {
	result := make(map[string]string)
	for _, item := range strings.Split(value, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		key, label, found := strings.Cut(item, "=")
		if !found {
			result[key] = "true"
		} else {
			result[strings.TrimSpace(key)] = strings.TrimSpace(label)
		}
	}
	return result
}

func env(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func envBool(key string, fallback bool) bool {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		slog.Error("invalid boolean environment variable", "key", key, "value", value)
		os.Exit(1)
	}
	return parsed
}

func envDuration(key string, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		slog.Error("invalid duration environment variable", "key", key, "value", value)
		os.Exit(1)
	}
	return parsed
}
