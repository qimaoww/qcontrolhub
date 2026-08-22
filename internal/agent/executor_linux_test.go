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

func TestAtomicDeployCreatesAndBacksUpConfiguration(t *testing.T) {
	t.Parallel()
	destination := filepath.Join(t.TempDir(), "nested", "config.json")
	first := `{"version":1}`
	backup, err := atomicDeploy(destination, first)
	if err != nil {
		t.Fatalf("first atomicDeploy() error = %v", err)
	}
	if backup != "" {
		t.Fatalf("first atomicDeploy() backup = %q, want empty", backup)
	}
	assertFileContentAndMode(t, destination, first, 0o600)
	if matches, err := filepath.Glob(filepath.Join(filepath.Dir(destination), ".qcontrolhub-config-*.tmp")); err != nil {
		t.Fatalf("glob temporary files: %v", err)
	} else if len(matches) != 0 {
		t.Fatalf("temporary files left behind after rename: %v", matches)
	}

	second := `{"version":2}`
	backup, err = atomicDeploy(destination, second)
	if err != nil {
		t.Fatalf("replacement atomicDeploy() error = %v", err)
	}
	if backup == "" || filepath.Dir(backup) != filepath.Dir(destination) {
		t.Fatalf("replacement backup path = %q", backup)
	}
	assertFileContentAndMode(t, destination, second, 0o600)
	assertFileContentAndMode(t, backup, first, 0o600)
}

func TestAtomicDeployRejectsUnsafeDestinations(t *testing.T) {
	t.Parallel()
	if _, err := atomicDeploy("relative/config.json", `{}`); err == nil {
		t.Fatal("atomicDeploy() accepted a relative destination")
	}
	if _, err := atomicDeploy("", `{}`); err == nil {
		t.Fatal("atomicDeploy() accepted an empty destination")
	}

	root := t.TempDir()
	realTarget := filepath.Join(root, "real.json")
	if err := os.WriteFile(realTarget, []byte("original"), 0o600); err != nil {
		t.Fatalf("write symlink target: %v", err)
	}
	symlink := filepath.Join(root, "config.json")
	if err := os.Symlink(realTarget, symlink); err != nil {
		t.Fatalf("create symlink: %v", err)
	}
	if _, err := atomicDeploy(symlink, "replacement"); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("atomicDeploy() symlink error = %v", err)
	}
	assertFileContentAndMode(t, realTarget, "original", 0o600)

	directoryDestination := filepath.Join(root, "directory")
	if err := os.Mkdir(directoryDestination, 0o750); err != nil {
		t.Fatalf("create directory destination: %v", err)
	}
	if _, err := atomicDeploy(directoryDestination, "replacement"); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("atomicDeploy() directory error = %v", err)
	}

	writableDestination := filepath.Join(root, "writable.json")
	if err := os.WriteFile(writableDestination, []byte("original"), 0o600); err != nil {
		t.Fatalf("write group-writable destination: %v", err)
	}
	if err := os.Chmod(writableDestination, 0o660); err != nil {
		t.Fatalf("make destination group-writable: %v", err)
	}
	if _, err := atomicDeploy(writableDestination, "replacement"); err == nil || !strings.Contains(err.Error(), "writable by group or others") {
		t.Fatalf("atomicDeploy() writable-file error = %v", err)
	}
	assertFileContentAndMode(t, writableDestination, "original", 0o660)
}

func TestAtomicDeployAndRollbackPreserveConfigurationMetadata(t *testing.T) {
	t.Parallel()
	destination := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(destination, []byte("original"), 0o640); err != nil {
		t.Fatal(err)
	}
	assignAlternateTestGroup(t, destination)
	want := statFileMetadata(t, destination)

	backup, err := atomicDeploy(destination, "replacement")
	if err != nil {
		t.Fatal(err)
	}
	assertFileContentAndMetadata(t, destination, "replacement", want)
	assertFileContentAndMetadata(t, backup, "original", want)

	if _, err := rollbackDeploy(destination, backup); err != nil {
		t.Fatal(err)
	}
	assertFileContentAndMetadata(t, destination, "original", want)
}

