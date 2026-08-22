package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/qimaoww/qcontrolhub/internal/core"
)

func (e *Executor) LoadCoreMigrationState() error {
	if e == nil || e.MigrationMarkerPrefix == "" || len(e.ExistingSpecs) == 0 {
		return nil
	}
	e.specsMu.Lock()
	defer e.specsMu.Unlock()
	for engine, existing := range e.ExistingSpecs {
		record, err := readCoreMigrationRecord(e.MigrationMarkerPrefix, engine)
		if err != nil {
			return err
		}
		if record.State == coreMigrationComplete && record.SourceDigest == coreMigrationSourceDigest(existing) {
			delete(e.ExistingSpecs, engine)
		}
	}
	return nil
}

// ReconcileExistingCoreServices closes the small crash window between a
// successful service switch and its durable marker. The existing service wins
// whenever it is still active; a stable managed service wins only after the
// existing service is already inactive.
func (e *Executor) ReconcileExistingCoreServices(ctx context.Context) error {
	if e == nil || len(e.ExistingSpecs) == 0 {
		return nil
	}
	e.migrationMu.Lock()
	defer e.migrationMu.Unlock()
	e.specsMu.RLock()
	pending := make(map[core.Engine]EngineSpec, len(e.ExistingSpecs))
	managed := make(map[core.Engine]EngineSpec, len(e.Specs))
	for engine, spec := range e.ExistingSpecs {
		pending[engine] = spec
		managed[engine] = e.Specs[engine]
	}
	e.specsMu.RUnlock()
	for engine, existing := range pending {
		migrationRecord, err := readCoreMigrationRecord(e.MigrationMarkerPrefix, engine)
		if err != nil {
			return err
		}
		if migrationRecord.State != coreMigrationInProgress {
			continue
		}
		if migrationRecord.SourceDigest != coreMigrationSourceDigest(existing) {
			return fmt.Errorf("existing %s mapping changed during an incomplete migration", engine)
		}
		existingStatus, err := serviceStatus(ctx, existing.Service)
		if err != nil {
			return err
		}
		managedStatus, err := serviceStatus(ctx, managed[engine].Service)
		if err != nil {
			return err
		}
		if !migrationEnableStatesSupported(migrationRecord.ExistingEnableState, migrationRecord.ManagedEnableState) {
			if err := restoreInterruptedCoreMigration(ctx, e.MigrationMarkerPrefix, engine, existing, managed[engine], migrationRecord, managedStatus); err != nil {
				return err
			}
			continue
		}
		if existingStatus == "active" {
			if err := restoreInterruptedCoreMigration(ctx, e.MigrationMarkerPrefix, engine, existing, managed[engine], migrationRecord, managedStatus); err != nil {
				return err
			}
			continue
		}
		if managedStatus == "active" {
			stableContext, stableCancel := context.WithTimeout(ctx, 5*time.Second)
			stableStatus, stableErr := waitForServiceState(stableContext, "active", 500*time.Millisecond, 100*time.Millisecond, func(probeContext context.Context) (string, error) {
				return serviceStatus(probeContext, managed[engine].Service)
			})
			stableCancel()
			if stableErr != nil || stableStatus != "active" {
				if err := restoreInterruptedCoreMigration(ctx, e.MigrationMarkerPrefix, engine, existing, managed[engine], migrationRecord, stableStatus); err != nil {
					return errors.Join(stableErr, err)
				}
				continue
			}
			if err := setServiceEnabled(ctx, managed[engine].Service, true); err != nil {
				return errors.Join(err, restoreInterruptedCoreMigration(ctx, e.MigrationMarkerPrefix, engine, existing, managed[engine], migrationRecord, managedStatus))
			}
			if err := disableServiceCompletely(ctx, existing.Service); err != nil {
				return errors.Join(err, restoreInterruptedCoreMigration(ctx, e.MigrationMarkerPrefix, engine, existing, managed[engine], migrationRecord, managedStatus))
			}
			if err := writeCoreMigrationMarker(e.MigrationMarkerPrefix, engine, coreMigrationComplete, migrationRecord.ConfigDigest, migrationRecord.SourceDigest, migrationRecord.ExistingEnableState, migrationRecord.ManagedEnableState); err != nil {
				return errors.Join(err, restoreInterruptedCoreMigration(ctx, e.MigrationMarkerPrefix, engine, existing, managed[engine], migrationRecord, managedStatus))
			}
			e.specsMu.Lock()
			delete(e.ExistingSpecs, engine)
			e.specsMu.Unlock()
			continue
		}
		if err := restoreInterruptedCoreMigration(ctx, e.MigrationMarkerPrefix, engine, existing, managed[engine], migrationRecord, managedStatus); err != nil {
			return err
		}
	}
	return nil
}

