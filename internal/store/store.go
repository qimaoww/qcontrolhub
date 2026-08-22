package store

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/qimaoww/qcontrolhub/internal/authn"
	"github.com/qimaoww/qcontrolhub/internal/core"
)

var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("conflict")
	ErrReplay   = errors.New("replayed request")
	ErrInvalid  = errors.New("invalid input")
)

type Store struct {
	pool       *pgxpool.Pool
	cryptor    *configCryptor
	taskWakeMu sync.Mutex
	taskWakes  map[string]chan struct{}
}

const currentSchemaVersion = 19

func Open(ctx context.Context, databaseURL string, allowInsecureRemote bool) (*Store, error) {
	return OpenWithConfigKey(ctx, databaseURL, allowInsecureRemote, "")
}

// OpenWithConfigKey opens the store and enables at-rest configuration
// encryption when a non-empty key is supplied. Existing plaintext rows keep
// working transparently; new writes are sealed with AES-256-GCM.
func OpenWithConfigKey(ctx context.Context, databaseURL string, allowInsecureRemote bool, configKey string) (*Store, error) {
	if strings.TrimSpace(databaseURL) == "" {
		return nil, errors.New("QCH_DATABASE_URL is required")
	}
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse PostgreSQL URL: %w", err)
	}
	if !localDatabaseHost(config.ConnConfig.Host) && !allowInsecureRemote {
		tlsConfig := config.ConnConfig.TLSConfig
		verifyFull := tlsConfig != nil && !tlsConfig.InsecureSkipVerify && tlsConfig.ServerName != "" && len(config.ConnConfig.Fallbacks) == 0
		if !verifyFull {
			return nil, errors.New("remote PostgreSQL connections must use sslmode=verify-full without cleartext fallback; set QCH_ALLOW_INSECURE_DATABASE=true only on a trusted development network")
		}
	}
	config.MaxConns = 20
	config.MinConns = 2
	config.MaxConnLifetime = 30 * time.Minute
	config.MaxConnIdleTime = 5 * time.Minute
	config.HealthCheckPeriod = 30 * time.Second
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("open PostgreSQL pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connect to PostgreSQL: %w", err)
	}
	cryptor, err := newConfigCryptor(configKey)
	if err != nil {
		return nil, err
	}
	if err := cryptor.verify(); err != nil {
		return nil, err
	}
	result := &Store{pool: pool, cryptor: cryptor}
	if err := result.migrate(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return result, nil
}

func localDatabaseHost(host string) bool {
	if strings.HasPrefix(host, "/") || strings.EqualFold(host, "localhost") {
		return true
	}
	address := net.ParseIP(host)
	return address != nil && address.IsLoopback()
}

func (s *Store) Close() {
	s.pool.Close()
}

func (s *Store) Ping(ctx context.Context) error {
	return s.pool.Ping(ctx)
}

func (s *Store) migrate(ctx context.Context) error {
	connection, err := s.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer connection.Release()
	if _, err := connection.Exec(ctx, "SELECT pg_advisory_lock($1)", int64(0x52464f524745)); err != nil {
		return fmt.Errorf("acquire migration lock: %w", err)
	}
	defer connection.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", int64(0x52464f524745))
	if _, err := connection.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS qcontrolhub_schema_migrations (
			version integer PRIMARY KEY CHECK (version > 0),
			applied_at timestamptz NOT NULL DEFAULT now()
		)`); err != nil {
		return fmt.Errorf("initialize schema migration ledger: %w", err)
	}
	var appliedVersion int
	if err := connection.QueryRow(ctx, `SELECT COALESCE(max(version),0) FROM qcontrolhub_schema_migrations`).Scan(&appliedVersion); err != nil {
		return fmt.Errorf("read schema migration version: %w", err)
	}
	if appliedVersion > currentSchemaVersion {
		return fmt.Errorf("database schema version %d is newer than this QControlHub binary supports (%d)", appliedVersion, currentSchemaVersion)
	}
	if appliedVersion == currentSchemaVersion {
		return nil
	}

	tx, err := connection.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin schema migration: %w", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, schemaSQL); err != nil {
		return fmt.Errorf("apply PostgreSQL schema: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO qcontrolhub_schema_migrations (version) VALUES ($1)`, currentSchemaVersion); err != nil {
		return fmt.Errorf("record schema migration version: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit schema migration: %w", err)
	}
	return nil
}

func (s *Store) EnrollAgent(ctx context.Context, request core.EnrollRequest, enrollmentToken string) (core.Agent, error) {
	id, err := core.NewID("agt")
	if err != nil {
		return core.Agent{}, err
	}
	publicKey, err := base64.RawURLEncoding.DecodeString(request.PublicKey)
	if err != nil || len(publicKey) != 32 {
		return core.Agent{}, fmt.Errorf("%w: invalid Ed25519 public key", ErrInvalid)
	}
	capabilities, _ := json.Marshal(request.Capabilities)
	features, _ := json.Marshal(request.Features)
	if len(request.Features) == 0 {
		features = []byte(`[]`)
	}
	labels, _ := json.Marshal(request.Labels)
	runtimeState := []byte(`{}`)
	enrolledAt := time.Now().UTC()
	lastSeen := time.Unix(0, 0).UTC()
	if len(enrollmentToken) < 32 {
		return core.Agent{}, ErrNotFound
	}
	tokenDigest := sha256.Sum256([]byte(enrollmentToken))
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return core.Agent{}, err
	}
	defer tx.Rollback(ctx)
	var enrollmentID, enrollmentName string
	var enrollmentAgentID *string
	var reusable bool
	err = tx.QueryRow(ctx, `
		UPDATE enrollment_tokens SET used_count=used_count+1
		WHERE token_hash=$1 AND revoked_at IS NULL
		  AND (reusable OR (expires_at>now() AND used_count<max_uses))
		RETURNING id,name,reusable,agent_id`, tokenDigest[:]).Scan(&enrollmentID, &enrollmentName, &reusable, &enrollmentAgentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return core.Agent{}, ErrNotFound
	}
	if err != nil {
		return core.Agent{}, err
	}
	name := strings.TrimSpace(request.Name)
	reinstalled := false
	if reusable {
		if name != enrollmentName {
			return core.Agent{}, ErrNotFound
		}
		boundAgentID := ""
		if enrollmentAgentID != nil {
			boundAgentID = strings.TrimSpace(*enrollmentAgentID)
		}
		if boundAgentID != "" {
			err = tx.QueryRow(ctx, `SELECT id FROM agents WHERE id=$1 AND revoked_at IS NULL FOR UPDATE`, boundAgentID).Scan(&id)
		} else {
			err = tx.QueryRow(ctx, `SELECT id FROM agents WHERE enrollment_id=$1 AND revoked_at IS NULL FOR UPDATE`, enrollmentID).Scan(&id)
		}
		if errors.Is(err, pgx.ErrNoRows) {
			if boundAgentID != "" {
				return core.Agent{}, ErrNotFound
			}
			err = nil
		} else if err != nil {
			return core.Agent{}, err
		} else {
			reinstalled = true
		}
	}
	if reinstalled {
		_, err = tx.Exec(ctx, `
			UPDATE agents SET name=$2,version=$3,os=$4,arch=$5,capabilities=$6,features=$7,labels=$8,runtime=$9,
				metrics='{}'::jsonb,public_key=$10,last_seen=$11,enrolled_at=$12,revoked_at=NULL
			WHERE id=$1`, id, name, strings.TrimSpace(request.Version), strings.TrimSpace(request.OS), strings.TrimSpace(request.Arch),
			capabilities, features, labels, runtimeState, publicKey, lastSeen, enrolledAt)
		if err == nil {
			_, err = tx.Exec(ctx, `DELETE FROM agent_nonces WHERE agent_id=$1`, id)
		}
	} else {
		_, err = tx.Exec(ctx, `
			INSERT INTO agents (id,name,version,os,arch,capabilities,features,labels,runtime,public_key,last_seen,enrolled_at,enrollment_id)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
			id, name, strings.TrimSpace(request.Version), strings.TrimSpace(request.OS), strings.TrimSpace(request.Arch),
			capabilities, features, labels, runtimeState, publicKey, lastSeen, enrolledAt, nullableEnrollmentID(reusable, enrollmentID))
	}
	if err != nil {
		return core.Agent{}, mapError(err)
	}
	if reusable {
		result, bindErr := tx.Exec(ctx, `
			UPDATE enrollment_tokens SET agent_id=$2
			WHERE id=$1 AND (agent_id IS NULL OR agent_id=$2)`, enrollmentID, id)
		if bindErr != nil {
			return core.Agent{}, mapError(bindErr)
		}
		if result.RowsAffected() == 0 {
			return core.Agent{}, ErrConflict
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return core.Agent{}, err
	}
	return core.Agent{
		ID: id, Name: name, Version: request.Version,
		OS: request.OS, Arch: request.Arch, Capabilities: append([]core.Engine(nil), request.Capabilities...), Features: append([]string(nil), request.Features...),
		Labels: cloneLabels(request.Labels), Runtime: map[core.Engine]core.RuntimeState{},
		LastSeen: lastSeen, EnrolledAt: enrolledAt, Status: "offline", Reinstalled: reinstalled,
	}, nil
}

func nullableEnrollmentID(reusable bool, enrollmentID string) any {
	if reusable {
		return enrollmentID
	}
	return nil
}

func (s *Store) AgentPublicKey(ctx context.Context, id string) ([]byte, error) {
	var publicKey []byte
	err := s.pool.QueryRow(ctx, `SELECT public_key FROM agents WHERE id=$1 AND revoked_at IS NULL`, id).Scan(&publicKey)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return publicKey, nil
}

func (s *Store) RecordNonce(ctx context.Context, agentID, nonce string, expiresAt time.Time) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO agent_nonces (agent_id, nonce, expires_at) VALUES ($1,$2,$3)`, agentID, nonce, expiresAt)
	if isUniqueViolation(err) {
		return ErrReplay
	}
	return err
}

