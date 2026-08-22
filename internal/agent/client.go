package agent

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/qimaoww/qcontrolhub/internal/authn"
	"github.com/qimaoww/qcontrolhub/internal/core"
)

type ClientConfig struct {
	ServerURL         string
	EnrollmentToken   string
	StatePath         string
	Name              string
	Version           string
	Labels            map[string]string
	Capabilities      []core.Engine
	HeartbeatEvery    time.Duration
	MetricsEvery      time.Duration
	AllowHTTP         bool
	AllowInsecureLive bool
	TLSCAFile         string
}

type credentials struct {
	AgentID        string                   `json:"agent_id"`
	PrivateKey     string                   `json:"private_key"`
	CompletedTasks map[string]completedTask `json:"completed_tasks,omitempty"`
}

type completedTask struct {
	Success     bool      `json:"success"`
	Output      string    `json:"output,omitempty"`
	Error       string    `json:"error,omitempty"`
	CompletedAt time.Time `json:"completed_at"`
}

type Client struct {
	config           ClientConfig
	executor         *Executor
	http             *http.Client
	creds            credentials
	websocketURL     string
	metrics          *MetricsCollector
	traffic          *TrafficManager
	logs             *CoreLogCollector
	credentialsMu    sync.Mutex
	executionsMu     sync.Mutex
	executions       map[string]*taskExecution
	restartAfterTask string
	executeFunc      func(context.Context, core.Task) (string, error)
}

type taskExecution struct {
	done        chan struct{}
	result      core.TaskResultRequest
	completedAt time.Time
}

const (
	defaultHeartbeatInterval = 15 * time.Second
	minHeartbeatInterval     = time.Second
	maxHeartbeatInterval     = 30 * time.Second
	// Metrics pushes are lightweight /proc snapshots on a dedicated wire
	// message so the panel's live card values refresh without waiting for the
	// full heartbeat cycle.
	defaultMetricsInterval = time.Second
	minMetricsInterval     = time.Second
)

// ErrIdentityRejected means the control plane permanently rejected the
// persisted Agent identity. Retrying cannot recover until an administrator
// removes the local state and enrolls a new identity.
var ErrIdentityRejected = errors.New("agent identity was rejected by the control plane")

func NewClient(config ClientConfig, executor *Executor) (*Client, error) {
	if err := executor.LoadCoreMigrationState(); err != nil {
		return nil, fmt.Errorf("load existing core migration state: %w", err)
	}
	if err := executor.Validate(); err != nil {
		return nil, err
	}
	reconcileContext, reconcileCancel := context.WithTimeout(context.Background(), 20*time.Second)
	if err := executor.ReconcileExistingCoreServices(reconcileContext); err != nil {
		reconcileCancel()
		return nil, fmt.Errorf("reconcile existing core migration: %w", err)
	}
	reconcileCancel()
	parsed, err := url.Parse(config.ServerURL)
	if err != nil || parsed.Host == "" {
		return nil, errors.New("QCH_SERVER_URL must be a valid absolute URL")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" && parsed.Scheme != "ws" && parsed.Scheme != "wss" {
		return nil, errors.New("QCH_SERVER_URL scheme must be wss, ws, https, or http")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return nil, errors.New("QCH_SERVER_URL must be a bare origin without credentials, path, query, or fragment")
	}
	secureWebSocket := parsed.Scheme == "https" || parsed.Scheme == "wss"
	if !secureWebSocket {
		isLocal := parsed.Hostname() == "localhost" || parsed.Hostname() == "127.0.0.1" || parsed.Hostname() == "::1"
		if !config.AllowHTTP && !isLocal {
			return nil, errors.New("remote control-plane URL must use WSS; set QCH_ALLOW_HTTP=true only on a trusted local network")
		}
		if !isLocal && executor != nil && !config.AllowInsecureLive {
			return nil, errors.New("live task execution is forbidden over remote cleartext HTTP; set QCH_ALLOW_INSECURE_LIVE=true to explicitly allow it on a trusted network")
		}
	}
	if config.HeartbeatEvery <= 0 {
		config.HeartbeatEvery = defaultHeartbeatInterval
	} else if config.HeartbeatEvery < minHeartbeatInterval || config.HeartbeatEvery > maxHeartbeatInterval {
		return nil, errors.New("QCH_HEARTBEAT_INTERVAL must be between 1s and 30s")
	}
	if config.MetricsEvery <= 0 {
		config.MetricsEvery = defaultMetricsInterval
	} else if config.MetricsEvery < minMetricsInterval || config.MetricsEvery > maxHeartbeatInterval {
		return nil, errors.New("QCH_METRICS_INTERVAL must be between 1s and 30s")
	}
	httpScheme, websocketScheme := "http", "ws"
	if secureWebSocket {
		httpScheme, websocketScheme = "https", "wss"
	}
	config.ServerURL = httpScheme + "://" + parsed.Host
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	if config.TLSCAFile != "" {
		rootCAs, err := loadTrustedCA(config.TLSCAFile)
		if err != nil {
			return nil, fmt.Errorf("load QCH_TLS_CA_FILE: %w", err)
		}
		transport.TLSClientConfig.RootCAs = rootCAs
	}
	// Seed the CPU and network baselines so the first heartbeat and metrics
	// push after a process start already report usage values instead of a
	// one-sample "unavailable" reading.
	metricsCollector := NewMetricsCollector()
	warmupContext, warmupCancel := context.WithTimeout(context.Background(), 3*time.Second)
	_, _ = metricsCollector.Collect(warmupContext)
	warmupCancel()
	return &Client{
		config:       config,
		executor:     executor,
		websocketURL: websocketScheme + "://" + parsed.Host + "/agent/v1/connect",
		metrics:      metricsCollector,
		traffic:      NewTrafficManager(config.StatePath),
		logs:         NewCoreLogCollector(executor.Specs, executor.ExistingSpecs),
		http: &http.Client{
			Transport: transport,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return errors.New("control-plane redirects are disabled")
			},
		},
	}, nil
}

