package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/qimaoww/qcontrolhub/internal/core"
)

func (s *Store) GetAgent(ctx context.Context, id string) (core.Agent, error) {
	var agent core.Agent
	var capabilities, features, labels, runtimeState, metricsState []byte
	err := s.pool.QueryRow(ctx, `
			SELECT id,name,version,os,arch,capabilities,features,labels,runtime,metrics,last_seen,enrolled_at
			FROM agents WHERE id=$1 AND revoked_at IS NULL`, id).Scan(
		&agent.ID, &agent.Name, &agent.Version, &agent.OS, &agent.Arch, &capabilities, &features, &labels, &runtimeState, &metricsState, &agent.LastSeen, &agent.EnrolledAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return core.Agent{}, ErrNotFound
	}
	if err != nil {
		return core.Agent{}, err
	}
	if err := json.Unmarshal(capabilities, &agent.Capabilities); err != nil {
		return core.Agent{}, err
	}
	if err := json.Unmarshal(features, &agent.Features); err != nil {
		return core.Agent{}, err
	}
	if err := json.Unmarshal(labels, &agent.Labels); err != nil {
		return core.Agent{}, err
	}
	if err := json.Unmarshal(runtimeState, &agent.Runtime); err != nil {
		return core.Agent{}, err
	}
	if err := json.Unmarshal(metricsState, &agent.Metrics); err != nil {
		return core.Agent{}, err
	}
	if agent.LastSeen.After(time.Now().UTC().Add(-45 * time.Second)) {
		agent.Status = "online"
	} else {
		agent.Status = "offline"
	}
	return agent, nil
}