func (s *Store) CleanupNonces(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM agent_nonces WHERE expires_at < now()`)
	return err
}

func (s *Store) CreateEnrollmentToken(ctx context.Context, request core.EnrollmentTokenRequest) (core.EnrollmentTokenCreated, error) {
	name := strings.TrimSpace(request.Name)
	if name == "" {
		name = "Add node"
	}
	if utf8.RuneCountInString(name) > 100 {
		return core.EnrollmentTokenCreated{}, fmt.Errorf("%w: add-node name exceeds 100 characters", ErrInvalid)
	}
	if !request.Reusable {
		if request.TTLMinutes == 0 {
			request.TTLMinutes = 15
		}
		if request.TTLMinutes < 1 || request.TTLMinutes > 1440 {
			return core.EnrollmentTokenCreated{}, fmt.Errorf("%w: enrollment token lifetime must be between 1 and 1440 minutes", ErrInvalid)
		}
		if request.MaxUses == 0 {
			request.MaxUses = 1
		}
		if request.MaxUses < 1 || request.MaxUses > 50 {
			return core.EnrollmentTokenCreated{}, fmt.Errorf("%w: enrollment token max uses must be between 1 and 50", ErrInvalid)
		}
	}
	if request.Reusable {
		var exists bool
		if err := s.pool.QueryRow(ctx, `
			SELECT EXISTS(
				SELECT 1 FROM enrollment_tokens
				WHERE reusable=TRUE AND revoked_at IS NULL AND lower(name)=lower($1)
				UNION ALL
				SELECT 1 FROM agents
				WHERE revoked_at IS NULL AND lower(name)=lower($1)
			)`, name).Scan(&exists); err != nil {
			return core.EnrollmentTokenCreated{}, err
		}
		if exists {
			return core.EnrollmentTokenCreated{}, ErrConflict
		}
	}
	id, err := core.NewID("enr")
	if err != nil {
		return core.EnrollmentTokenCreated{}, err
	}
	rawToken, err := core.NewToken()
	if err != nil {
		return core.EnrollmentTokenCreated{}, err
	}
	digest := sha256.Sum256([]byte(rawToken))
	now := time.Now().UTC()
	value := core.EnrollmentToken{
		ID: id, Name: name, MaxUses: request.MaxUses, UsedCount: 0, Reusable: request.Reusable, CreatedAt: now,
	}
	if !request.Reusable {
		expiresAt := now.Add(time.Duration(request.TTLMinutes) * time.Minute)
		value.ExpiresAt = &expiresAt
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO enrollment_tokens (id,name,token_hash,expires_at,max_uses,used_count,reusable,created_at)
		VALUES ($1,$2,$3,$4,$5,0,$6,$7)`,
		value.ID, value.Name, digest[:], value.ExpiresAt, value.MaxUses, value.Reusable, value.CreatedAt)
	if err != nil {
		return core.EnrollmentTokenCreated{}, mapError(err)
	}
	return core.EnrollmentTokenCreated{EnrollmentToken: value, Token: rawToken}, nil
}

// CreateAgentEnrollmentToken adds a reusable credential for an existing agent.
// Existing credentials remain valid and the plaintext token is returned once.
func (s *Store) CreateAgentEnrollmentToken(ctx context.Context, agentID string) (core.EnrollmentTokenCreated, error) {
	agentID = strings.TrimSpace(agentID)
	if agentID == "" {
		return core.EnrollmentTokenCreated{}, ErrInvalid
	}
	rawToken, err := core.NewToken()
	if err != nil {
		return core.EnrollmentTokenCreated{}, err
	}
	digest := sha256.Sum256([]byte(rawToken))
	now := time.Now().UTC()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return core.EnrollmentTokenCreated{}, err
	}
	defer tx.Rollback(ctx)

	var name string
	if err := tx.QueryRow(ctx, `
		SELECT name FROM agents
		WHERE id=$1 AND revoked_at IS NULL FOR UPDATE`, agentID).Scan(&name); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return core.EnrollmentTokenCreated{}, ErrNotFound
		}
		return core.EnrollmentTokenCreated{}, err
	}

	value := core.EnrollmentToken{
		AgentID: agentID, Name: strings.TrimSpace(name), MaxUses: 0, UsedCount: 0,
		Reusable: true, CreatedAt: now,
	}
	if value.Name == "" {
		return core.EnrollmentTokenCreated{}, fmt.Errorf("%w: agent name is empty", ErrInvalid)
	}
	value.ID, err = core.NewID("enr")
	if err != nil {
		return core.EnrollmentTokenCreated{}, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO enrollment_tokens
			(id,agent_id,name,token_hash,expires_at,max_uses,used_count,reusable,created_at)
		VALUES ($1,$2,$3,$4,NULL,0,0,TRUE,$5)`,
		value.ID, value.AgentID, value.Name, digest[:], now); err != nil {
		return core.EnrollmentTokenCreated{}, mapError(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return core.EnrollmentTokenCreated{}, err
	}
	return core.EnrollmentTokenCreated{EnrollmentToken: value, Token: rawToken}, nil
}

// EnrollmentTokenUsable checks an add-node credential without consuming it.
// Reusable node credentials remain valid until explicitly deleted.
func (s *Store) EnrollmentTokenUsable(ctx context.Context, rawToken string) bool {
	rawToken = strings.TrimSpace(rawToken)
	if len(rawToken) < 32 {
		return false
	}
	digest := sha256.Sum256([]byte(rawToken))
	var expiresAt *time.Time
	var maxUses, usedCount int
	var reusable bool
	var revokedAt *time.Time
	err := s.pool.QueryRow(ctx, `
		SELECT expires_at,max_uses,used_count,reusable,revoked_at
		FROM enrollment_tokens WHERE token_hash=$1`, digest[:]).Scan(&expiresAt, &maxUses, &usedCount, &reusable, &revokedAt)
	return err == nil && revokedAt == nil && (reusable || (expiresAt != nil && usedCount < maxUses && time.Now().Before(*expiresAt)))
}

func (s *Store) ListEnrollmentTokens(ctx context.Context) ([]core.EnrollmentToken, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id,COALESCE(agent_id,''),name,expires_at,max_uses,used_count,reusable,created_at,revoked_at
		FROM enrollment_tokens ORDER BY created_at DESC LIMIT 100`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]core.EnrollmentToken, 0)
	for rows.Next() {
		var value core.EnrollmentToken
		if err := rows.Scan(&value.ID, &value.AgentID, &value.Name, &value.ExpiresAt, &value.MaxUses, &value.UsedCount, &value.Reusable, &value.CreatedAt, &value.RevokedAt); err != nil {
			return nil, err
		}
		result = append(result, value)
	}
	return result, rows.Err()
}

