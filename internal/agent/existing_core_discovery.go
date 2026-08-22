//go:build linux

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/qimaoww/qcontrolhub/internal/core"
)

const existingCoreDiscoveryStateVersion = 1

type existingDiscoveryCandidateSet struct {
	services    []string
	executables []string
	configs     []string
}

var existingDiscoveryCandidates = map[core.Engine]existingDiscoveryCandidateSet{
	core.EngineXray: {
		services:    []string{"xray.service"},
		executables: []string{"/usr/local/bin/xray", "/usr/bin/xray"},
		configs:     []string{"/usr/local/etc/xray/config.json", "/etc/xray/config.json"},
	},
	core.EngineSingBox: {
		services:    []string{"sing-box.service", "singbox.service"},
		executables: []string{"/usr/local/bin/sing-box", "/usr/bin/sing-box"},
		configs:     []string{"/etc/sing-box/config.json", "/usr/local/etc/sing-box/config.json"},
	},
}

var existingDiscoveryManagedUnitRoot = "/etc/systemd/system"

type existingCoreDiscoveryState struct {
	Version int                                   `json:"version"`
	Specs   map[core.Engine]existingDiscoverySpec `json:"specs,omitempty"`
	Issues  map[core.Engine]string                `json:"issues,omitempty"`
}

type existingDiscoverySpec struct {
	Binary          string `json:"binary"`
	ConfigPath      string `json:"config_path"`
	ConfigDirectory string `json:"config_directory,omitempty"`
	ServiceBinary   string `json:"service_binary,omitempty"`
	Service         string `json:"service"`
}

func discoverySpecFromEngineSpec(spec EngineSpec) existingDiscoverySpec {
	return existingDiscoverySpec{
		Binary: spec.Binary, ConfigPath: spec.ConfigPath, ConfigDirectory: spec.ConfigDirectory,
		ServiceBinary: spec.ServiceBinary, Service: spec.Service,
	}
}

func (spec existingDiscoverySpec) engineSpec() EngineSpec {
	return EngineSpec{
		Binary: spec.Binary, ConfigPath: spec.ConfigPath, ConfigDirectory: spec.ConfigDirectory,
		ServiceBinary: spec.ServiceBinary, Service: spec.Service,
	}
}

// RefreshExistingCoreDiscovery performs the same fail-closed, read-only
// mapping used by a fresh installer. It persists only mappings discovered by
// the Agent; explicit QCH_EXISTING_* mappings are never replaced and always
// take precedence. A persisted mapping is reused only to reconcile an
// in-progress migration whose original service may already be inactive.
func RefreshExistingCoreDiscovery(
	ctx context.Context,
	discoveryStatePath string,
	migrationMarkerPrefix string,
	managedSpecs map[core.Engine]EngineSpec,
	manualSpecs map[core.Engine]EngineSpec,
) (map[core.Engine]EngineSpec, map[core.Engine]string, error) {
	if err := os.MkdirAll(filepath.Dir(discoveryStatePath), 0o700); err != nil {
		return nil, nil, fmt.Errorf("create existing-core discovery state directory: %w", err)
	}
	if err := validateStateDirectory(filepath.Dir(discoveryStatePath)); err != nil {
		return nil, nil, err
	}
	previous, err := loadExistingCoreDiscoveryState(discoveryStatePath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, nil, fmt.Errorf("load existing-core discovery state: %w", err)
	}
	if errors.Is(err, os.ErrNotExist) {
		previous = existingCoreDiscoveryState{Version: existingCoreDiscoveryStateVersion}
	}
	validationRoot, err := os.MkdirTemp(filepath.Dir(discoveryStatePath), ".existing-core-discovery-")
	if err != nil {
		return nil, nil, fmt.Errorf("create protected existing-core validation directory: %w", err)
	}
	defer os.RemoveAll(validationRoot)

	automatic := make(map[core.Engine]EngineSpec)
	issues := make(map[core.Engine]string)
	for _, engine := range []core.Engine{core.EngineXray, core.EngineSingBox} {
		if _, explicit := manualSpecs[engine]; explicit {
			continue
		}
		managed, enabled := managedSpecs[engine]
		if !enabled {
			continue
		}
		record, recordErr := readCoreMigrationRecord(migrationMarkerPrefix, engine)
		if recordErr != nil {
			return nil, nil, fmt.Errorf("read %s migration state during discovery: %w", engine, recordErr)
		}
		if record.State == coreMigrationInProgress {
			previousSpec, ok := previous.Specs[engine]
			if !ok || record.SourceDigest != coreMigrationSourceDigest(previousSpec.engineSpec()) {
				return nil, nil, fmt.Errorf("persisted %s discovery mapping does not match the in-progress migration", engine)
			}
			automatic[engine] = previousSpec.engineSpec()
			continue
		}
		spec, found, issue := discoverExistingCoreService(ctx, engine, managed, filepath.Join(validationRoot, string(engine)))
		if ctx.Err() != nil {
			return nil, nil, fmt.Errorf("discover existing %s service: %w", engine, ctx.Err())
		}
		if found {
			automatic[engine] = spec
			continue
		}
		if issue != "" {
			issues[engine] = limitDiscoveryIssue(issue)
		}
	}

	state := existingCoreDiscoveryState{
		Version: existingCoreDiscoveryStateVersion,
		Specs:   make(map[core.Engine]existingDiscoverySpec, len(automatic)),
		Issues:  issues,
	}
	for engine, spec := range automatic {
		state.Specs[engine] = discoverySpecFromEngineSpec(spec)
	}
	if err := saveExistingCoreDiscoveryState(discoveryStatePath, state); err != nil {
		return nil, nil, fmt.Errorf("persist existing-core discovery state: %w", err)
	}

	result := make(map[core.Engine]EngineSpec, len(manualSpecs)+len(automatic))
	for engine, spec := range automatic {
		result[engine] = spec
	}
	for engine, spec := range manualSpecs {
		result[engine] = spec
		delete(issues, engine)
	}
	return result, issues, nil
}

