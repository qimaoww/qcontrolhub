package agent

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/qimaoww/qcontrolhub/internal/core"
)

type EngineSpec struct {
	Binary          string
	ConfigPath      string
	ConfigDirectory string
	ServiceBinary   string
	Service         string
}

type Executor struct {
	Specs                   map[core.Engine]EngineSpec
	ExistingSpecs           map[core.Engine]EngineSpec
	ExistingDiscoveryIssues map[core.Engine]string
	MigrationMarkerPrefix   string
	Updater                 *CoreUpdater
	specsMu                 sync.RWMutex
	migrationMu             sync.Mutex
}

var systemctlPath = "/usr/bin/systemctl"

func DefaultSpecs() map[core.Engine]EngineSpec {
	return map[core.Engine]EngineSpec{
		core.EngineMihomo:          {Binary: "/usr/local/lib/qagent/cores/mihomo", ConfigPath: "/etc/qagent/mihomo/config.yaml", Service: "qagent-mihomo.service"},
		core.EngineXray:            {Binary: "/usr/local/lib/qagent/cores/xray", ConfigPath: "/etc/qagent/xray/config.json", Service: "qagent-xray.service"},
		core.EngineSingBox:         {Binary: "/usr/local/lib/qagent/cores/sing-box", ConfigPath: "/etc/qagent/sing-box/config.json", Service: "qagent-sing-box.service"},
		core.EngineShadowsocksRust: {Binary: "/usr/local/lib/qagent/cores/ssserver", ConfigPath: "/etc/qagent/shadowsocks-rust/config.json", Service: "qagent-shadowsocks-rust.service"},
	}
}

func (e *Executor) Validate() error {
	if e == nil {
		return errors.New("agent executor is required")
	}
	if os.Geteuid() != 0 {
		return errors.New("Agent execution must run as root")
	}
	if err := validatePrivilegedExecutable(systemctlPath); err != nil {
		return fmt.Errorf("unsafe systemctl binary: %w", err)
	}
	if len(e.ExistingSpecs) > 0 && strings.TrimSpace(e.MigrationMarkerPrefix) == "" {
		return errors.New("existing core mappings require a migration state path")
	}
	for engine, spec := range e.Specs {
		if !engine.Valid() {
			return fmt.Errorf("invalid executor engine %q", engine)
		}
		if !safeServiceName(spec.Service) {
			return fmt.Errorf("unsafe systemd service name %q", spec.Service)
		}
		if !filepath.IsAbs(spec.Binary) || !filepath.IsAbs(spec.ConfigPath) {
			return fmt.Errorf("live executor paths for %s must be absolute", engine)
		}
		if err := validatePrivilegedExecutable(spec.Binary); err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("unsafe %s binary: %w", engine, err)
			}
			if err := validateCoreInstallDestination(spec.Binary); err != nil {
				return fmt.Errorf("unsafe %s install destination: %w", engine, err)
			}
		}
	}
	for engine, spec := range e.ExistingSpecs {
		if _, enabled := e.Specs[engine]; !enabled {
			return fmt.Errorf("existing %s mapping is not an enabled engine", engine)
		}
		if !supportedExistingService(engine, spec.Service) {
			return fmt.Errorf("unsupported existing %s service %q", engine, spec.Service)
		}
		if !filepath.IsAbs(spec.Binary) || !filepath.IsAbs(spec.ConfigPath) {
			return fmt.Errorf("existing %s paths must be absolute", engine)
		}
		for label, path := range map[string]string{
			"binary": spec.Binary, "configuration": spec.ConfigPath,
			"configuration directory": spec.ConfigDirectory, "service executable": existingServiceBinary(spec),
		} {
			if strings.ContainsAny(path, " \t\r\n") {
				return fmt.Errorf("existing %s %s path contains unsupported whitespace", engine, label)
			}
		}
		if spec.ConfigDirectory != "" && (engine != core.EngineSingBox || !filepath.IsAbs(spec.ConfigDirectory)) {
			return fmt.Errorf("existing %s configuration directory is unsupported or not absolute", engine)
		}
		if err := validatePrivilegedExecutable(spec.Binary); err != nil {
			return fmt.Errorf("unsafe existing %s binary: %w", engine, err)
		}
		if err := validateProtectedDirectoryChain(filepath.Dir(spec.Binary)); err != nil {
			return fmt.Errorf("unsafe existing %s binary parent chain: %w", engine, err)
		}
		if err := validateExistingServiceExecutable(spec); err != nil {
			return fmt.Errorf("unsafe existing %s service executable: %w", engine, err)
		}
	}
	return nil
}

func supportedExistingService(engine core.Engine, service string) bool {
	return (engine == core.EngineXray && service == "xray.service") ||
		(engine == core.EngineSingBox && (service == "sing-box.service" || service == "singbox.service"))
}

func validatePrivilegedExecutable(path string) error {
	if !filepath.IsAbs(path) {
		return errors.New("executable path is not absolute")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return errors.New("executable must be a regular, non-symlink file")
	}
	if info.Mode().Perm()&0o111 == 0 {
		return errors.New("file is not executable")
	}
	if info.Mode().Perm()&0o022 != 0 {
		return errors.New("executable is writable by group or others")
	}
	if err := validateOwner(info, "privileged executable"); err != nil {
		return err
	}
	directoryInfo, err := os.Lstat(filepath.Dir(path))
	if err != nil {
		return err
	}
	if directoryInfo.Mode()&os.ModeSymlink != 0 || !directoryInfo.IsDir() || directoryInfo.Mode().Perm()&0o022 != 0 {
		return errors.New("executable directory is symlinked or writable by group/others")
	}
	return validateOwner(directoryInfo, "executable directory")
}