func (s *Store) DeleteEnrollmentToken(ctx context.Context, id string) error {
	command, err := s.pool.Exec(ctx, `DELETE FROM enrollment_tokens WHERE id=$1`, id)
	if err != nil {
		return err
	}
	if command.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) Heartbeat(ctx context.Context, id string, heartbeat core.HeartbeatRequest) error {
	receivedAt := time.Now().UTC()
	heartbeat.Version = strings.TrimSpace(heartbeat.Version)
	if utf8.RuneCountInString(heartbeat.Version) > 100 {
		return fmt.Errorf("%w: agent version exceeds 100 characters", ErrInvalid)
	}
	runtimeState, err := json.Marshal(heartbeat.Runtime)
	if err != nil {
		return err
	}
	metricsState, err := encodeHeartbeatMetrics(heartbeat.Metrics, receivedAt)
	if err != nil {
		return err
	}
	featuresState, err := json.Marshal(heartbeat.Features)
	if err != nil {
		return err
	}
	if len(heartbeat.Features) == 0 {
		featuresState = []byte(`[]`)
	}
	command, err := s.pool.Exec(ctx, `
			UPDATE agents SET last_seen=now(), version=CASE WHEN $2='' THEN version ELSE $2 END, runtime=$3,
			                  metrics=CASE
			                    WHEN $4::jsonb IS NULL THEN metrics
			                    WHEN $4::jsonb ? 'network_interfaces' OR NOT (metrics ? 'network_interfaces') THEN $4::jsonb
			                    ELSE $4::jsonb || jsonb_build_object('network_interfaces', metrics->'network_interfaces')
			                  END,
			                  features=CASE WHEN jsonb_array_length($5::jsonb)=0 THEN features ELSE $5::jsonb END
			WHERE id=$1 AND revoked_at IS NULL`, id, heartbeat.Version, runtimeState, metricsState, featuresState)
	if err != nil {
		return err
	}
	if command.RowsAffected() == 0 {
		return ErrNotFound
	}
	return s.UpdatePortTrafficUsage(ctx, id, heartbeat.TrafficUsage, receivedAt)
}