func (c *Client) Run(ctx context.Context) error {
	loaded, err := loadCredentials(c.config.StatePath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("load agent credentials: %w", err)
	}
	if errors.Is(err, os.ErrNotExist) {
		if c.config.EnrollmentToken == "" {
			return errors.New("QCH_ENROLLMENT_TOKEN is required for first enrollment")
		}
		publicKey, privateKey, keyErr := ed25519.GenerateKey(rand.Reader)
		if keyErr != nil {
			return fmt.Errorf("generate agent identity: %w", keyErr)
		}
		loaded, err = c.enroll(ctx, publicKey, privateKey)
		if err != nil {
			return err
		}
		if err := saveCredentials(c.config.StatePath, loaded); err != nil {
			return fmt.Errorf("save agent credentials: %w", err)
		}
	}
	c.creds = loaded
	if err := ensureManagedCoreLogStreaming(ctx, c.executor.Specs); err != nil {
		slog.Warn("prepare volatile managed core logs", "error", err)
	}
	go c.logs.Run(ctx)
	c.traffic.Start(ctx)
	slog.Info("agent identity loaded", "agent_id", c.creds.AgentID, "server", c.websocketURL)
	backoff := time.Second
	for {
		started := time.Now()
		err := c.runWebSocket(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if errors.Is(err, ErrIdentityRejected) {
			cleanupContext, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
			if cleanupErr := c.traffic.ClearPolicies(cleanupContext); cleanupErr != nil {
				slog.Warn("remove traffic rules for rejected Agent identity", "error", cleanupErr)
			}
			cleanupCancel()
			return err
		}
		slog.Warn("WSS connection lost", "error", err, "reconnect_in", backoff)
		if time.Since(started) > time.Minute {
			backoff = time.Second
		}
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
		if backoff < 30*time.Second {
			backoff *= 2
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
		}
	}
}

func (c *Client) enroll(ctx context.Context, publicKey ed25519.PublicKey, privateKey ed25519.PrivateKey) (credentials, error) {
	request := core.EnrollRequest{
		Name:         c.config.Name,
		Version:      c.config.Version,
		OS:           runtime.GOOS,
		Arch:         runtime.GOARCH,
		Capabilities: c.config.Capabilities,
		Features:     advertisedAgentFeatures(),
		Labels:       c.config.Labels,
		PublicKey:    authn.EncodePublicKey(publicKey),
	}
	var response core.EnrollResponse
	if err := c.doJSON(ctx, http.MethodPost, "/agent/v1/enroll", c.config.EnrollmentToken, "", request, &response); err != nil {
		return credentials{}, fmt.Errorf("enroll agent: %w", err)
	}
	if response.AgentID == "" {
		return credentials{}, errors.New("control plane returned incomplete enrollment credentials")
	}
	return credentials{AgentID: response.AgentID, PrivateKey: authn.EncodePrivateKey(privateKey)}, nil
}

func (c *Client) runWebSocket(ctx context.Context) error {
	privateKey, err := authn.DecodePrivateKey(c.creds.PrivateKey)
	if err != nil {
		return err
	}
	handshake, err := http.NewRequestWithContext(ctx, http.MethodGet, c.websocketURL, nil)
	if err != nil {
		return err
	}
	if err := authn.SignRequest(handshake, nil, c.creds.AgentID, privateKey, time.Now().UTC()); err != nil {
		return err
	}
	connection, response, err := websocket.Dial(ctx, c.websocketURL, &websocket.DialOptions{
		HTTPClient:      c.http,
		HTTPHeader:      handshake.Header,
		CompressionMode: websocket.CompressionDisabled,
		Subprotocols:    []string{"qcontrolhub.agent.v1"},
	})
	if err != nil {
		if response != nil {
			defer response.Body.Close()
			contents, _ := io.ReadAll(io.LimitReader(response.Body, 8<<10))
			if response.StatusCode == http.StatusUnauthorized {
				return fmt.Errorf("%w: WSS handshake returned %s: %s", ErrIdentityRejected, response.Status, strings.TrimSpace(string(contents)))
			}
			return fmt.Errorf("WSS handshake returned %s: %s", response.Status, strings.TrimSpace(string(contents)))
		}
		return explainTLSError(err)
	}
	defer connection.Close(websocket.StatusNormalClosure, "agent reconnecting")
	if connection.Subprotocol() != "qcontrolhub.agent.v1" {
		return errors.New("control plane did not negotiate the QControlHub Agent subprotocol")
	}
	connection.SetReadLimit(core.MaxConfigEnvelopeBytes)
	slog.Info("WSS session established", "agent_id", c.creds.AgentID)

	sessionContext, cancel := context.WithCancel(ctx)
	defer cancel()
	incoming := make(chan core.WireMessage, 1)
	outgoing := make(chan core.WireMessage, 16)
	readErrors := make(chan error, 1)
	writeErrors := make(chan error, 1)
	go func() {
		for {
			var message core.WireMessage
			if err := wsjson.Read(sessionContext, connection, &message); err != nil {
				select {
				case readErrors <- err:
				default:
				}
				return
			}
			select {
			case incoming <- message:
			case <-sessionContext.Done():
				return
			}
		}
	}()
	go func() {
		for {
			select {
			case <-sessionContext.Done():
				return
			case message := <-outgoing:
				writeContext, writeCancel := context.WithTimeout(sessionContext, 15*time.Second)
				err := wsjson.Write(writeContext, connection, message)
				writeCancel()
				if err != nil {
					select {
					case writeErrors <- err:
					default:
					}
					return
				}
			}
		}
	}()

	heartbeatTicker := time.NewTicker(c.config.HeartbeatEvery)
	defer heartbeatTicker.Stop()
	metricsTicker := time.NewTicker(c.config.MetricsEvery)
	defer metricsTicker.Stop()
	logTicker := time.NewTicker(500 * time.Millisecond)
	defer logTicker.Stop()
	if err := c.queueHeartbeat(sessionContext, outgoing); err != nil {
		return err
	}
	var activeTask string
	var sentLogBatch string
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-readErrors:
			return err
		case err := <-writeErrors:
			return err
		case <-heartbeatTicker.C:
			if err := c.queueHeartbeat(sessionContext, outgoing); err != nil {
				return err
			}
		case <-metricsTicker.C:
			if err := c.queueMetrics(sessionContext, outgoing); err != nil {
				return err
			}
		case <-logTicker.C:
			if sentLogBatch == "" {
				if batch := c.logs.NextBatch(); batch != nil {
					select {
					case outgoing <- core.WireMessage{Type: core.WireCoreLogs, CoreLogs: batch}:
						sentLogBatch = batch.ID
					default:
					}
				}
			}
		case message := <-incoming:
			switch message.Type {
			case core.WireHello:
				if err := c.traffic.SetPolicies(sessionContext, message.TrafficPolicies, c.creds.AgentID); err != nil {
					return fmt.Errorf("apply control-plane traffic policies: %w", err)
				}
				continue
			case core.WireTask:
				if message.Task == nil || activeTask != "" || !c.validTask(*message.Task) {
					return errors.New("control plane returned an invalid or concurrent task envelope")
				}
				task := *message.Task
				activeTask = task.ID
				// A transient WSS disconnect must not terminate an in-flight system
				// operation. Execution follows the Agent process lifetime; only result
				// delivery follows this individual websocket session.
				go c.executeTaskForSession(ctx, sessionContext, task, outgoing)
			case core.WireResultAck:
				if message.TaskID == "" || message.TaskID != activeTask {
					return errors.New("control plane acknowledged an unexpected task")
				}
				slog.Info("task result acknowledged", "task_id", message.TaskID)
				c.acknowledgeTaskResult(message.TaskID)
				activeTask = ""
			case core.WireCoreLogsAck:
				if message.BatchID == "" || message.BatchID != sentLogBatch || !c.logs.Acknowledge(message.BatchID) {
					return errors.New("control plane acknowledged an unexpected core log batch")
				}
				sentLogBatch = ""
			case core.WireError:
				return fmt.Errorf("control plane WSS error: %s", message.Error)
			default:
				return fmt.Errorf("unsupported control-plane message type %q", message.Type)
			}
		}
	}
}