// SetAgentClientAddress stores the operator-provided address used when
// building client connection profiles. It lives in the agent labels so the
// value survives Agent reconnects. Older enrollments may have stored a JSON
// null instead of an empty labels object, so normalize that shape on update.
func (s *Store) SetAgentClientAddress(ctx context.Context, id, address string) error {
	command, err := s.pool.Exec(ctx, `
		UPDATE agents
		SET labels = CASE
			WHEN $2 = '' THEN COALESCE(NULLIF(labels, 'null'::jsonb), '{}'::jsonb) - 'client_address'
			ELSE jsonb_set(
				COALESCE(NULLIF(labels, 'null'::jsonb), '{}'::jsonb),
				'{client_address}',
				to_jsonb($2::text),
				true
			)
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

// AgentConfig returns the one active configuration owned by an agent/core
// pair. Node-owned configurations cannot accidentally be deployed elsewhere.
func (s *Store) AgentConfig(ctx context.Context, agentID string, engine core.Engine) (core.Config, error) {
	var config core.Config
	err := s.pool.QueryRow(ctx, `
		SELECT id,COALESCE(agent_id,''),name,description,engine,content,version,created_at,updated_at
		FROM configs WHERE agent_id=$1 AND engine=$2 AND deleted_at IS NULL`, agentID, engine).Scan(
		&config.ID, &config.AgentID, &config.Name, &config.Description, &config.Engine, &config.Content,
		&config.Version, &config.CreatedAt, &config.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return core.Config{}, ErrNotFound
	}
	if err != nil {
		return core.Config{}, err
	}
	config.Content, err = s.decryptContent(config.Content)
	if err != nil {
		return core.Config{}, err
	}
	return config, nil
}

// ListAgentConfigs returns every active node-owned configuration. The control
// plane uses this for fleet-level deployment drift and listener summaries;
// general configuration workspaces remain isolated through ListConfigs.
func (s *Store) ListAgentConfigs(ctx context.Context) ([]core.Config, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id,COALESCE(agent_id,''),name,description,engine,content,version,created_at,updated_at
		FROM configs WHERE agent_id IS NOT NULL AND deleted_at IS NULL ORDER BY updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	configs := make([]core.Config, 0)
	for rows.Next() {
		var config core.Config
		if err := rows.Scan(&config.ID, &config.AgentID, &config.Name, &config.Description, &config.Engine, &config.Content,
			&config.Version, &config.CreatedAt, &config.UpdatedAt); err != nil {
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

func (s *Store) LatestDeployments(ctx context.Context) ([]core.Deployment, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT ON (agent_id,engine) agent_id,engine,COALESCE(config_id,''),COALESCE(config_version,0),finished_at
		FROM tasks
		WHERE action IN ('deploy','import-existing') AND status='succeeded' AND finished_at IS NOT NULL
		ORDER BY agent_id,engine,finished_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]core.Deployment, 0)
	for rows.Next() {
		var deployment core.Deployment
		if err := rows.Scan(&deployment.AgentID, &deployment.Engine, &deployment.ConfigID, &deployment.ConfigVersion, &deployment.DeployedAt); err != nil {
			return nil, err
		}
		result = append(result, deployment)
	}
	return result, rows.Err()
}

// SaveAgentConfig creates or updates an agent-owned configuration using an
// optimistic version check. expectedVersion must be zero for the first save.
func (s *Store) SaveAgentConfig(ctx context.Context, input core.Config, expectedVersion int) (core.Config, error) {
	if input.AgentID == "" {
		return core.Config{}, fmt.Errorf("%w: agent ID is required", ErrInvalid)
	}
	if err := core.ValidateConfig(input.Engine, input.Content); err != nil {
		return core.Config{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if expectedVersion < 0 {
		return core.Config{}, fmt.Errorf("%w: invalid configuration version", ErrInvalid)
	}
	name, description, err := validateConfigMetadata(input.Name, input.Description)
	if err != nil {
		return core.Config{}, err
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
	var capabilitiesJSON []byte
	if err := tx.QueryRow(ctx, `SELECT capabilities FROM agents WHERE id=$1 AND revoked_at IS NULL FOR UPDATE`, input.AgentID).Scan(&capabilitiesJSON); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return core.Config{}, fmt.Errorf("agent: %w", ErrNotFound)
		}
		return core.Config{}, err
	}
	var capabilities []core.Engine
	if err := json.Unmarshal(capabilitiesJSON, &capabilities); err != nil {
		return core.Config{}, err
	}
	if !containsEngine(capabilities, input.Engine) {
		return core.Config{}, fmt.Errorf("%w: agent does not advertise the requested engine", ErrInvalid)
	}

	var currentID string
	var currentVersion int
	err = tx.QueryRow(ctx, `SELECT id,version FROM configs
		WHERE agent_id=$1 AND engine=$2 AND deleted_at IS NULL FOR UPDATE`, input.AgentID, input.Engine).Scan(&currentID, &currentVersion)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return core.Config{}, err
	}
	if errors.Is(err, pgx.ErrNoRows) {
		if expectedVersion != 0 {
			return core.Config{}, fmt.Errorf("%w: configuration changed; reload before saving", ErrConflict)
		}
		currentID, err = core.NewID("cfg")
		if err != nil {
			return core.Config{}, err
		}
		now := time.Now().UTC()
		_, err = tx.Exec(ctx, `INSERT INTO configs
			(id,agent_id,name,description,engine,content,version,created_at,updated_at)
			VALUES ($1,$2,$3,$4,$5,$6,1,$7,$7)`, currentID, input.AgentID, name,
			description, input.Engine, storedContent, now)
		if err != nil {
			return core.Config{}, mapError(err)
		}
	} else {
		if expectedVersion != currentVersion {
			return core.Config{}, fmt.Errorf("%w: configuration changed; reload before saving", ErrConflict)
		}
		_, err = tx.Exec(ctx, `UPDATE configs SET name=$2,description=$3,content=$4,version=version+1,updated_at=now()
				WHERE id=$1`, currentID, name, description, storedContent)
		if err != nil {
			return core.Config{}, err
		}
	}
	var saved core.Config
	err = tx.QueryRow(ctx, `SELECT id,COALESCE(agent_id,''),name,description,engine,content,version,created_at,updated_at
		FROM configs WHERE id=$1`, currentID).Scan(&saved.ID, &saved.AgentID, &saved.Name, &saved.Description,
		&saved.Engine, &saved.Content, &saved.Version, &saved.CreatedAt, &saved.UpdatedAt)
	if err != nil {
		return core.Config{}, err
	}
	saved.Content, err = s.decryptContent(saved.Content)
	if err != nil {
		return core.Config{}, err
	}
	if err := s.insertConfigRevision(ctx, tx, saved); err != nil {
		return core.Config{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return core.Config{}, err
	}
	return saved, nil
}