func existingServiceBinary(spec EngineSpec) string {
	if spec.ServiceBinary != "" {
		return spec.ServiceBinary
	}
	return spec.Binary
}

// validateExistingServiceExecutable accepts the real core directly, a symlink
// to that core, or one narrowly defined forwarding script. The forwarding
// script must contain exactly a /bin/sh shebang and an unconditional
// `exec <protected-real-core> "$@"`; arbitrary wrappers are never invoked or
// copied into the managed core namespace.
func validateExistingServiceExecutable(spec EngineSpec) error {
	serviceBinary := existingServiceBinary(spec)
	if strings.ContainsAny(serviceBinary+spec.Binary, " \t\r\n") {
		return errors.New("service executable mapping contains unsupported whitespace")
	}
	if serviceBinary == spec.Binary {
		if err := validateProtectedDirectoryChain(filepath.Dir(spec.Binary)); err != nil {
			return fmt.Errorf("service executable parent chain: %w", err)
		}
		return validatePrivilegedExecutable(spec.Binary)
	}
	if !filepath.IsAbs(serviceBinary) {
		return errors.New("service executable path is not absolute")
	}
	if err := validateProtectedDirectoryChain(filepath.Dir(serviceBinary)); err != nil {
		return fmt.Errorf("service executable parent chain: %w", err)
	}
	serviceInfo, err := os.Lstat(serviceBinary)
	if err != nil {
		return err
	}
	if serviceInfo.Mode()&os.ModeSymlink == 0 {
		return errors.New("alternate service executable must be a symlink")
	}
	resolved, err := os.Readlink(serviceBinary)
	if err != nil {
		return err
	}
	if !filepath.IsAbs(resolved) {
		resolved = filepath.Join(filepath.Dir(serviceBinary), resolved)
	}
	resolved = filepath.Clean(resolved)
	resolvedInfo, err := os.Lstat(resolved)
	if err != nil {
		return err
	}
	if resolvedInfo.Mode()&os.ModeSymlink != 0 {
		return errors.New("service executable must use at most one symlink")
	}
	if resolved == spec.Binary {
		if err := validateProtectedDirectoryChain(filepath.Dir(spec.Binary)); err != nil {
			return fmt.Errorf("real core parent chain: %w", err)
		}
		return validateNativeCoreExecutable(spec.Binary)
	}
	if err := validateProtectedDirectoryChain(filepath.Dir(resolved)); err != nil {
		return fmt.Errorf("forwarder parent chain: %w", err)
	}
	if err := validatePrivilegedExecutable(resolved); err != nil {
		return fmt.Errorf("forwarder script: %w", err)
	}
	contents, err := os.ReadFile(resolved)
	if err != nil {
		return err
	}
	if len(contents) > 1024 {
		return errors.New("forwarder script exceeds the supported fixed form")
	}
	want := "#!/bin/sh\nexec " + spec.Binary + " \"$@\"\n"
	if string(contents) != want {
		return errors.New("service wrapper is not the supported fixed exec forwarder")
	}
	if err := validateProtectedDirectoryChain(filepath.Dir(spec.Binary)); err != nil {
		return fmt.Errorf("real core parent chain: %w", err)
	}
	return validateNativeCoreExecutable(spec.Binary)
}

func validateNativeCoreExecutable(path string) error {
	if err := validatePrivilegedExecutable(path); err != nil {
		return err
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	prefix := make([]byte, 2)
	if _, err := io.ReadFull(file, prefix); err != nil {
		return err
	}
	if string(prefix) == "#!" {
		return errors.New("real core executable must not be a script")
	}
	return nil
}

func validateProtectedDirectoryChain(directory string) error {
	if !filepath.IsAbs(directory) {
		return errors.New("path is not absolute")
	}
	for {
		info, err := os.Lstat(directory)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("%s is not a real directory", directory)
		}
		if info.Mode().Perm()&0o022 != 0 {
			// A sticky directory is a safe traversal boundary: another user
			// cannot replace a protected child entry. This keeps tests and
			// deliberately staged installations below /tmp safe without
			// accepting a writable non-sticky parent.
			if info.Mode()&os.ModeSticky != 0 {
				if err := validateOwnerOrRoot(info, "sticky protected path parent"); err != nil {
					return err
				}
				return nil
			}
			return fmt.Errorf("%s is writable by group or others", directory)
		}
		if err := validateOwner(info, "protected path parent"); err != nil {
			return err
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			return nil
		}
		directory = parent
	}
}

