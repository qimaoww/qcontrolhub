package core

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

type Engine string

// AgentFeatureSelfUpgrade identifies the signed task protocol needed for a
// running Agent to replace its own executable and reconnect. Older Agents do
// not advertise this feature and must be reinstalled once before remote
// upgrades can be used.
const AgentFeatureSelfUpgrade = "agent-self-upgrade-v1"

// AgentFeaturePortTraffic identifies Agents that can account and enforce
// per-port traffic quotas independently of the managed proxy engine.
const AgentFeaturePortTraffic = "port-traffic-v1"

// AgentFeatureCoreLogs identifies Agents that stream managed core logs to the
// control plane without persisting them on the node.
const AgentFeatureCoreLogs = "core-logs-v1"

// Role identifies the account class. Fine-grained access is carried by the
// explicit Permissions field on a user; only admin/user are persisted.
type Role string

const (
	RoleAdmin Role = "admin"
	RoleUser  Role = "user"
	// Deprecated aliases keep older integrations compiling. They serialize as
	// "user" and are not accepted as separate persisted identities.
	RoleOperator Role = RoleUser
	RoleAuditor  Role = RoleUser
	RoleReadonly Role = RoleUser
)

// AtLeast reports whether the role grants at least the given privilege level.
func (role Role) AtLeast(minimum Role) bool {
	rank := func(value Role) int {
		switch value {
		case RoleAdmin:
			return 3
		case RoleUser:
			return 1
		default:
			return 1
		}
	}
	return rank(role) >= rank(minimum)
}

func (role Role) Valid() bool {
	switch role {
	case RoleAdmin, RoleUser:
		return true
	default:
		return false
	}
}

// User is a durable panel login identity. Password hashes are intentionally
// never included in this public model.
type User struct {
	ID          string       `json:"id"`
	Username    string       `json:"username"`
	DisplayName string       `json:"display_name,omitempty"`
	Role        Role         `json:"role"`
	Permissions []Permission `json:"permissions,omitempty"`
	Disabled    bool         `json:"disabled"`
	CreatedAt   time.Time    `json:"created_at"`
	UpdatedAt   time.Time    `json:"updated_at"`
	LastLoginAt *time.Time   `json:"last_login_at,omitempty"`
}

type UserRequest struct {
	Username    string       `json:"username"`
	DisplayName string       `json:"display_name"`
	Role        Role         `json:"role"`
	Password    string       `json:"password"`
	Permissions []Permission `json:"permissions,omitempty"`
}

type UserUpdate struct {
	DisplayName *string       `json:"display_name"`
	Role        *Role         `json:"role"`
	Password    *string       `json:"password"`
	Disabled    *bool         `json:"disabled"`
	Permissions *[]Permission `json:"permissions"`
}

const (
	EngineMihomo          Engine = "mihomo"
	EngineXray            Engine = "xray"
	EngineSingBox         Engine = "sing-box"
	EngineShadowsocksRust Engine = "ss-rust"
)

func (s TaskStatus) Valid() bool {
	switch s {
	case TaskPending, TaskRunning, TaskSucceeded, TaskFailed, TaskCanceled:
		return true
	default:
		return false
	}
}

func (e Engine) Valid() bool {
	switch e {
	case EngineMihomo, EngineXray, EngineSingBox, EngineShadowsocksRust:
		return true
	default:
		return false
	}
}

func ParseEngine(value string) (Engine, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "singbox" {
		value = string(EngineSingBox)
	}
	if value == "ssrust" || value == "shadowsocks-rust" || value == "shadowsocksrust" {
		value = string(EngineShadowsocksRust)
	}
	engine := Engine(value)
	if !engine.Valid() {
		return "", fmt.Errorf("unsupported engine %q", value)
	}
	return engine, nil
}

type Action string

const (
	ActionValidate       Action = "validate"
	ActionDeploy         Action = "deploy"
	ActionStart          Action = "start"
	ActionStop           Action = "stop"
	ActionRestart        Action = "restart"
	ActionStatus         Action = "status"
	ActionInstall        Action = "install"
	ActionReadConfig     Action = "read-config"
	ActionImportExisting Action = "import-existing"
	ActionUpgradeAgent   Action = "upgrade-agent"
)

