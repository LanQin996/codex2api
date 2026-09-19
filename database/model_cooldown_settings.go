package database

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

const (
	ModelCooldownModeOff      = "off"
	ModelCooldownModeFixed    = "fixed"
	ModelCooldownModeAdaptive = "adaptive"

	DefaultRelayModelCooldownSeconds = 2
	DefaultOAuthModelCooldownSeconds = 300
	MaxModelCooldownSeconds          = 1800
)

// ModelCooldownSettings separates transient API/relay throttling from OAuth
// account-pool throttling. Relay defaults intentionally avoid persistent
// cooldowns because an isolated upstream 429 is normal load-shedding, not an
// account health failure.
type ModelCooldownSettings struct {
	ResponsesMode       string
	ResponsesSeconds    int
	RelayMode           string
	RelaySeconds        int
	RelayBackoffEnabled bool
	OAuthMode           string
	OAuthSeconds        int
	OAuthBackoffEnabled bool
}

type ModelCooldownSettingsUpdate struct {
	ResponsesMode       *string
	ResponsesSeconds    *int
	RelayMode           *string
	RelaySeconds        *int
	RelayBackoffEnabled *bool
	OAuthMode           *string
	OAuthSeconds        *int
	OAuthBackoffEnabled *bool
}

func DefaultModelCooldownSettings() ModelCooldownSettings {
	return ModelCooldownSettings{
		ResponsesMode: ModelCooldownModeAdaptive, ResponsesSeconds: 15,
		RelayMode:           ModelCooldownModeOff,
		RelaySeconds:        DefaultRelayModelCooldownSeconds,
		RelayBackoffEnabled: false,
		OAuthMode:           ModelCooldownModeAdaptive,
		OAuthSeconds:        DefaultOAuthModelCooldownSeconds,
		OAuthBackoffEnabled: true,
	}
}

func NormalizeModelCooldownMode(value, fallback string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case ModelCooldownModeOff:
		return ModelCooldownModeOff
	case ModelCooldownModeFixed:
		return ModelCooldownModeFixed
	case ModelCooldownModeAdaptive:
		return ModelCooldownModeAdaptive
	default:
		switch fallback {
		case ModelCooldownModeOff, ModelCooldownModeFixed, ModelCooldownModeAdaptive:
			return fallback
		default:
			return ModelCooldownModeAdaptive
		}
	}
}

func IsValidModelCooldownMode(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case ModelCooldownModeOff, ModelCooldownModeFixed, ModelCooldownModeAdaptive:
		return true
	default:
		return false
	}
}

func NormalizeModelCooldownSeconds(value, fallback int) int {
	if fallback < 0 {
		fallback = 0
	}
	if fallback > MaxModelCooldownSeconds {
		fallback = MaxModelCooldownSeconds
	}
	if value < 0 {
		return fallback
	}
	if value > MaxModelCooldownSeconds {
		return MaxModelCooldownSeconds
	}
	return value
}

func NormalizeModelCooldownSettings(value ModelCooldownSettings) ModelCooldownSettings {
	defaults := DefaultModelCooldownSettings()
	value.ResponsesMode = NormalizeModelCooldownMode(value.ResponsesMode, defaults.ResponsesMode)
	if value.ResponsesSeconds <= 0 {
		value.ResponsesSeconds = 15
	}
	value.ResponsesSeconds = NormalizeModelCooldownSeconds(value.ResponsesSeconds, 15)
	value.RelayMode = NormalizeModelCooldownMode(value.RelayMode, defaults.RelayMode)
	if value.RelaySeconds <= 0 {
		value.RelaySeconds = defaults.RelaySeconds
	}
	value.RelaySeconds = NormalizeModelCooldownSeconds(value.RelaySeconds, defaults.RelaySeconds)
	value.OAuthMode = NormalizeModelCooldownMode(value.OAuthMode, defaults.OAuthMode)
	if value.OAuthSeconds <= 0 {
		value.OAuthSeconds = defaults.OAuthSeconds
	}
	value.OAuthSeconds = NormalizeModelCooldownSeconds(value.OAuthSeconds, defaults.OAuthSeconds)
	return value
}