func (e *Executor) Execute(parent context.Context, task core.Task) (string, error) {
	if !task.Action.Valid() || !task.Engine.Valid() {
		return "", errors.New("task contains an unsupported action or engine")
	}
	e.specsMu.RLock()
	spec, ok := e.Specs[task.Engine]
	existing, hasExisting := e.ExistingSpecs[task.Engine]
	e.specsMu.RUnlock()
	if !ok {
		return "", fmt.Errorf("engine %s is not enabled on this agent", task.Engine)
	}
	if !safeServiceName(spec.Service) {
		return "", errors.New("configured systemd service name is unsafe")
	}
	timeout := 45 * time.Second
	if task.Action == core.ActionInstall {
		timeout = 4 * time.Minute
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	switch task.Action {
	case core.ActionReadConfig:
		if hasExisting {
			return e.readExistingConfig(ctx, task.Engine, spec, existing)
		}
		return e.readCurrentConfig(ctx, task.Engine, spec)
	case core.ActionImportExisting:
		if !hasExisting {
			completed, err := completedCoreMigrationMatches(e.MigrationMarkerPrefix, task.Engine, task.ConfigContent)
			if err != nil {
				return "", fmt.Errorf("check completed %s migration: %w", task.Engine, err)
			}
			if completed {
				return fmt.Sprintf("%s existing service migration was already completed with this configuration", task.Engine), nil
			}
			return "", fmt.Errorf("%s has no existing service pending manual import", task.Engine)
		}
		return e.importExistingConfig(ctx, task.Engine, spec, existing, task.ConfigContent)
	case core.ActionValidate:
		if hasExisting {
			return "", errors.New("import the existing configuration before validating managed changes")
		}
		return e.validate(ctx, task.Engine, spec, task.ConfigContent)
	case core.ActionDeploy:
		if hasExisting {
			return "", errors.New("import the existing configuration before deploying managed changes")
		}
		validation, err := e.validate(ctx, task.Engine, spec, task.ConfigContent)
		if err != nil {
			return validation, err
		}
		if err := ensureManagedCoreServiceCapabilities(ctx, task.Engine, spec); err != nil {
			return validation, err
		}
		backup, err := atomicDeploy(spec.ConfigPath, task.ConfigContent)
		if err != nil {
			return validation, err
		}
		restartOutput, err := serviceCommandAndVerify(ctx, spec.Service, core.ActionRestart)
		output := validation + "\ndeployed to " + spec.ConfigPath
		if backup != "" {
			output += "\nbackup: " + backup
		}
		if restartOutput != "" {
			output += "\n" + restartOutput
		}
		if err != nil {
			rollbackOutput, rollbackErr := rollbackDeploy(spec.ConfigPath, backup)
			if rollbackOutput != "" {
				output += "\n" + rollbackOutput
			}
			if rollbackErr != nil {
				return output, fmt.Errorf("configuration deployed but service restart failed (%v); rollback also failed: %w", err, rollbackErr)
			}
			recoveryContext, recoveryCancel := context.WithTimeout(context.Background(), 30*time.Second)
			recoveryOutput, recoveryErr := serviceCommandAndVerify(recoveryContext, spec.Service, core.ActionRestart)
			recoveryCancel()
			if recoveryOutput != "" {
				output += "\nrollback restart: " + recoveryOutput
			}
			if recoveryErr != nil {
				return output, fmt.Errorf("configuration restart failed (%v); previous file was restored but service recovery failed: %w", err, recoveryErr)
			}
			return output, fmt.Errorf("configuration restart failed and the previous configuration was restored: %w", err)
		}
		return output, nil
	case core.ActionStart, core.ActionRestart:
		if hasExisting {
			return "", errors.New("import the existing configuration before starting the QAgent service")
		}
		if err := ensureManagedCoreServiceCapabilities(ctx, task.Engine, spec); err != nil {
			return "", err
		}
		return serviceCommandAndVerify(ctx, spec.Service, task.Action)
	case core.ActionStop:
		if hasExisting {
			return "", errors.New("import the existing configuration before changing service state")
		}
		return serviceCommandAndVerify(ctx, spec.Service, task.Action)
	case core.ActionStatus:
		if hasExisting {
			return serviceStatus(ctx, existing.Service)
		}
		return serviceStatus(ctx, spec.Service)
	case core.ActionInstall:
		if hasExisting {
			return "", errors.New("import the existing configuration before installing a managed core")
		}
		version, err := core.NormalizeCoreVersionSelector(task.CoreVersion)
		if err != nil {
			return "", err
		}
		if err := ensureManagedCoreServiceCapabilities(ctx, task.Engine, spec); err != nil {
			return "", err
		}
		updater := e.Updater
		if updater == nil {
			updater = NewCoreUpdater()
		}
		return updater.Install(ctx, task.Engine, spec, version)
	default:
		return "", fmt.Errorf("unsupported action %q", task.Action)
	}
}

func (e *Executor) readCurrentConfig(ctx context.Context, engine core.Engine, spec EngineSpec) (string, error) {
	content, err := readConfigurationFile(spec.ConfigPath)
	if err != nil {
		return "", fmt.Errorf("read current %s configuration: %w", engine, err)
	}
	if err := validatePrivilegedExecutable(spec.Binary); err != nil {
		return "", fmt.Errorf("cannot safely invoke %s for current configuration validation: %w", engine, err)
	}
	_, err = e.validateSnapshot(ctx, engine, spec, content)
	if err != nil {
		return "", fmt.Errorf("current %s configuration failed real core validation: %w", engine, err)
	}
	return content, nil
}

func (e *Executor) readExistingConfig(ctx context.Context, engine core.Engine, managed, existing EngineSpec) (string, error) {
	content, sourceDigest, err := readExistingConfigurationSources(existing)
	if err != nil {
		return "", fmt.Errorf("read existing %s configuration: %w", engine, err)
	}
	validationSpec := managed
	validationSpec.Binary = existing.Binary
	if err := validatePrivilegedExecutable(existing.Binary); err != nil {
		return "", fmt.Errorf("cannot safely invoke existing %s binary: %w", engine, err)
	}
	if err := validateProtectedDirectoryChain(filepath.Dir(existing.Binary)); err != nil {
		return "", fmt.Errorf("cannot safely traverse existing %s binary path: %w", engine, err)
	}
	if err := validateExistingServiceExecutable(existing); err != nil {
		return "", fmt.Errorf("cannot safely map existing %s service executable: %w", engine, err)
	}
	if err := validateExistingSourceInvocation(ctx, engine, existing); err != nil {
		return "", fmt.Errorf("existing %s configuration sources failed real core validation: %w", engine, err)
	}
	if _, err := e.validateSnapshot(ctx, engine, validationSpec, content); err != nil {
		return "", fmt.Errorf("existing %s configuration failed real core validation: %w", engine, err)
	}
	_, currentDigest, err := readExistingConfigurationSources(existing)
	if err != nil || currentDigest != sourceDigest {
		if err == nil {
			err = errors.New("configuration sources changed while the snapshot was validated")
		}
		return "", fmt.Errorf("recheck existing %s configuration sources: %w", engine, err)
	}
	return content, nil
}

func readExistingConfigurationSources(spec EngineSpec) (string, string, error) {
	primary, err := readConfigurationFile(spec.ConfigPath)
	if err != nil {
		return "", "", err
	}
	if spec.ConfigDirectory == "" {
		digest := sha256.Sum256([]byte(spec.ConfigPath + "\x00" + primary))
		return primary, hex.EncodeToString(digest[:]), nil
	}
	sources := []existingConfigSource{{path: spec.ConfigPath, content: primary}}
	if spec.ConfigDirectory != "" {
		if err := validateProtectedDirectoryChain(spec.ConfigDirectory); err != nil {
			return "", "", fmt.Errorf("configuration directory parent chain is unsafe: %w", err)
		}
		entries, err := os.ReadDir(spec.ConfigDirectory)
		if err != nil {
			return "", "", err
		}
		for _, entry := range entries {
			if !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			if entry.Type()&os.ModeSymlink != 0 || entry.IsDir() {
				return "", "", fmt.Errorf("configuration directory entry %q is not a regular non-symlink JSON file", entry.Name())
			}
			path := filepath.Join(spec.ConfigDirectory, entry.Name())
			contents, err := readConfigurationFile(path)
			if err != nil {
				return "", "", fmt.Errorf("read configuration directory entry %q: %w", entry.Name(), err)
			}
			sources = append(sources, existingConfigSource{path: path, content: contents})
		}
	}
	sort.SliceStable(sources, func(i, j int) bool { return sources[i].path < sources[j].path })
	total := 0
	digest := sha256.New()
	var merged any
	for _, source := range sources {
		total += len(source.content)
		if total > core.MaxConfigBytes {
			return "", "", fmt.Errorf("combined configuration sources exceed %d bytes", core.MaxConfigBytes)
		}
		digest.Write([]byte(source.path))
		digest.Write([]byte{0})
		digest.Write([]byte(source.content))
		digest.Write([]byte{0})
		var decoded any
		decoder := json.NewDecoder(strings.NewReader(source.content))
		decoder.UseNumber()
		if err := decoder.Decode(&decoded); err != nil {
			return "", "", fmt.Errorf("decode configuration source %q: %w", source.path, err)
		}
		if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
			return "", "", fmt.Errorf("decode configuration source %q: trailing data", source.path)
		}
		if merged == nil {
			merged = decoded
		} else {
			merged, err = mergeSingBoxJSON(decoded, merged)
			if err != nil {
				return "", "", fmt.Errorf("merge configuration source %q: %w", source.path, err)
			}
		}
	}
	contents, err := json.MarshalIndent(merged, "", "  ")
	if err != nil {
		return "", "", err
	}
	contents = append(contents, '\n')
	if len(contents) > core.MaxConfigBytes {
		return "", "", fmt.Errorf("merged configuration exceeds %d bytes", core.MaxConfigBytes)
	}
	return string(contents), hex.EncodeToString(digest.Sum(nil)), nil
}