func discoverExistingCoreService(ctx context.Context, engine core.Engine, managed EngineSpec, validationDirectory string) (EngineSpec, bool, string) {
	candidates := existingDiscoveryCandidates[engine]
	activeServices := make([]string, 0, len(candidates.services))
	for _, service := range candidates.services {
		status, err := serviceStatus(ctx, service)
		if err != nil {
			continue
		}
		if status == "active" {
			activeServices = append(activeServices, service)
		}
	}
	if len(activeServices) == 0 {
		return EngineSpec{}, false, ""
	}
	if len(activeServices) != 1 {
		return EngineSpec{}, false, fmt.Sprintf("检测到多个活动的标准 %s 服务，自动迁移已安全禁用", engine)
	}
	service := activeServices[0]
	execStart, err := run(ctx, systemctlPath, "show", service, "--property=ExecStart", "--value")
	if err != nil {
		return EngineSpec{}, false, fmt.Sprintf("检测到活动的 %s 服务，但无法读取其唯一 ExecStart", engine)
	}
	executable, argv, err := parseSingleSystemdExecStart(execStart)
	if err != nil {
		return EngineSpec{}, false, fmt.Sprintf("检测到活动的 %s 服务，但 ExecStart 包含多命令或结构不受支持", engine)
	}
	if !stringInSlice(executable, candidates.executables) {
		return EngineSpec{}, false, fmt.Sprintf("检测到活动的 %s 服务，但 executable 不在受支持的标准路径", engine)
	}
	configPath, configDirectory, ok := parseDiscoveredExistingArgv(engine, executable, argv, candidates.configs)
	if !ok {
		return EngineSpec{}, false, fmt.Sprintf("检测到活动的 %s 服务，但 ExecStart 参数不属于受支持的精确配置形式", engine)
	}
	realBinary, err := resolveDiscoveredExistingBinary(executable)
	if err != nil {
		return EngineSpec{}, false, fmt.Sprintf("检测到活动的 %s 服务和配置，但 executable wrapper 不在安全支持范围；请改为真实二进制、一跳二进制链接或固定 exec 转发器", engine)
	}
	if err := validateManagedServiceForExistingDiscovery(ctx, engine, managed); err != nil {
		return EngineSpec{}, false, fmt.Sprintf("检测到活动的 %s 服务，但 QAgent 专用服务不是受支持的安全非活动 unit", engine)
	}
	spec := EngineSpec{
		Binary: realBinary, ConfigPath: configPath, ConfigDirectory: configDirectory,
		ServiceBinary: executable, Service: service,
	}
	if err := verifyExistingServiceMapping(ctx, engine, spec); err != nil {
		return EngineSpec{}, false, fmt.Sprintf("检测到活动的 %s 服务，但映射在核验期间发生变化", engine)
	}
	validationSpec := managed
	validationSpec.ConfigPath = filepath.Join(validationDirectory, "config.json")
	probe := &Executor{Specs: map[core.Engine]EngineSpec{engine: validationSpec}, ExistingSpecs: map[core.Engine]EngineSpec{engine: spec}}
	if _, err := probe.readExistingConfig(ctx, engine, validationSpec, spec); err != nil {
		return EngineSpec{}, false, fmt.Sprintf("检测到活动的 %s 服务，但配置源未通过受保护路径与真实内核校验", engine)
	}
	if err := verifyExistingServiceMapping(ctx, engine, spec); err != nil {
		return EngineSpec{}, false, fmt.Sprintf("检测到活动的 %s 服务，但映射在配置核验期间发生变化", engine)
	}
	return spec, true, ""
}

