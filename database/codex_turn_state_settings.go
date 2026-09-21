package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

// CodexTurnStateSettings controls the server-side ticket harvester. The
// harvest proxy is intentionally kept out of the regular SystemSettings
// response so callers can request a masked value through the admin layer.
type CodexTurnStateSettings struct {
	Enabled               bool
	HarvestProxyURL       string
	Models                []string
	ProbeModels           []string
	TargetLength          int
	TTLSeconds            int
	RefreshBeforeSeconds  int
	ProbeIntervalSeconds  int
	AttemptTimeoutSeconds int
	Concurrency           int
	PreserveExisting      bool
	FailClosed            bool
}

const (
	defaultCodexTicketModels = `["gpt-6-astra","gpt-5.6-sol"]`
	defaultCodexTicketLength = 292
)

func normalizeCodexTicketSettings(s *CodexTurnStateSettings) *CodexTurnStateSettings {
	if s == nil {
		s = &CodexTurnStateSettings{
			Models:           []string{"gpt-6-astra", "gpt-5.6-sol"},
			ProbeModels:      []string{"gpt-6-astra", "gpt-5.6-sol"},
			PreserveExisting: true,
		}
	}
	normalizeModels := func(values []string, exactOnly bool) []string {
		models := make([]string, 0, len(values))
		seen := make(map[string]struct{})
		for _, model := range values {
			model = strings.TrimSpace(model)
			if model == "" || (exactOnly && strings.Contains(model, "*")) {
				continue
			}
			key := strings.ToLower(model)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			models = append(models, model)
		}
		return models
	}
	models := normalizeModels(s.Models, false)
	probeModels := normalizeModels(s.ProbeModels, true)
	if s.ProbeModels == nil {
		// Existing installations used the managed model list for both matching
		// and probing. Carry exact entries forward, but never send wildcard
		// patterns to the upstream API as literal model names.
		probeModels = normalizeModels(models, true)
	}
	if s.TargetLength <= 0 {
		s.TargetLength = defaultCodexTicketLength
	}
	if s.TTLSeconds <= 0 {
		s.TTLSeconds = 3600
	}
	if s.RefreshBeforeSeconds < 0 {
		s.RefreshBeforeSeconds = 0
	}
	if s.RefreshBeforeSeconds >= s.TTLSeconds {
		s.RefreshBeforeSeconds = s.TTLSeconds / 6
	}
	if s.ProbeIntervalSeconds <= 0 {
		s.ProbeIntervalSeconds = 6
	}
	if s.AttemptTimeoutSeconds <= 0 {
		s.AttemptTimeoutSeconds = 25
	}
	if s.Concurrency <= 0 {
		s.Concurrency = 1
	}
	if s.Concurrency > 64 {
		s.Concurrency = 64
	}
	s.HarvestProxyURL = strings.TrimSpace(s.HarvestProxyURL)
	s.Models = models
	s.ProbeModels = probeModels
	return s
}

func (db *DB) GetCodexTurnStateSettings(ctx context.Context) (*CodexTurnStateSettings, error) {
	if db == nil || db.conn == nil {
		return normalizeCodexTicketSettings(nil), nil
	}
	query := `SELECT enabled, harvest_proxy_url, models, probe_models, target_length, ttl_seconds, refresh_before_seconds, probe_interval_seconds, attempt_timeout_seconds, concurrency, preserve_existing, fail_closed FROM codex_turn_state_settings WHERE id = 1`
	s := &CodexTurnStateSettings{}
	var modelsRaw, probeModelsRaw string
	err := db.conn.QueryRowContext(ctx, query).Scan(&s.Enabled, &s.HarvestProxyURL, &modelsRaw, &probeModelsRaw, &s.TargetLength, &s.TTLSeconds, &s.RefreshBeforeSeconds, &s.ProbeIntervalSeconds, &s.AttemptTimeoutSeconds, &s.Concurrency, &s.PreserveExisting, &s.FailClosed)
	if err == sql.ErrNoRows {
		return normalizeCodexTicketSettings(nil), nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(modelsRaw), &s.Models); err != nil {
		return nil, fmt.Errorf("parse codex turn-state models: %w", err)
	}
	if err := json.Unmarshal([]byte(probeModelsRaw), &s.ProbeModels); err != nil {
		return nil, fmt.Errorf("parse codex turn-state probe models: %w", err)
	}
	return normalizeCodexTicketSettings(s), nil
}

func (db *DB) UpdateCodexTurnStateSettings(ctx context.Context, s *CodexTurnStateSettings) error {
	if db == nil || db.conn == nil {
		return fmt.Errorf("database is not initialized")
	}
	s = normalizeCodexTicketSettings(s)
	modelsRaw, err := json.Marshal(s.Models)
	if err != nil {
		return err
	}
	probeModelsRaw, err := json.Marshal(s.ProbeModels)
	if err != nil {
		return err
	}
	if db.isSQLite() {
		return db.withSQLiteWriteLock(ctx, func() error {
			_, err := db.conn.ExecContext(ctx, `INSERT INTO codex_turn_state_settings (id, enabled, harvest_proxy_url, models, probe_models, target_length, ttl_seconds, refresh_before_seconds, probe_interval_seconds, attempt_timeout_seconds, concurrency, preserve_existing, fail_closed) VALUES (1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(id) DO UPDATE SET enabled=excluded.enabled, harvest_proxy_url=excluded.harvest_proxy_url, models=excluded.models, probe_models=excluded.probe_models, target_length=excluded.target_length, ttl_seconds=excluded.ttl_seconds, refresh_before_seconds=excluded.refresh_before_seconds, probe_interval_seconds=excluded.probe_interval_seconds, attempt_timeout_seconds=excluded.attempt_timeout_seconds, concurrency=excluded.concurrency, preserve_existing=excluded.preserve_existing, fail_closed=excluded.fail_closed`, s.Enabled, s.HarvestProxyURL, string(modelsRaw), string(probeModelsRaw), s.TargetLength, s.TTLSeconds, s.RefreshBeforeSeconds, s.ProbeIntervalSeconds, s.AttemptTimeoutSeconds, s.Concurrency, s.PreserveExisting, s.FailClosed)
			return err
		})
	}
	_, err = db.conn.ExecContext(ctx, `INSERT INTO codex_turn_state_settings (id, enabled, harvest_proxy_url, models, probe_models, target_length, ttl_seconds, refresh_before_seconds, probe_interval_seconds, attempt_timeout_seconds, concurrency, preserve_existing, fail_closed) VALUES (1, $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12) ON CONFLICT (id) DO UPDATE SET enabled=EXCLUDED.enabled, harvest_proxy_url=EXCLUDED.harvest_proxy_url, models=EXCLUDED.models, probe_models=EXCLUDED.probe_models, target_length=EXCLUDED.target_length, ttl_seconds=EXCLUDED.ttl_seconds, refresh_before_seconds=EXCLUDED.refresh_before_seconds, probe_interval_seconds=EXCLUDED.probe_interval_seconds, attempt_timeout_seconds=EXCLUDED.attempt_timeout_seconds, concurrency=EXCLUDED.concurrency, preserve_existing=EXCLUDED.preserve_existing, fail_closed=EXCLUDED.fail_closed`, s.Enabled, s.HarvestProxyURL, string(modelsRaw), string(probeModelsRaw), s.TargetLength, s.TTLSeconds, s.RefreshBeforeSeconds, s.ProbeIntervalSeconds, s.AttemptTimeoutSeconds, s.Concurrency, s.PreserveExisting, s.FailClosed)
	return err
}
