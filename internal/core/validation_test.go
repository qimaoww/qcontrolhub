package core

import (
	"strings"
	"testing"
)

func TestParseEngine(t *testing.T) {
	t.Parallel()
	tests := []struct {
		input string
		want  Engine
	}{
		{input: "mihomo", want: EngineMihomo},
		{input: " XRAY ", want: EngineXray},
		{input: "sing-box", want: EngineSingBox},
		{input: "SingBox", want: EngineSingBox},
		{input: "ss-rust", want: EngineShadowsocksRust},
		{input: "ssrust", want: EngineShadowsocksRust},
	}
	for _, test := range tests {
		t.Run(test.input, func(t *testing.T) {
			got, err := ParseEngine(test.input)
			if err != nil {
				t.Fatalf("ParseEngine(%q) error = %v", test.input, err)
			}
			if got != test.want {
				t.Fatalf("ParseEngine(%q) = %q, want %q", test.input, got, test.want)
			}
		})
	}
	if _, err := ParseEngine("unknown"); err == nil {
		t.Fatal("ParseEngine() accepted an unsupported engine")
	}
}

func TestActionWhitelistIncludesCurrentConfigurationRead(t *testing.T) {
	t.Parallel()
	for _, action := range []Action{ActionValidate, ActionDeploy, ActionStart, ActionStop, ActionRestart, ActionStatus, ActionInstall, ActionReadConfig, ActionImportExisting} {
		if !action.Valid() {
			t.Errorf("action %q is not valid", action)
		}
	}
	if Action("read-file").Valid() {
		t.Fatal("unrecognized file read action was accepted")
	}
}

func TestValidateConfigAcceptsSupportedFormats(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		engine  Engine
		content string
	}{
		{name: "mihomo yaml", engine: EngineMihomo, content: "mixed-port: 7890\nallow-lan: false\nproxies: []\n"},
		{name: "xray json", engine: EngineXray, content: `{"log":{"loglevel":"warning"},"inbounds":[],"outbounds":[]}`},
		{name: "sing-box json", engine: EngineSingBox, content: `{"log":{"level":"info"},"inbounds":[],"outbounds":[]}`},
		{name: "ss-rust json", engine: EngineShadowsocksRust, content: `{"server":"127.0.0.1","server_port":8388,"password":"correct-horse-battery-staple","method":"chacha20-ietf-poly1305"}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := ValidateConfig(test.engine, test.content); err != nil {
				t.Fatalf("ValidateConfig() error = %v", err)
			}
		})
	}
}

func TestValidateConfigRejectsInvalidOrUnsafeInput(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		engine  Engine
		content string
	}{
		{name: "unsupported engine", engine: Engine("other"), content: `{}`},
		{name: "empty", engine: EngineMihomo, content: " \n\t"},
		{name: "oversized", engine: EngineMihomo, content: strings.Repeat("x", MaxConfigBytes+1)},
		{name: "malformed yaml", engine: EngineMihomo, content: "proxies: ["},
		{name: "empty yaml mapping", engine: EngineMihomo, content: "{}"},
		{name: "yaml sequence", engine: EngineMihomo, content: "- one\n- two\n"},
		{name: "multiple yaml documents", engine: EngineMihomo, content: "mixed-port: 7890\n---\nmixed-port: 7891\n"},
		{name: "malformed json", engine: EngineXray, content: `{"inbounds":`},
		{name: "json array", engine: EngineXray, content: `[]`},
		{name: "json null", engine: EngineSingBox, content: `null`},
		{name: "trailing json document", engine: EngineSingBox, content: `{} {}`},
		{name: "ss-rust null", engine: EngineShadowsocksRust, content: `null`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := ValidateConfig(test.engine, test.content); err == nil {
				t.Fatal("ValidateConfig() unexpectedly accepted invalid input")
			}
		})
	}
}

func TestValidateConfigAcceptsMaximumSize(t *testing.T) {
	t.Parallel()
	content := `{}` + strings.Repeat(" ", MaxConfigBytes-2)
	if got := len(content); got != MaxConfigBytes {
		t.Fatalf("test fixture length = %d, want %d", got, MaxConfigBytes)
	}
	if err := ValidateConfig(EngineXray, content); err != nil {
		t.Fatalf("ValidateConfig() rejected an exactly-at-limit document: %v", err)
	}
}
