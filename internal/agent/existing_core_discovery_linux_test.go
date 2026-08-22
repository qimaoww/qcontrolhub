//go:build linux

package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qimaoww/qcontrolhub/internal/core"
)

const existingDiscoveryCoreHelperName = "existing-discovery-core"

var existingDiscoveryCoreHelper []byte

func TestMain(tests *testing.M) {
	helper, err := buildExistingDiscoveryCoreHelper()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	existingDiscoveryCoreHelper = helper
	os.Exit(tests.Run())
}

func buildExistingDiscoveryCoreHelper() ([]byte, error) {
	directory, err := os.MkdirTemp("", ".qcontrolhub-existing-discovery-core-")
	if err != nil {
		return nil, fmt.Errorf("create discovery core helper directory: %w", err)
	}
	defer os.RemoveAll(directory)
	sourcePath := filepath.Join(directory, "main.go")
	const source = `package main

import (
	"encoding/json"
	"os"
	"path/filepath"
)

func main() {
	arguments := os.Args[1:]
	if len(arguments) != 3 && len(arguments) != 5 {
		os.Exit(2)
	}
	if arguments[0] != "check" || arguments[1] != "-c" || !filepath.IsAbs(arguments[2]) {
		os.Exit(2)
	}
	contents, err := os.ReadFile(arguments[2])
	if err != nil || !json.Valid(contents) {
		os.Exit(1)
	}
	if len(arguments) == 5 {
		if arguments[3] != "-C" || !filepath.IsAbs(arguments[4]) {
			os.Exit(2)
		}
		info, err := os.Stat(arguments[4])
		if err != nil || !info.IsDir() {
			os.Exit(1)
		}
	}
}
`
	if err := os.WriteFile(sourcePath, []byte(source), 0o600); err != nil {
		return nil, fmt.Errorf("write discovery core helper source: %w", err)
	}
	binaryPath := filepath.Join(directory, existingDiscoveryCoreHelperName)
	command := exec.Command("go", "build", "-buildvcs=false", "-trimpath", "-o", binaryPath, sourcePath)
	output, err := command.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("build discovery core helper: %w: %s", err, strings.TrimSpace(string(output)))
	}
	helper, err := os.ReadFile(binaryPath)
	if err != nil {
		return nil, fmt.Errorf("read discovery core helper: %w", err)
	}
	if len(helper) < 4 || string(helper[:4]) != "\x7fELF" {
		return nil, errors.New("discovery core helper is not an ELF executable")
	}
	return helper, nil
}