type existingConfigSource struct {
	path    string
	content string
}

// mergeSingBoxJSON mirrors sing-box's ordered badjson merge: an existing
// destination wins scalar conflicts, objects merge recursively, and source
// arrays append after the destination array. Paths are sorted before this
// function is called.
func mergeSingBoxJSON(source, destination any) (any, error) {
	if source == nil {
		return destination, nil
	}
	if destination == nil {
		return source, nil
	}
	switch current := destination.(type) {
	case []any:
		if values, ok := source.([]any); ok {
			return append(current, values...), nil
		}
		return append(current, source), nil
	case map[string]any:
		values, ok := source.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("cannot merge JSON object with %T", source)
		}
		for key, value := range values {
			if previous, exists := current[key]; exists {
				var err error
				value, err = mergeSingBoxJSON(value, previous)
				if err != nil {
					return nil, err
				}
			}
			current[key] = value
		}
		return current, nil
	default:
		return destination, nil
	}
}

func validateExistingSourceInvocation(ctx context.Context, engine core.Engine, spec EngineSpec) error {
	if engine != core.EngineSingBox || spec.ConfigDirectory == "" {
		return nil
	}
	_, err := runInDirectory(ctx, filepath.Dir(spec.ConfigPath), spec.Binary,
		"check", "-c", spec.ConfigPath, "-C", spec.ConfigDirectory)
	return err
}