func validateManagedServiceForExistingDiscovery(ctx context.Context, engine core.Engine, managed EngineSpec) error {
	defaultSpec, ok := DefaultSpecs()[engine]
	if !ok || managed != defaultSpec {
		return errors.New("managed service mapping is not the QAgent default")
	}
	loadState, err := run(ctx, systemctlPath, "show", managed.Service, "--property=LoadState", "--value")
	if err != nil || strings.TrimSpace(loadState) != "loaded" {
		return errors.New("managed service unit is not loaded")
	}
	status, err := serviceStatus(ctx, managed.Service)
	if err != nil || (status != "inactive" && status != "failed") {
		return errors.New("managed service is not inactive or failed")
	}
	fragmentPath, err := run(ctx, systemctlPath, "show", managed.Service, "--property=FragmentPath", "--value")
	expectedFragmentPath := filepath.Join(existingDiscoveryManagedUnitRoot, managed.Service)
	if err != nil || strings.TrimSpace(fragmentPath) != expectedFragmentPath {
		return errors.New("managed service fragment path is not the QAgent-owned location")
	}
	fragmentPath = strings.TrimSpace(fragmentPath)
	if err := validateProtectedDirectoryChain(filepath.Dir(fragmentPath)); err != nil {
		return err
	}
	info, err := os.Lstat(fragmentPath)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0o022 != 0 {
		return errors.New("managed service unit file is unsafe")
	}
	if err := validateOwner(info, "managed service unit file"); err != nil {
		return err
	}
	contents, err := os.ReadFile(fragmentPath)
	if err != nil {
		return err
	}
	if !strings.Contains(string(contents), "Description="+engineDisplayName(engine)+" core managed by QAgent\n") {
		return errors.New("managed service unit lacks the QAgent ownership marker")
	}
	return nil
}

func engineDisplayName(engine core.Engine) string {
	switch engine {
	case core.EngineXray:
		return "Xray"
	case core.EngineSingBox:
		return "sing-box"
	default:
		return string(engine)
	}
}

func parseDiscoveredExistingArgv(engine core.Engine, executable, argv string, configs []string) (string, string, bool) {
	for _, configPath := range configs {
		switch engine {
		case core.EngineXray:
			if argv == executable+" run -config "+configPath || argv == executable+" run -c "+configPath {
				return configPath, "", true
			}
		case core.EngineSingBox:
			if argv == executable+" run -c "+configPath || argv == executable+" run --config "+configPath {
				return configPath, "", true
			}
			prefix := executable + " run -c " + configPath + " -C "
			if strings.HasPrefix(argv, prefix) {
				directory := strings.TrimPrefix(argv, prefix)
				if filepath.IsAbs(directory) && directory != "" && !strings.ContainsAny(directory, " \t\r\n") {
					return configPath, directory, true
				}
			}
		}
	}
	return "", "", false
}

func resolveDiscoveredExistingBinary(serviceBinary string) (string, error) {
	if !filepath.IsAbs(serviceBinary) || strings.ContainsAny(serviceBinary, " \t\r\n") {
		return "", errors.New("service executable path is unsafe")
	}
	if err := validateProtectedDirectoryChain(filepath.Dir(serviceBinary)); err != nil {
		return "", err
	}
	info, err := os.Lstat(serviceBinary)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink == 0 {
		if err := validateNativeCoreExecutable(serviceBinary); err != nil {
			return "", err
		}
		return serviceBinary, nil
	}
	target, err := os.Readlink(serviceBinary)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(serviceBinary), target)
	}
	target = filepath.Clean(target)
	targetInfo, err := os.Lstat(target)
	if err != nil {
		return "", err
	}
	if targetInfo.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("service executable uses more than one symlink")
	}
	if err := validateProtectedDirectoryChain(filepath.Dir(target)); err != nil {
		return "", err
	}
	if err := validateNativeCoreExecutable(target); err == nil {
		return target, nil
	}
	contents, err := os.ReadFile(target)
	if err != nil || len(contents) > 1024 {
		return "", errors.New("service executable wrapper is unsupported")
	}
	lines := strings.Split(string(contents), "\n")
	if len(lines) != 3 || lines[0] != "#!/bin/sh" || lines[2] != "" {
		return "", errors.New("service executable wrapper is not the fixed two-line form")
	}
	const prefix = "exec "
	const suffix = " \"$@\""
	if !strings.HasPrefix(lines[1], prefix) || !strings.HasSuffix(lines[1], suffix) {
		return "", errors.New("service executable wrapper is not an unconditional exec forwarder")
	}
	realBinary := strings.TrimSuffix(strings.TrimPrefix(lines[1], prefix), suffix)
	if !filepath.IsAbs(realBinary) || strings.ContainsAny(realBinary, " \t\r\n") {
		return "", errors.New("forwarded core path is unsafe")
	}
	if err := validateProtectedDirectoryChain(filepath.Dir(realBinary)); err != nil {
		return "", err
	}
	if err := validateNativeCoreExecutable(realBinary); err != nil {
		return "", err
	}
	return realBinary, nil
}