func TestExistingCoreDiscoveryFindsAndRefreshesAfterAgentRestart(t *testing.T) {
	fixture := newExistingCoreDiscoveryFixture(t)
	specs, issues, err := RefreshExistingCoreDiscovery(
		context.Background(), fixture.discoveryStatePath, fixture.markerPrefix,
		fixture.managedSpecs, nil,
	)
	if err != nil {
		t.Fatalf("initial discovery after upgraded Agent restart: %v", err)
	}
	assertDiscoveredSingBoxSpec(t, specs[core.EngineSingBox], fixture.realBinary, fixture.serviceBinary, fixture.configPath, fixture.configDirectory)
	if len(issues) != 0 {
		t.Fatalf("initial discovery issues = %+v", issues)
	}
	info, err := os.Stat(fixture.discoveryStatePath)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("discovery state mode = %v, %v", info, err)
	}

	refreshedDirectory := filepath.Join(fixture.root, "refreshed-conf.d")
	if err := os.Mkdir(refreshedDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(refreshedDirectory, "20-route.json"), []byte(`{"route":{"final":"direct"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	fixture.writeExecStart(t, "sing-box.service", systemdExecStart(
		fixture.serviceBinary,
		fixture.serviceBinary+" run -c "+fixture.configPath+" -C "+refreshedDirectory,
	))
	specs, issues, err = RefreshExistingCoreDiscovery(
		context.Background(), fixture.discoveryStatePath, fixture.markerPrefix,
		fixture.managedSpecs, nil,
	)
	if err != nil {
		t.Fatalf("refresh discovery after restart: %v", err)
	}
	assertDiscoveredSingBoxSpec(t, specs[core.EngineSingBox], fixture.realBinary, fixture.serviceBinary, fixture.configPath, refreshedDirectory)
	if len(issues) != 0 {
		t.Fatalf("refreshed discovery issues = %+v", issues)
	}
	state, err := loadExistingCoreDiscoveryState(fixture.discoveryStatePath)
	if err != nil {
		t.Fatalf("reload refreshed discovery state: %v", err)
	}
	if got := state.Specs[core.EngineSingBox].ConfigDirectory; got != refreshedDirectory {
		t.Fatalf("persisted refreshed config directory = %q", got)
	}
}

func TestExistingCoreDiscoveryClearsStateWhenNoCandidateRemains(t *testing.T) {
	fixture := newExistingCoreDiscoveryFixture(t)
	if _, _, err := RefreshExistingCoreDiscovery(context.Background(), fixture.discoveryStatePath, fixture.markerPrefix, fixture.managedSpecs, nil); err != nil {
		t.Fatal(err)
	}
	fixture.writeStatus(t, "sing-box.service", "inactive")
	specs, issues, err := RefreshExistingCoreDiscovery(context.Background(), fixture.discoveryStatePath, fixture.markerPrefix, fixture.managedSpecs, nil)
	if err != nil {
		t.Fatalf("clear absent discovery: %v", err)
	}
	if len(specs) != 0 || len(issues) != 0 {
		t.Fatalf("absent discovery = specs %+v issues %+v", specs, issues)
	}
	state, err := loadExistingCoreDiscoveryState(fixture.discoveryStatePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Specs) != 0 || len(state.Issues) != 0 {
		t.Fatalf("stale persisted discovery = %+v", state)
	}
}

func TestExistingCoreDiscoveryKeepsMappingForInterruptedMigrationRecovery(t *testing.T) {
	fixture := newExistingCoreDiscoveryFixture(t)
	specs, _, err := RefreshExistingCoreDiscovery(context.Background(), fixture.discoveryStatePath, fixture.markerPrefix, fixture.managedSpecs, nil)
	if err != nil {
		t.Fatal(err)
	}
	spec := specs[core.EngineSingBox]
	if err := writeCoreMigrationMarker(
		fixture.markerPrefix, core.EngineSingBox, coreMigrationInProgress,
		coreMigrationConfigDigest(`{"inbounds":[]}`), coreMigrationSourceDigest(spec), "enabled", "disabled",
	); err != nil {
		t.Fatal(err)
	}
	fixture.writeStatus(t, "sing-box.service", "inactive")
	specs, issues, err := RefreshExistingCoreDiscovery(context.Background(), fixture.discoveryStatePath, fixture.markerPrefix, fixture.managedSpecs, nil)
	if err != nil {
		t.Fatalf("retain interrupted discovery: %v", err)
	}
	if specs[core.EngineSingBox] != spec || len(issues) != 0 {
		t.Fatalf("interrupted discovery = specs %+v issues %+v", specs, issues)
	}
}

func TestExistingCoreDiscoveryReportsAmbiguousAndUnsupportedServices(t *testing.T) {
	t.Run("ambiguous", func(t *testing.T) {
		fixture := newExistingCoreDiscoveryFixture(t)
		fixture.writeStatus(t, "singbox.service", "active")
		specs, issues, err := RefreshExistingCoreDiscovery(context.Background(), fixture.discoveryStatePath, fixture.markerPrefix, fixture.managedSpecs, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(specs) != 0 || !strings.Contains(issues[core.EngineSingBox], "多个活动") {
			t.Fatalf("ambiguous discovery = specs %+v issues %+v", specs, issues)
		}
	})

	t.Run("unsupported wrapper", func(t *testing.T) {
		fixture := newExistingCoreDiscoveryFixture(t)
		wrapper := filepath.Join(fixture.root, "complex-wrapper")
		contents := fmt.Sprintf("#!/bin/sh\nset -eu\nexport FOO=bar\nexec %s \"$@\"\n# extra\n", fixture.realBinary)
		if err := os.WriteFile(wrapper, []byte(contents), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(fixture.serviceBinary); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(wrapper, fixture.serviceBinary); err != nil {
			t.Fatal(err)
		}
		specs, issues, err := RefreshExistingCoreDiscovery(context.Background(), fixture.discoveryStatePath, fixture.markerPrefix, fixture.managedSpecs, nil)
		if err != nil {
			t.Fatal(err)
		}
		reason := issues[core.EngineSingBox]
		if len(specs) != 0 || !strings.Contains(reason, "wrapper 不在安全支持范围") {
			t.Fatalf("unsupported-wrapper discovery = specs %+v reason %q", specs, reason)
		}
		state, err := loadExistingCoreDiscoveryState(fixture.discoveryStatePath)
		if err != nil || state.Issues[core.EngineSingBox] != reason {
			t.Fatalf("persisted unsupported-wrapper issue = %+v, %v", state.Issues, err)
		}
		runtime := (&Executor{
			Specs:                   fixture.managedSpecs,
			ExistingDiscoveryIssues: map[core.Engine]string{core.EngineSingBox: reason},
		}).Runtime(context.Background())[core.EngineSingBox]
		if runtime.Installed || runtime.ServiceStatus != "active" || runtime.ExistingConfigUnsupportedReason != reason || runtime.ExistingConfigAvailable {
			t.Fatalf("unsupported-wrapper runtime = %+v", runtime)
		}
	})

	t.Run("custom managed unit", func(t *testing.T) {
		fixture := newExistingCoreDiscoveryFixture(t)
		unitPath := filepath.Join(existingDiscoveryManagedUnitRoot, "qagent-sing-box.service")
		if err := os.WriteFile(unitPath, []byte("[Unit]\nDescription=administrator unit\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		specs, issues, err := RefreshExistingCoreDiscovery(context.Background(), fixture.discoveryStatePath, fixture.markerPrefix, fixture.managedSpecs, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(specs) != 0 || !strings.Contains(issues[core.EngineSingBox], "QAgent 专用服务") {
			t.Fatalf("custom managed unit discovery = specs %+v issues %+v", specs, issues)
		}
	})

	t.Run("active managed unit", func(t *testing.T) {
		fixture := newExistingCoreDiscoveryFixture(t)
		fixture.writeStatus(t, "qagent-sing-box.service", "active")
		specs, issues, err := RefreshExistingCoreDiscovery(context.Background(), fixture.discoveryStatePath, fixture.markerPrefix, fixture.managedSpecs, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(specs) != 0 || !strings.Contains(issues[core.EngineSingBox], "QAgent 专用服务") {
			t.Fatalf("active managed unit discovery = specs %+v issues %+v", specs, issues)
		}
	})
}

func TestExistingCoreDiscoveryManualMappingWinsAndStatePermissionsFailClosed(t *testing.T) {
	fixture := newExistingCoreDiscoveryFixture(t)
	manual := EngineSpec{Binary: "/manual/sing-box", ConfigPath: "/manual/config.json", Service: "sing-box.service"}
	specs, issues, err := RefreshExistingCoreDiscovery(
		context.Background(), fixture.discoveryStatePath, fixture.markerPrefix,
		fixture.managedSpecs, map[core.Engine]EngineSpec{core.EngineSingBox: manual},
	)
	if err != nil {
		t.Fatal(err)
	}
	if specs[core.EngineSingBox] != manual || len(issues) != 0 {
		t.Fatalf("manual precedence = specs %+v issues %+v", specs, issues)
	}
	state, err := loadExistingCoreDiscoveryState(fixture.discoveryStatePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Specs) != 0 || len(state.Issues) != 0 {
		t.Fatalf("manual mapping was persisted as automatic state: %+v", state)
	}
	if err := os.Chmod(fixture.discoveryStatePath, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := RefreshExistingCoreDiscovery(context.Background(), fixture.discoveryStatePath, fixture.markerPrefix, fixture.managedSpecs, nil); err == nil || !strings.Contains(err.Error(), "protected regular file") {
		t.Fatalf("unsafe persisted discovery permissions error = %v", err)
	}
}

type existingCoreDiscoveryFixture struct {
	root               string
	stateDirectory     string
	discoveryStatePath string
	markerPrefix       string
	realBinary         string
	serviceBinary      string
	configPath         string
	configDirectory    string
	managedSpecs       map[core.Engine]EngineSpec
}

func newExistingCoreDiscoveryFixture(t *testing.T) existingCoreDiscoveryFixture {
	t.Helper()
	root := t.TempDir()
	stateDirectory := filepath.Join(root, "systemctl")
	configDirectory := filepath.Join(root, "conf.d")
	managedDirectory := filepath.Join(root, "managed")
	managedUnitDirectory := filepath.Join(root, "managed-units")
	agentStateDirectory := filepath.Join(root, "agent-state")
	for _, directory := range []string{stateDirectory, configDirectory, managedDirectory, managedUnitDirectory, agentStateDirectory} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	configPath := filepath.Join(root, "config.json")
	if err := os.WriteFile(configPath, []byte(`{"inbounds":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDirectory, "10-outbounds.json"), []byte(`{"outbounds":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if len(existingDiscoveryCoreHelper) == 0 {
		t.Fatal("discovery core helper was not built")
	}
	realBinary := filepath.Join(root, existingDiscoveryCoreHelperName)
	if err := os.WriteFile(realBinary, existingDiscoveryCoreHelper, 0o700); err != nil {
		t.Fatal(err)
	}
	serviceBinary := filepath.Join(root, "sing-box")
	if err := os.Symlink(realBinary, serviceBinary); err != nil {
		t.Fatal(err)
	}
	fakeSystemctl := filepath.Join(root, "fake-systemctl")
	script := "#!/bin/sh\nset -eu\nstate=" + shellQuote(stateDirectory) + "\ncommand=$1\nshift\nservice=$1\nshift\ncase \"$command\" in\n  is-active) value=$(cat \"$state/$service.active\"); printf '%s\\n' \"$value\"; [ \"$value\" = active ] ;;\n  show) property=ExecStart; for argument in \"$@\"; do case \"$argument\" in --property=*) property=${argument#--property=} ;; esac; done; case \"$property\" in ExecStart) cat \"$state/$service.exec-start\" ;; LoadState) cat \"$state/$service.load-state\" ;; FragmentPath) cat \"$state/$service.fragment-path\" ;; *) exit 1 ;; esac ;;\n  *) exit 1 ;;\nesac\n"
	if err := os.WriteFile(fakeSystemctl, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	previousSystemctl := systemctlPath
	previousCandidates := existingDiscoveryCandidates
	previousManagedUnitRoot := existingDiscoveryManagedUnitRoot
	systemctlPath = fakeSystemctl
	existingDiscoveryManagedUnitRoot = managedUnitDirectory
	existingDiscoveryCandidates = map[core.Engine]existingDiscoveryCandidateSet{
		core.EngineSingBox: {
			services:    []string{"sing-box.service", "singbox.service"},
			executables: []string{serviceBinary},
			configs:     []string{configPath},
		},
	}
	t.Cleanup(func() {
		systemctlPath = previousSystemctl
		existingDiscoveryCandidates = previousCandidates
		existingDiscoveryManagedUnitRoot = previousManagedUnitRoot
	})
	fixture := existingCoreDiscoveryFixture{
		root: root, stateDirectory: stateDirectory,
		discoveryStatePath: filepath.Join(agentStateDirectory, "agent-state.json.existing-cores"),
		markerPrefix:       filepath.Join(agentStateDirectory, "agent-state.json.core-migration"),
		realBinary:         realBinary, serviceBinary: serviceBinary,
		configPath: configPath, configDirectory: configDirectory,
		managedSpecs: map[core.Engine]EngineSpec{core.EngineSingBox: DefaultSpecs()[core.EngineSingBox]},
	}
	managedUnitPath := filepath.Join(managedUnitDirectory, "qagent-sing-box.service")
	if err := os.WriteFile(managedUnitPath, []byte("[Unit]\nDescription=sing-box core managed by QAgent\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDirectory, "qagent-sing-box.service.load-state"), []byte("loaded\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDirectory, "qagent-sing-box.service.fragment-path"), []byte(managedUnitPath+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fixture.writeStatus(t, "sing-box.service", "active")
	fixture.writeStatus(t, "singbox.service", "inactive")
	fixture.writeStatus(t, "qagent-sing-box.service", "inactive")
	fixture.writeExecStart(t, "sing-box.service", systemdExecStart(
		serviceBinary,
		serviceBinary+" run -c "+configPath+" -C "+configDirectory,
	))
	fixture.writeExecStart(t, "singbox.service", systemdExecStart(
		serviceBinary,
		serviceBinary+" run -c "+configPath+" -C "+configDirectory,
	))
	return fixture
}

func (fixture existingCoreDiscoveryFixture) writeStatus(t *testing.T, service, status string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(fixture.stateDirectory, service+".active"), []byte(status+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (fixture existingCoreDiscoveryFixture) writeExecStart(t *testing.T, service, value string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(fixture.stateDirectory, service+".exec-start"), []byte(value+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertDiscoveredSingBoxSpec(t *testing.T, spec EngineSpec, realBinary, serviceBinary, configPath, configDirectory string) {
	t.Helper()
	if spec.Binary != realBinary || spec.ServiceBinary != serviceBinary || spec.ConfigPath != configPath ||
		spec.ConfigDirectory != configDirectory || spec.Service != "sing-box.service" {
		t.Fatalf("discovered sing-box spec = %+v", spec)
	}
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}