func (a Action) Valid() bool {
	switch a {
	case ActionValidate, ActionDeploy, ActionStart, ActionStop, ActionRestart, ActionStatus, ActionInstall, ActionReadConfig, ActionImportExisting, ActionUpgradeAgent:
		return true
	default:
		return false
	}
}

type RuntimeState struct {
	Installed                       bool   `json:"installed"`
	Version                         string `json:"version,omitempty"`
	ServiceStatus                   string `json:"service_status,omitempty"`
	ExistingConfigAvailable         bool   `json:"existing_config_available,omitempty"`
	ExistingConfigUnsupportedReason string `json:"existing_config_unsupported_reason,omitempty"`
}

type HostMetrics struct {
	CollectedAt       time.Time              `json:"collected_at"`
	CPUAvailable      bool                   `json:"cpu_available"`
	CPUPercent        float64                `json:"cpu_percent"`
	MemoryAvailable   bool                   `json:"memory_available"`
	MemoryUsedBytes   uint64                 `json:"memory_used_bytes"`
	MemoryTotalBytes  uint64                 `json:"memory_total_bytes"`
	DiskAvailable     bool                   `json:"disk_available"`
	DiskUsedBytes     uint64                 `json:"disk_used_bytes"`
	DiskTotalBytes    uint64                 `json:"disk_total_bytes"`
	NetworkAvailable  bool                   `json:"network_available"`
	NetworkRXBytes    uint64                 `json:"network_rx_bytes"`
	NetworkTXBytes    uint64                 `json:"network_tx_bytes"`
	NetworkRXBPS      uint64                 `json:"network_rx_bps"`
	NetworkTXBPS      uint64                 `json:"network_tx_bps"`
	NetworkInterfaces []HostNetworkInterface `json:"network_interfaces,omitempty"`
	// ObservedPublicIP is assigned by the control plane from the authenticated
	// WSS source. Agent-provided values are never trusted.
	ObservedPublicIP string `json:"observed_public_ip,omitempty"`
}

// HostNetworkInterface describes addresses assigned to an interface that
// carries a default route. Agents only report usable unicast addresses; the
// control plane never needs wildcard, loopback, multicast or link-local
// addresses to generate client connection details.
type HostNetworkInterface struct {
	Name      string   `json:"name"`
	Addresses []string `json:"addresses,omitempty"`
}

type Agent struct {
	ID           string                  `json:"id"`
	Name         string                  `json:"name"`
	Version      string                  `json:"version,omitempty"`
	OS           string                  `json:"os"`
	Arch         string                  `json:"arch"`
	Capabilities []Engine                `json:"capabilities"`
	Features     []string                `json:"features,omitempty"`
	Labels       map[string]string       `json:"labels,omitempty"`
	Runtime      map[Engine]RuntimeState `json:"runtime,omitempty"`
	Metrics      HostMetrics             `json:"metrics,omitempty"`
	LastSeen     time.Time               `json:"last_seen"`
	EnrolledAt   time.Time               `json:"enrolled_at"`
	PublicKey    []byte                  `json:"-"`
	Status       string                  `json:"status,omitempty"`
	Reinstalled  bool                    `json:"-"`
}

type Config struct {
	ID          string    `json:"id"`
	AgentID     string    `json:"agent_id,omitempty"`
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	Engine      Engine    `json:"engine"`
	Content     string    `json:"content"`
	Version     int       `json:"version"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type TaskStatus string

const (
	TaskPending   TaskStatus = "pending"
	TaskRunning   TaskStatus = "running"
	TaskSucceeded TaskStatus = "succeeded"
	TaskFailed    TaskStatus = "failed"
	TaskCanceled  TaskStatus = "canceled"
)

type Task struct {
	ID            string     `json:"id"`
	AgentID       string     `json:"agent_id"`
	Action        Action     `json:"action"`
	Engine        Engine     `json:"engine"`
	ConfigID      string     `json:"config_id,omitempty"`
	ConfigVersion int        `json:"config_version,omitempty"`
	ConfigContent string     `json:"config_content,omitempty"`
	CoreVersion   string     `json:"core_version,omitempty"`
	Status        TaskStatus `json:"status"`
	Attempt       int        `json:"attempt"`
	LeaseID       string     `json:"lease_id,omitempty"`
	Output        string     `json:"output,omitempty"`
	Error         string     `json:"error,omitempty"`
	Reused        bool       `json:"reused,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	StartedAt     *time.Time `json:"started_at,omitempty"`
	FinishedAt    *time.Time `json:"finished_at,omitempty"`
}