type coreMigrationState string

type coreMigrationRecord struct {
	State               coreMigrationState
	ConfigDigest        string
	SourceDigest        string
	ExistingEnableState string
	ManagedEnableState  string
}

const (
	coreMigrationNone       coreMigrationState = ""
	coreMigrationInProgress coreMigrationState = "migrating"
	coreMigrationComplete   coreMigrationState = "migrated"
)

func coreMigrationMarked(prefix string, engine core.Engine) (bool, error) {
	record, err := readCoreMigrationRecord(prefix, engine)
	return record.State == coreMigrationComplete, err
}

func restoreInterruptedCoreMigration(ctx context.Context, prefix string, engine core.Engine, existing, managed EngineSpec, record coreMigrationRecord, managedStatus string) error {
	var restoreErr error
	if managedStatus != "inactive" {
		_, err := serviceCommandAndVerify(ctx, managed.Service, core.ActionStop)
		restoreErr = errors.Join(restoreErr, err)
	}
	restoreErr = errors.Join(restoreErr, restoreServiceEnableState(ctx, managed.Service, record.ManagedEnableState))
	restoreErr = errors.Join(restoreErr, restoreServiceEnableState(ctx, existing.Service, record.ExistingEnableState))
	if status, err := serviceStatus(ctx, existing.Service); err != nil {
		restoreErr = errors.Join(restoreErr, err)
	} else if status != "active" {
		_, err := serviceCommandAndVerify(ctx, existing.Service, core.ActionStart)
		restoreErr = errors.Join(restoreErr, err)
	}
	if restoreErr == nil {
		restoreErr = removeCoreMigrationMarker(prefix, engine)
	}
	if restoreErr != nil {
		return fmt.Errorf("restore existing %s service after interrupted migration: %w", engine, restoreErr)
	}
	return nil
}