func stringInSlice(value string, values []string) bool {
	for _, candidate := range values {
		if value == candidate {
			return true
		}
	}
	return false
}

func limitDiscoveryIssue(value string) string {
	value = strings.TrimSpace(strings.ToValidUTF8(value, "�"))
	if len(value) <= 512 {
		return value
	}
	return strings.ToValidUTF8(value[:512], "�")
}

func loadExistingCoreDiscoveryState(path string) (existingCoreDiscoveryState, error) {
	if err := validateStateDirectory(filepath.Dir(path)); err != nil {
		return existingCoreDiscoveryState{}, err
	}
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return existingCoreDiscoveryState{}, err
	}
	defer root.Close()
	info, err := root.Lstat(filepath.Base(path))
	if err != nil {
		return existingCoreDiscoveryState{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return existingCoreDiscoveryState{}, errors.New("existing-core discovery state must be a protected regular file")
	}
	if err := validateOwner(info, "existing-core discovery state"); err != nil {
		return existingCoreDiscoveryState{}, err
	}
	file, err := root.Open(filepath.Base(path))
	if err != nil {
		return existingCoreDiscoveryState{}, err
	}
	defer file.Close()
	contents, err := io.ReadAll(io.LimitReader(file, (32<<10)+1))
	if err != nil {
		return existingCoreDiscoveryState{}, err
	}
	if len(contents) > 32<<10 {
		return existingCoreDiscoveryState{}, errors.New("existing-core discovery state is too large")
	}
	var state existingCoreDiscoveryState
	if err := json.Unmarshal(contents, &state); err != nil {
		return existingCoreDiscoveryState{}, err
	}
	if state.Version != existingCoreDiscoveryStateVersion {
		return existingCoreDiscoveryState{}, errors.New("existing-core discovery state version is unsupported")
	}
	for engine, issue := range state.Issues {
		if (engine != core.EngineXray && engine != core.EngineSingBox) || issue == "" || len(issue) > 512 || !utf8.ValidString(issue) {
			return existingCoreDiscoveryState{}, errors.New("existing-core discovery issue is invalid")
		}
	}
	for engine, stored := range state.Specs {
		spec := stored.engineSpec()
		if !supportedExistingService(engine, spec.Service) || !filepath.IsAbs(spec.Binary) || !filepath.IsAbs(spec.ConfigPath) ||
			(spec.ConfigDirectory != "" && !filepath.IsAbs(spec.ConfigDirectory)) || !filepath.IsAbs(existingServiceBinary(spec)) {
			return existingCoreDiscoveryState{}, errors.New("existing-core discovery mapping is invalid")
		}
	}
	return state, nil
}

func saveExistingCoreDiscoveryState(path string, state existingCoreDiscoveryState) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	if err := validateStateDirectory(directory); err != nil {
		return err
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return err
	}
	defer root.Close()
	if info, statErr := root.Lstat(filepath.Base(path)); statErr == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
			return errors.New("existing-core discovery state destination is unsafe")
		}
		if err := validateOwner(info, "existing-core discovery state destination"); err != nil {
			return err
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	}
	suffix, err := randomSuffix(10)
	if err != nil {
		return err
	}
	tempName := ".existing-cores-" + suffix + ".tmp"
	temp, err := root.OpenFile(tempName, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer root.Remove(tempName)
	if err := json.NewEncoder(temp).Encode(state); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := root.Rename(tempName, filepath.Base(path)); err != nil {
		return err
	}
	return syncRootDirectory(root)
}