// UpdateAgentMetrics refreshes only the live metrics snapshot from the
// high-frequency metrics pushes. The push proves liveness, so last_seen is
// refreshed as well, while version, runtime, and features stay untouched.
func (s *Store) UpdateAgentMetrics(ctx context.Context, id string, metrics core.HostMetrics) error {
	metricsState, err := encodeHeartbeatMetrics(&metrics, time.Now().UTC())
	if err != nil {
		return err
	}
	command, err := s.pool.Exec(ctx, `
			UPDATE agents SET last_seen=now(), metrics=CASE
			  WHEN $2::jsonb ? 'network_interfaces' OR NOT (metrics ? 'network_interfaces') THEN $2::jsonb
			  ELSE $2::jsonb || jsonb_build_object('network_interfaces', metrics->'network_interfaces')
			END
			WHERE id=$1 AND revoked_at IS NULL`, id, metricsState)
	if err != nil {
		return err
	}
	if command.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// UpdateAgentObservedPublicIP stores the authenticated WSS peer address in
// the existing metrics snapshot without disturbing Agent-reported counters or
// default-route interfaces. An empty value removes a stale observation so the
// client address resolver falls back to the current interface snapshot.
func (s *Store) UpdateAgentObservedPublicIP(ctx context.Context, id, address string) error {
	address = strings.TrimSpace(address)
	if address != "" {
		address = authn.NormalizePublicIP(address)
		if address == "" {
			return fmt.Errorf("%w: invalid observed public agent address", ErrInvalid)
		}
	}
	command, err := s.pool.Exec(ctx, `
		UPDATE agents SET metrics=CASE
			WHEN $2='' THEN metrics - 'observed_public_ip'
			ELSE jsonb_set(metrics, '{observed_public_ip}', to_jsonb($2::text), true)
		END
		WHERE id=$1 AND revoked_at IS NULL`, id, address)
	if err != nil {
		return err
	}
	if command.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) ListAgents(ctx context.Context) ([]core.Agent, error) {
	rows, err := s.pool.Query(ctx, `
			SELECT id,name,version,os,arch,capabilities,features,labels,runtime,metrics,last_seen,enrolled_at
		FROM agents WHERE revoked_at IS NULL ORDER BY enrolled_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	agents := make([]core.Agent, 0)
	now := time.Now().UTC()
	for rows.Next() {
		var agent core.Agent
		var capabilities, features, labels, runtimeState, metricsState []byte
		if err := rows.Scan(&agent.ID, &agent.Name, &agent.Version, &agent.OS, &agent.Arch, &capabilities, &features, &labels, &runtimeState, &metricsState, &agent.LastSeen, &agent.EnrolledAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(capabilities, &agent.Capabilities); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(features, &agent.Features); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(labels, &agent.Labels); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(runtimeState, &agent.Runtime); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(metricsState, &agent.Metrics); err != nil {
			return nil, err
		}
		if agent.LastSeen.After(now.Add(-45 * time.Second)) {
			agent.Status = "online"
		} else {
			agent.Status = "offline"
		}
		agents = append(agents, agent)
	}
	return agents, rows.Err()
}

func (s *Store) DeleteAgent(ctx context.Context, id string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var enrollmentID *string
	err = tx.QueryRow(ctx, `
		UPDATE agents SET revoked_at=now()
		WHERE id=$1 AND revoked_at IS NULL
		RETURNING enrollment_id`, id).Scan(&enrollmentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		UPDATE tasks SET status='failed', error='agent identity was revoked', finished_at=now(), config_content=NULL, lease_id=NULL
		WHERE agent_id=$1 AND status IN ('pending','running')`, id)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE configs SET deleted_at=now(),content='',updated_at=now()
			WHERE agent_id=$1 AND deleted_at IS NULL`, id)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM config_revisions WHERE config_id IN (SELECT id FROM configs WHERE agent_id=$1)`, id); err != nil {
		return err
	}
	legacyEnrollmentID := ""
	if enrollmentID != nil {
		legacyEnrollmentID = strings.TrimSpace(*enrollmentID)
	}
	if _, err := tx.Exec(ctx, `
		DELETE FROM enrollment_tokens
		WHERE agent_id=$1 OR id=NULLIF($2,'')`, id, legacyEnrollmentID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// AgentName returns the display name of an active registered agent.
func (s *Store) AgentName(ctx context.Context, id string) (string, error) {
	var name string
	if err := s.pool.QueryRow(ctx, `SELECT name FROM agents WHERE id=$1 AND revoked_at IS NULL`, id).Scan(&name); err != nil {
		return "", err
	}
	return name, nil
}

func (s *Store) CreateConfig(ctx context.Context, input core.Config) (core.Config, error) {
	if input.AgentID != "" {
		return core.Config{}, fmt.Errorf("%w: node-owned configurations must use the agent configuration workflow", ErrInvalid)
	}
	if err := core.ValidateConfig(input.Engine, input.Content); err != nil {
		return core.Config{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	name, description, err := validateConfigMetadata(input.Name, input.Description)
	if err != nil {
		return core.Config{}, err
	}
	id, err := core.NewID("cfg")
	if err != nil {
		return core.Config{}, err
	}
	storedContent, err := s.encryptContent(input.Content)
	if err != nil {
		return core.Config{}, err
	}
	now := time.Now().UTC()
	config := core.Config{
		ID: id, AgentID: input.AgentID, Name: name, Description: description,
		Engine: input.Engine, Content: input.Content, Version: 1, CreatedAt: now, UpdatedAt: now,
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return core.Config{}, err
	}
	defer tx.Rollback(ctx)
	_, err = tx.Exec(ctx, `
			INSERT INTO configs (id,agent_id,name,description,engine,content,version,created_at,updated_at)
		VALUES ($1,NULLIF($2,''),$3,$4,$5,$6,$7,$8,$8)`,
		config.ID, config.AgentID, config.Name, config.Description, config.Engine, storedContent, config.Version, now)
	if err != nil {
		return core.Config{}, mapError(err)
	}
	if err := s.insertConfigRevision(ctx, tx, config); err != nil {
		return core.Config{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return core.Config{}, err
	}
	return config, nil
}

func (s *Store) UpdateConfig(ctx context.Context, id string, input core.Config) (core.Config, error) {
	if err := core.ValidateConfig(input.Engine, input.Content); err != nil {
		return core.Config{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	name, description, err := validateConfigMetadata(input.Name, input.Description)
	if err != nil {
		return core.Config{}, err
	}
	if input.Version < 1 {
		return core.Config{}, fmt.Errorf("%w: configuration version is required", ErrInvalid)
	}
	storedContent, err := s.encryptContent(input.Content)
	if err != nil {
		return core.Config{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return core.Config{}, err
	}
	defer tx.Rollback(ctx)
	var config core.Config
	err = tx.QueryRow(ctx, `
		UPDATE configs SET name=$2,description=$3,engine=$4,content=$5,version=version+1,updated_at=now()
		WHERE id=$1 AND deleted_at IS NULL AND agent_id IS NULL AND version=$6
		RETURNING id,COALESCE(agent_id,''),name,description,engine,content,version,created_at,updated_at`,
		id, name, description, input.Engine, storedContent, input.Version).Scan(
		&config.ID, &config.AgentID, &config.Name, &config.Description, &config.Engine, &config.Content, &config.Version, &config.CreatedAt, &config.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			var exists bool
			if existsErr := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM configs WHERE id=$1 AND deleted_at IS NULL AND agent_id IS NULL)`, id).Scan(&exists); existsErr != nil {
				return core.Config{}, existsErr
			}
			if exists {
				return core.Config{}, fmt.Errorf("%w: configuration changed; reload before saving", ErrConflict)
			}
			return core.Config{}, ErrNotFound
		}
		return core.Config{}, mapError(err)
	}
	config.Content, err = s.decryptContent(config.Content)
	if err != nil {
		return core.Config{}, err
	}
	if err := s.insertConfigRevision(ctx, tx, config); err != nil {
		return core.Config{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return core.Config{}, err
	}
	return config, nil
}

func (s *Store) DeleteConfig(ctx context.Context, id string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	command, err := tx.Exec(ctx, `UPDATE configs SET deleted_at=now(),content='' WHERE id=$1 AND deleted_at IS NULL AND agent_id IS NULL`, id)
	if err != nil {
		return err
	}
	if command.RowsAffected() == 0 {
		return ErrNotFound
	}
	if _, err := tx.Exec(ctx, `DELETE FROM config_revisions WHERE config_id=$1`, id); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		UPDATE tasks SET status='failed',error='configuration was deleted before dispatch',finished_at=now(),config_content=NULL,lease_id=NULL
		WHERE config_id=$1 AND status='pending'`, id)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) ListConfigs(ctx context.Context) ([]core.Config, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id,COALESCE(agent_id,''),name,description,engine,content,version,created_at,updated_at
		FROM configs WHERE deleted_at IS NULL AND agent_id IS NULL ORDER BY updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	configs := make([]core.Config, 0)
	for rows.Next() {
		var config core.Config
		if err := rows.Scan(&config.ID, &config.AgentID, &config.Name, &config.Description, &config.Engine, &config.Content, &config.Version, &config.CreatedAt, &config.UpdatedAt); err != nil {
			return nil, err
		}
		config.Content, err = s.decryptContent(config.Content)
		if err != nil {
			return nil, err
		}
		configs = append(configs, config)
	}
	return configs, rows.Err()
}

func (s *Store) ExistingConfigIDs(ctx context.Context, ids []string) (map[string]bool, error) {
	existing := make(map[string]bool)
	if len(ids) == 0 {
		return existing, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT id FROM configs WHERE deleted_at IS NULL AND id=ANY($1::text[])`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		existing[id] = true
	}
	return existing, rows.Err()
}

func (s *Store) CreateTask(ctx context.Context, request core.TaskRequest) (core.Task, error) {
	if !request.Action.Valid() {
		return core.Task{}, fmt.Errorf("%w: unsupported action %q", ErrInvalid, request.Action)
	}
	if request.Action == core.ActionUpgradeAgent {
		if request.Engine != "" || request.ConfigID != "" || request.CoreVersion != "" {
			return core.Task{}, fmt.Errorf("%w: agent upgrade tasks cannot reference an engine, configuration, or core version", ErrInvalid)
		}
	} else if !request.Engine.Valid() {
		return core.Task{}, fmt.Errorf("%w: unsupported engine %q", ErrInvalid, request.Engine)
	}
	if request.Action == core.ActionInstall {
		normalizedVersion, versionErr := core.NormalizeCoreVersionSelector(request.CoreVersion)
		if versionErr != nil {
			return core.Task{}, fmt.Errorf("%w: %v", ErrInvalid, versionErr)
		}
		request.CoreVersion = normalizedVersion
		if request.ConfigID != "" {
			return core.Task{}, fmt.Errorf("%w: install tasks cannot reference a configuration", ErrInvalid)
		}
	} else {
		request.CoreVersion = ""
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return core.Task{}, err
	}
	defer tx.Rollback(ctx)
	var capabilitiesJSON, featuresJSON []byte
	if err := tx.QueryRow(ctx, `SELECT capabilities,features FROM agents WHERE id=$1 AND revoked_at IS NULL FOR UPDATE`, request.AgentID).Scan(&capabilitiesJSON, &featuresJSON); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return core.Task{}, fmt.Errorf("agent: %w", ErrNotFound)
		}
		return core.Task{}, err
	}
	var capabilities []core.Engine
	if err := json.Unmarshal(capabilitiesJSON, &capabilities); err != nil {
		return core.Task{}, err
	}
	var features []string
	if err := json.Unmarshal(featuresJSON, &features); err != nil {
		return core.Task{}, err
	}
	if request.Action == core.ActionUpgradeAgent && !containsFeature(features, core.AgentFeatureSelfUpgrade) {
		return core.Task{}, fmt.Errorf("%w: this Agent does not support remote upgrades; run the current one-click installation once", ErrConflict)
	}
	if request.Action != core.ActionUpgradeAgent && !containsEngine(capabilities, request.Engine) {
		return core.Task{}, fmt.Errorf("%w: agent does not advertise the requested engine", ErrInvalid)
	}

	task := core.Task{
		AgentID: request.AgentID, Action: request.Action, Engine: request.Engine,
		ConfigID: request.ConfigID, CoreVersion: request.CoreVersion, Status: core.TaskPending, CreatedAt: time.Now().UTC(),
	}
	if request.Action == core.ActionDeploy || request.Action == core.ActionValidate || request.Action == core.ActionImportExisting {
		var configEngine core.Engine
		var configAgentID string
		err := tx.QueryRow(ctx, `SELECT engine,content,version,COALESCE(agent_id,'') FROM configs WHERE id=$1 AND deleted_at IS NULL FOR UPDATE`, request.ConfigID).Scan(&configEngine, &task.ConfigContent, &task.ConfigVersion, &configAgentID)
		if errors.Is(err, pgx.ErrNoRows) {
			return core.Task{}, fmt.Errorf("configuration: %w", ErrNotFound)
		}
		if err != nil {
			return core.Task{}, err
		}
		if configEngine != request.Engine {
			return core.Task{}, fmt.Errorf("%w: task engine does not match configuration engine", ErrInvalid)
		}
		if request.Action == core.ActionImportExisting && configAgentID != request.AgentID {
			return core.Task{}, fmt.Errorf("%w: existing service migration requires this agent's saved snapshot", ErrInvalid)
		}
		if configAgentID != "" && configAgentID != request.AgentID {
			return core.Task{}, fmt.Errorf("%w: node-owned configuration cannot be deployed to another agent", ErrInvalid)
		}
		task.ConfigContent, err = s.decryptContent(task.ConfigContent)
		if err != nil {
			return core.Task{}, err
		}
	} else {
		task.ConfigID = ""
	}
	existing, existingErr := scanTask(tx.QueryRow(ctx, `
		SELECT id,agent_id,action,engine,COALESCE(config_id,''),COALESCE(config_version,0),COALESCE(core_version,''),status,attempt,
		       COALESCE(output,''),COALESCE(error,''),created_at,started_at,finished_at
		FROM tasks
		WHERE agent_id=$1 AND action=$2 AND engine=$3
		  AND COALESCE(config_id,'')=$4 AND COALESCE(config_version,0)=$5 AND COALESCE(core_version,'')=$6
		  AND status IN ('pending','running')
		ORDER BY created_at DESC LIMIT 1`,
		task.AgentID, task.Action, task.Engine, task.ConfigID, task.ConfigVersion, task.CoreVersion), false)
	if existingErr == nil {
		if err := tx.Commit(ctx); err != nil {
			return core.Task{}, err
		}
		existing.Reused = true
		return existing, nil
	}
	if !errors.Is(existingErr, pgx.ErrNoRows) {
		return core.Task{}, existingErr
	}
	task.ID, err = core.NewID("tsk")
	if err != nil {
		return core.Task{}, err
	}
	storedConfigContent, err := s.encryptContent(task.ConfigContent)
	if err != nil {
		return core.Task{}, err
	}
	_, err = tx.Exec(ctx, `
			INSERT INTO tasks (id,agent_id,action,engine,config_id,config_version,config_content,core_version,status,attempt,created_at)
			VALUES ($1,$2,$3,$4,NULLIF($5,''),NULLIF($6,0),NULLIF($7,''),NULLIF($8,''),$9,0,$10)`,
		task.ID, task.AgentID, task.Action, task.Engine, task.ConfigID, task.ConfigVersion, storedConfigContent, task.CoreVersion, task.Status, task.CreatedAt)
	if err != nil {
		return core.Task{}, mapError(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return core.Task{}, err
	}
	s.signalTaskReady(task.AgentID)
	return task, nil
}

// TaskReady returns a coalescing signal for newly created tasks assigned to an agent.
func (s *Store) TaskReady(agentID string) <-chan struct{} {
	return s.taskReadyChannel(agentID)
}

func (s *Store) taskReadyChannel(agentID string) chan struct{} {
	s.taskWakeMu.Lock()
	defer s.taskWakeMu.Unlock()
	if s.taskWakes == nil {
		s.taskWakes = make(map[string]chan struct{})
	}
	wake := s.taskWakes[agentID]
	if wake == nil {
		wake = make(chan struct{}, 1)
		s.taskWakes[agentID] = wake
	}
	return wake
}

func (s *Store) signalTaskReady(agentID string) {
	wake := s.taskReadyChannel(agentID)
	select {
	case wake <- struct{}{}:
	default:
	}
}

func (s *Store) ListTasks(ctx context.Context, agentID string, limit int) ([]core.Task, error) {
	return s.ListTasksFiltered(ctx, agentID, "", "", limit)
}

func (s *Store) ListTasksFiltered(ctx context.Context, agentID string, status core.TaskStatus, action core.Action, limit int) ([]core.Task, error) {
	if limit < 1 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id,agent_id,action,engine,COALESCE(config_id,''),COALESCE(config_version,0),COALESCE(core_version,''),status,attempt,
		       COALESCE(output,''),COALESCE(error,''),created_at,started_at,finished_at
		FROM tasks
		WHERE ($1='' OR agent_id=$1) AND ($2='' OR status=$2) AND ($3='' OR action=$3)
		ORDER BY created_at DESC LIMIT $4`, agentID, status, action, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tasks := make([]core.Task, 0)
	for rows.Next() {
		task, err := scanTask(rows, false)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, task)
	}
	return tasks, rows.Err()
}

func (s *Store) GetTask(ctx context.Context, id string) (core.Task, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id,agent_id,action,engine,COALESCE(config_id,''),COALESCE(config_version,0),COALESCE(core_version,''),status,attempt,
		       COALESCE(output,''),COALESCE(error,''),created_at,started_at,finished_at
		FROM tasks WHERE id=$1`, id)
	task, err := scanTask(row, false)
	if errors.Is(err, pgx.ErrNoRows) {
		return core.Task{}, ErrNotFound
	}
	return task, err
}

func (s *Store) CancelTask(ctx context.Context, id string) error {
	command, err := s.pool.Exec(ctx, `
		UPDATE tasks SET status='canceled',error='canceled by administrator',finished_at=now(),config_content=NULL,lease_id=NULL
		WHERE id=$1 AND status='pending'`, id)
	if err != nil {
		return err
	}
	if command.RowsAffected() != 0 {
		return nil
	}
	var exists bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM tasks WHERE id=$1)`, id).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return ErrNotFound
	}
	return fmt.Errorf("%w: only pending tasks can be canceled", ErrConflict)
}

func (s *Store) RetryTask(ctx context.Context, id string) (core.Task, error) {
	previous, err := s.GetTask(ctx, id)
	if err != nil {
		return core.Task{}, err
	}
	if previous.Status != core.TaskFailed && previous.Status != core.TaskCanceled {
		return core.Task{}, fmt.Errorf("%w: only failed or canceled tasks can be retried", ErrConflict)
	}
	return s.CreateTask(ctx, core.TaskRequest{
		AgentID: previous.AgentID, Action: previous.Action, Engine: previous.Engine,
		ConfigID: previous.ConfigID, CoreVersion: previous.CoreVersion,
	})
}

// RunningTask returns the task lease currently owned by an agent. A reconnecting
// Agent can resume result delivery without waiting for the stale-lease janitor.
func (s *Store) RunningTask(ctx context.Context, agentID string) (*core.Task, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id,agent_id,action,engine,COALESCE(config_id,''),COALESCE(config_version,0),
		       COALESCE(config_content,''),COALESCE(core_version,''),status,attempt,COALESCE(lease_id,''),
		       COALESCE(output,''),COALESCE(error,''),created_at,started_at,finished_at
		FROM tasks WHERE agent_id=$1 AND status='running'
		ORDER BY started_at DESC LIMIT 1`, agentID)
	task, err := scanTask(row, true)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if task.ConfigContent != "" {
		task.ConfigContent, err = s.decryptContent(task.ConfigContent)
		if err != nil {
			return nil, err
		}
	}
	return &task, nil
}

func (s *Store) ClaimTask(ctx context.Context, agentID string) (*core.Task, error) {
	leaseID, err := core.NewToken()
	if err != nil {
		return nil, err
	}
	row := s.pool.QueryRow(ctx, `
		WITH next_task AS (
			SELECT t.id FROM tasks t JOIN agents a ON a.id=t.agent_id
			WHERE t.agent_id=$1 AND t.status='pending' AND a.revoked_at IS NULL
			  AND NOT EXISTS (SELECT 1 FROM tasks running WHERE running.agent_id=$1 AND running.status='running')
			ORDER BY t.created_at ASC FOR UPDATE OF t SKIP LOCKED LIMIT 1
		)
		UPDATE tasks t SET status='running',started_at=now(),attempt=attempt+1,lease_id=$2
		FROM next_task n WHERE t.id=n.id
		RETURNING t.id,t.agent_id,t.action,t.engine,COALESCE(t.config_id,''),COALESCE(t.config_version,0),
		          COALESCE(t.config_content,''),COALESCE(t.core_version,''),t.status,t.attempt,COALESCE(t.lease_id,''),COALESCE(t.output,''),COALESCE(t.error,''),
		          t.created_at,t.started_at,t.finished_at`, agentID, leaseID)
	task, err := scanTask(row, true)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if task.ConfigContent != "" {
		task.ConfigContent, err = s.decryptContent(task.ConfigContent)
		if err != nil {
			return nil, err
		}
	}
	return &task, nil
}

func (s *Store) CompleteTask(ctx context.Context, agentID, taskID string, result core.TaskResultRequest) error {
	if len(result.LeaseID) < 32 {
		return fmt.Errorf("%w: invalid task lease", ErrConflict)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var action core.Action
	var engine core.Engine
	if err := tx.QueryRow(ctx, `
		SELECT action,engine FROM tasks
		WHERE id=$1 AND agent_id=$2 AND lease_id=$3 AND status='running'
		FOR UPDATE`, taskID, agentID, result.LeaseID).Scan(&action, &engine); err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM tasks WHERE id=$1 AND agent_id=$2)`, taskID, agentID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return ErrNotFound
		}
		return fmt.Errorf("%w: task is not running", ErrConflict)
	}
	status := core.TaskFailed
	if result.Success {
		status = core.TaskSucceeded
	}
	storedContent := ""
	storedOutput := truncate(result.Output, 64<<10)
	storedError := truncate(result.Error, 8<<10)
	if action == core.ActionReadConfig && result.Success {
		content := result.Output
		if !utf8.ValidString(content) {
			status = core.TaskFailed
			storedOutput = ""
			storedError = "agent returned a current configuration that is not valid UTF-8"
		} else if len(content) > core.MaxConfigBytes {
			status = core.TaskFailed
			storedOutput = ""
			storedError = "agent returned a current configuration larger than the supported limit"
		} else if validationErr := core.ValidateConfig(engine, content); validationErr != nil {
			status = core.TaskFailed
			storedOutput = ""
			storedError = "agent returned an invalid current configuration: " + validationErr.Error()
		} else {
			storedContent = content
			storedOutput = "current configuration read and validated"
			storedError = ""
		}
	}
	storedContent, err = s.encryptContent(storedContent)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		UPDATE tasks SET status=$4,output=$5,error=$6,finished_at=now(),config_content=NULLIF($7,''),lease_id=NULL
		WHERE id=$1 AND agent_id=$2 AND lease_id=$3 AND status='running'`,
		taskID, agentID, result.LeaseID, status, storedOutput, storedError, storedContent)
	if err != nil {
		return err
	}
	if action == core.ActionReadConfig && status == core.TaskSucceeded {
		if _, err := tx.Exec(ctx, `
			UPDATE tasks SET config_content=NULL
			WHERE agent_id=$1 AND engine=$2 AND action=$3 AND id<>$4 AND config_content IS NOT NULL`,
			agentID, engine, core.ActionReadConfig, taskID); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (s *Store) ReadTaskConfigSnapshot(ctx context.Context, taskID, agentID string, engine core.Engine) (string, error) {
	var content string
	err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(config_content,'') FROM tasks
		WHERE id=$1 AND agent_id=$2 AND engine=$3 AND action=$4 AND status='succeeded'`,
		taskID, agentID, engine, core.ActionReadConfig).Scan(&content)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && content == "") {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	return s.decryptContent(content)
}

func (s *Store) RecentReadTask(ctx context.Context, agentID string, engine core.Engine, maxAge time.Duration) (core.Task, error) {
	if maxAge <= 0 {
		return core.Task{}, ErrNotFound
	}
	row := s.pool.QueryRow(ctx, `
		SELECT id,agent_id,action,engine,COALESCE(config_id,''),COALESCE(config_version,0),COALESCE(core_version,''),status,attempt,
		       COALESCE(output,''),COALESCE(error,''),created_at,started_at,finished_at
		FROM tasks
		WHERE agent_id=$1 AND engine=$2 AND action=$3 AND status='succeeded'
		  AND config_content IS NOT NULL AND finished_at > now()-$4::interval
		ORDER BY finished_at DESC LIMIT 1`, agentID, engine, core.ActionReadConfig, intervalString(maxAge))
	task, err := scanTask(row, false)
	if errors.Is(err, pgx.ErrNoRows) {
		return core.Task{}, ErrNotFound
	}
	return task, err
}

func (s *Store) RequeueStaleTasks(ctx context.Context, age, installAge time.Duration, maxAttempts int) error {
	if installAge < age {
		installAge = age
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE tasks SET
			status=CASE WHEN attempt >= $3 THEN 'failed' ELSE 'pending' END,
			error=CASE WHEN attempt >= $3 THEN 'agent did not report a result before the execution lease expired' ELSE error END,
			finished_at=CASE WHEN attempt >= $3 THEN now() ELSE NULL END,
			started_at=CASE WHEN attempt >= $3 THEN started_at ELSE NULL END,
			config_content=CASE WHEN attempt >= $3 THEN NULL ELSE config_content END,
			lease_id=NULL
		WHERE status='running' AND started_at < now() - CASE WHEN action='install' THEN $2::interval ELSE $1::interval END`,
		intervalString(age), intervalString(installAge), maxAttempts)
	return err
}

func (s *Store) Overview(ctx context.Context) (core.Overview, error) {
	var result core.Overview
	err := s.pool.QueryRow(ctx, `
		SELECT
			(SELECT count(*) FROM agents WHERE revoked_at IS NULL),
			(SELECT count(*) FROM agents WHERE revoked_at IS NULL AND last_seen > now()-interval '45 seconds'),
			(SELECT count(*) FROM configs WHERE deleted_at IS NULL AND agent_id IS NULL),
			(SELECT count(*) FROM configs WHERE deleted_at IS NULL AND agent_id IS NOT NULL),
			(SELECT count(*) FROM tasks WHERE status IN ('pending','running')),
			(SELECT count(*) FROM tasks WHERE status='pending'),
			(SELECT count(*) FROM tasks WHERE status='running'),
			(SELECT count(*) FROM tasks WHERE status='failed')`).Scan(
		&result.Agents, &result.AgentsOnline, &result.Configs, &result.NodeConfigs,
		&result.TasksPending, &result.TasksQueued, &result.TasksRunning, &result.TasksFailed)
	return result, err
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanTask(row rowScanner, includeContent bool) (core.Task, error) {
	var task core.Task
	var err error
	if includeContent {
		err = row.Scan(&task.ID, &task.AgentID, &task.Action, &task.Engine, &task.ConfigID, &task.ConfigVersion,
			&task.ConfigContent, &task.CoreVersion, &task.Status, &task.Attempt, &task.LeaseID, &task.Output, &task.Error,
			&task.CreatedAt, &task.StartedAt, &task.FinishedAt)
	} else {
		err = row.Scan(&task.ID, &task.AgentID, &task.Action, &task.Engine, &task.ConfigID, &task.ConfigVersion,
			&task.CoreVersion, &task.Status, &task.Attempt, &task.Output, &task.Error,
			&task.CreatedAt, &task.StartedAt, &task.FinishedAt)
	}
	return task, err
}

func cloneLabels(source map[string]string) map[string]string {
	if source == nil {
		return nil
	}
	result := make(map[string]string, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func validateConfigMetadata(rawName, rawDescription string) (string, string, error) {
	name := strings.TrimSpace(rawName)
	description := strings.TrimSpace(rawDescription)
	if name == "" || utf8.RuneCountInString(name) > 100 {
		return "", "", fmt.Errorf("%w: configuration name is required and must not exceed 100 characters", ErrInvalid)
	}
	if utf8.RuneCountInString(description) > 300 {
		return "", "", fmt.Errorf("%w: configuration description exceeds 300 characters", ErrInvalid)
	}
	return name, description, nil
}

func containsEngine(values []core.Engine, expected core.Engine) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func containsFeature(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func mapError(err error) error {
	if err == nil {
		return nil
	}
	if isUniqueViolation(err) {
		return fmt.Errorf("%w: duplicate value", ErrConflict)
	}
	return err
}

func isUniqueViolation(err error) bool {
	var pgError *pgconn.PgError
	return errors.As(err, &pgError) && pgError.Code == "23505"
}

func intervalString(duration time.Duration) string {
	seconds := int64(duration.Seconds())
	if seconds < 1 {
		seconds = 1
	}
	return fmt.Sprintf("%d seconds", seconds)
}

func truncate(value string, limit int) string {
	value = strings.ToValidUTF8(value, "�")
	if len(value) <= limit {
		return value
	}
	return strings.ToValidUTF8(value[:limit], "�") + "\n… output truncated by QControlHub"
}

const schemaSQL = `
CREATE TABLE IF NOT EXISTS agents (
    id text PRIMARY KEY,
    name varchar(100) NOT NULL,
    version varchar(100) NOT NULL DEFAULT '',
    os varchar(50) NOT NULL,
    arch varchar(50) NOT NULL,
    capabilities jsonb NOT NULL,
	features jsonb NOT NULL DEFAULT '[]'::jsonb,
    labels jsonb NOT NULL DEFAULT '{}'::jsonb,
	    runtime jsonb NOT NULL DEFAULT '{}'::jsonb,
	    metrics jsonb NOT NULL DEFAULT '{}'::jsonb,
    public_key bytea NOT NULL CHECK (octet_length(public_key) = 32),
    last_seen timestamptz NOT NULL,
    enrolled_at timestamptz NOT NULL,
    revoked_at timestamptz
	);

	ALTER TABLE agents ADD COLUMN IF NOT EXISTS metrics jsonb NOT NULL DEFAULT '{}'::jsonb;
	ALTER TABLE agents ADD COLUMN IF NOT EXISTS features jsonb NOT NULL DEFAULT '[]'::jsonb;

CREATE TABLE IF NOT EXISTS core_log_batches (
    id text PRIMARY KEY,
    agent_id text NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    received_at timestamptz NOT NULL
);

CREATE TABLE IF NOT EXISTS core_logs (
    id bigserial PRIMARY KEY,
    batch_id text NOT NULL REFERENCES core_log_batches(id) ON DELETE CASCADE,
    entry_index smallint NOT NULL CHECK (entry_index >= 0),
    agent_id text NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    engine varchar(20) NOT NULL CHECK (engine IN ('mihomo','xray','sing-box','ss-rust')),
    level varchar(10) NOT NULL CHECK (level IN ('debug','info','warning','error','critical')),
    message text NOT NULL CHECK (octet_length(message) BETWEEN 1 AND 4096),
    logged_at timestamptz NOT NULL,
    received_at timestamptz NOT NULL,
    UNIQUE (batch_id,entry_index)
);

CREATE INDEX IF NOT EXISTS core_logs_agent_recent_idx ON core_logs(agent_id,id DESC);
CREATE INDEX IF NOT EXISTS core_logs_engine_recent_idx ON core_logs(engine,id DESC);
CREATE INDEX IF NOT EXISTS core_logs_received_idx ON core_logs(received_at);
CREATE INDEX IF NOT EXISTS core_log_batches_received_idx ON core_log_batches(received_at);

CREATE TABLE IF NOT EXISTS configs (
    id text PRIMARY KEY,
    agent_id text REFERENCES agents(id),
    name varchar(100) NOT NULL,
    description varchar(300) NOT NULL DEFAULT '',
    engine varchar(20) NOT NULL CHECK (engine IN ('mihomo','xray','sing-box','ss-rust')),
    content text NOT NULL CHECK (octet_length(content) <= 4194304),
    version integer NOT NULL CHECK (version > 0),
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    deleted_at timestamptz
);
ALTER TABLE configs DROP CONSTRAINT IF EXISTS configs_content_check;
ALTER TABLE configs ADD CONSTRAINT configs_content_check CHECK (octet_length(content) <= 4194304);

	ALTER TABLE configs ADD COLUMN IF NOT EXISTS agent_id text REFERENCES agents(id);

	CREATE TABLE IF NOT EXISTS config_revisions (
	    config_id text NOT NULL REFERENCES configs(id),
	    version integer NOT NULL CHECK (version > 0),
	    agent_id text REFERENCES agents(id),
	    name varchar(100) NOT NULL,
	    description varchar(300) NOT NULL DEFAULT '',
	    engine varchar(20) NOT NULL CHECK (engine IN ('mihomo','xray','sing-box','ss-rust')),
	    content text NOT NULL CHECK (octet_length(content) <= 4194304),
	    created_at timestamptz NOT NULL,
	    PRIMARY KEY (config_id,version)
	);
	ALTER TABLE config_revisions DROP CONSTRAINT IF EXISTS config_revisions_content_check;
	ALTER TABLE config_revisions ADD CONSTRAINT config_revisions_content_check CHECK (octet_length(content) <= 4194304);

	INSERT INTO config_revisions (config_id,version,agent_id,name,description,engine,content,created_at)
	SELECT id,version,agent_id,name,description,engine,content,updated_at FROM configs
	WHERE deleted_at IS NULL
	ON CONFLICT (config_id,version) DO NOTHING;

CREATE TABLE IF NOT EXISTS tasks (
    id text PRIMARY KEY,
    agent_id text NOT NULL REFERENCES agents(id),
    action varchar(20) NOT NULL CHECK (action IN ('validate','deploy','import-existing','read-config','start','stop','restart','status','install','upgrade-agent')),
	    engine varchar(20) NOT NULL CHECK (engine IN ('mihomo','xray','sing-box','ss-rust') OR (action='upgrade-agent' AND engine='')),
    config_id text REFERENCES configs(id),
    config_version integer,
	    config_content text,
	    core_version varchar(64),
	    status varchar(20) NOT NULL CHECK (status IN ('pending','running','succeeded','failed','canceled')),
	    attempt integer NOT NULL DEFAULT 0,
    output text,
    error text,
    created_at timestamptz NOT NULL,
    started_at timestamptz,
    finished_at timestamptz
);

ALTER TABLE tasks ADD COLUMN IF NOT EXISTS lease_id text;
	ALTER TABLE tasks ADD COLUMN IF NOT EXISTS core_version varchar(64);
	DROP INDEX IF EXISTS tasks_latest_deployment_idx;
	ALTER TABLE tasks DROP COLUMN IF EXISTS simulated;
	ALTER TABLE tasks DROP CONSTRAINT IF EXISTS tasks_action_check;
	ALTER TABLE tasks ADD CONSTRAINT tasks_action_check CHECK (action IN ('validate','deploy','import-existing','read-config','start','stop','restart','status','install','upgrade-agent'));
	ALTER TABLE tasks DROP CONSTRAINT IF EXISTS tasks_status_check;
	ALTER TABLE tasks ADD CONSTRAINT tasks_status_check CHECK (status IN ('pending','running','succeeded','failed','canceled'));
	ALTER TABLE configs DROP CONSTRAINT IF EXISTS configs_engine_check;
	ALTER TABLE configs ADD CONSTRAINT configs_engine_check CHECK (engine IN ('mihomo','xray','sing-box','ss-rust'));
	ALTER TABLE config_revisions DROP CONSTRAINT IF EXISTS config_revisions_engine_check;
	ALTER TABLE config_revisions ADD CONSTRAINT config_revisions_engine_check CHECK (engine IN ('mihomo','xray','sing-box','ss-rust'));
	ALTER TABLE tasks DROP CONSTRAINT IF EXISTS tasks_engine_check;
	ALTER TABLE tasks ADD CONSTRAINT tasks_engine_check CHECK (engine IN ('mihomo','xray','sing-box','ss-rust') OR (action='upgrade-agent' AND engine=''));

CREATE TABLE IF NOT EXISTS enrollment_tokens (
    id text PRIMARY KEY,
    name varchar(100) NOT NULL,
    token_hash bytea NOT NULL UNIQUE CHECK (octet_length(token_hash) = 32),
	    expires_at timestamptz,
	    max_uses integer NOT NULL CHECK (max_uses BETWEEN 0 AND 50),
	    used_count integer NOT NULL DEFAULT 0 CHECK (used_count >= 0),
	    reusable boolean NOT NULL DEFAULT false,
	    created_at timestamptz NOT NULL,
	    revoked_at timestamptz
);

ALTER TABLE enrollment_tokens ADD COLUMN IF NOT EXISTS reusable boolean NOT NULL DEFAULT false;
ALTER TABLE enrollment_tokens ALTER COLUMN expires_at DROP NOT NULL;
ALTER TABLE enrollment_tokens DROP CONSTRAINT IF EXISTS enrollment_tokens_max_uses_check;
ALTER TABLE enrollment_tokens ADD CONSTRAINT enrollment_tokens_max_uses_check CHECK (max_uses BETWEEN 0 AND 50);
ALTER TABLE enrollment_tokens ADD COLUMN IF NOT EXISTS agent_id text;
DROP INDEX IF EXISTS enrollment_tokens_reusable_name_unique_idx;

ALTER TABLE agents ADD COLUMN IF NOT EXISTS enrollment_id text;
ALTER TABLE agents DROP CONSTRAINT IF EXISTS agents_enrollment_id_fkey;
ALTER TABLE agents ADD CONSTRAINT agents_enrollment_id_fkey FOREIGN KEY (enrollment_id) REFERENCES enrollment_tokens(id) ON DELETE SET NULL;
CREATE UNIQUE INDEX IF NOT EXISTS agents_enrollment_id_unique_idx ON agents(enrollment_id) WHERE enrollment_id IS NOT NULL;

UPDATE enrollment_tokens AS token
SET agent_id=agent.id
FROM agents AS agent
WHERE agent.enrollment_id=token.id AND token.agent_id IS NULL;
ALTER TABLE enrollment_tokens DROP CONSTRAINT IF EXISTS enrollment_tokens_agent_id_fkey;
ALTER TABLE enrollment_tokens ADD CONSTRAINT enrollment_tokens_agent_id_fkey FOREIGN KEY (agent_id) REFERENCES agents(id) ON DELETE CASCADE;
CREATE INDEX IF NOT EXISTS enrollment_tokens_agent_id_idx ON enrollment_tokens(agent_id,created_at DESC);
CREATE UNIQUE INDEX IF NOT EXISTS enrollment_tokens_reusable_unbound_name_unique_idx ON enrollment_tokens(lower(name)) WHERE reusable AND agent_id IS NULL;

CREATE TABLE IF NOT EXISTS agent_nonces (
    agent_id text NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    nonce varchar(100) NOT NULL,
    expires_at timestamptz NOT NULL,
    PRIMARY KEY (agent_id, nonce)
);

CREATE TABLE IF NOT EXISTS panel_settings (
    id smallint PRIMARY KEY CHECK (id = 1),
    panel_name varchar(40) NOT NULL,
    panel_description varchar(120) NOT NULL DEFAULT '',
    task_page_size integer NOT NULL CHECK (task_page_size IN (50,100,500)),
    task_poll_interval_ms integer NOT NULL CHECK (task_poll_interval_ms IN (600,1000,2000,5000)),
    webhook_url varchar(500) NOT NULL DEFAULT '',
    updated_at timestamptz NOT NULL
);
ALTER TABLE panel_settings ADD COLUMN IF NOT EXISTS webhook_url varchar(500) NOT NULL DEFAULT '';
ALTER TABLE panel_settings DROP COLUMN IF EXISTS enrollment_ttl_minutes;

CREATE TABLE IF NOT EXISTS panel_users (
    id text PRIMARY KEY,
    username varchar(64) NOT NULL,
    display_name varchar(100) NOT NULL DEFAULT '',
    role varchar(20) NOT NULL CHECK (role IN ('admin','user')),
    permissions jsonb NOT NULL DEFAULT '[]'::jsonb,
    password_hash varchar(100) NOT NULL,
    disabled boolean NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    last_login_at timestamptz
);
ALTER TABLE panel_users DROP CONSTRAINT IF EXISTS panel_users_role_check;
ALTER TABLE panel_users ADD COLUMN IF NOT EXISTS permissions jsonb NOT NULL DEFAULT '[]'::jsonb;
UPDATE panel_users SET permissions = CASE role
    WHEN 'admin' THEN '[]'::jsonb
    WHEN 'operator' THEN '["overview.read","agents.read","deployments.read","client-access.read","catalogs.read","agent-config.read","agent-config.write","configs.read","configs.write","tasks.read","tasks.execute","settings.read","audit.read","metrics.read","core-logs.read","templates.read","templates.write"]'::jsonb
    WHEN 'auditor' THEN '["overview.read","agents.read","deployments.read","tasks.read","audit.read","metrics.read","core-logs.read"]'::jsonb
    WHEN 'readonly' THEN '["overview.read","agents.read","deployments.read","client-access.read","catalogs.read","agent-config.read","configs.read","tasks.read","settings.read","audit.read","metrics.read","core-logs.read","templates.read"]'::jsonb
    ELSE permissions END
    WHERE role IN ('operator','auditor','readonly');
UPDATE panel_users SET role='user' WHERE role IN ('operator','auditor','readonly');
ALTER TABLE panel_users ADD CONSTRAINT panel_users_role_check CHECK (role IN ('admin','user'));
CREATE UNIQUE INDEX IF NOT EXISTS panel_users_username_unique_idx ON panel_users(lower(username));
CREATE INDEX IF NOT EXISTS panel_users_status_idx ON panel_users(disabled,username);

INSERT INTO panel_settings (
    id,panel_name,panel_description,task_page_size,task_poll_interval_ms,updated_at
) VALUES (1,'QControlHub','可信远程编排',100,600,now())
ON CONFLICT (id) DO NOTHING;

CREATE INDEX IF NOT EXISTS agents_active_seen_idx ON agents(last_seen DESC) WHERE revoked_at IS NULL;
CREATE UNIQUE INDEX IF NOT EXISTS agents_public_key_unique_idx ON agents(public_key);
	CREATE INDEX IF NOT EXISTS configs_active_updated_idx ON configs(updated_at DESC) WHERE deleted_at IS NULL;
	CREATE UNIQUE INDEX IF NOT EXISTS configs_agent_engine_unique_idx ON configs(agent_id,engine) WHERE agent_id IS NOT NULL AND deleted_at IS NULL;
	CREATE INDEX IF NOT EXISTS config_revisions_recent_idx ON config_revisions(config_id,version DESC);
CREATE INDEX IF NOT EXISTS tasks_agent_queue_idx ON tasks(agent_id, status, created_at);
CREATE INDEX IF NOT EXISTS tasks_created_idx ON tasks(created_at DESC);
CREATE INDEX IF NOT EXISTS tasks_latest_deployment_idx ON tasks(agent_id,engine,finished_at DESC) WHERE action IN ('deploy','import-existing') AND status='succeeded';
CREATE UNIQUE INDEX IF NOT EXISTS tasks_one_running_per_agent_idx ON tasks(agent_id) WHERE status='running';
CREATE TABLE IF NOT EXISTS metric_samples (
    agent_id text NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    sampled_at timestamptz NOT NULL,
    cpu_percent real NOT NULL,
    memory_percent real NOT NULL DEFAULT 0,
    rx_rate_bps bigint NOT NULL DEFAULT 0 CHECK (rx_rate_bps >= 0),
    tx_rate_bps bigint NOT NULL DEFAULT 0 CHECK (tx_rate_bps >= 0),
    PRIMARY KEY (agent_id, sampled_at)
);
CREATE INDEX IF NOT EXISTS metric_samples_recent_idx ON metric_samples(agent_id, sampled_at DESC);

CREATE TABLE IF NOT EXISTS port_traffic_policies (
    id text PRIMARY KEY,
    agent_id text NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    name varchar(100) NOT NULL,
    engine varchar(20) NOT NULL CHECK (engine IN ('mihomo','xray','sing-box','ss-rust')),
    port integer NOT NULL CHECK (port BETWEEN 1 AND 65535),
    protocol varchar(8) NOT NULL CHECK (protocol IN ('tcp','udp','both')),
    cycle varchar(8) NOT NULL CHECK (cycle IN ('monthly','yearly')),
    cycle_anchor date NOT NULL,
    limit_bytes bigint NOT NULL CHECK (limit_bytes > 0),
    reset_generation bigint NOT NULL DEFAULT 1 CHECK (reset_generation > 0),
    received_bytes bigint NOT NULL DEFAULT 0 CHECK (received_bytes >= 0),
    sent_bytes bigint NOT NULL DEFAULT 0 CHECK (sent_bytes >= 0),
    used_bytes bigint NOT NULL DEFAULT 0 CHECK (used_bytes >= 0),
    receive_bps bigint NOT NULL DEFAULT 0 CHECK (receive_bps >= 0),
    send_bps bigint NOT NULL DEFAULT 0 CHECK (send_bps >= 0),
    period_start timestamptz,
    period_end timestamptz,
    blocked boolean NOT NULL DEFAULT false,
    enforcement_available boolean NOT NULL DEFAULT false,
    enforcement_error varchar(500) NOT NULL DEFAULT '',
    last_reported_at timestamptz,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    UNIQUE (agent_id,port)
);
CREATE INDEX IF NOT EXISTS port_traffic_policies_agent_idx ON port_traffic_policies(agent_id,port);

CREATE TABLE IF NOT EXISTS audit_logs (
    id bigserial PRIMARY KEY,
    acted_at timestamptz NOT NULL DEFAULT now(),
    actor varchar(40) NOT NULL DEFAULT 'admin',
    action varchar(40) NOT NULL,
    target text NOT NULL DEFAULT '',
    detail text NOT NULL DEFAULT '',
    remote_ip varchar(64) NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS audit_logs_recent_idx ON audit_logs(acted_at DESC);CREATE TABLE IF NOT EXISTS config_templates ( id text PRIMARY KEY, name varchar(100) NOT NULL, engine varchar(20) NOT NULL CHECK (engine IN ('mihomo','xray','sing-box','ss-rust')), content text NOT NULL CHECK (octet_length(content) <= 4194304), created_at timestamptz NOT NULL, updated_at timestamptz NOT NULL ); CREATE INDEX IF NOT EXISTS config_templates_recent_idx ON config_templates(updated_at DESC);
CREATE INDEX IF NOT EXISTS enrollment_tokens_active_idx ON enrollment_tokens(expires_at) WHERE revoked_at IS NULL;
CREATE INDEX IF NOT EXISTS agent_nonces_expiry_idx ON agent_nonces(expires_at);
`
