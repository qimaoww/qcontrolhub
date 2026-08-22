//go:build linux

package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qimaoww/qcontrolhub/internal/core"
)

func TestExistingCoreMigrationSwitchesServicesAndPersistsCompletion(t *testing.T) {
	requireAgentRoot(t)
	fixture := newExistingCoreMigrationFixture(t, false)
	output, err := fixture.executor.Execute(context.Background(), core.Task{
		Action: core.ActionImportExisting, Engine: core.EngineXray, ConfigContent: fixture.importedConfig,
	})
	if err != nil {
		t.Fatalf("import existing config: %v\n%s", err, output)
	}
	if !strings.Contains(output, "stopped and disabled xray.service") {
		t.Fatalf("migration output = %q", output)
	}
	fixture.assertServiceState(t, "xray.service", "inactive", "disabled")
	fixture.assertServiceState(t, "qagent-xray.service", "active", "enabled")
	assertFileContentAndMode(t, fixture.managed.ConfigPath, fixture.importedConfig, 0o600)
	if _, err := os.Stat(fixture.managed.Binary); err != nil {
		t.Fatalf("managed binary was not installed: %v", err)
	}
	if _, pending := fixture.executor.ExistingSpecs[core.EngineXray]; pending {
		t.Fatal("completed migration remained pending in memory")
	}
	output, err = fixture.executor.Execute(context.Background(), core.Task{
		Action: core.ActionImportExisting, Engine: core.EngineXray, ConfigContent: fixture.importedConfig,
	})
	if err != nil || !strings.Contains(output, "already completed") {
		t.Fatalf("idempotent migration retry = %q, %v", output, err)
	}
	if _, err := fixture.executor.Execute(context.Background(), core.Task{
		Action: core.ActionImportExisting, Engine: core.EngineXray, ConfigContent: `{"inbounds":[],"outbounds":[],"tag":"different"}`,
	}); err == nil || !strings.Contains(err.Error(), "no existing service") {
		t.Fatalf("different migration retry error = %v", err)
	}

	restarted := &Executor{
		ExistingSpecs:         map[core.Engine]EngineSpec{core.EngineXray: fixture.existing},
		MigrationMarkerPrefix: fixture.markerPrefix,
	}
	if err := restarted.LoadCoreMigrationState(); err != nil {
		t.Fatalf("reload migration marker: %v", err)
	}
	if _, pending := restarted.ExistingSpecs[core.EngineXray]; pending {
		t.Fatal("completed migration returned after Agent restart")
	}
	changedSource := fixture.existing
	changedSource.ConfigPath = filepath.Join(filepath.Dir(changedSource.ConfigPath), "replacement.json")
	remapped := &Executor{
		ExistingSpecs:         map[core.Engine]EngineSpec{core.EngineXray: changedSource},
		MigrationMarkerPrefix: fixture.markerPrefix,
	}
	if err := remapped.LoadCoreMigrationState(); err != nil {
		t.Fatalf("load stale migration marker: %v", err)
	}
	if _, pending := remapped.ExistingSpecs[core.EngineXray]; !pending {
		t.Fatal("a completed marker for another source suppressed a new mapping")
	}
}