func (db *DB) GetModelCooldownSettings(ctx context.Context) (ModelCooldownSettings, error) {
	defaults := DefaultModelCooldownSettings()
	value := defaults
	err := db.conn.QueryRowContext(ctx, `
		SELECT
			COALESCE(NULLIF(TRIM(relay_model_cooldown_mode), ''), $1),
			COALESCE(relay_model_cooldown_seconds, $2),
			COALESCE(relay_model_cooldown_backoff_enabled, $3),
			COALESCE(NULLIF(TRIM(oauth_model_cooldown_mode), ''), $4),
			COALESCE(oauth_model_cooldown_seconds, $5),
			COALESCE(oauth_model_cooldown_backoff_enabled, $6), responses_cooldown_mode, responses_cooldown_seconds
		FROM system_settings
		WHERE id = 1
	`, defaults.RelayMode, defaults.RelaySeconds, defaults.RelayBackoffEnabled,
		defaults.OAuthMode, defaults.OAuthSeconds, defaults.OAuthBackoffEnabled,
	).Scan(
		&value.RelayMode,
		&value.RelaySeconds,
		&value.RelayBackoffEnabled,
		&value.OAuthMode,
		&value.OAuthSeconds,
		&value.OAuthBackoffEnabled, &value.ResponsesMode, &value.ResponsesSeconds,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return defaults, nil
		}
		return defaults, fmt.Errorf("读取模型冷却设置失败: %w", err)
	}
	return NormalizeModelCooldownSettings(value), nil
}

func (db *DB) UpdateModelCooldownSettings(ctx context.Context, update ModelCooldownSettingsUpdate) (ModelCooldownSettings, error) {
	current, err := db.GetModelCooldownSettings(ctx)
	if err != nil {
		return ModelCooldownSettings{}, err
	}
	if update.RelayMode != nil {
		current.RelayMode = NormalizeModelCooldownMode(*update.RelayMode, current.RelayMode)
	}
	if update.RelaySeconds != nil {
		current.RelaySeconds = NormalizeModelCooldownSeconds(*update.RelaySeconds, current.RelaySeconds)
	}
	if update.RelayBackoffEnabled != nil {
		current.RelayBackoffEnabled = *update.RelayBackoffEnabled
	}
	if update.OAuthMode != nil {
		current.OAuthMode = NormalizeModelCooldownMode(*update.OAuthMode, current.OAuthMode)
	}
	if update.OAuthSeconds != nil {
		current.OAuthSeconds = NormalizeModelCooldownSeconds(*update.OAuthSeconds, current.OAuthSeconds)
	}
	if update.OAuthBackoffEnabled != nil {
		current.OAuthBackoffEnabled = *update.OAuthBackoffEnabled
	}
	if update.ResponsesMode != nil {
		current.ResponsesMode = *update.ResponsesMode
	}
	if update.ResponsesSeconds != nil {
		current.ResponsesSeconds = *update.ResponsesSeconds
	}
	current = NormalizeModelCooldownSettings(current)

	if _, err := db.conn.ExecContext(ctx, `INSERT INTO system_settings (id) VALUES (1) ON CONFLICT(id) DO NOTHING`); err != nil {
		return ModelCooldownSettings{}, fmt.Errorf("初始化模型冷却设置失败: %w", err)
	}
	_, err = db.conn.ExecContext(ctx, `
		UPDATE system_settings SET
			relay_model_cooldown_mode = $1,
			relay_model_cooldown_seconds = $2,
			relay_model_cooldown_backoff_enabled = $3,
			oauth_model_cooldown_mode = $4,
			oauth_model_cooldown_seconds = $5,
			oauth_model_cooldown_backoff_enabled = $6, responses_cooldown_mode = $7, responses_cooldown_seconds = $8
		WHERE id = 1
	`, current.RelayMode, current.RelaySeconds, current.RelayBackoffEnabled,
		current.OAuthMode, current.OAuthSeconds, current.OAuthBackoffEnabled, current.ResponsesMode, current.ResponsesSeconds,
	)
	if err != nil {
		return ModelCooldownSettings{}, fmt.Errorf("保存模型冷却设置失败: %w", err)
	}
	return current, nil
}
