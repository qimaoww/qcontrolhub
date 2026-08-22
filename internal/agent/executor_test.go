package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qimaoww/qcontrolhub/internal/core"
)

func TestProductionAgentUnitAllowsOnlyRequiredCapabilities(t *testing.T) {
	t.Parallel()
	contents, err := os.ReadFile("../../deploy/systemd/qagent.service")
	if err != nil {
		t.Fatal(err)
	}
	unit := string(contents)
	if !strings.Contains(unit, "CapabilityBoundingSet=CAP_CHOWN CAP_NET_ADMIN") || !strings.Contains(unit, "AmbientCapabilities=CAP_CHOWN CAP_NET_ADMIN") {
		t.Fatal("production Agent unit does not retain the metadata and traffic-accounting capabilities")
	}
	for _, forbidden := range []string{"CAP_SYS_ADMIN", "CAP_SYS_PTRACE", "CAP_DAC_OVERRIDE", "CAP_NET_RAW"} {
		if strings.Contains(unit, forbidden) {
			t.Fatalf("production Agent unit grants unnecessary capability %s", forbidden)
		}
	}
	if !strings.Contains(unit, "ReadWritePaths=-/usr/local/lib/qagent/cores") {
		t.Fatal("production Agent unit does not allow updates in the private core directory")
	}
	if strings.Contains(unit, "ReadWritePaths=-/usr/local/bin\n") {
		t.Fatal("production Agent unit can modify administrator-managed /usr/local/bin programs")
	}
	if !strings.Contains(unit, "ProtectProc=invisible") {
		t.Fatal("production Agent unit must hide process details")
	}
	if strings.Contains(unit, "ProcSubset=pid") {
		t.Fatal("production Agent unit hides /proc/stat, /proc/meminfo, and /proc/net metrics")
	}
	if !strings.Contains(unit, "RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6 AF_NETLINK") {
		t.Fatal("production Agent unit does not allow the nftables netlink transport")
	}
}

func TestDefaultSpecsUsePrivateQAgentNamespace(t *testing.T) {
	t.Parallel()
	want := map[core.Engine]EngineSpec{
		core.EngineMihomo:          {Binary: "/usr/local/lib/qagent/cores/mihomo", ConfigPath: "/etc/qagent/mihomo/config.yaml", Service: "qagent-mihomo.service"},
		core.EngineXray:            {Binary: "/usr/local/lib/qagent/cores/xray", ConfigPath: "/etc/qagent/xray/config.json", Service: "qagent-xray.service"},
		core.EngineSingBox:         {Binary: "/usr/local/lib/qagent/cores/sing-box", ConfigPath: "/etc/qagent/sing-box/config.json", Service: "qagent-sing-box.service"},
		core.EngineShadowsocksRust: {Binary: "/usr/local/lib/qagent/cores/ssserver", ConfigPath: "/etc/qagent/shadowsocks-rust/config.json", Service: "qagent-shadowsocks-rust.service"},
	}
	for engine, expected := range want {
		actual := DefaultSpecs()[engine]
		if actual != expected {
			t.Errorf("DefaultSpecs()[%s] = %+v, want %+v", engine, actual, expected)
		}
		unitPath := filepath.Join("../../deploy/systemd", expected.Service)
		contents, err := os.ReadFile(unitPath)
		if err != nil {
			t.Errorf("read %s: %v", unitPath, err)
			continue
		}
		unit := string(contents)
		for _, required := range []string{
			"ConditionFileIsExecutable=" + expected.Binary,
			"ConditionPathExists=" + expected.ConfigPath,
			"ExecStart=" + expected.Binary,
			"CapabilityBoundingSet=CAP_NET_BIND_SERVICE",
			"AmbientCapabilities=CAP_NET_BIND_SERVICE",
			"LogNamespace=qagent-cores",
			"StandardOutput=journal",
			"StandardError=journal",
		} {
			if !strings.Contains(unit, required) {
				t.Errorf("%s is missing %q", expected.Service, required)
			}
		}
		for _, forbidden := range []string{"CAP_NET_ADMIN", "CAP_NET_RAW", "CAP_SYS_ADMIN", "CAP_DAC_OVERRIDE"} {
			if strings.Contains(unit, forbidden) {
				t.Errorf("%s grants unnecessary capability %s", expected.Service, forbidden)
			}
		}
	}
	journalConfig, err := os.ReadFile("../../deploy/systemd/qagent-core-journal.conf")
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"Storage=volatile", "RuntimeMaxUse=16M", "MaxRetentionSec=15min"} {
		if !strings.Contains(string(journalConfig), required) {
			t.Errorf("core journal configuration is missing %q", required)
		}
	}
}