func readCoreMigrationRecord(prefix string, engine core.Engine) (coreMigrationRecord, error) {
	if prefix == "" {
		return coreMigrationRecord{}, nil
	}
	path := coreMigrationMarkerPath(prefix, engine)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return coreMigrationRecord{}, nil
	}
	if err != nil {
		return coreMigrationRecord{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0o022 != 0 {
		return coreMigrationRecord{}, errors.New("core migration marker is not a protected regular file")
	}
	if err := validateOwner(info, "core migration marker"); err != nil {
		return coreMigrationRecord{}, err
	}
	directoryInfo, err := os.Lstat(filepath.Dir(path))
	if err != nil || directoryInfo.Mode()&os.ModeSymlink != 0 || !directoryInfo.IsDir() || directoryInfo.Mode().Perm()&0o022 != 0 {
		return coreMigrationRecord{}, errors.New("core migration state directory is unsafe")
	}
	if err := validateOwner(directoryInfo, "core migration state directory"); err != nil {
		return coreMigrationRecord{}, err
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return coreMigrationRecord{}, err
	}
	fields := strings.Fields(string(contents))
	if len(fields) != 5 {
		return coreMigrationRecord{}, errors.New("core migration marker is invalid")
	}
	state := coreMigrationState(fields[0])
	if state != coreMigrationInProgress && state != coreMigrationComplete {
		return coreMigrationRecord{}, errors.New("core migration marker is invalid")
	}
	if decoded, err := hex.DecodeString(fields[1]); err != nil || len(decoded) != sha256.Size {
		return coreMigrationRecord{}, errors.New("core migration configuration digest is invalid")
	}
	if decoded, err := hex.DecodeString(fields[2]); err != nil || len(decoded) != sha256.Size {
		return coreMigrationRecord{}, errors.New("core migration source digest is invalid")
	}
	if !validServiceEnableState(fields[3]) || !validServiceEnableState(fields[4]) {
		return coreMigrationRecord{}, errors.New("core migration enable state is invalid")
	}
	return coreMigrationRecord{
		State: state, ConfigDigest: fields[1], SourceDigest: fields[2],
		ExistingEnableState: fields[3], ManagedEnableState: fields[4],
	}, nil
}

func coreMigrationMarkerPath(prefix string, engine core.Engine) string {
	return prefix + "-" + string(engine)
}

func completedCoreMigrationMatches(prefix string, engine core.Engine, content string) (bool, error) {
	record, err := readCoreMigrationRecord(prefix, engine)
	if err != nil || record.State != coreMigrationComplete {
		return false, err
	}
	return record.ConfigDigest == coreMigrationConfigDigest(content), nil
}

func coreMigrationConfigDigest(content string) string {
	digest := sha256.Sum256([]byte(content))
	return hex.EncodeToString(digest[:])
}

func coreMigrationSourceDigest(existing EngineSpec) string {
	source := existing.Binary + "\x00" + existing.ConfigPath + "\x00" + existing.Service
	if existing.ConfigDirectory != "" || existingServiceBinary(existing) != existing.Binary {
		source = existing.Binary + "\x00" + existing.ConfigPath + "\x00" + existing.ConfigDirectory + "\x00" + existingServiceBinary(existing) + "\x00" + existing.Service
	}
	digest := sha256.Sum256([]byte(source))
	return hex.EncodeToString(digest[:])
}

func (e *Executor) importExistingConfig(ctx context.Context, engine core.Engine, managed, existing EngineSpec, content string) (string, error) {
	e.migrationMu.Lock()
	defer e.migrationMu.Unlock()

	e.specsMu.RLock()
	currentExisting, stillPending := e.ExistingSpecs[engine]
	e.specsMu.RUnlock()
	if !stillPending || currentExisting != existing {
		return "", fmt.Errorf("%s existing service migration is no longer pending", engine)
	}
	if err := verifyExistingServiceMapping(ctx, engine, existing); err != nil {
		return "", err
	}
	if err := requireManagedServiceSafeInactive(ctx, engine, managed); err != nil {
		return "", err
	}
	currentContent, err := e.readExistingConfig(ctx, engine, managed, existing)
	if err != nil {
		return "", err
	}
	if currentContent != content {
		return "", fmt.Errorf("existing %s configuration sources changed after the saved snapshot; both services were left unchanged", engine)
	}
	existingEnableState, err := serviceEnableState(ctx, existing.Service)
	if err != nil {
		return "", err
	}
	managedEnableState, err := serviceEnableState(ctx, managed.Service)
	if err != nil {
		return "", err
	}
	if !migrationEnableStatesSupported(existingEnableState, managedEnableState) {
		return "", fmt.Errorf("systemd enable states cannot be migrated safely: existing %s is %s and managed %s is %s; both services were left unchanged", existing.Service, existingEnableState, managed.Service, managedEnableState)
	}

	validationSpec := managed
	validationSpec.Binary = existing.Binary
	if _, err := e.validate(ctx, engine, validationSpec, content); err != nil {
		return "", fmt.Errorf("existing %s configuration is not safe for managed deployment: %w", engine, err)
	}
	if err := requireManagedServiceSafeInactive(ctx, engine, managed); err != nil {
		return "", err
	}

	binaryBackup, err := copyExistingCoreBinary(existing.Binary, managed.Binary)
	if err != nil {
		return "", fmt.Errorf("copy existing %s binary into the QAgent namespace: %w", engine, err)
	}
	rollbackBinary := func() error {
		root, openErr := os.OpenRoot(filepath.Dir(managed.Binary))
		if openErr != nil {
			return openErr
		}
		defer root.Close()
		_, rollbackErr := rollbackCoreBinary(root, filepath.Base(managed.Binary), binaryBackup)
		return rollbackErr
	}
	if _, err := e.validate(ctx, engine, managed, content); err != nil {
		return "", errors.Join(fmt.Errorf("copied %s binary rejected the configuration: %w", engine, err), rollbackBinary())
	}

	configBackup, err := atomicDeploy(managed.ConfigPath, content)
	if err != nil {
		return "", errors.Join(err, rollbackBinary())
	}
	rollbackFiles := func() error {
		_, configErr := rollbackDeploy(managed.ConfigPath, configBackup)
		return errors.Join(configErr, rollbackBinary())
	}
	currentExistingEnableState, err := serviceEnableState(ctx, existing.Service)
	if err != nil {
		return "", errors.Join(err, rollbackFiles())
	}
	currentManagedEnableState, err := serviceEnableState(ctx, managed.Service)
	if err != nil {
		return "", errors.Join(err, rollbackFiles())
	}
	if currentExistingEnableState != existingEnableState || currentManagedEnableState != managedEnableState {
		return "", errors.Join(fmt.Errorf("systemd enable states changed during migration preparation: existing %s changed from %s to %s and managed %s changed from %s to %s; both services were left unchanged", existing.Service, existingEnableState, currentExistingEnableState, managed.Service, managedEnableState, currentManagedEnableState), rollbackFiles())
	}
	if err := verifyExistingServiceMapping(ctx, engine, existing); err != nil {
		return "", errors.Join(err, rollbackFiles())
	}
	currentContent, err = e.readExistingConfig(ctx, engine, managed, existing)
	if err != nil {
		return "", errors.Join(err, rollbackFiles())
	}
	if currentContent != content {
		return "", errors.Join(fmt.Errorf("existing %s configuration sources changed during migration preparation; both services were left unchanged", engine), rollbackFiles())
	}
	if err := requireManagedServiceSafeInactive(ctx, engine, managed); err != nil {
		return "", errors.Join(err, rollbackFiles())
	}
	if err := ensureManagedCoreServiceCapabilities(ctx, engine, managed); err != nil {
		return "", errors.Join(err, rollbackFiles())
	}
	if err := requireManagedServiceSafeInactive(ctx, engine, managed); err != nil {
		return "", errors.Join(err, rollbackFiles())
	}
	configDigest := coreMigrationConfigDigest(content)
	sourceDigest := coreMigrationSourceDigest(existing)
	if err := writeCoreMigrationMarker(e.MigrationMarkerPrefix, engine, coreMigrationInProgress, configDigest, sourceDigest, existingEnableState, managedEnableState); err != nil {
		return "", errors.Join(fmt.Errorf("persist migration intent: %w", err), rollbackFiles())
	}

	oldStopAttempted := false
	rollbackServices := func(cause error) (string, error) {
		rollbackContext, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		var serviceRollbackErr error
		if oldStopAttempted {
			_, stopErr := serviceCommandAndVerify(rollbackContext, managed.Service, core.ActionStop)
			serviceRollbackErr = errors.Join(serviceRollbackErr, stopErr)
			serviceRollbackErr = errors.Join(serviceRollbackErr, restoreServiceEnableState(rollbackContext, managed.Service, managedEnableState))
			serviceRollbackErr = errors.Join(serviceRollbackErr, restoreServiceEnableState(rollbackContext, existing.Service, existingEnableState))
			_, startErr := serviceCommandAndVerify(rollbackContext, existing.Service, core.ActionStart)
			serviceRollbackErr = errors.Join(serviceRollbackErr, startErr)
		}
		var markerErr error
		if serviceRollbackErr == nil {
			markerErr = removeCoreMigrationMarker(e.MigrationMarkerPrefix, engine)
		}
		rollbackErr := errors.Join(serviceRollbackErr, markerErr, rollbackFiles())
		if rollbackErr != nil {
			return "migration failed and rollback was incomplete", fmt.Errorf("%v; rollback: %w", cause, rollbackErr)
		}
		return "migration failed; original configuration, binary, and service were restored", cause
	}

	oldStopAttempted = true
	if _, err := serviceCommandAndVerify(ctx, existing.Service, core.ActionStop); err != nil {
		return rollbackServices(fmt.Errorf("stop existing %s service: %w", engine, err))
	}
	if _, err := serviceCommandAndVerify(ctx, managed.Service, core.ActionStart); err != nil {
		return rollbackServices(fmt.Errorf("start QAgent %s service: %w", engine, err))
	}
	if err := setServiceEnabled(ctx, managed.Service, true); err != nil {
		return rollbackServices(err)
	}
	if err := disableServiceCompletely(ctx, existing.Service); err != nil {
		return rollbackServices(err)
	}
	if status, err := serviceStatus(ctx, existing.Service); err != nil || status != "inactive" {
		if err == nil {
			err = fmt.Errorf("service returned to %s", status)
		}
		return rollbackServices(fmt.Errorf("verify existing %s service stayed inactive: %w", engine, err))
	}
	if err := writeCoreMigrationMarker(e.MigrationMarkerPrefix, engine, coreMigrationComplete, configDigest, sourceDigest, existingEnableState, managedEnableState); err != nil {
		return rollbackServices(fmt.Errorf("persist completed migration: %w", err))
	}

	e.specsMu.Lock()
	delete(e.ExistingSpecs, engine)
	e.specsMu.Unlock()
	return fmt.Sprintf("imported %s configuration; stopped and disabled %s; started and enabled %s", engine, existing.Service, managed.Service), nil
}

func requireManagedServiceSafeInactive(ctx context.Context, engine core.Engine, managed EngineSpec) error {
	status, err := serviceStatus(ctx, managed.Service)
	if err != nil {
		return fmt.Errorf("query QAgent %s service before migration: %w", engine, err)
	}
	if status != "inactive" && status != "failed" {
		return fmt.Errorf("QAgent %s service must remain inactive or failed before migration (status %q); both services were left unchanged", engine, status)
	}
	return nil
}

func copyExistingCoreBinary(source, destination string) (string, error) {
	if source == destination {
		return "", errors.New("existing and managed core binary paths must differ")
	}
	if err := validatePrivilegedExecutable(source); err != nil {
		return "", err
	}
	if err := validateCoreInstallDestination(destination); err != nil {
		return "", err
	}
	sourceInfo, err := os.Lstat(source)
	if err != nil {
		return "", err
	}
	sourceRoot, err := os.OpenRoot(filepath.Dir(source))
	if err != nil {
		return "", err
	}
	defer sourceRoot.Close()
	input, err := sourceRoot.Open(filepath.Base(source))
	if err != nil {
		return "", err
	}
	defer input.Close()
	openedInfo, err := input.Stat()
	if err != nil || !os.SameFile(sourceInfo, openedInfo) {
		return "", errors.New("existing core binary changed while it was being opened")
	}
	if openedInfo.Size() <= 0 || openedInfo.Size() > maxReleaseAssetSize {
		return "", fmt.Errorf("existing core binary size is outside the supported limit")
	}

	destinationRoot, err := os.OpenRoot(filepath.Dir(destination))
	if err != nil {
		return "", err
	}
	defer destinationRoot.Close()
	tempName, err := randomCoreTempName(destinationRoot)
	if err != nil {
		return "", err
	}
	defer destinationRoot.Remove(tempName)
	output, err := destinationRoot.OpenFile(tempName, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o700)
	if err != nil {
		return "", err
	}
	written, copyErr := io.Copy(output, io.LimitReader(input, maxReleaseAssetSize+1))
	if copyErr == nil && (written <= 0 || written > maxReleaseAssetSize) {
		copyErr = errors.New("existing core binary copy exceeded the supported limit")
	}
	if copyErr == nil {
		copyErr = output.Sync()
	}
	closeErr := output.Close()
	if copyErr != nil {
		return "", copyErr
	}
	if closeErr != nil {
		return "", closeErr
	}
	return replaceCoreBinary(destinationRoot, filepath.Base(destination), tempName)
}

func verifyExistingServiceMapping(ctx context.Context, engine core.Engine, existing EngineSpec) error {
	status, err := serviceStatus(ctx, existing.Service)
	if err != nil {
		return fmt.Errorf("query existing %s service before migration: %w", engine, err)
	}
	if status != "active" {
		return fmt.Errorf("existing %s service must remain active before migration (status %q)", engine, status)
	}
	output, err := run(ctx, systemctlPath, "show", existing.Service, "--property=ExecStart", "--value")
	if err != nil {
		return fmt.Errorf("query existing %s service ExecStart before migration: %w", engine, err)
	}
	executable, argv, err := parseSingleSystemdExecStart(output)
	if err != nil || executable != existingServiceBinary(existing) || !supportedExistingExecStart(engine, existing, argv) {
		return fmt.Errorf("existing %s service ExecStart no longer matches the exact discovered binary and single configuration", engine)
	}
	if err := validateExistingServiceExecutable(existing); err != nil {
		return fmt.Errorf("existing %s service executable mapping is no longer safe: %w", engine, err)
	}
	status, err = serviceStatus(ctx, existing.Service)
	if err != nil {
		return fmt.Errorf("recheck existing %s service before migration: %w", engine, err)
	}
	if status != "active" {
		return fmt.Errorf("existing %s service changed to %q while its mapping was checked", engine, status)
	}
	return nil
}

func parseSingleSystemdExecStart(value string) (string, string, error) {
	value = strings.TrimSuffix(value, "\n")
	value = strings.TrimSuffix(value, "\r")
	if strings.ContainsAny(value, "\r\n") || strings.Contains(value, "} {") || strings.Contains(value, "; path=") {
		return "", "", errors.New("systemd ExecStart contains multiple commands")
	}
	const prefix = "{ path="
	if !strings.HasPrefix(value, prefix) {
		return "", "", errors.New("systemd ExecStart has an unsupported structure")
	}
	remainder := strings.TrimPrefix(value, prefix)
	const argvSeparator = " ; argv[]="
	argvIndex := strings.Index(remainder, argvSeparator)
	if argvIndex <= 0 {
		return "", "", errors.New("systemd ExecStart has no executable argv")
	}
	executable := remainder[:argvIndex]
	remainder = remainder[argvIndex+len(argvSeparator):]
	const metadataSeparator = " ; ignore_errors="
	metadataIndex := strings.Index(remainder, metadataSeparator)
	if metadataIndex <= 0 {
		return "", "", errors.New("systemd ExecStart has no command metadata")
	}
	argv := remainder[:metadataIndex]
	metadata := remainder[metadataIndex:]
	if !strings.HasSuffix(metadata, " }") || strings.ContainsAny(strings.TrimSuffix(metadata, " }"), "{}") {
		return "", "", errors.New("systemd ExecStart has ambiguous command metadata")
	}
	return executable, argv, nil
}

func supportedExistingExecStart(engine core.Engine, existing EngineSpec, argv string) bool {
	serviceBinary := existingServiceBinary(existing)
	switch engine {
	case core.EngineXray:
		return existing.ConfigDirectory == "" && (argv == serviceBinary+" run -config "+existing.ConfigPath ||
			argv == serviceBinary+" run -c "+existing.ConfigPath)
	case core.EngineSingBox:
		if existing.ConfigDirectory != "" {
			return argv == serviceBinary+" run -c "+existing.ConfigPath+" -C "+existing.ConfigDirectory
		}
		return argv == serviceBinary+" run -c "+existing.ConfigPath ||
			argv == serviceBinary+" run --config "+existing.ConfigPath
	default:
		return false
	}
}

func serviceEnableState(ctx context.Context, service string) (string, error) {
	output, err := run(ctx, systemctlPath, "is-enabled", service)
	state := strings.TrimSpace(output)
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if validServiceEnableState(state) {
		return state, nil
	}
	if err == nil {
		err = errors.New("unexpected systemd enable state")
	}
	return "", fmt.Errorf("query whether systemd service %s is enabled: %w: %s", service, err, state)
}

func validServiceEnableState(state string) bool {
	return state == "enabled" || state == "enabled-runtime" || state == "disabled" || state == "static" || state == "indirect"
}

func migrationEnableStatesSupported(existing, managed string) bool {
	supported := func(state string) bool {
		return state == "enabled" || state == "enabled-runtime" || state == "disabled"
	}
	return supported(existing) && supported(managed)
}

func setServiceEnabled(ctx context.Context, service string, enabled bool) error {
	action := "disable"
	if enabled {
		action = "enable"
	}
	if output, err := run(ctx, systemctlPath, action, service); err != nil {
		return fmt.Errorf("systemctl %s %s: %w: %s", action, service, err, output)
	}
	return nil
}

func disableServiceCompletely(ctx context.Context, service string) error {
	if err := setServiceEnabled(ctx, service, false); err != nil {
		return err
	}
	if output, err := run(ctx, systemctlPath, "disable", "--runtime", service); err != nil {
		return fmt.Errorf("systemctl disable --runtime %s: %w: %s", service, err, output)
	}
	return nil
}

func restoreServiceEnableState(ctx context.Context, service, state string) error {
	switch state {
	case "enabled":
		return setServiceEnabled(ctx, service, true)
	case "enabled-runtime":
		if err := disableServiceCompletely(ctx, service); err != nil {
			return err
		}
		if output, err := run(ctx, systemctlPath, "enable", "--runtime", service); err != nil {
			return fmt.Errorf("systemctl enable --runtime %s: %w: %s", service, err, output)
		}
		restored, err := serviceEnableState(ctx, service)
		if err != nil {
			return err
		}
		if restored != "enabled-runtime" {
			return fmt.Errorf("systemd service %s enable state restored as %s instead of enabled-runtime", service, restored)
		}
		return nil
	case "disabled":
		return disableServiceCompletely(ctx, service)
	case "static", "indirect":
		return nil
	default:
		return errors.New("invalid original systemd enable state")
	}
}

func writeCoreMigrationMarker(prefix string, engine core.Engine, state coreMigrationState, configDigest, sourceDigest, existingEnableState, managedEnableState string) error {
	if prefix == "" {
		return errors.New("core migration marker path is not configured")
	}
	if state != coreMigrationInProgress && state != coreMigrationComplete {
		return errors.New("invalid core migration state")
	}
	if decoded, err := hex.DecodeString(configDigest); err != nil || len(decoded) != sha256.Size {
		return errors.New("invalid core migration configuration digest")
	}
	if decoded, err := hex.DecodeString(sourceDigest); err != nil || len(decoded) != sha256.Size {
		return errors.New("invalid core migration source digest")
	}
	if !validServiceEnableState(existingEnableState) || !validServiceEnableState(managedEnableState) {
		return errors.New("invalid core migration enable state")
	}
	path := coreMigrationMarkerPath(prefix, engine)
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(directory)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm()&0o022 != 0 {
		return errors.New("core migration state directory is unsafe")
	}
	if err := validateOwner(info, "core migration state directory"); err != nil {
		return err
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return err
	}
	defer root.Close()
	suffix, err := randomSuffix(8)
	if err != nil {
		return err
	}
	tempName := ".qagent-core-migration-" + suffix
	defer root.Remove(tempName)
	file, err := root.OpenFile(tempName, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.WriteString(string(state) + " " + configDigest + " " + sourceDigest + " " + existingEnableState + " " + managedEnableState + "\n"); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := root.Rename(tempName, filepath.Base(path)); err != nil {
		return err
	}
	return syncRootDirectory(root)
}

func removeCoreMigrationMarker(prefix string, engine core.Engine) error {
	path := coreMigrationMarkerPath(prefix, engine)
	directory := filepath.Dir(path)
	info, err := os.Lstat(directory)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm()&0o022 != 0 {
		return errors.New("core migration state directory is unsafe")
	}
	if err := validateOwner(info, "core migration state directory"); err != nil {
		return err
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return err
	}
	defer root.Close()
	if err := root.Remove(filepath.Base(path)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncRootDirectory(root)
}
