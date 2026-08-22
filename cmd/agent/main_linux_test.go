//go:build linux

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qimaoww/qcontrolhub/internal/agent"
	"github.com/qimaoww/qcontrolhub/internal/core"
)

func TestInspectExistingValidatesACopyOutsideTheSourceDirectory(t *testing.T) {
	root := t.TempDir()
	sourceDirectory := filepath.Join(root, "existing")
	if err := os.Mkdir(sourceDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(sourceDirectory, "config.json")
	if err := os.WriteFile(configPath, []byte(`{"inbounds":[],"outbounds":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	binaryPath := filepath.Join(sourceDirectory, "xray")
	script := fmt.Sprintf("#!/bin/sh\n[ \"$1 $2 $3\" = 'run -test -config' ]\n[ \"$4\" != %q ]\ngrep -q '\"inbounds\"' \"$4\"\n", configPath)
	if err := os.WriteFile(binaryPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadDir(sourceDirectory)
	if err != nil {
		t.Fatal(err)
	}
	if err := runUtilityCommand(map[core.Engine]agent.EngineSpec{
		core.EngineXray: {Binary: binaryPath, ConfigPath: configPath, Service: "xray.service"},
	}, []string{"inspect-existing", "xray"}); err != nil {
		t.Fatalf("inspect existing: %v", err)
	}
	after, err := os.ReadDir(sourceDirectory)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("inspection changed source directory: before=%d after=%d", len(before), len(after))
	}
}

func TestExistingSingBoxSpecCarriesConfigDirectoryAndServiceExecutable(t *testing.T) {
	t.Setenv("QCH_EXISTING_SING_BOX_BINARY", "/usr/lib/sing-box/sing-box")
	t.Setenv("QCH_EXISTING_SING_BOX_CONFIG", "/etc/sing-box/config.json")
	t.Setenv("QCH_EXISTING_SING_BOX_CONFIG_DIRECTORY", "/etc/sing-box/conf.d")
	t.Setenv("QCH_EXISTING_SING_BOX_SERVICE_BINARY", "/usr/local/bin/sing-box")
	t.Setenv("QCH_EXISTING_SING_BOX_SERVICE", "sing-box.service")
	spec, ok := existingSpec("SING_BOX")
	if !ok {
		t.Fatal("complete sing-box directory mapping was not loaded")
	}
	want := agent.EngineSpec{
		Binary: "/usr/lib/sing-box/sing-box", ConfigPath: "/etc/sing-box/config.json",
		ConfigDirectory: "/etc/sing-box/conf.d", ServiceBinary: "/usr/local/bin/sing-box",
		Service: "sing-box.service",
	}
	if spec != want {
		t.Fatalf("existing sing-box spec = %+v, want %+v", spec, want)
	}
}

func TestAgentStartupRefreshesAutomaticExistingCoreDiscovery(t *testing.T) {
	contents, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	mainSource := string(contents)
	for _, required := range []string{
		"manualExistingSpecs", "RefreshExistingCoreDiscovery", `statePath+".existing-cores"`,
		"ExistingDiscoveryIssues: discoveryIssues", "MigrationMarkerPrefix:",
	} {
		if !strings.Contains(mainSource, required) {
			t.Errorf("Agent startup discovery is missing %q", required)
		}
	}
}