func TestCoreBootstrapDoesNotTouchLegacyInstallations(t *testing.T) {
	t.Parallel()
	contents, err := os.ReadFile("../../deploy/bootstrap-core-services.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := string(contents)
	for _, required := range []string{"/usr/local/lib/qagent/cores", "/etc/systemd/system/qagent-$engine.service", "install_managed_unit", "preserved non-QAgent unit"} {
		if !strings.Contains(script, required) {
			t.Errorf("core bootstrap is missing private namespace %q", required)
		}
	}
	for _, forbidden := range []string{"/usr/local/bin/mihomo", "/usr/local/bin/xray", "managed by QControlHub", "legacy_service"} {
		if strings.Contains(script, forbidden) {
			t.Errorf("core bootstrap unexpectedly touches legacy installation %q", forbidden)
		}
	}
}

func TestOneClickInstallerMapsOnlyValidatedExistingCorePaths(t *testing.T) {
	t.Parallel()
	contents, err := os.ReadFile("../../deploy/remote/install-agent.sh")
	if err != nil {
		t.Fatal(err)
	}
	mapping, err := os.ReadFile("../../deploy/existing-core-mapping.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := string(contents) + string(mapping)
	for _, required := range []string{
		"discover_existing_xray", "discover_existing_singbox", "systemctl is-active --quiet",
		"existing-core-mapping.sh", "QCH_EXISTING_XRAY_CONFIG", "QCH_EXISTING_SING_BOX_CONFIG",
	} {
		if !strings.Contains(script, required) {
			t.Errorf("one-click installer is missing inherited-core guard %q", required)
		}
	}
	for _, forbidden := range []string{"systemctl stop xray.service", "systemctl stop sing-box.service", "QCH_INHERIT_CONFIGS", "validate-inherited"} {
		if strings.Contains(script, forbidden) {
			t.Errorf("one-click installer stops an existing service via %q", forbidden)
		}
	}
}

func TestPersistentCoreLogOutputsAreRejected(t *testing.T) {
	t.Parallel()
	fixtures := []struct {
		engine  core.Engine
		content string
	}{
		{core.EngineXray, `{"log":{"loglevel":"info","access":"/var/log/xray.log"}}`},
		{core.EngineSingBox, `{"log":{"level":"info","output":"/var/log/sing-box.log"}}`},
		{core.EngineSingBox, `{"log":{"level":"info","output":"none"}}`},
	}
	for _, fixture := range fixtures {
		if err := validateNoPersistentCoreLogs(fixture.engine, fixture.content); err == nil {
			t.Errorf("%s persistent log output was accepted", fixture.engine)
		}
	}
	if err := validateNoPersistentCoreLogs(core.EngineXray, `{"log":{"loglevel":"info"}}`); err != nil {
		t.Fatalf("stdout Xray logging was rejected: %v", err)
	}
	if err := validateNoPersistentCoreLogs(core.EngineXray, `{"log":{"loglevel":"info","access":"none"}}`); err != nil {
		t.Fatalf("disabled Xray file logging was rejected: %v", err)
	}
}

func TestServiceVerificationRejectsTransientActiveState(t *testing.T) {
	t.Parallel()
	statuses := []string{"active", "active", "failed"}
	index := 0
	status, err := waitForServiceState(context.Background(), "active", 20*time.Millisecond, time.Millisecond, func(context.Context) (string, error) {
		if index < len(statuses)-1 {
			value := statuses[index]
			index++
			return value, nil
		}
		return statuses[len(statuses)-1], nil
	})
	if err != nil || status != "failed" {
		t.Fatalf("transient active verification = %q, %v; want failed", status, err)
	}
}

func TestServiceVerificationRequiresStableActiveState(t *testing.T) {
	t.Parallel()
	status, err := waitForServiceState(context.Background(), "active", 5*time.Millisecond, time.Millisecond, func(context.Context) (string, error) {
		return "active", nil
	})
	if err != nil || status != "active" {
		t.Fatalf("stable active verification = %q, %v", status, err)
	}
}

func TestExecutorRejectsUnsafeTasksAndPaths(t *testing.T) {
	t.Parallel()
	executor := &Executor{
		Specs: map[core.Engine]EngineSpec{
			core.EngineMihomo: {
				Binary:     "relative-binary",
				ConfigPath: filepath.Join(t.TempDir(), "config.yaml"),
				Service:    "qagent-mihomo.service",
			},
		},
	}
	rejected := []core.Task{
		{Action: core.Action("restart; rm -rf /"), Engine: core.EngineMihomo},
		{Action: core.ActionStatus, Engine: core.Engine("mihomo;evil")},
		{Action: core.ActionStatus, Engine: core.EngineXray},
		{Action: core.ActionInstall, Engine: core.EngineMihomo, CoreVersion: "https://evil.example/core"},
	}
	for _, task := range rejected {
		if _, err := executor.Execute(context.Background(), task); err == nil {
			t.Fatalf("Execute() accepted non-whitelisted task: action=%q engine=%q", task.Action, task.Engine)
		}
	}
}