type TaskRequest struct {
	AgentID     string `json:"agent_id"`
	Action      Action `json:"action"`
	Engine      Engine `json:"engine"`
	ConfigID    string `json:"config_id,omitempty"`
	CoreVersion string `json:"core_version,omitempty"`
}

type Deployment struct {
	AgentID       string    `json:"agent_id"`
	Engine        Engine    `json:"engine"`
	ConfigID      string    `json:"config_id"`
	ConfigVersion int       `json:"config_version"`
	DeployedAt    time.Time `json:"deployed_at"`
}

type EnrollRequest struct {
	Name         string            `json:"name"`
	Version      string            `json:"version,omitempty"`
	OS           string            `json:"os"`
	Arch         string            `json:"arch"`
	Capabilities []Engine          `json:"capabilities"`
	Features     []string          `json:"features,omitempty"`
	Labels       map[string]string `json:"labels,omitempty"`
	PublicKey    string            `json:"public_key"`
}

type EnrollResponse struct {
	AgentID string `json:"agent_id"`
}

type EnrollmentToken struct {
	ID        string     `json:"id"`
	AgentID   string     `json:"agent_id,omitempty"`
	Name      string     `json:"name"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	MaxUses   int        `json:"max_uses"`
	UsedCount int        `json:"used_count"`
	Reusable  bool       `json:"reusable,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
}

type EnrollmentTokenRequest struct {
	Name       string `json:"name"`
	TTLMinutes int    `json:"ttl_minutes"`
	MaxUses    int    `json:"max_uses"`
	Reusable   bool   `json:"reusable,omitempty"`
}

type EnrollmentTokenCreated struct {
	EnrollmentToken
	Token string `json:"token"`
}

type HeartbeatRequest struct {
	Version      string                  `json:"version,omitempty"`
	Runtime      map[Engine]RuntimeState `json:"runtime,omitempty"`
	Metrics      *HostMetrics            `json:"metrics,omitempty"`
	TrafficUsage []PortTrafficUsage      `json:"traffic_usage,omitempty"`
	Features     []string                `json:"features,omitempty"`
}

type TaskResultRequest struct {
	LeaseID string `json:"lease_id"`
	Success bool   `json:"success"`
	Output  string `json:"output,omitempty"`
	Error   string `json:"error,omitempty"`
}

const (
	WireHello       = "hello"
	WireHeartbeat   = "heartbeat"
	WireMetrics     = "metrics"
	WireTask        = "task"
	WireResult      = "result"
	WireResultAck   = "result_ack"
	WireCoreLogs    = "core_logs"
	WireCoreLogsAck = "core_logs_ack"
	WireError       = "error"
)

const (
	MaxCoreLogBatchEntries = 32
	MaxCoreLogMessageBytes = 4096
	// JSON escaping can expand a batch of control-character-heavy messages to
	// nearly six times the raw message size.
	MaxCoreLogWireBytes = 1 << 20
)

// CoreLogEntry is one line emitted by a managed proxy core. AgentID and ID are
// assigned by the control plane; Agents only submit the remaining fields.
type CoreLogEntry struct {
	ID         int64     `json:"id,omitempty"`
	AgentID    string    `json:"agent_id,omitempty"`
	Engine     Engine    `json:"engine"`
	Level      string    `json:"level"`
	Message    string    `json:"message"`
	LoggedAt   time.Time `json:"logged_at"`
	ReceivedAt time.Time `json:"received_at,omitempty"`
}

type CoreLogBatch struct {
	ID      string         `json:"id"`
	Entries []CoreLogEntry `json:"entries"`
}

type TaskResultEnvelope struct {
	TaskID string            `json:"task_id"`
	Result TaskResultRequest `json:"result"`
}