func TestRollbackDeployRestoresBackupAndBackupRetentionIsBounded(t *testing.T) {
	t.Parallel()
	destination := filepath.Join(t.TempDir(), "config.json")
	if _, err := atomicDeploy(destination, "version-0"); err != nil {
		t.Fatalf("initial deploy: %v", err)
	}
	var latestBackup string
	for version := 1; version <= 5; version++ {
		backup, err := atomicDeploy(destination, fmt.Sprintf("version-%d", version))
		if err != nil {
			t.Fatalf("deploy version %d: %v", version, err)
		}
		latestBackup = backup
	}
	backups, err := filepath.Glob(destination + ".bak-*")
	if err != nil {
		t.Fatalf("glob backups: %v", err)
	}
	if len(backups) != 3 {
		t.Fatalf("backup count = %d, want 3 (%v)", len(backups), backups)
	}
	message, err := rollbackDeploy(destination, latestBackup)
	if err != nil || !strings.Contains(message, "restored") {
		t.Fatalf("rollbackDeploy() message=%q error=%v", message, err)
	}
	assertFileContentAndMode(t, destination, "version-4", 0o600)
}

func TestExecutorReadsAndValidatesCurrentConfigurationWithoutWriting(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	binary := filepath.Join(root, "mihomo")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "config.yaml")
	content := "mixed-port: 7890\nmode: rule\nproxies: []\n"
	if err := os.WriteFile(configPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	executor := &Executor{Specs: map[core.Engine]EngineSpec{
		core.EngineMihomo: {Binary: binary, ConfigPath: configPath, Service: "qagent-mihomo.service"},
	}}
	output, err := executor.Execute(context.Background(), core.Task{Action: core.ActionReadConfig, Engine: core.EngineMihomo})
	if err != nil {
		t.Fatalf("read current configuration: %v", err)
	}
	if output != content {
		t.Fatalf("read configuration output = %q, want exact current file", output)
	}
	assertFileContentAndMode(t, configPath, content, 0o600)

	if err := os.WriteFile(binary, []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := executor.Execute(context.Background(), core.Task{Action: core.ActionReadConfig, Engine: core.EngineMihomo}); err == nil || !strings.Contains(err.Error(), "real core validation") {
		t.Fatalf("read configuration with rejecting core error = %v", err)
	}
}

func TestReadCurrentConfigValidatesTheReturnedSnapshot(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "config.json")
	original := `{"inbounds":[],"outbounds":[],"tag":"original"}`
	changed := `{"inbounds":[],"outbounds":[],"tag":"changed"}`
	if err := os.WriteFile(configPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(root, "xray")
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s' %q > %q\ngrep -q '\"tag\":\"original\"' \"$4\"\n", changed, configPath)
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	executor := &Executor{Specs: map[core.Engine]EngineSpec{
		core.EngineXray: {Binary: binary, ConfigPath: configPath, Service: "xray.service"},
	}}

	content, err := executor.ReadCurrentConfig(context.Background(), core.EngineXray)
	if err != nil {
		t.Fatalf("ReadCurrentConfig() error = %v", err)
	}
	if content != original {
		t.Fatalf("returned snapshot = %q, want original %q", content, original)
	}
	if live, err := os.ReadFile(configPath); err != nil || string(live) != changed {
		t.Fatalf("live file after concurrent change = %q, %v", live, err)
	}
}

func TestManualReadAllowsViewingConfigThatManagedDeployWouldReject(t *testing.T) {
	root := t.TempDir()
	binary := filepath.Join(root, "xray")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "config.json")
	content := `{"log":{"access":"/var/log/xray/access.log"},"inbounds":[],"outbounds":[]}`
	if err := os.WriteFile(configPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	executor := &Executor{Specs: map[core.Engine]EngineSpec{
		core.EngineXray: {Binary: binary, ConfigPath: configPath, Service: "xray.service"},
	}}

	read, err := executor.Execute(context.Background(), core.Task{Action: core.ActionReadConfig, Engine: core.EngineXray})
	if err != nil || read != content {
		t.Fatalf("manual read = %q, %v", read, err)
	}
	if _, err := executor.Execute(context.Background(), core.Task{
		Action: core.ActionValidate, Engine: core.EngineXray, ConfigContent: read,
	}); err == nil || !strings.Contains(err.Error(), "persistent") {
		t.Fatalf("managed validation accepted persistent logs: %v", err)
	}
}