// ReadCurrentConfig returns an exact, real-core-validated snapshot for explicit
// inspection in the manual configuration page. It does not save or deploy it.
func (e *Executor) ReadCurrentConfig(ctx context.Context, engine core.Engine) (string, error) {
	spec, ok := e.Specs[engine]
	if !ok {
		return "", fmt.Errorf("engine %s is not enabled", engine)
	}
	return e.readCurrentConfig(ctx, engine, spec)
}

func readConfigurationFile(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", errors.New("configuration path is not absolute")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", errors.New("configuration must be a regular, non-symlink file")
	}
	if info.Mode().Perm()&0o022 != 0 {
		return "", errors.New("configuration is writable by group or others")
	}
	if err := validateOwner(info, "configuration file"); err != nil {
		return "", err
	}
	directory := filepath.Dir(path)
	if err := validateProtectedDirectoryChain(directory); err != nil {
		return "", fmt.Errorf("configuration parent chain is unsafe: %w", err)
	}
	directoryInfo, err := os.Lstat(directory)
	if err != nil {
		return "", err
	}
	if directoryInfo.Mode()&os.ModeSymlink != 0 || !directoryInfo.IsDir() || directoryInfo.Mode().Perm()&0o022 != 0 {
		return "", errors.New("configuration directory is symlinked or writable by group/others")
	}
	if err := validateOwner(directoryInfo, "configuration directory"); err != nil {
		return "", err
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return "", err
	}
	defer root.Close()
	file, err := root.Open(filepath.Base(path))
	if err != nil {
		return "", err
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil {
		return "", err
	}
	if !os.SameFile(info, openedInfo) || !openedInfo.Mode().IsRegular() {
		return "", errors.New("configuration changed while it was being opened")
	}
	content, err := io.ReadAll(io.LimitReader(file, core.MaxConfigBytes+1))
	if err != nil {
		return "", err
	}
	if len(content) > core.MaxConfigBytes {
		return "", fmt.Errorf("configuration exceeds %d bytes", core.MaxConfigBytes)
	}
	if !utf8.Valid(content) {
		return "", errors.New("configuration is not valid UTF-8")
	}
	return string(content), nil
}

func validateConfigurationPath(ctx context.Context, engine core.Engine, binary, configPath string) (string, error) {
	var args []string
	switch engine {
	case core.EngineMihomo:
		args = []string{"-t", "-f", configPath}
	case core.EngineXray:
		args = []string{"run", "-test", "-config", configPath}
	case core.EngineSingBox:
		args = []string{"check", "-c", configPath}
	case core.EngineShadowsocksRust:
		return "ss-rust configuration syntax validated (ssserver has no non-running check mode)", nil
	default:
		return "", fmt.Errorf("unsupported engine %q", engine)
	}
	return runInDirectory(ctx, filepath.Dir(configPath), binary, args...)
}

func (e *Executor) Runtime(ctx context.Context) map[core.Engine]core.RuntimeState {
	e.specsMu.RLock()
	specs := make(map[core.Engine]EngineSpec, len(e.Specs))
	existingSpecs := make(map[core.Engine]EngineSpec, len(e.ExistingSpecs))
	discoveryIssues := make(map[core.Engine]string, len(e.ExistingDiscoveryIssues))
	for engine, spec := range e.Specs {
		specs[engine] = spec
	}
	for engine, spec := range e.ExistingSpecs {
		existingSpecs[engine] = spec
	}
	for engine, issue := range e.ExistingDiscoveryIssues {
		discoveryIssues[engine] = issue
	}
	e.specsMu.RUnlock()
	result := make(map[core.Engine]core.RuntimeState, len(specs))
	for engine, spec := range specs {
		state := core.RuntimeState{}
		if issue := discoveryIssues[engine]; issue != "" {
			state.ServiceStatus = "active"
			state.ExistingConfigUnsupportedReason = issue
			result[engine] = state
			continue
		}
		if existing, ok := existingSpecs[engine]; ok {
			spec = existing
			state.ExistingConfigAvailable = true
		}
		if path, err := exec.LookPath(spec.Binary); err == nil {
			state.Installed = true
			state.Version = binaryVersion(ctx, engine, path)
		}
		if status, err := serviceStatus(ctx, spec.Service); err == nil {
			state.ServiceStatus = strings.TrimSpace(status)
		} else {
			state.ServiceStatus = "unknown"
		}
		result[engine] = state
	}
	return result
}

func (e *Executor) validate(ctx context.Context, engine core.Engine, spec EngineSpec, content string) (string, error) {
	if err := core.ValidateConfig(engine, content); err != nil {
		return "", err
	}
	if err := validateNoPersistentCoreLogs(engine, content); err != nil {
		return "", err
	}
	return e.validateSnapshot(ctx, engine, spec, content)
}