type WireMessage struct {
	Type            string              `json:"type"`
	Heartbeat       *HeartbeatRequest   `json:"heartbeat,omitempty"`
	Metrics         *HostMetrics        `json:"metrics,omitempty"`
	Task            *Task               `json:"task,omitempty"`
	Result          *TaskResultEnvelope `json:"result,omitempty"`
	CoreLogs        *CoreLogBatch       `json:"core_logs,omitempty"`
	TrafficPolicies []PortTrafficPolicy `json:"traffic_policies,omitempty"`
	TaskID          string              `json:"task_id,omitempty"`
	BatchID         string              `json:"batch_id,omitempty"`
	Error           string              `json:"error,omitempty"`
}

type Overview struct {
	Agents       int `json:"agents"`
	AgentsOnline int `json:"agents_online"`
	Configs      int `json:"configs"`
	NodeConfigs  int `json:"node_configs"`
	TasksPending int `json:"tasks_pending"`
	TasksQueued  int `json:"tasks_queued"`
	TasksRunning int `json:"tasks_running"`
	TasksFailed  int `json:"tasks_failed"`
}

// MetricSample is one historical resource sample recorded for an agent.
// Rates are stored in bytes per second and percentages as 0-100 values.
type MetricSample struct {
	SampledAt     time.Time `json:"sampled_at"`
	CPUPercent    float64   `json:"cpu_percent"`
	MemoryPercent float64   `json:"memory_percent"`
	RXRateBPS     uint64    `json:"rx_rate_bps"`
	TXRateBPS     uint64    `json:"tx_rate_bps"`
}

// AuditLogEntry is one administrative action recorded for the audit trail.
type AuditLogEntry struct {
	ID       int64     `json:"id"`
	ActedAt  time.Time `json:"acted_at"`
	Actor    string    `json:"actor"`
	Action   string    `json:"action"`
	Target   string    `json:"target,omitempty"`
	Detail   string    `json:"detail,omitempty"`
	RemoteIP string    `json:"remote_ip,omitempty"`
}

// ConfigTemplate is a reusable configuration body with {{variable}} placeholders
// that are rendered per node when the template is applied.
type ConfigTemplate struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Engine    Engine    `json:"engine"`
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type PanelSettings struct {
	PanelName          string    `json:"panel_name"`
	PanelDescription   string    `json:"panel_description"`
	TaskPageSize       int       `json:"task_page_size"`
	TaskPollIntervalMS int       `json:"task_poll_interval_ms"`
	WebhookURL         string    `json:"webhook_url"`
	UpdatedAt          time.Time `json:"updated_at"`
}

func DefaultPanelSettings() PanelSettings {
	return PanelSettings{
		PanelName:          "QControlHub",
		PanelDescription:   "可信远程编排",
		TaskPageSize:       100,
		TaskPollIntervalMS: 600,
	}
}

func (settings PanelSettings) Validate() error {
	if settings.PanelName == "" {
		return errors.New("panel name is required")
	}
	if utf8.RuneCountInString(settings.PanelName) > 40 {
		return errors.New("panel name must not exceed 40 characters")
	}
	if utf8.RuneCountInString(settings.PanelDescription) > 120 {
		return errors.New("panel description must not exceed 120 characters")
	}
	if !oneOf(settings.TaskPageSize, 50, 100, 500) {
		return errors.New("unsupported task page size")
	}
	if !oneOf(settings.TaskPollIntervalMS, 600, 1000, 2000, 5000) {
		return errors.New("unsupported task polling interval")
	}
	settings.WebhookURL = strings.TrimSpace(settings.WebhookURL)
	if settings.WebhookURL != "" {
		if utf8.RuneCountInString(settings.WebhookURL) > 500 {
			return errors.New("webhook URL must not exceed 500 characters")
		}
		parsed, err := url.Parse(settings.WebhookURL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return errors.New("webhook URL must be an absolute http(s) URL")
		}
	}
	return nil
}

func oneOf(value int, allowed ...int) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}

func NewID(prefix string) (string, error) {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return prefix + "_" + hex.EncodeToString(buf), nil
}

func NewToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
