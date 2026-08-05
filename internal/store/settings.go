package store

import (
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strconv"

	"github.com/RP2/gh-vault/internal/model"
)

type SettingsStore interface {
	Get(key string) (string, error)
	Set(key, value string) error
	GetAll() (model.Settings, error)
}

var _ SettingsStore = (*settingsStore)(nil)

const maxRetentionDays = 36500

const settingGetSQL = "SELECT value FROM settings WHERE key = ?"

const settingUpsertSQL = `INSERT INTO settings (key, value) VALUES (?, ?)
ON CONFLICT(key) DO UPDATE SET value = excluded.value`

const settingListSQL = "SELECT key, value FROM settings"

func validateSetting(key, value string) error {
	switch key {
	case "dry_run":
		if _, err := strconv.ParseBool(value); err != nil {
			return fmt.Errorf("store: invalid dry_run %q: %w", value, err)
		}
	case "auto_archive_days":
		n, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("store: invalid auto_archive_days %q: %w", value, err)
		}
		if n < 0 || n > maxRetentionDays {
			return fmt.Errorf("store: auto_archive_days out of range [0, %d]: %d", maxRetentionDays, n)
		}
	case "log_retention_days":
		n, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("store: invalid log_retention_days %q: %w", value, err)
		}
		if n < 0 || n > maxRetentionDays {
			return fmt.Errorf("store: log_retention_days out of range [0, %d]: %d", maxRetentionDays, n)
		}
	case "cron_schedule":
		if value == "" {
			return errors.New("store: cron_schedule must not be empty")
		}
	}
	return nil
}

func (s *settingsStore) Get(key string) (string, error) {
	var value string
	err := s.db.QueryRow(settingGetSQL, key).Scan(&value)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrNotFound
		}
		return "", fmt.Errorf("store: get setting %q: %w", key, err)
	}
	return value, nil
}

func (s *settingsStore) Set(key, value string) error {
	if err := validateSetting(key, value); err != nil {
		return err
	}
	_, err := s.db.Exec(settingUpsertSQL, key, value)
	if err != nil {
		return fmt.Errorf("store: set setting %q: %w", key, err)
	}
	return nil
}

func (s *settingsStore) GetAll() (model.Settings, error) {
	rows, err := s.db.Query(settingListSQL)
	if err != nil {
		return model.Settings{}, fmt.Errorf("store: list settings: %w", err)
	}
	defer rows.Close()

	var out model.Settings
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return model.Settings{}, fmt.Errorf("store: scan setting: %w", err)
		}
		switch key {
		case "cron_schedule":
			out.CronSchedule = value
		case "dry_run":
			b, err := strconv.ParseBool(value)
			if err != nil {
				slog.Warn("store: invalid dry_run setting, using zero value", "value", value, "err", err)
				continue
			}
			out.DryRun = b
		case "auto_archive_days":
			n, err := strconv.Atoi(value)
			if err != nil {
				slog.Warn("store: invalid auto_archive_days setting, using zero value", "value", value, "err", err)
				continue
			}
			if n < 0 || n > maxRetentionDays {
				slog.Warn("store: auto_archive_days out of range, using zero value", "value", n, "max", maxRetentionDays)
				continue
			}
			out.AutoArchiveDays = n
		case "log_retention_days":
			n, err := strconv.Atoi(value)
			if err != nil {
				slog.Warn("store: invalid log_retention_days setting, using zero value", "value", value, "err", err)
				continue
			}
			if n < 0 || n > maxRetentionDays {
				slog.Warn("store: log_retention_days out of range, using zero value", "value", n, "max", maxRetentionDays)
				continue
			}
			out.LogRetentionDays = n
		}
	}
	if err := rows.Err(); err != nil {
		return model.Settings{}, fmt.Errorf("store: iterate settings: %w", err)
	}
	return out, nil
}