func (e *Executor) validateSnapshot(ctx context.Context, engine core.Engine, spec EngineSpec, content string) (string, error) {
	if err := core.ValidateConfig(engine, content); err != nil {
		return "", err
	}
	if _, err := exec.LookPath(spec.Binary); err != nil {
		return "", fmt.Errorf("%s binary not found in PATH", spec.Binary)
	}
	extension := ".json"
	if engine == core.EngineMihomo {
		extension = ".yaml"
	}
	directory := filepath.Dir(spec.ConfigPath)
	if err := os.MkdirAll(directory, 0o750); err != nil {
		return "", fmt.Errorf("create configuration directory for validation: %w", err)
	}
	directoryInfo, err := os.Lstat(directory)
	if err != nil {
		return "", fmt.Errorf("open configuration directory for validation: %w", err)
	}
	if directoryInfo.Mode()&os.ModeSymlink != 0 || !directoryInfo.IsDir() || directoryInfo.Mode().Perm()&0o022 != 0 {
		return "", errors.New("configuration directory for validation is unsafe")
	}
	if err := validateOwner(directoryInfo, "configuration directory"); err != nil {
		return "", err
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return "", err
	}
	defer root.Close()
	suffix, err := randomSuffix(10)
	if err != nil {
		return "", err
	}
	tempName := ".qcontrolhub-validate-" + suffix + extension
	temp, err := root.OpenFile(tempName, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	tempPath := filepath.Join(directory, tempName)
	defer root.Remove(tempName)
	if _, err := temp.WriteString(content); err != nil {
		temp.Close()
		return "", err
	}
	if err := temp.Close(); err != nil {
		return "", err
	}

	var args []string
	switch engine {
	case core.EngineMihomo:
		args = []string{"-t", "-f", tempPath}
	case core.EngineXray:
		args = []string{"run", "-test", "-config", tempPath}
	case core.EngineSingBox:
		args = []string{"check", "-c", tempPath}
	case core.EngineShadowsocksRust:
		return "ss-rust configuration syntax validated (ssserver has no non-running check mode)", nil
	}
	output, err := runInDirectory(ctx, directory, spec.Binary, args...)
	if err != nil {
		return output, fmt.Errorf("%s rejected the configuration: %w", engine, err)
	}
	if output == "" {
		output = fmt.Sprintf("%s validation passed", engine)
	}
	return output, nil
}

func validateNoPersistentCoreLogs(engine core.Engine, content string) error {
	if engine != core.EngineXray && engine != core.EngineSingBox {
		return nil
	}
	var root map[string]any
	if err := json.Unmarshal([]byte(content), &root); err != nil {
		return err
	}
	logging, _ := root["log"].(map[string]any)
	if logging == nil {
		return nil
	}
	keys := []string{"output"}
	if engine == core.EngineXray {
		keys = []string{"access", "error"}
	}
	for _, key := range keys {
		value, _ := logging[key].(string)
		value = strings.TrimSpace(value)
		if value != "" && (engine != core.EngineXray || !strings.EqualFold(value, "none")) {
			return fmt.Errorf("persistent %s log output %q is disabled; managed core logs are stored by the control plane", engine, key)
		}
	}
	return nil
}

func serviceCommand(ctx context.Context, service string, action core.Action) (string, error) {
	if service == "" {
		return "", errors.New("service name is not configured")
	}
	if action != core.ActionStart && action != core.ActionStop && action != core.ActionRestart {
		return "", errors.New("unsupported service action")
	}
	output, err := run(ctx, systemctlPath, string(action), service)
	if err != nil {
		return output, fmt.Errorf("systemctl %s %s: %w", action, service, err)
	}
	if output == "" {
		output = fmt.Sprintf("systemctl %s %s completed", action, service)
	}
	return output, nil
}

func serviceCommandAndVerify(ctx context.Context, service string, action core.Action) (string, error) {
	output, err := serviceCommand(ctx, service, action)
	if err != nil {
		return output, err
	}
	expected := "active"
	stableFor := 500 * time.Millisecond
	if action == core.ActionStop {
		expected = "inactive"
		stableFor = 0
	}
	verifyContext, verifyCancel := context.WithTimeout(ctx, 5*time.Second)
	status, statusErr := waitForServiceState(verifyContext, expected, stableFor, 100*time.Millisecond, func(probeContext context.Context) (string, error) {
		return serviceStatus(probeContext, service)
	})
	verifyCancel()
	if statusErr != nil {
		return output, fmt.Errorf("verify systemd service %s after %s: %w", service, action, statusErr)
	}
	if status != expected {
		return output + "\nservice status: " + status, fmt.Errorf("systemd service %s is %s after %s, expected %s", service, status, action, expected)
	}
	return output + "\nservice status: " + status, nil
}

type serviceStatusProbe func(context.Context) (string, error)

func waitForServiceState(ctx context.Context, expected string, stableFor, pollEvery time.Duration, probe serviceStatusProbe) (string, error) {
	if probe == nil || pollEvery <= 0 || stableFor < 0 {
		return "", errors.New("invalid service verification settings")
	}
	ticker := time.NewTicker(pollEvery)
	defer ticker.Stop()
	var stableSince time.Time
	lastStatus := "unknown"
	for {
		status, err := probe(ctx)
		if err != nil {
			return lastStatus, err
		}
		lastStatus = status
		if status == expected {
			if stableFor == 0 {
				return status, nil
			}
			if stableSince.IsZero() {
				stableSince = time.Now()
			}
			if time.Since(stableSince) >= stableFor {
				return status, nil
			}
		} else {
			stableSince = time.Time{}
			if status == "failed" || status == "inactive" {
				return status, nil
			}
		}
		select {
		case <-ctx.Done():
			return lastStatus, ctx.Err()
		case <-ticker.C:
		}
	}
}

func serviceStatus(ctx context.Context, service string) (string, error) {
	if !safeServiceName(service) {
		return "", errors.New("configured systemd service name is unsafe")
	}
	output, err := run(ctx, systemctlPath, "is-active", service)
	if err != nil {
		trimmed := strings.TrimSpace(output)
		if trimmed == "inactive" || trimmed == "failed" || trimmed == "activating" || trimmed == "deactivating" {
			return trimmed, nil
		}
		return output, err
	}
	return strings.TrimSpace(output), nil
}

func binaryVersion(ctx context.Context, engine core.Engine, binary string) string {
	args := []string{"version"}
	if engine == core.EngineMihomo {
		args = []string{"-v"}
	} else if engine == core.EngineShadowsocksRust {
		args = []string{"--version"}
	}
	output, err := run(ctx, binary, args...)
	if err != nil {
		return "unknown"
	}
	if line, _, found := strings.Cut(output, "\n"); found {
		output = line
	}
	if len(output) > 160 {
		output = output[:160]
	}
	return strings.TrimSpace(output)
}

func run(ctx context.Context, name string, args ...string) (string, error) {
	return runInDirectory(ctx, "", name, args...)
}

func runInDirectory(ctx context.Context, directory, name string, args ...string) (string, error) {
	if !filepath.IsAbs(name) {
		return "", errors.New("refusing to execute a non-absolute binary path")
	}
	commandContext, cancel := context.WithCancel(ctx)
	defer cancel()
	command := exec.CommandContext(commandContext, name, args...)
	if directory != "" {
		command.Dir = directory
	}
	command.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C", "LC_ALL=C"}
	configureCommand(command)
	output := &boundedOutput{limit: 64 << 10, onLimit: cancel}
	command.Stdout = output
	command.Stderr = output
	err := command.Run()
	value := strings.TrimSpace(strings.ToValidUTF8(output.String(), "�"))
	if output.Truncated() {
		value += "\n… process terminated after exceeding the 64 KiB output limit"
		if ctx.Err() == nil {
			err = errors.New("command output limit exceeded")
		}
	}
	if ctx.Err() != nil {
		return value, ctx.Err()
	}
	return value, err
}

type boundedOutput struct {
	mu        sync.Mutex
	contents  []byte
	limit     int
	truncated bool
	onLimit   func()
}

func (w *boundedOutput) Write(input []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	originalLength := len(input)
	remaining := w.limit - len(w.contents)
	if remaining > 0 {
		if len(input) > remaining {
			input = input[:remaining]
		}
		w.contents = append(w.contents, input...)
	}
	if originalLength > remaining && !w.truncated {
		w.truncated = true
		if w.onLimit != nil {
			go w.onLimit()
		}
	}
	return originalLength, nil
}

func (w *boundedOutput) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return string(append([]byte(nil), w.contents...))
}