func TestExistingSingBoxConfigDirectoryMigrationSucceeds(t *testing.T) {
	requireAgentRoot(t)
	fixture, content, _ := configureSingBoxDirectoryFixture(t, newExistingCoreMigrationFixture(t, false))
	forwarder := filepath.Join(filepath.Dir(fixture.existing.ConfigPath), "sing-box-forwarder")
	serviceBinary := filepath.Join(filepath.Dir(fixture.existing.ConfigPath), "sing-box-service")
	if err := os.WriteFile(forwarder, []byte("#!/bin/sh\nexec /usr/bin/true \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(forwarder, serviceBinary); err != nil {
		t.Fatal(err)
	}
	fixture.existing.Binary = "/usr/bin/true"
	fixture.existing.ServiceBinary = serviceBinary
	fixture.executor.ExistingSpecs[core.EngineSingBox] = fixture.existing
	writeMigrationExecStart(t, fixture.stateDirectory, fixture.existing.Service, systemdExecStart(
		serviceBinary, serviceBinary+" run -c "+fixture.existing.ConfigPath+" -C "+fixture.existing.ConfigDirectory,
	))
	output, err := fixture.executor.Execute(context.Background(), core.Task{
		Action: core.ActionImportExisting, Engine: core.EngineSingBox, ConfigContent: content,
	})
	if err != nil {
		t.Fatalf("migrate sing-box config directory: %v\n%s", err, output)
	}
	if !strings.Contains(output, "stopped and disabled sing-box.service") {
		t.Fatalf("migration output = %q", output)
	}
	fixture.assertServiceState(t, "sing-box.service", "inactive", "disabled")
	fixture.assertServiceState(t, "qagent-sing-box.service", "active", "enabled")
	assertFileContentAndMode(t, fixture.managed.ConfigPath, content, 0o600)
	if _, pending := fixture.executor.ExistingSpecs[core.EngineSingBox]; pending {
		t.Fatal("completed sing-box directory migration remained pending")
	}
}

func TestExistingSingBoxConfigDirectoryDriftRollsBackPreparation(t *testing.T) {
	requireAgentRoot(t)
	fixture, content, overlay := configureSingBoxDirectoryFixture(t, newExistingCoreMigrationFixture(t, false))
	counter := filepath.Join(fixture.stateDirectory, "sing-box-validation-count")
	script := fmt.Sprintf(`#!/bin/sh
set -eu
count=0
[ ! -f %q ] || count=$(cat %q)
count=$((count + 1))
printf '%%s\n' "$count" > %q
if [ "$count" -ge 4 ]; then
  printf '%%s\n' '{"outbounds":[{"tag":"changed"}]}' > %q
fi
exit 0
`, counter, counter, counter, overlay)
	if err := os.WriteFile(fixture.existing.Binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.executor.Execute(context.Background(), core.Task{
		Action: core.ActionImportExisting, Engine: core.EngineSingBox, ConfigContent: content,
	}); err == nil || !strings.Contains(err.Error(), "changed during migration preparation") {
		t.Fatalf("config-directory preparation drift error = %v", err)
	}
	fixture.assertServiceState(t, "sing-box.service", "active", "enabled")
	fixture.assertServiceState(t, "qagent-sing-box.service", "inactive", "disabled")
	assertFileContentAndMode(t, fixture.managed.ConfigPath, fixture.originalManagedConfig, 0o600)
	if _, err := os.Stat(fixture.managed.Binary); !os.IsNotExist(err) {
		t.Fatalf("drifted migration left managed binary: %v", err)
	}
	if _, err := os.Stat(coreMigrationMarkerPath(fixture.markerPrefix, core.EngineSingBox)); !os.IsNotExist(err) {
		t.Fatalf("drifted migration left marker: %v", err)
	}
}

func TestExistingSingBoxConfigDirectoryArgvDriftRollsBackPreparation(t *testing.T) {
	requireAgentRoot(t)
	fixture, content, _ := configureSingBoxDirectoryFixture(t, newExistingCoreMigrationFixture(t, false))
	replacementDirectory := fixture.existing.ConfigDirectory + "-replacement"
	drifted := systemdExecStart(fixture.existing.Binary,
		fixture.existing.Binary+" run -c "+fixture.existing.ConfigPath+" -C "+replacementDirectory)
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' %q > %q\nexit 0\n", drifted, filepath.Join(fixture.stateDirectory, fixture.existing.Service+".exec-start"))
	if err := os.WriteFile(fixture.existing.Binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.executor.Execute(context.Background(), core.Task{
		Action: core.ActionImportExisting, Engine: core.EngineSingBox, ConfigContent: content,
	}); err == nil || !strings.Contains(err.Error(), "ExecStart no longer matches") {
		t.Fatalf("config-directory argv drift error = %v", err)
	}
	fixture.assertServiceState(t, "sing-box.service", "active", "enabled")
	fixture.assertServiceState(t, "qagent-sing-box.service", "inactive", "disabled")
	assertFileContentAndMode(t, fixture.managed.ConfigPath, fixture.originalManagedConfig, 0o600)
	if _, err := os.Stat(fixture.managed.Binary); !os.IsNotExist(err) {
		t.Fatalf("argv drift left managed binary: %v", err)
	}
}

func TestExistingCoreMigrationRestoresOriginalServiceWhenNewServiceFails(t *testing.T) {
	requireAgentRoot(t)
	fixture := newExistingCoreMigrationFixture(t, true)
	output, err := fixture.executor.Execute(context.Background(), core.Task{
		Action: core.ActionImportExisting, Engine: core.EngineXray, ConfigContent: fixture.importedConfig,
	})
	if err == nil || !strings.Contains(output, "original configuration, binary, and service were restored") {
		t.Fatalf("failed migration = %q, %v", output, err)
	}
	fixture.assertServiceState(t, "xray.service", "active", "enabled")
	fixture.assertServiceState(t, "qagent-xray.service", "inactive", "disabled")
	assertFileContentAndMode(t, fixture.managed.ConfigPath, fixture.originalManagedConfig, 0o600)
	if _, err := os.Stat(fixture.managed.Binary); !os.IsNotExist(err) {
		t.Fatalf("failed migration left managed binary: %v", err)
	}
	if _, pending := fixture.executor.ExistingSpecs[core.EngineXray]; !pending {
		t.Fatal("failed migration cleared pending existing service")
	}
	if marked, err := coreMigrationMarked(fixture.markerPrefix, core.EngineXray); err != nil || marked {
		t.Fatalf("failed migration marker = %t, %v", marked, err)
	}
}

func TestExistingCoreMigrationRejectsActiveManagedServiceBeforeChanges(t *testing.T) {
	requireAgentRoot(t)
	fixture := newExistingCoreMigrationFixture(t, false)
	writeMigrationServiceState(t, fixture.stateDirectory, "qagent-xray.service", "active", "disabled")
	if _, err := fixture.executor.Execute(context.Background(), core.Task{
		Action: core.ActionImportExisting, Engine: core.EngineXray, ConfigContent: fixture.importedConfig,
	}); err == nil || !strings.Contains(err.Error(), `must remain inactive or failed`) || !strings.Contains(err.Error(), `status "active"`) {
		t.Fatalf("active managed service error = %v", err)
	}
	fixture.assertServiceState(t, "xray.service", "active", "enabled")
	fixture.assertServiceState(t, "qagent-xray.service", "active", "disabled")
	assertFileContentAndMode(t, fixture.managed.ConfigPath, fixture.originalManagedConfig, 0o600)
	if _, err := os.Stat(fixture.managed.Binary); !os.IsNotExist(err) {
		t.Fatalf("rejected migration installed managed binary: %v", err)
	}
	if _, err := os.Stat(coreMigrationMarkerPath(fixture.markerPrefix, core.EngineXray)); !os.IsNotExist(err) {
		t.Fatalf("rejected migration left marker: %v", err)
	}
}

func TestExistingCoreMigrationRejectsManagedTransientStatesBeforeChanges(t *testing.T) {
	requireAgentRoot(t)
	for _, status := range []string{"activating", "reloading", "deactivating"} {
		t.Run(status, func(t *testing.T) {
			fixture := newExistingCoreMigrationFixture(t, false)
			writeMigrationServiceState(t, fixture.stateDirectory, "qagent-xray.service", status, "disabled")
			if _, err := fixture.executor.Execute(context.Background(), core.Task{
				Action: core.ActionImportExisting, Engine: core.EngineXray, ConfigContent: fixture.importedConfig,
			}); err == nil {
				t.Fatalf("managed %s state was accepted", status)
			}
			fixture.assertServiceState(t, "xray.service", "active", "enabled")
			fixture.assertServiceState(t, "qagent-xray.service", status, "disabled")
			assertFileContentAndMode(t, fixture.managed.ConfigPath, fixture.originalManagedConfig, 0o600)
			if _, err := os.Stat(fixture.managed.Binary); !os.IsNotExist(err) {
				t.Fatalf("managed %s rejection installed binary: %v", status, err)
			}
			if _, err := os.Stat(coreMigrationMarkerPath(fixture.markerPrefix, core.EngineXray)); !os.IsNotExist(err) {
				t.Fatalf("managed %s rejection left marker: %v", status, err)
			}
		})
	}
}

func TestExistingCoreMigrationRejectsManagedServiceActivationDuringPreparation(t *testing.T) {
	requireAgentRoot(t)
	fixture := newExistingCoreMigrationFixture(t, false)
	counter := filepath.Join(fixture.stateDirectory, "managed-activation-validation-count")
	script := fmt.Sprintf(`#!/bin/sh
set -eu
count=0
[ ! -f %q ] || count=$(cat %q)
count=$((count + 1))
printf '%%s\n' "$count" > %q
if [ "$count" -ge 3 ]; then
  printf 'active\n' > %q
fi
exit 0
`, counter, counter, counter, filepath.Join(fixture.stateDirectory, "qagent-xray.service.active"))
	if err := os.WriteFile(fixture.existing.Binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.executor.Execute(context.Background(), core.Task{
		Action: core.ActionImportExisting, Engine: core.EngineXray, ConfigContent: fixture.importedConfig,
	}); err == nil || !strings.Contains(err.Error(), `must remain inactive or failed`) || !strings.Contains(err.Error(), `status "active"`) {
		t.Fatalf("managed preparation activation error = %v", err)
	}
	fixture.assertServiceState(t, "xray.service", "active", "enabled")
	fixture.assertServiceState(t, "qagent-xray.service", "active", "disabled")
	assertFileContentAndMode(t, fixture.managed.ConfigPath, fixture.originalManagedConfig, 0o600)
	if _, err := os.Stat(fixture.managed.Binary); !os.IsNotExist(err) {
		t.Fatalf("managed activation left prepared binary: %v", err)
	}
	if _, err := os.Stat(coreMigrationMarkerPath(fixture.markerPrefix, core.EngineXray)); !os.IsNotExist(err) {
		t.Fatalf("managed activation left marker: %v", err)
	}
}

func TestExistingCoreMigrationAcceptsFailedManagedService(t *testing.T) {
	requireAgentRoot(t)
	fixture := newExistingCoreMigrationFixture(t, false)
	writeMigrationServiceState(t, fixture.stateDirectory, "qagent-xray.service", "failed", "disabled")
	if _, err := fixture.executor.Execute(context.Background(), core.Task{
		Action: core.ActionImportExisting, Engine: core.EngineXray, ConfigContent: fixture.importedConfig,
	}); err != nil {
		t.Fatalf("migrate with failed managed service: %v", err)
	}
	fixture.assertServiceState(t, "xray.service", "inactive", "disabled")
	fixture.assertServiceState(t, "qagent-xray.service", "active", "enabled")
}

func TestExistingCoreMigrationRestoresDisabledEnableState(t *testing.T) {
	requireAgentRoot(t)
	fixture := newExistingCoreMigrationFixture(t, true)
	writeMigrationServiceState(t, fixture.stateDirectory, "xray.service", "active", "disabled")
	writeMigrationServiceState(t, fixture.stateDirectory, "qagent-xray.service", "inactive", "enabled")
	if _, err := fixture.executor.Execute(context.Background(), core.Task{
		Action: core.ActionImportExisting, Engine: core.EngineXray, ConfigContent: fixture.importedConfig,
	}); err == nil {
		t.Fatal("migration unexpectedly succeeded")
	}
	fixture.assertServiceState(t, "xray.service", "active", "disabled")
	fixture.assertServiceState(t, "qagent-xray.service", "inactive", "enabled")
}

func TestExistingCoreMigrationRestoresRuntimeEnableState(t *testing.T) {
	requireAgentRoot(t)
	fixture := newExistingCoreMigrationFixture(t, true)
	writeMigrationServiceState(t, fixture.stateDirectory, "xray.service", "active", "enabled-runtime")
	if _, err := fixture.executor.Execute(context.Background(), core.Task{
		Action: core.ActionImportExisting, Engine: core.EngineXray, ConfigContent: fixture.importedConfig,
	}); err == nil {
		t.Fatal("migration unexpectedly succeeded")
	}
	fixture.assertServiceState(t, "xray.service", "active", "enabled-runtime")
}

func TestExistingCoreMigrationDisablesRuntimeEnabledOriginalOnSuccess(t *testing.T) {
	requireAgentRoot(t)
	fixture := newExistingCoreMigrationFixture(t, false)
	writeMigrationServiceState(t, fixture.stateDirectory, "xray.service", "active", "enabled-runtime")
	if _, err := fixture.executor.Execute(context.Background(), core.Task{
		Action: core.ActionImportExisting, Engine: core.EngineXray, ConfigContent: fixture.importedConfig,
	}); err != nil {
		t.Fatalf("migrate runtime-enabled original: %v", err)
	}
	fixture.assertServiceState(t, "xray.service", "inactive", "disabled")
	fixture.assertServiceState(t, "qagent-xray.service", "active", "enabled")
	_, persistent, runtime, stateErr := readMigrationEnableState(fixture.stateDirectory, "xray.service")
	if stateErr != nil || persistent || runtime {
		t.Fatalf("original enable layers after migration: persistent=%t runtime=%t error=%v", persistent, runtime, stateErr)
	}
}

func TestExistingCoreMigrationRestoresManagedRuntimeOnlyAfterPersistentEnable(t *testing.T) {
	requireAgentRoot(t)
	fixture := newExistingCoreMigrationFixture(t, false)
	writeMigrationServiceState(t, fixture.stateDirectory, "qagent-xray.service", "inactive", "enabled-runtime")
	if err := os.WriteFile(filepath.Join(fixture.stateDirectory, "fail-existing-disable"), []byte("1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	output, err := fixture.executor.Execute(context.Background(), core.Task{
		Action: core.ActionImportExisting, Engine: core.EngineXray, ConfigContent: fixture.importedConfig,
	})
	if err == nil || !strings.Contains(output, "original configuration, binary, and service were restored") {
		t.Fatalf("migration after persistent managed enable = %q, %v", output, err)
	}
	fixture.assertServiceState(t, "xray.service", "active", "enabled")
	fixture.assertServiceState(t, "qagent-xray.service", "inactive", "enabled-runtime")
	_, persistent, runtime, stateErr := readMigrationEnableState(fixture.stateDirectory, "qagent-xray.service")
	if stateErr != nil || persistent || !runtime {
		t.Fatalf("managed runtime-only layers after rollback: persistent=%t runtime=%t error=%v", persistent, runtime, stateErr)
	}
	assertFileContentAndMode(t, fixture.managed.ConfigPath, fixture.originalManagedConfig, 0o600)
	if _, err := os.Stat(fixture.managed.Binary); !os.IsNotExist(err) {
		t.Fatalf("failed migration left managed binary: %v", err)
	}
	if _, err := os.Stat(coreMigrationMarkerPath(fixture.markerPrefix, core.EngineXray)); !os.IsNotExist(err) {
		t.Fatalf("failed migration left marker: %v", err)
	}
}

func TestExistingCoreMigrationRejectsDriftedExecStartBeforeChanges(t *testing.T) {
	requireAgentRoot(t)
	tests := []struct {
		name      string
		execStart func(existing EngineSpec) string
	}{
		{
			name: "executable",
			execStart: func(existing EngineSpec) string {
				replacement := existing.Binary + "-replacement"
				return systemdExecStart(replacement, replacement+" run -config "+existing.ConfigPath)
			},
		},
		{
			name: "configuration argv",
			execStart: func(existing EngineSpec) string {
				return systemdExecStart(existing.Binary, existing.Binary+" run -config "+existing.ConfigPath+"-replacement")
			},
		},
		{
			name: "multiple commands",
			execStart: func(existing EngineSpec) string {
				exact := systemdExecStart(existing.Binary, existing.Binary+" run -config "+existing.ConfigPath)
				return exact + "\n" + exact
			},
		},
		{
			name: "wrapper",
			execStart: func(existing EngineSpec) string {
				wrapper := existing.Binary + "-wrapper"
				return systemdExecStart(wrapper, wrapper+" "+existing.Binary+" run -config "+existing.ConfigPath)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newExistingCoreMigrationFixture(t, false)
			writeMigrationExecStart(t, fixture.stateDirectory, fixture.existing.Service, test.execStart(fixture.existing))
			if _, err := fixture.executor.Execute(context.Background(), core.Task{
				Action: core.ActionImportExisting, Engine: core.EngineXray, ConfigContent: fixture.importedConfig,
			}); err == nil || !strings.Contains(err.Error(), "ExecStart no longer matches") {
				t.Fatalf("drifted ExecStart error = %v", err)
			}
			fixture.assertServiceState(t, "xray.service", "active", "enabled")
			fixture.assertServiceState(t, "qagent-xray.service", "inactive", "disabled")
			assertFileContentAndMode(t, fixture.managed.ConfigPath, fixture.originalManagedConfig, 0o600)
			if _, err := os.Stat(fixture.managed.Binary); !os.IsNotExist(err) {
				t.Fatalf("rejected migration installed managed binary: %v", err)
			}
			if _, err := os.Stat(coreMigrationMarkerPath(fixture.markerPrefix, core.EngineXray)); !os.IsNotExist(err) {
				t.Fatalf("rejected migration left marker: %v", err)
			}
			if _, pending := fixture.executor.ExistingSpecs[core.EngineXray]; !pending {
				t.Fatal("rejected migration cleared pending existing service")
			}
		})
	}
}

func TestExistingCoreMigrationRejectsExecStartDriftDuringPreparation(t *testing.T) {
	requireAgentRoot(t)
	fixture := newExistingCoreMigrationFixture(t, false)
	wrapper := fixture.existing.Binary + "-wrapper"
	drifted := systemdExecStart(wrapper, wrapper+" "+fixture.existing.Binary+" run -config "+fixture.existing.ConfigPath)
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' %q > %q\nexit 0\n", drifted, filepath.Join(fixture.stateDirectory, fixture.existing.Service+".exec-start"))
	if err := os.WriteFile(fixture.existing.Binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.executor.Execute(context.Background(), core.Task{
		Action: core.ActionImportExisting, Engine: core.EngineXray, ConfigContent: fixture.importedConfig,
	}); err == nil || !strings.Contains(err.Error(), "ExecStart no longer matches") {
		t.Fatalf("preparation ExecStart drift error = %v", err)
	}
	fixture.assertServiceState(t, "xray.service", "active", "enabled")
	fixture.assertServiceState(t, "qagent-xray.service", "inactive", "disabled")
	assertFileContentAndMode(t, fixture.managed.ConfigPath, fixture.originalManagedConfig, 0o600)
	if _, err := os.Stat(fixture.managed.Binary); !os.IsNotExist(err) {
		t.Fatalf("drifted migration left managed binary: %v", err)
	}
	if _, err := os.Stat(coreMigrationMarkerPath(fixture.markerPrefix, core.EngineXray)); !os.IsNotExist(err) {
		t.Fatalf("drifted migration left marker: %v", err)
	}
	if _, pending := fixture.executor.ExistingSpecs[core.EngineXray]; !pending {
		t.Fatal("drifted migration cleared pending existing service")
	}
}

func TestExistingCoreMigrationAcceptsExactSupportedExecStartForms(t *testing.T) {
	tests := []struct {
		name            string
		engine          core.Engine
		binary          string
		config          string
		configDirectory string
		serviceBinary   string
		argv            string
	}{
		{name: "xray config", engine: core.EngineXray, binary: "/usr/bin/xray", config: "/etc/xray/config.json", argv: "/usr/bin/xray run -config /etc/xray/config.json"},
		{name: "xray short config", engine: core.EngineXray, binary: "/usr/bin/xray", config: "/etc/xray/config.json", argv: "/usr/bin/xray run -c /etc/xray/config.json"},
		{name: "sing-box config", engine: core.EngineSingBox, binary: "/usr/bin/sing-box", config: "/etc/sing-box/config.json", argv: "/usr/bin/sing-box run --config /etc/sing-box/config.json"},
		{name: "sing-box short config", engine: core.EngineSingBox, binary: "/usr/bin/sing-box", config: "/etc/sing-box/config.json", argv: "/usr/bin/sing-box run -c /etc/sing-box/config.json"},
		{name: "sing-box config directory", engine: core.EngineSingBox, binary: "/usr/lib/sing-box/sing-box", serviceBinary: "/usr/local/bin/sing-box", config: "/etc/sing-box/config.json", configDirectory: "/etc/sing-box/conf.d", argv: "/usr/local/bin/sing-box run -c /etc/sing-box/config.json -C /etc/sing-box/conf.d"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			serviceBinary := test.serviceBinary
			if serviceBinary == "" {
				serviceBinary = test.binary
			}
			executable, argv, err := parseSingleSystemdExecStart(systemdExecStart(serviceBinary, test.argv) + "\n")
			if err != nil {
				t.Fatalf("parse exact ExecStart: %v", err)
			}
			existing := EngineSpec{Binary: test.binary, ServiceBinary: test.serviceBinary, ConfigPath: test.config, ConfigDirectory: test.configDirectory}
			if executable != serviceBinary || !supportedExistingExecStart(test.engine, existing, argv) {
				t.Fatalf("exact ExecStart rejected: executable=%q argv=%q", executable, argv)
			}
		})
	}
}

func TestExistingCoreSourceDigestKeepsDirectSingleFileCompatibility(t *testing.T) {
	legacy := EngineSpec{Binary: "/usr/bin/sing-box", ConfigPath: "/etc/sing-box/config.json", Service: "sing-box.service"}
	explicit := legacy
	explicit.ServiceBinary = explicit.Binary
	if coreMigrationSourceDigest(legacy) != coreMigrationSourceDigest(explicit) {
		t.Fatal("explicit direct service executable changed the existing single-file marker digest")
	}
	explicit.ConfigDirectory = "/etc/sing-box/conf.d"
	if coreMigrationSourceDigest(legacy) == coreMigrationSourceDigest(explicit) {
		t.Fatal("config-directory source was not included in the migration marker digest")
	}
}

func TestExistingCoreMigrationRejectsUnsupportedSingBoxConfigDirectoryArgv(t *testing.T) {
	existing := EngineSpec{
		Binary: "/usr/bin/sing-box", ConfigPath: "/etc/sing-box/config.json",
		ConfigDirectory: "/etc/sing-box/conf.d",
	}
	for name, argv := range map[string]string{
		"unknown extra":      "/usr/bin/sing-box run -c /etc/sing-box/config.json -C /etc/sing-box/conf.d --unknown",
		"reordered":          "/usr/bin/sing-box run -C /etc/sing-box/conf.d -c /etc/sing-box/config.json",
		"long directory":     "/usr/bin/sing-box run -c /etc/sing-box/config.json --config-directory /etc/sing-box/conf.d",
		"multiple directory": "/usr/bin/sing-box run -c /etc/sing-box/config.json -C /etc/sing-box/conf.d -C /etc/sing-box/other",
	} {
		t.Run(name, func(t *testing.T) {
			if supportedExistingExecStart(core.EngineSingBox, existing, argv) {
				t.Fatalf("unsupported sing-box argv was accepted: %s", argv)
			}
		})
	}
}

func TestExistingCoreMigrationRejectsUnrestorableEnableStatesBeforeChanges(t *testing.T) {
	requireAgentRoot(t)
	tests := []struct {
		name            string
		existingEnabled string
		managedEnabled  string
	}{
		{name: "static original", existingEnabled: "static", managedEnabled: "disabled"},
		{name: "indirect original", existingEnabled: "indirect", managedEnabled: "disabled"},
		{name: "static managed", existingEnabled: "enabled", managedEnabled: "static"},
		{name: "indirect managed", existingEnabled: "enabled", managedEnabled: "indirect"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newExistingCoreMigrationFixture(t, false)
			writeMigrationServiceState(t, fixture.stateDirectory, "xray.service", "active", test.existingEnabled)
			writeMigrationServiceState(t, fixture.stateDirectory, "qagent-xray.service", "inactive", test.managedEnabled)
			if _, err := fixture.executor.Execute(context.Background(), core.Task{
				Action: core.ActionImportExisting, Engine: core.EngineXray, ConfigContent: fixture.importedConfig,
			}); err == nil || !strings.Contains(err.Error(), "enable states cannot be migrated safely") {
				t.Fatalf("unrestorable enable state error = %v", err)
			}
			fixture.assertServiceState(t, "xray.service", "active", test.existingEnabled)
			fixture.assertServiceState(t, "qagent-xray.service", "inactive", test.managedEnabled)
			assertFileContentAndMode(t, fixture.managed.ConfigPath, fixture.originalManagedConfig, 0o600)
			if _, err := os.Stat(fixture.managed.Binary); !os.IsNotExist(err) {
				t.Fatalf("rejected migration installed a managed binary: %v", err)
			}
			if _, pending := fixture.executor.ExistingSpecs[core.EngineXray]; !pending {
				t.Fatal("rejected migration cleared pending existing service")
			}
			if _, err := os.Stat(coreMigrationMarkerPath(fixture.markerPrefix, core.EngineXray)); !os.IsNotExist(err) {
				t.Fatalf("rejected migration left a marker: %v", err)
			}
		})
	}
}

func TestExistingCoreMigrationRejectsPersistentLogsBeforeStoppingOriginalService(t *testing.T) {
	requireAgentRoot(t)
	fixture := newExistingCoreMigrationFixture(t, false)
	content := `{"log":{"access":"/var/log/xray/access.log"},"inbounds":[],"outbounds":[]}`
	if err := os.WriteFile(fixture.existing.ConfigPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.executor.Execute(context.Background(), core.Task{
		Action: core.ActionImportExisting, Engine: core.EngineXray, ConfigContent: content,
	}); err == nil || !strings.Contains(err.Error(), "persistent xray log") {
		t.Fatalf("persistent log migration error = %v", err)
	}
	fixture.assertServiceState(t, "xray.service", "active", "enabled")
	fixture.assertServiceState(t, "qagent-xray.service", "inactive", "disabled")
	assertFileContentAndMode(t, fixture.managed.ConfigPath, fixture.originalManagedConfig, 0o600)
	if _, err := os.Stat(fixture.managed.Binary); !os.IsNotExist(err) {
		t.Fatalf("rejected migration installed a managed binary: %v", err)
	}
}

func TestExistingCoreMigrationRejectsEnableStateChangeBeforeStoppingOriginalService(t *testing.T) {
	requireAgentRoot(t)
	fixture := newExistingCoreMigrationFixture(t, false)
	counter := filepath.Join(fixture.stateDirectory, "validation-count")
	script := fmt.Sprintf("#!/bin/sh\ncount=0\n[ ! -f %q ] || count=$(cat %q)\ncount=$((count + 1))\nprintf '%%s\\n' \"$count\" > %q\nif [ \"$count\" -ge 3 ]; then\nprintf '0\\n' > %q\nprintf '0\\n' > %q\nprintf 'static\\n' > %q\nfi\nexit 0\n",
		counter,
		counter,
		counter,
		filepath.Join(fixture.stateDirectory, "xray.service.persistent"),
		filepath.Join(fixture.stateDirectory, "xray.service.runtime"),
		filepath.Join(fixture.stateDirectory, "xray.service.fixed"),
	)
	if err := os.WriteFile(fixture.existing.Binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.executor.Execute(context.Background(), core.Task{
		Action: core.ActionImportExisting, Engine: core.EngineXray, ConfigContent: fixture.importedConfig,
	}); err == nil || !strings.Contains(err.Error(), "enable states changed during migration preparation") {
		t.Fatalf("changed enable state error = %v", err)
	}
	fixture.assertServiceState(t, "xray.service", "active", "static")
	fixture.assertServiceState(t, "qagent-xray.service", "inactive", "disabled")
	assertFileContentAndMode(t, fixture.managed.ConfigPath, fixture.originalManagedConfig, 0o600)
	if _, err := os.Stat(fixture.managed.Binary); !os.IsNotExist(err) {
		t.Fatalf("changed enable state left managed binary: %v", err)
	}
	if _, err := os.Stat(coreMigrationMarkerPath(fixture.markerPrefix, core.EngineXray)); !os.IsNotExist(err) {
		t.Fatalf("changed enable state left a marker: %v", err)
	}
}

func TestExistingCoreReconcileRequiresPersistedMigrationIntent(t *testing.T) {
	requireAgentRoot(t)
	fixture := newExistingCoreMigrationFixture(t, false)
	writeMigrationServiceState(t, fixture.stateDirectory, "xray.service", "inactive", "enabled")
	writeMigrationServiceState(t, fixture.stateDirectory, "qagent-xray.service", "active", "disabled")
	if err := fixture.executor.ReconcileExistingCoreServices(context.Background()); err != nil {
		t.Fatalf("reconcile without intent: %v", err)
	}
	fixture.assertServiceState(t, "xray.service", "inactive", "enabled")
	fixture.assertServiceState(t, "qagent-xray.service", "active", "disabled")
	if _, pending := fixture.executor.ExistingSpecs[core.EngineXray]; !pending {
		t.Fatal("unmarked service state was treated as a completed migration")
	}
}

func TestExistingCoreReconcileRestoresOriginalAfterInterruptedStop(t *testing.T) {
	requireAgentRoot(t)
	fixture := newExistingCoreMigrationFixture(t, false)
	writeMigrationServiceState(t, fixture.stateDirectory, "xray.service", "inactive", "enabled")
	writeMigrationServiceState(t, fixture.stateDirectory, "qagent-xray.service", "inactive", "disabled")
	if err := writeCoreMigrationMarker(fixture.markerPrefix, core.EngineXray, coreMigrationInProgress, coreMigrationConfigDigest(fixture.importedConfig), coreMigrationSourceDigest(fixture.existing), "enabled", "disabled"); err != nil {
		t.Fatal(err)
	}
	if err := fixture.executor.ReconcileExistingCoreServices(context.Background()); err != nil {
		t.Fatalf("reconcile interrupted stop: %v", err)
	}
	fixture.assertServiceState(t, "xray.service", "active", "enabled")
	fixture.assertServiceState(t, "qagent-xray.service", "inactive", "disabled")
	if record, err := readCoreMigrationRecord(fixture.markerPrefix, core.EngineXray); err != nil || record.State != coreMigrationNone {
		t.Fatalf("recovered migration marker = %q, %v", record.State, err)
	}
}

func TestExistingCoreReconcileFinalizesStartedManagedService(t *testing.T) {
	requireAgentRoot(t)
	fixture := newExistingCoreMigrationFixture(t, false)
	writeMigrationServiceState(t, fixture.stateDirectory, "xray.service", "inactive", "enabled")
	writeMigrationServiceState(t, fixture.stateDirectory, "qagent-xray.service", "active", "disabled")
	if err := writeCoreMigrationMarker(fixture.markerPrefix, core.EngineXray, coreMigrationInProgress, coreMigrationConfigDigest(fixture.importedConfig), coreMigrationSourceDigest(fixture.existing), "enabled", "disabled"); err != nil {
		t.Fatal(err)
	}
	if err := fixture.executor.ReconcileExistingCoreServices(context.Background()); err != nil {
		t.Fatalf("reconcile started managed service: %v", err)
	}
	fixture.assertServiceState(t, "xray.service", "inactive", "disabled")
	fixture.assertServiceState(t, "qagent-xray.service", "active", "enabled")
	if _, pending := fixture.executor.ExistingSpecs[core.EngineXray]; pending {
		t.Fatal("reconciled migration remained pending")
	}
	if marked, err := coreMigrationMarked(fixture.markerPrefix, core.EngineXray); err != nil || !marked {
		t.Fatalf("reconciled migration marker = %t, %v", marked, err)
	}
}

func TestExistingCoreReconcileDisablesRuntimeOriginalBeforeFinalizing(t *testing.T) {
	requireAgentRoot(t)
	fixture := newExistingCoreMigrationFixture(t, false)
	writeMigrationServiceState(t, fixture.stateDirectory, "xray.service", "inactive", "enabled-runtime")
	writeMigrationServiceState(t, fixture.stateDirectory, "qagent-xray.service", "active", "disabled")
	if err := writeCoreMigrationMarker(fixture.markerPrefix, core.EngineXray, coreMigrationInProgress, coreMigrationConfigDigest(fixture.importedConfig), coreMigrationSourceDigest(fixture.existing), "enabled-runtime", "disabled"); err != nil {
		t.Fatal(err)
	}
	if err := fixture.executor.ReconcileExistingCoreServices(context.Background()); err != nil {
		t.Fatalf("reconcile runtime-enabled original: %v", err)
	}
	fixture.assertServiceState(t, "xray.service", "inactive", "disabled")
	fixture.assertServiceState(t, "qagent-xray.service", "active", "enabled")
	_, persistent, runtime, stateErr := readMigrationEnableState(fixture.stateDirectory, "xray.service")
	if stateErr != nil || persistent || runtime {
		t.Fatalf("original enable layers after reconcile: persistent=%t runtime=%t error=%v", persistent, runtime, stateErr)
	}
}

func TestExistingCoreReconcileRollsBackLegacyStaticMigration(t *testing.T) {
	requireAgentRoot(t)
	fixture := newExistingCoreMigrationFixture(t, false)
	writeMigrationServiceState(t, fixture.stateDirectory, "xray.service", "inactive", "static")
	writeMigrationServiceState(t, fixture.stateDirectory, "qagent-xray.service", "active", "enabled")
	if err := writeCoreMigrationMarker(fixture.markerPrefix, core.EngineXray, coreMigrationInProgress, coreMigrationConfigDigest(fixture.importedConfig), coreMigrationSourceDigest(fixture.existing), "static", "disabled"); err != nil {
		t.Fatal(err)
	}
	if err := fixture.executor.ReconcileExistingCoreServices(context.Background()); err != nil {
		t.Fatalf("reconcile legacy static migration: %v", err)
	}
	fixture.assertServiceState(t, "xray.service", "active", "static")
	fixture.assertServiceState(t, "qagent-xray.service", "inactive", "disabled")
	if _, pending := fixture.executor.ExistingSpecs[core.EngineXray]; !pending {
		t.Fatal("rolled back legacy migration cleared pending existing service")
	}
	if record, err := readCoreMigrationRecord(fixture.markerPrefix, core.EngineXray); err != nil || record.State != coreMigrationNone {
		t.Fatalf("rolled back legacy migration marker = %q, %v", record.State, err)
	}
}

type existingCoreMigrationFixture struct {
	executor              *Executor
	existing              EngineSpec
	managed               EngineSpec
	importedConfig        string
	originalManagedConfig string
	markerPrefix          string
	stateDirectory        string
}

func configureSingBoxDirectoryFixture(t *testing.T, fixture existingCoreMigrationFixture) (existingCoreMigrationFixture, string, string) {
	t.Helper()
	configDirectory := filepath.Join(filepath.Dir(fixture.existing.ConfigPath), "conf.d")
	if err := os.Mkdir(configDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixture.existing.ConfigPath, []byte(`{"inbounds":[{"tag":"primary"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	overlay := filepath.Join(configDirectory, "10-outbounds.json")
	if err := os.WriteFile(overlay, []byte(`{"outbounds":[{"tag":"direct"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	writeMigrationServiceState(t, fixture.stateDirectory, "sing-box.service", "active", "enabled")
	writeMigrationServiceState(t, fixture.stateDirectory, "qagent-sing-box.service", "inactive", "disabled")
	fixture.existing.ConfigDirectory = configDirectory
	fixture.existing.Service = "sing-box.service"
	fixture.managed.Service = "qagent-sing-box.service"
	writeMigrationExecStart(t, fixture.stateDirectory, fixture.existing.Service, systemdExecStart(
		fixture.existing.Binary,
		fixture.existing.Binary+" run -c "+fixture.existing.ConfigPath+" -C "+configDirectory,
	))
	content, _, err := readExistingConfigurationSources(fixture.existing)
	if err != nil {
		t.Fatalf("build merged sing-box fixture: %v", err)
	}
	fixture.importedConfig = content
	fixture.executor.Specs = map[core.Engine]EngineSpec{core.EngineSingBox: fixture.managed}
	fixture.executor.ExistingSpecs = map[core.Engine]EngineSpec{core.EngineSingBox: fixture.existing}
	return fixture, content, overlay
}

func newExistingCoreMigrationFixture(t *testing.T, failManagedStart bool) existingCoreMigrationFixture {
	t.Helper()
	root := t.TempDir()
	existingDirectory := filepath.Join(root, "existing")
	managedBinaryDirectory := filepath.Join(root, "managed-bin")
	managedConfigDirectory := filepath.Join(root, "managed-config")
	stateDirectory := filepath.Join(root, "systemctl")
	for _, directory := range []string{existingDirectory, managedBinaryDirectory, managedConfigDirectory, stateDirectory} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	existingBinary := filepath.Join(existingDirectory, "xray")
	if err := os.WriteFile(existingBinary, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	existingConfig := filepath.Join(existingDirectory, "config.json")
	importedConfig := `{"inbounds":[],"outbounds":[],"tag":"imported"}`
	if err := os.WriteFile(existingConfig, []byte(importedConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	originalManagedConfig := `{"inbounds":[],"outbounds":[],"tag":"original-managed"}`
	managedConfig := filepath.Join(managedConfigDirectory, "config.json")
	if err := os.WriteFile(managedConfig, []byte(originalManagedConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	writeMigrationServiceState(t, stateDirectory, "xray.service", "active", "enabled")
	writeMigrationServiceState(t, stateDirectory, "qagent-xray.service", "inactive", "disabled")
	writeMigrationExecStart(t, stateDirectory, "xray.service", systemdExecStart(existingBinary, existingBinary+" run -config "+existingConfig))
	if failManagedStart {
		if err := os.WriteFile(filepath.Join(stateDirectory, "fail-managed-start"), []byte("1"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	fakeSystemctl := filepath.Join(root, "fake-systemctl")
	script := fmt.Sprintf(`#!/bin/sh
set -eu
state=%q
command=$1
shift
service=${1:-}
active_file="$state/$service.active"
case "$command" in
  is-active)
    value=$(cat "$active_file")
    printf '%%s\n' "$value"
    [ "$value" = active ]
    ;;
  is-enabled)
    fixed=$(cat "$state/$service.fixed")
    if [ -n "$fixed" ]; then
      printf '%%s\n' "$fixed"
      exit 1
    fi
    if [ "$(cat "$state/$service.persistent")" = 1 ]; then
      printf 'enabled\n'
      exit 0
    fi
    if [ "$(cat "$state/$service.runtime")" = 1 ]; then
      printf 'enabled-runtime\n'
      exit 0
    fi
    printf 'disabled\n'
    exit 1
    ;;
  show)
    cat "$state/$service.exec-start"
    ;;
  stop)
    printf 'inactive\n' > "$active_file"
    ;;
  start|restart)
    if [ "$service" = qagent-xray.service ] && [ -f "$state/fail-managed-start" ]; then
      printf 'failed\n' > "$active_file"
      exit 1
    fi
    printf 'active\n' > "$active_file"
    ;;
  enable)
	if [ "$service" = --runtime ]; then
	  service=$2
	  printf '1\n' > "$state/$service.runtime"
	  exit 0
	fi
    printf '1\n' > "$state/$service.persistent"
    ;;
  disable)
    if [ "$service" = --runtime ]; then
      service=$2
      printf '0\n' > "$state/$service.runtime"
      exit 0
    fi
    if [ "$service" = xray.service ] && [ -f "$state/fail-existing-disable" ]; then
      exit 1
    fi
    printf '0\n' > "$state/$service.persistent"
    ;;
  *) exit 1 ;;
esac
`, stateDirectory)
	if err := os.WriteFile(fakeSystemctl, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	previousSystemctl := systemctlPath
	systemctlPath = fakeSystemctl
	t.Cleanup(func() { systemctlPath = previousSystemctl })

	existing := EngineSpec{Binary: existingBinary, ConfigPath: existingConfig, Service: "xray.service"}
	managed := EngineSpec{
		Binary: filepath.Join(managedBinaryDirectory, "xray"), ConfigPath: managedConfig, Service: "qagent-xray.service",
	}
	markerPrefix := filepath.Join(root, "agent-state.json.core-migration")
	return existingCoreMigrationFixture{
		executor: &Executor{
			Specs:                 map[core.Engine]EngineSpec{core.EngineXray: managed},
			ExistingSpecs:         map[core.Engine]EngineSpec{core.EngineXray: existing},
			MigrationMarkerPrefix: markerPrefix,
		},
		existing: existing, managed: managed, importedConfig: importedConfig,
		originalManagedConfig: originalManagedConfig, markerPrefix: markerPrefix, stateDirectory: stateDirectory,
	}
}

func writeMigrationServiceState(t *testing.T, directory, service, active, enabled string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(directory, service+".active"), []byte(active+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	persistent, runtime, fixed := "0", "0", ""
	switch enabled {
	case "enabled":
		persistent = "1"
	case "enabled-runtime":
		runtime = "1"
	case "disabled":
	case "static", "indirect":
		fixed = enabled
	default:
		t.Fatalf("unsupported test enable state %q", enabled)
	}
	for suffix, value := range map[string]string{
		".persistent": persistent,
		".runtime":    runtime,
		".fixed":      fixed,
	} {
		if err := os.WriteFile(filepath.Join(directory, service+suffix), []byte(value+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func writeMigrationExecStart(t *testing.T, directory, service, value string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(directory, service+".exec-start"), []byte(value+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func systemdExecStart(executable, argv string) string {
	return fmt.Sprintf("{ path=%s ; argv[]=%s ; ignore_errors=no ; start_time=[n/a] ; stop_time=[n/a] ; pid=1 ; code=(null) ; status=0/0 }", executable, argv)
}

func (fixture existingCoreMigrationFixture) assertServiceState(t *testing.T, service, active, enabled string) {
	t.Helper()
	activeValue, err := os.ReadFile(filepath.Join(fixture.stateDirectory, service+".active"))
	if err != nil || strings.TrimSpace(string(activeValue)) != active {
		t.Fatalf("%s active state = %q, %v", service, activeValue, err)
	}
	actualEnabled, persistent, runtime, err := readMigrationEnableState(fixture.stateDirectory, service)
	if err != nil || actualEnabled != enabled {
		t.Fatalf("%s enabled state = %q (persistent=%t runtime=%t), %v", service, actualEnabled, persistent, runtime, err)
	}
}

func readMigrationEnableState(directory, service string) (string, bool, bool, error) {
	read := func(suffix string) (string, error) {
		value, err := os.ReadFile(filepath.Join(directory, service+suffix))
		return strings.TrimSpace(string(value)), err
	}
	fixed, err := read(".fixed")
	if err != nil {
		return "", false, false, err
	}
	persistentValue, err := read(".persistent")
	if err != nil {
		return "", false, false, err
	}
	runtimeValue, err := read(".runtime")
	if err != nil {
		return "", false, false, err
	}
	persistent := persistentValue == "1"
	runtime := runtimeValue == "1"
	switch {
	case fixed != "":
		return fixed, persistent, runtime, nil
	case persistent:
		return "enabled", persistent, runtime, nil
	case runtime:
		return "enabled-runtime", persistent, runtime, nil
	default:
		return "disabled", persistent, runtime, nil
	}
}
