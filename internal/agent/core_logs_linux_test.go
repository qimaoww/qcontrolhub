//go:build linux

package agent

import (
	"strings"
	"testing"
	"time"

	"github.com/qimaoww/qcontrolhub/internal/core"
)

func TestDecodeJournalCoreLogMapsManagedUnitsAndPriorities(t *testing.T) {
	t.Parallel()
	value := []byte(`{"MESSAGE":"accepted connection","_SYSTEMD_UNIT":"qagent-sing-box.service","PRIORITY":"4","__REALTIME_TIMESTAMP":"1787310000123456"}`)
	units := map[string]core.Engine{"qagent-sing-box.service": core.EngineSingBox}
	entry, _, ok := decodeJournalCoreLog(value, units)
	if !ok {
		t.Fatal("managed journal entry was rejected")
	}
	if entry.Engine != core.EngineSingBox || entry.Level != "warning" || entry.Message != "accepted connection" {
		t.Fatalf("decoded entry = %+v", entry)
	}
	if entry.LoggedAt.UnixMicro() != 1787310000123456 {
		t.Fatalf("logged_at = %s", entry.LoggedAt)
	}
	if _, _, ok := decodeJournalCoreLog([]byte(`{"MESSAGE":"ignored","_SYSTEMD_UNIT":"ssh.service","PRIORITY":"6"}`), units); ok {
		t.Fatal("unmanaged service journal was accepted")
	}
}

func TestCoreLogCollectorKeepsBatchUntilAcknowledged(t *testing.T) {
	t.Parallel()
	collector := NewCoreLogCollector()
	collector.append(core.CoreLogEntry{Engine: core.EngineXray, Level: "info", Message: "first", LoggedAt: time.Now()})
	collector.append(core.CoreLogEntry{Engine: core.EngineMihomo, Level: "debug", Message: "second", LoggedAt: time.Now()})
	first := collector.NextBatch()
	if first == nil || len(first.Entries) != 2 || !strings.HasPrefix(first.ID, "log_") {
		t.Fatalf("first batch = %+v", first)
	}
	retry := collector.NextBatch()
	if retry == nil || retry.ID != first.ID || len(retry.Entries) != 2 {
		t.Fatalf("retry batch = %+v", retry)
	}
	if collector.Acknowledge("log_0000000000000000") {
		t.Fatal("collector accepted an unrelated acknowledgment")
	}
	if !collector.Acknowledge(first.ID) || collector.NextBatch() != nil {
		t.Fatal("acknowledged batch remained queued")
	}
}

func TestDecodeJournalCoreLogBoundsMessages(t *testing.T) {
	t.Parallel()
	message := strings.Repeat("a", core.MaxCoreLogMessageBytes-1) + "日志"
	value := []byte(`{"MESSAGE":"` + message + `","_SYSTEMD_UNIT":"qagent-xray.service","PRIORITY":"6"}`)
	units := map[string]core.Engine{"qagent-xray.service": core.EngineXray}
	entry, _, ok := decodeJournalCoreLog(value, units)
	if !ok || len([]byte(entry.Message)) > core.MaxCoreLogMessageBytes {
		t.Fatalf("bounded entry = %d bytes, ok=%v", len([]byte(entry.Message)), ok)
	}
	nulEntry, _, ok := decodeJournalCoreLog([]byte(`{"MESSAGE":"before\u0000after","_SYSTEMD_UNIT":"qagent-xray.service","PRIORITY":"6"}`), units)
	if !ok || strings.ContainsRune(nulEntry.Message, '\x00') {
		t.Fatalf("NUL-containing journal entry was not sanitized: %+v, ok=%v", nulEntry, ok)
	}
}

func TestCoreLogSourcesMixManagedAndExactGenericUnits(t *testing.T) {
	t.Parallel()
	sources := coreLogJournalSources(map[core.Engine]EngineSpec{
		core.EngineMihomo:  {Service: "qagent-mihomo.service"},
		core.EngineXray:    {Service: "xray.service"},
		core.EngineSingBox: {Service: "sing-box.service"},
	})
	if len(sources) != 2 {
		t.Fatalf("source count = %d, want 2", len(sources))
	}
	managed, generic := sources[0], sources[1]
	if !containsArgument(managed.arguments, "--namespace=qagent-cores") ||
		!containsArgument(managed.arguments, "--unit=qagent-mihomo.service") {
		t.Fatalf("managed journal arguments = %v", managed.arguments)
	}
	if containsArgument(generic.arguments, "--namespace=qagent-cores") ||
		!containsArgument(generic.arguments, "--unit=xray.service") ||
		!containsArgument(generic.arguments, "--unit=sing-box.service") {
		t.Fatalf("generic journal arguments = %v", generic.arguments)
	}
	if engine, ok := coreLogEngineForUnit("xray.service", generic.unitEngines); !ok || engine != core.EngineXray {
		t.Fatalf("generic Xray mapping = %q, %t", engine, ok)
	}
	if _, ok := coreLogEngineForUnit("ssh.service", generic.unitEngines); ok {
		t.Fatal("unrelated default-namespace unit was accepted")
	}
}

func TestCoreLogSourcesRejectArbitraryCustomUnitsAndDeduplicateCursors(t *testing.T) {
	t.Parallel()
	sources := coreLogJournalSources(map[core.Engine]EngineSpec{
		core.EngineMihomo:          {Service: "qagent-xray.service"},
		core.EngineXray:            {Service: "qagent-xray.service"},
		core.EngineSingBox:         {Service: "singbox.service"},
		core.EngineShadowsocksRust: {Service: "custom-ss.service"},
	})
	if len(sources) != 1 || containsArgument(sources[0].arguments, "--unit=qagent-xray.service") ||
		containsArgument(sources[0].arguments, "--unit=custom-ss.service") {
		t.Fatalf("custom source filtering = %+v", sources)
	}
	collector := NewCoreLogCollector(map[core.Engine]EngineSpec{})
	entry := core.CoreLogEntry{Engine: core.EngineSingBox, Level: "info", Message: "once", LoggedAt: time.Now()}
	collector.appendJournal(entry, "cursor-1")
	collector.appendJournal(entry, "cursor-1")
	batch := collector.NextBatch()
	if batch == nil || len(batch.Entries) != 1 {
		t.Fatalf("deduplicated batch = %+v", batch)
	}
}

func containsArgument(arguments []string, expected string) bool {
	for _, argument := range arguments {
		if argument == expected {
			return true
		}
	}
	return false
}