func (c *Client) queueHeartbeat(ctx context.Context, outgoing chan<- core.WireMessage) error {
	runtimeContext, cancel := context.WithTimeout(ctx, 10*time.Second)
	runtimeState := c.executor.Runtime(runtimeContext)
	cancel()
	metrics, metricsErr := c.metrics.Collect(ctx)
	if metricsErr != nil {
		slog.Debug("host metrics collection was partial", "error", metricsErr)
	}
	heartbeat := &core.HeartbeatRequest{
		Version: c.config.Version, Runtime: runtimeState,
		Features: advertisedAgentFeatures(), TrafficUsage: c.traffic.Snapshot(),
	}
	if metricsHaveData(metrics) {
		heartbeat.Metrics = &metrics
	}
	message := core.WireMessage{Type: core.WireHeartbeat, Heartbeat: heartbeat}
	select {
	case outgoing <- message:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// queueMetrics sends a lightweight metrics-only push between full heartbeats
// so the panel's live resource values refresh at the configured cadence
// instead of waiting for the heartbeat cycle. Samples without a CPU reading
// are skipped so one partial collection never overwrites the last complete
// snapshot stored on the control plane.
func (c *Client) queueMetrics(ctx context.Context, outgoing chan<- core.WireMessage) error {
	metrics, metricsErr := c.metrics.Collect(ctx)
	if metricsErr != nil {
		slog.Debug("host metrics collection was partial", "error", metricsErr)
	}
	if !metricsHaveData(metrics) || !metrics.CPUAvailable {
		return nil
	}
	message := core.WireMessage{Type: core.WireMetrics, Metrics: &metrics}
	select {
	case outgoing <- message:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func advertisedAgentFeatures() []string {
	return []string{core.AgentFeatureSelfUpgrade, core.AgentFeaturePortTraffic, core.AgentFeatureCoreLogs}
}

func (c *Client) executeTask(ctx context.Context, task core.Task, outgoing chan<- core.WireMessage) {
	c.executeTaskForSession(ctx, ctx, task, outgoing)
}

func (c *Client) executeTaskForSession(executionContext, deliveryContext context.Context, task core.Task, outgoing chan<- core.WireMessage) {
	result := c.resultForTask(executionContext, task)
	message := core.WireMessage{Type: core.WireResult, Result: &core.TaskResultEnvelope{TaskID: task.ID, Result: result}}
	select {
	case outgoing <- message:
	case <-deliveryContext.Done():
	}
}

func (c *Client) resultForTask(ctx context.Context, task core.Task) core.TaskResultRequest {
	if cached, ok := c.cachedTaskResult(task); ok {
		slog.Info("returning cached task result", "task_id", task.ID)
		return cached
	}
	c.executionsMu.Lock()
	if c.executions == nil {
		c.executions = make(map[string]*taskExecution)
	}
	c.pruneExecutionsLocked(time.Now())
	if running, ok := c.executions[task.ID]; ok {
		done := running.done
		c.executionsMu.Unlock()
		slog.Info("joining in-flight task after reconnect", "task_id", task.ID)
		select {
		case <-done:
			c.executionsMu.Lock()
			result := running.result
			c.executionsMu.Unlock()
			result.LeaseID = task.LeaseID
			return result
		case <-ctx.Done():
			return core.TaskResultRequest{LeaseID: task.LeaseID, Error: "agent stopped while waiting for the in-flight task"}
		}
	}
	execution := &taskExecution{done: make(chan struct{})}
	c.executions[task.ID] = execution
	c.executionsMu.Unlock()

	slog.Info("executing task", "task_id", task.ID, "action", task.Action, "engine", task.Engine)
	execute := c.executeFunc
	var output string
	var executionErr error
	if task.Action == core.ActionUpgradeAgent {
		output, executionErr = c.upgradeAgent(ctx)
		if executionErr == nil {
			c.executionsMu.Lock()
			c.restartAfterTask = task.ID
			c.executionsMu.Unlock()
		}
	} else {
		if execute == nil {
			execute = c.executor.Execute
		}
		output, executionErr = execute(ctx, task)
	}
	result := core.TaskResultRequest{
		LeaseID: task.LeaseID, Success: executionErr == nil, Output: output,
	}
	if executionErr != nil {
		result.Error = executionErr.Error()
		slog.Warn("task failed", "task_id", task.ID, "error", executionErr)
	} else {
		slog.Info("task completed", "task_id", task.ID)
	}
	if task.Action != core.ActionReadConfig {
		if err := c.rememberTaskResult(task.ID, result); err != nil {
			slog.Warn("persist completed task result", "task_id", task.ID, "error", err)
		}
	}
	c.executionsMu.Lock()
	execution.result = result
	execution.completedAt = time.Now()
	close(execution.done)
	c.executionsMu.Unlock()
	return result
}

func (c *Client) pruneExecutionsLocked(now time.Time) {
	const retention = 10 * time.Minute
	for taskID, execution := range c.executions {
		if !execution.completedAt.IsZero() && now.Sub(execution.completedAt) >= retention {
			delete(c.executions, taskID)
		}
	}
	for len(c.executions) >= 64 {
		oldestID := ""
		var oldest time.Time
		for taskID, execution := range c.executions {
			if execution.completedAt.IsZero() {
				continue
			}
			if oldestID == "" || execution.completedAt.Before(oldest) {
				oldestID, oldest = taskID, execution.completedAt
			}
		}
		if oldestID == "" {
			return
		}
		delete(c.executions, oldestID)
	}
}

func (c *Client) acknowledgeTaskResult(taskID string) {
	restart := false
	c.executionsMu.Lock()
	delete(c.executions, taskID)
	if c.restartAfterTask == taskID {
		c.restartAfterTask = ""
		restart = true
	}
	c.executionsMu.Unlock()
	if restart {
		go c.reexecAfterUpgrade()
	}
}

func (c *Client) cachedTaskResult(task core.Task) (core.TaskResultRequest, bool) {
	c.credentialsMu.Lock()
	defer c.credentialsMu.Unlock()
	cached, ok := c.creds.CompletedTasks[task.ID]
	if !ok {
		return core.TaskResultRequest{}, false
	}
	return core.TaskResultRequest{
		LeaseID: task.LeaseID, Success: cached.Success, Output: cached.Output, Error: cached.Error,
	}, true
}

func (c *Client) rememberTaskResult(taskID string, result core.TaskResultRequest) error {
	c.credentialsMu.Lock()
	defer c.credentialsMu.Unlock()
	if c.creds.CompletedTasks == nil {
		c.creds.CompletedTasks = make(map[string]completedTask)
	}
	for len(c.creds.CompletedTasks) >= 64 {
		oldestID := ""
		var oldest time.Time
		for id, item := range c.creds.CompletedTasks {
			if oldestID == "" || item.CompletedAt.Before(oldest) {
				oldestID, oldest = id, item.CompletedAt
			}
		}
		delete(c.creds.CompletedTasks, oldestID)
	}
	c.creds.CompletedTasks[taskID] = completedTask{
		Success: result.Success, Output: limitStateValue(result.Output, 4<<10),
		Error: limitStateValue(result.Error, 2<<10), CompletedAt: time.Now().UTC(),
	}
	return saveCredentials(c.config.StatePath, c.creds)
}

func limitStateValue(value string, limit int) string {
	value = strings.ToValidUTF8(value, "�")
	if len(value) <= limit {
		return value
	}
	return strings.ToValidUTF8(value[:limit], "�") + "\n… cached output truncated"
}

func (c *Client) validTask(task core.Task) bool {
	engineValid := task.Engine.Valid()
	if task.Action == core.ActionUpgradeAgent {
		engineValid = task.Engine == ""
	}
	return task.AgentID == c.creds.AgentID && validTaskID(task.ID) && len(task.LeaseID) >= 32 &&
		task.Status == core.TaskRunning && task.Action.Valid() && engineValid
}

func (c *Client) upgradeAgent(ctx context.Context) (string, error) {
	executable := ""
	var err error
	executable, err = os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve running Agent executable: %w", err)
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return "", fmt.Errorf("resolve running Agent executable path: %w", err)
	}
	downloadDirectory := ""
	if executable != "" {
		downloadDirectory = filepath.Dir(executable)
	}
	temporary, version, size, err := c.downloadAgentBinary(ctx, downloadDirectory)
	if err != nil {
		return "", err
	}
	defer os.Remove(temporary)
	info, err := os.Lstat(executable)
	if err != nil {
		return "", fmt.Errorf("inspect running Agent executable: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", errors.New("running Agent executable is not a regular executable file")
	}
	if err := os.Chmod(temporary, info.Mode().Perm()); err != nil {
		return "", fmt.Errorf("set upgraded Agent permissions: %w", err)
	}
	if err := os.Rename(temporary, executable); err != nil {
		return "", fmt.Errorf("replace Agent executable atomically: %w", err)
	}
	return fmt.Sprintf("Agent binary replaced with %s (%d bytes); reconnecting with the upgraded process", versionLabel(version), size), nil
}

func (c *Client) downloadAgentBinary(ctx context.Context, directory string) (string, string, int64, error) {
	privateKey, err := authn.DecodePrivateKey(c.creds.PrivateKey)
	if err != nil {
		return "", "", 0, err
	}
	requestContext, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	request, err := http.NewRequestWithContext(requestContext, http.MethodGet, c.config.ServerURL+"/agent/v1/binary", nil)
	if err != nil {
		return "", "", 0, err
	}
	if err := authn.SignRequest(request, nil, c.creds.AgentID, privateKey, time.Now().UTC()); err != nil {
		return "", "", 0, err
	}
	response, err := c.http.Do(request)
	if err != nil {
		return "", "", 0, explainTLSError(err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		contents, _ := io.ReadAll(io.LimitReader(response.Body, 8<<10))
		return "", "", 0, fmt.Errorf("control plane returned %s: %s", response.Status, strings.TrimSpace(string(contents)))
	}
	limit := int64(core.MaxAgentBinaryBytes)
	if response.ContentLength > limit {
		return "", "", 0, fmt.Errorf("Agent binary exceeds %d bytes", limit)
	}
	temporary, err := os.CreateTemp(directory, "qagent-upgrade-*")
	if err != nil {
		return "", "", 0, fmt.Errorf("create temporary Agent binary: %w", err)
	}
	temporaryPath := temporary.Name()
	cleanup := func() { _ = temporary.Close(); _ = os.Remove(temporaryPath) }
	hash := sha256.New()
	written, err := io.Copy(io.MultiWriter(temporary, hash), io.LimitReader(response.Body, limit+1))
	if err != nil {
		cleanup()
		return "", "", 0, fmt.Errorf("download Agent binary: %w", err)
	}
	if written == 0 || written > limit {
		cleanup()
		return "", "", 0, errors.New("downloaded Agent binary has an invalid size")
	}
	if expected := strings.TrimSpace(response.Header.Get("X-QControlHub-Agent-SHA256")); expected != "" && !strings.EqualFold(expected, fmt.Sprintf("%x", hash.Sum(nil))) {
		cleanup()
		return "", "", 0, errors.New("downloaded Agent binary checksum mismatch")
	}
	if err := temporary.Sync(); err != nil {
		cleanup()
		return "", "", 0, fmt.Errorf("sync downloaded Agent binary: %w", err)
	}
	if err := temporary.Close(); err != nil {
		_ = os.Remove(temporaryPath)
		return "", "", 0, fmt.Errorf("close downloaded Agent binary: %w", err)
	}
	return temporaryPath, strings.TrimSpace(response.Header.Get("X-QControlHub-Agent-Version")), written, nil
}

func (c *Client) reexecAfterUpgrade() {
	time.Sleep(150 * time.Millisecond)
	executable, err := os.Executable()
	if err != nil {
		slog.Error("resolve upgraded Agent executable for restart", "error", err)
		return
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		slog.Error("resolve upgraded Agent executable path for restart", "error", err)
		return
	}
	if err := syscall.Exec(executable, os.Args, os.Environ()); err != nil {
		slog.Error("restart upgraded Agent", "error", err)
	}
}

func versionLabel(value string) string {
	if value == "" {
		return "the current control-plane build"
	}
	return value
}

func validTaskID(value string) bool {
	if len(value) != 20 || !strings.HasPrefix(value, "tsk_") {
		return false
	}
	for _, character := range value[4:] {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func (c *Client) doJSON(ctx context.Context, method, path, credential, id string, input, output any) error {
	_, err := c.doJSONStatus(ctx, method, path, credential, id, input, output)
	return err
}

func (c *Client) doJSONStatus(ctx context.Context, method, path, credential, id string, input, output any) (int, error) {
	requestContext, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var body io.Reader
	var encoded []byte
	if input != nil {
		var err error
		encoded, err = json.Marshal(input)
		if err != nil {
			return 0, err
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(requestContext, method, c.config.ServerURL+path, body)
	if err != nil {
		return 0, err
	}
	if id != "" {
		privateKey, err := authn.DecodePrivateKey(credential)
		if err != nil {
			return 0, err
		}
		if err := authn.SignRequest(request, encoded, id, privateKey, time.Now().UTC()); err != nil {
			return 0, err
		}
	} else {
		request.Header.Set("Authorization", "Bearer "+credential)
	}
	if input != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.http.Do(request)
	if err != nil {
		return 0, explainTLSError(err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		contents, _ := io.ReadAll(io.LimitReader(response.Body, 8<<10))
		return response.StatusCode, fmt.Errorf("control plane returned %s: %s", response.Status, strings.TrimSpace(string(contents)))
	}
	if output != nil && response.StatusCode != http.StatusNoContent {
		if err := json.NewDecoder(io.LimitReader(response.Body, core.MaxConfigBytes+256<<10)).Decode(output); err != nil {
			return response.StatusCode, err
		}
	}
	return response.StatusCode, nil
}

func loadCredentials(path string) (credentials, error) {
	directory := filepath.Dir(path)
	if err := validateStateDirectory(directory); err != nil {
		return credentials{}, err
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return credentials{}, err
	}
	defer root.Close()
	baseName := filepath.Base(path)
	linkInfo, err := root.Lstat(baseName)
	if err != nil {
		return credentials{}, err
	}
	if linkInfo.Mode()&os.ModeSymlink != 0 {
		return credentials{}, errors.New("agent credential path must be a regular, non-symlink file")
	}
	file, err := root.Open(baseName)
	if err != nil {
		return credentials{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return credentials{}, errors.New("agent credential path must be a regular, non-symlink file")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return credentials{}, fmt.Errorf("agent credential file permissions %04o are too broad; expected 0600 or stricter", info.Mode().Perm())
	}
	if err := validateOwner(info, "agent credential file"); err != nil {
		return credentials{}, err
	}
	const maxAgentStateBytes = 512 << 10
	contents, err := io.ReadAll(io.LimitReader(file, maxAgentStateBytes+1))
	if err != nil {
		return credentials{}, err
	}
	if len(contents) > maxAgentStateBytes {
		return credentials{}, errors.New("agent credential state exceeds 512 KiB")
	}
	var value credentials
	if err := json.Unmarshal(contents, &value); err != nil {
		return credentials{}, err
	}
	if value.AgentID == "" || value.PrivateKey == "" {
		return credentials{}, errors.New("agent credential file is incomplete")
	}
	if _, err := authn.DecodePrivateKey(value.PrivateKey); err != nil {
		return credentials{}, err
	}
	return value, nil
}

func saveCredentials(path string, value credentials) error {
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
	suffix, err := randomSuffix(10)
	if err != nil {
		return err
	}
	tempName := ".agent-state-" + suffix + ".tmp"
	temp, err := root.OpenFile(tempName, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer root.Remove(tempName)
	if err := json.NewEncoder(temp).Encode(value); err != nil {
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

func validateStateDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect agent state directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("agent state directory must be a real directory, not a symlink")
	}
	if info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("agent state directory permissions %04o allow group/other writes", info.Mode().Perm())
	}
	if err := validateOwner(info, "agent state directory"); err != nil {
		return err
	}
	return nil
}

func loadTrustedCA(path string) (*x509.CertPool, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("custom CA path must be absolute")
	}
	directory := filepath.Dir(path)
	directoryInfo, err := os.Lstat(directory)
	if err != nil {
		return nil, err
	}
	if directoryInfo.Mode()&os.ModeSymlink != 0 || !directoryInfo.IsDir() || directoryInfo.Mode().Perm()&0o022 != 0 {
		return nil, errors.New("custom CA directory is symlinked or writable by group/others")
	}
	if err := validateOwner(directoryInfo, "custom CA directory"); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	baseName := filepath.Base(path)
	linkInfo, err := root.Lstat(baseName)
	if err != nil {
		return nil, err
	}
	if linkInfo.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("custom CA file must not be a symlink")
	}
	file, err := root.Open(baseName)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("custom CA must be a regular file")
	}
	if info.Mode().Perm()&0o022 != 0 {
		return nil, errors.New("custom CA file is writable by group or others")
	}
	if err := validateOwner(info, "custom CA file"); err != nil {
		return nil, err
	}
	contents, err := io.ReadAll(io.LimitReader(file, 1<<20+1))
	if err != nil {
		return nil, err
	}
	if len(contents) > 1<<20 {
		return nil, errors.New("custom CA bundle exceeds 1 MiB")
	}
	rootCAs, err := x509.SystemCertPool()
	if err != nil || rootCAs == nil {
		rootCAs = x509.NewCertPool()
	}
	if !rootCAs.AppendCertsFromPEM(contents) {
		return nil, errors.New("custom CA file contains no valid PEM certificates")
	}
	return rootCAs, nil
}

func explainTLSError(err error) error {
	if err == nil {
		return nil
	}
	var unknownAuthority x509.UnknownAuthorityError
	if errors.As(err, &unknownAuthority) || strings.Contains(err.Error(), "certificate signed by unknown authority") {
		return fmt.Errorf("TLS certificate is not trusted; install the control-plane CA and set QCH_TLS_CA_FILE to its absolute path: %w", err)
	}
	var hostnameError x509.HostnameError
	if errors.As(err, &hostnameError) {
		return fmt.Errorf("TLS certificate does not cover the QCH_SERVER_URL host: %w", err)
	}
	return err
}

func validateOwner(info os.FileInfo, description string) error {
	expected := os.Geteuid()
	uid, known := fileOwnerUID(info)
	if expected < 0 || !known {
		return nil
	}
	if int(uid) != expected {
		return fmt.Errorf("%s is owned by uid %d, expected uid %d", description, uid, expected)
	}
	return nil
}

func validateOwnerOrRoot(info os.FileInfo, description string) error {
	expected := os.Geteuid()
	uid, known := fileOwnerUID(info)
	if expected < 0 || !known || uid == 0 || int(uid) == expected {
		return nil
	}
	return fmt.Errorf("%s is owned by uid %d, expected root or uid %d", description, uid, expected)
}

func fileOwnerUID(info os.FileInfo) (uint64, bool) {
	if info == nil || info.Sys() == nil {
		return 0, false
	}
	value := reflect.Indirect(reflect.ValueOf(info.Sys()))
	if !value.IsValid() {
		return 0, false
	}
	uid := value.FieldByName("Uid")
	if !uid.IsValid() || !uid.CanUint() {
		return 0, false
	}
	return uid.Uint(), true
}