func TestReadConfigurationFileRejectsUnsafeOrOversizedSources(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	regular := filepath.Join(root, "regular.yaml")
	if err := os.WriteFile(regular, []byte("mixed-port: 7890\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	symlink := filepath.Join(root, "symlink.yaml")
	if err := os.Symlink(regular, symlink); err != nil {
		t.Fatal(err)
	}
	if _, err := readConfigurationFile(symlink); err == nil || !strings.Contains(err.Error(), "non-symlink") {
		t.Fatalf("symlink configuration error = %v", err)
	}
	unsafe := filepath.Join(root, "unsafe.yaml")
	if err := os.WriteFile(unsafe, []byte("mixed-port: 7890\n"), 0o620); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(unsafe, 0o620); err != nil {
		t.Fatal(err)
	}
	if _, err := readConfigurationFile(unsafe); err == nil || !strings.Contains(err.Error(), "writable by group") {
		t.Fatalf("group-writable configuration error = %v", err)
	}
	oversized := filepath.Join(root, "oversized.yaml")
	if err := os.WriteFile(oversized, []byte(strings.Repeat("x", core.MaxConfigBytes+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readConfigurationFile(oversized); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized configuration error = %v", err)
	}
}

func TestExistingSingBoxSnapshotMergesExactConfigDirectory(t *testing.T) {
	root := t.TempDir()
	configDirectory := filepath.Join(root, "conf.d")
	validationDirectory := filepath.Join(root, "managed")
	if err := os.MkdirAll(configDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(validationDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	primary := filepath.Join(root, "config.json")
	if err := os.WriteFile(primary, []byte(`{"log":{"level":"info"},"inbounds":[{"tag":"primary"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDirectory, "10-outbounds.json"), []byte(`{"outbounds":[{"tag":"direct","type":"direct"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDirectory, "20-log.json"), []byte(`{"log":{"level":"debug"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(root, "sing-box")
	script := `#!/bin/sh
set -eu
[ "$1" = check ] && [ "$2" = -c ]
if [ "$#" -eq 5 ]; then
  [ "$4" = -C ] && [ -d "$5" ]
fi
grep -q '"inbounds"' "$3"
`
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	existing := EngineSpec{Binary: binary, ConfigPath: primary, ConfigDirectory: configDirectory, Service: "sing-box.service"}
	managed := EngineSpec{Binary: binary, ConfigPath: filepath.Join(validationDirectory, "config.json"), Service: "qagent-sing-box.service"}
	executor := &Executor{Specs: map[core.Engine]EngineSpec{core.EngineSingBox: managed}, ExistingSpecs: map[core.Engine]EngineSpec{core.EngineSingBox: existing}}
	content, err := executor.readExistingConfig(context.Background(), core.EngineSingBox, managed, existing)
	if err != nil {
		t.Fatalf("read merged sing-box snapshot: %v", err)
	}
	for _, required := range []string{`"tag": "primary"`, `"tag": "direct"`, `"level": "debug"`} {
		if !strings.Contains(content, required) {
			t.Errorf("merged snapshot is missing %s: %s", required, content)
		}
	}
	if strings.Contains(content, `"level": "info"`) {
		t.Fatalf("later path unexpectedly replaced the earlier sorted sing-box value: %s", content)
	}

	if err := os.Symlink(filepath.Join(configDirectory, "10-outbounds.json"), filepath.Join(configDirectory, "30-linked.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := executor.readExistingConfig(context.Background(), core.EngineSingBox, managed, existing); err == nil || !strings.Contains(err.Error(), "non-symlink") {
		t.Fatalf("symlinked config-directory entry error = %v", err)
	}
	if err := os.Remove(filepath.Join(configDirectory, "30-linked.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(configDirectory, 0o770); err != nil {
		t.Fatal(err)
	}
	if _, err := executor.readExistingConfig(context.Background(), core.EngineSingBox, managed, existing); err == nil || !strings.Contains(err.Error(), "writable by group") {
		t.Fatalf("group-writable config-directory error = %v", err)
	}
	if err := os.Chmod(configDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	realDirectory := configDirectory + "-real"
	if err := os.Rename(configDirectory, realDirectory); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realDirectory, configDirectory); err != nil {
		t.Fatal(err)
	}
	if _, err := executor.readExistingConfig(context.Background(), core.EngineSingBox, managed, existing); err == nil || !strings.Contains(err.Error(), "not a real directory") {
		t.Fatalf("symlinked config-directory error = %v", err)
	}
}

func TestExistingServiceExecutableAcceptsOnlyFixedForwarder(t *testing.T) {
	requireAgentRoot(t)
	root := t.TempDir()
	forwarder := filepath.Join(root, "sing-box-forwarder")
	serviceBinary := filepath.Join(root, "sing-box")
	if err := os.WriteFile(forwarder, []byte("#!/bin/sh\nexec /usr/bin/true \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(forwarder, serviceBinary); err != nil {
		t.Fatal(err)
	}
	spec := EngineSpec{Binary: "/usr/bin/true", ServiceBinary: serviceBinary}
	if err := validateExistingServiceExecutable(spec); err != nil {
		t.Fatalf("fixed exec forwarder rejected: %v", err)
	}
	secondLink := filepath.Join(root, "sing-box-second-link")
	if err := os.Symlink(serviceBinary, secondLink); err != nil {
		t.Fatal(err)
	}
	multiHop := spec
	multiHop.ServiceBinary = secondLink
	if err := validateExistingServiceExecutable(multiHop); err == nil || !strings.Contains(err.Error(), "at most one symlink") {
		t.Fatalf("multi-hop service executable error = %v", err)
	}
	if err := os.WriteFile(forwarder, []byte("#!/bin/sh\necho unsafe\nexec /usr/bin/true \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := validateExistingServiceExecutable(spec); err == nil || !strings.Contains(err.Error(), "fixed exec forwarder") {
		t.Fatalf("arbitrary wrapper error = %v", err)
	}
	realScript := filepath.Join(root, "not-a-core")
	if err := os.WriteFile(realScript, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(forwarder, []byte("#!/bin/sh\nexec "+realScript+" \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	spec.Binary = realScript
	if err := validateExistingServiceExecutable(spec); err == nil || !strings.Contains(err.Error(), "must not be a script") {
		t.Fatalf("script target error = %v", err)
	}
}

func assertFileContentAndMode(t *testing.T, path, want string, wantMode os.FileMode) {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(content) != want {
		t.Fatalf("content of %s = %q, want %q", path, content, want)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if got := info.Mode().Perm(); got != wantMode {
		t.Fatalf("mode of %s = %o, want %o", path, got, wantMode)
	}
}

func statFileMetadata(t *testing.T, path string) fileMetadata {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return metadataFromFileInfo(info)
}

func assertFileContentAndMetadata(t *testing.T, path, wantContent string, want fileMetadata) {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(contents) != wantContent {
		t.Fatalf("content of %s = %q, want %q", path, contents, wantContent)
	}
	got := statFileMetadata(t, path)
	if got.mode != want.mode {
		t.Fatalf("mode of %s = %o, want %o", path, got.mode, want.mode)
	}
	if want.ownershipKnown && (!got.ownershipKnown || got.uid != want.uid || got.gid != want.gid) {
		t.Fatalf("ownership of %s = %d:%d (known=%t), want %d:%d", path, got.uid, got.gid, got.ownershipKnown, want.uid, want.gid)
	}
}

func assignAlternateTestGroup(t *testing.T, path string) {
	t.Helper()
	if os.Geteuid() != 0 {
		return
	}
	gid := 65534
	if gid == os.Getegid() {
		gid = 1
	}
	if err := os.Chown(path, os.Geteuid(), gid); err != nil {
		t.Fatalf("assign alternate test group: %v", err)
	}
}