func (w *boundedOutput) Truncated() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.truncated
}

func safeServiceName(value string) bool {
	if value == "" || strings.HasPrefix(value, "-") || len(value) > 128 {
		return false
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || strings.ContainsRune("_.@:-", character) {
			continue
		}
		return false
	}
	return true
}

func atomicDeploy(destination, content string) (string, error) {
	if destination == "" || !filepath.IsAbs(destination) {
		return "", errors.New("configuration destination must be an absolute path")
	}
	directory := filepath.Dir(destination)
	if err := os.MkdirAll(directory, 0o750); err != nil {
		return "", fmt.Errorf("create configuration directory: %w", err)
	}
	directoryInfo, err := os.Lstat(directory)
	if err != nil {
		return "", err
	}
	if directoryInfo.Mode()&os.ModeSymlink != 0 || !directoryInfo.IsDir() {
		return "", errors.New("configuration directory must be a real directory, not a symlink")
	}
	if directoryInfo.Mode().Perm()&0o022 != 0 {
		return "", errors.New("configuration directory must not be writable by group or others")
	}
	if err := validateOwner(directoryInfo, "configuration directory"); err != nil {
		return "", err
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return "", fmt.Errorf("open configuration directory: %w", err)
	}
	defer root.Close()
	baseName := filepath.Base(destination)
	metadata := fileMetadata{mode: 0o600}
	var backup string
	var backupName string
	if info, err := root.Lstat(baseName); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("refusing to replace a symlinked configuration file")
		}
		if !info.Mode().IsRegular() {
			return "", errors.New("configuration destination is not a regular file")
		}
		if err := validateOwner(info, "configuration file"); err != nil {
			return "", err
		}
		if info.Mode().Perm()&0o022 != 0 {
			return "", errors.New("configuration file must not be writable by group or others")
		}
		metadata = metadataFromFileInfo(info)
		suffix, err := randomSuffix(6)
		if err != nil {
			return "", err
		}
		backupName = baseName + ".bak-" + time.Now().UTC().Format("20060102T150405Z") + "-" + suffix
		backup = filepath.Join(directory, backupName)
		if err := copyFileInRoot(root, baseName, backupName, metadata); err != nil {
			return "", fmt.Errorf("back up current configuration: %w", err)
		}
		if err := cleanupBackups(root, baseName, backupName, 3); err != nil {
			_ = root.Remove(backupName)
			return "", fmt.Errorf("clean up old configuration backups: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}

	suffix, err := randomSuffix(10)
	if err != nil {
		return backup, err
	}
	tempName := ".qcontrolhub-config-" + suffix + ".tmp"
	temp, err := root.OpenFile(tempName, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return backup, err
	}
	defer root.Remove(tempName)
	if _, err := temp.WriteString(content); err != nil {
		temp.Close()
		return backup, err
	}
	if err := applyFileMetadata(temp, metadata); err != nil {
		temp.Close()
		return backup, fmt.Errorf("preserve configuration metadata: %w", err)
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return backup, err
	}
	if err := temp.Close(); err != nil {
		return backup, err
	}
	if err := root.Rename(tempName, baseName); err != nil {
		return backup, err
	}
	if err := syncRootDirectory(root); err != nil {
		return backup, fmt.Errorf("sync configuration directory: %w", err)
	}
	return backup, nil
}

func syncRootDirectory(root *os.Root) error {
	directory, err := root.Open(".")
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

type fileMetadata struct {
	mode           os.FileMode
	uid            int
	gid            int
	ownershipKnown bool
}

func metadataFromFileInfo(info os.FileInfo) fileMetadata {
	uid, gid, known := fileOwnership(info)
	return fileMetadata{mode: info.Mode().Perm(), uid: uid, gid: gid, ownershipKnown: known}
}

func applyFileMetadata(file *os.File, metadata fileMetadata) error {
	if metadata.ownershipKnown {
		if err := file.Chown(metadata.uid, metadata.gid); err != nil {
			return err
		}
	}
	return file.Chmod(metadata.mode)
}

func applyRootFileMetadata(root *os.Root, name string, metadata fileMetadata) error {
	if metadata.ownershipKnown {
		if err := root.Chown(name, metadata.uid, metadata.gid); err != nil {
			return err
		}
	}
	return root.Chmod(name, metadata.mode)
}

func copyFileInRoot(root *os.Root, source, destination string, metadata fileMetadata) (err error) {
	input, err := root.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := root.OpenFile(destination, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = root.Remove(destination)
		}
	}()
	if _, err := io.Copy(output, input); err != nil {
		output.Close()
		return err
	}
	if err := applyFileMetadata(output, metadata); err != nil {
		output.Close()
		return err
	}
	if err := output.Sync(); err != nil {
		output.Close()
		return err
	}
	return output.Close()
}

func rollbackDeploy(destination, backup string) (string, error) {
	directory := filepath.Dir(destination)
	root, err := os.OpenRoot(directory)
	if err != nil {
		return "", err
	}
	defer root.Close()
	destinationName := filepath.Base(destination)
	if backup == "" {
		if err := root.Remove(destinationName); err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		if err := syncRootDirectory(root); err != nil {
			return "", err
		}
		return "rollback: removed newly created configuration", nil
	}
	if filepath.Dir(backup) != directory {
		return "", errors.New("backup is outside the configuration directory")
	}
	backupName := filepath.Base(backup)
	if !strings.HasPrefix(backupName, destinationName+".bak-") {
		return "", errors.New("backup name does not match configuration")
	}
	if err := root.Rename(backupName, destinationName); err != nil {
		return "", err
	}
	if err := syncRootDirectory(root); err != nil {
		return "", err
	}
	return "rollback: previous configuration restored", nil
}

func cleanupBackups(root *os.Root, baseName, preserve string, keep int) error {
	directory, err := root.Open(".")
	if err != nil {
		return err
	}
	entries, err := directory.ReadDir(-1)
	directory.Close()
	if err != nil {
		return err
	}
	prefix := baseName + ".bak-"
	type backupEntry struct {
		name     string
		modified time.Time
	}
	backups := make([]backupEntry, 0)
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasPrefix(entry.Name(), prefix) {
			info, err := entry.Info()
			if err != nil {
				return err
			}
			backups = append(backups, backupEntry{name: entry.Name(), modified: info.ModTime()})
		}
	}
	sort.Slice(backups, func(left, right int) bool {
		if backups[left].modified.Equal(backups[right].modified) {
			return backups[left].name < backups[right].name
		}
		return backups[left].modified.Before(backups[right].modified)
	})
	for len(backups) > keep {
		removeIndex := 0
		if backups[removeIndex].name == preserve && len(backups) > 1 {
			removeIndex = 1
		}
		if err := root.Remove(backups[removeIndex].name); err != nil {
			return err
		}
		backups = append(backups[:removeIndex], backups[removeIndex+1:]...)
	}
	return nil
}

func randomSuffix(bytesCount int) (string, error) {
	value := make([]byte, bytesCount)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}
