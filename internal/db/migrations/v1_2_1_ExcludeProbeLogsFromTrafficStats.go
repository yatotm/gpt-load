package db

import (
	"fmt"
	"gpt-load/internal/models"
	"time"

	"gorm.io/gorm"
)

type migrationRecord struct {
	Name       string    `gorm:"primaryKey;size:191"`
	ExecutedAt time.Time `gorm:"not null"`
}

type probeHourlyAdjustment struct {
	Time         time.Time
	GroupID      uint
	SuccessCount int64
	FailureCount int64
}

func V1_2_1_ExcludeProbeLogsFromTrafficStats(db *gorm.DB) error {
	return runOneTimeMigration(db, "v1_2_1_exclude_probe_logs_from_traffic_stats", func(tx *gorm.DB) error {
		type probeLog struct {
			Timestamp time.Time
			GroupID   uint
			KeyHash   string
			IsSuccess bool
		}

		probeCountsByGroup := make(map[uint]map[string]int64)
		hourlyAdjustments := make(map[string]*probeHourlyAdjustment)

		var batch []probeLog
		if err := tx.Model(&models.RequestLog{}).
			Select("timestamp, group_id, key_hash, is_success").
			Where("request_type = ?", models.RequestTypeProbe).
			FindInBatches(&batch, 1000, func(_ *gorm.DB, _ int) error {
				for _, log := range batch {
					if log.IsSuccess && log.KeyHash != "" {
						if _, exists := probeCountsByGroup[log.GroupID]; !exists {
							probeCountsByGroup[log.GroupID] = make(map[string]int64)
						}
						probeCountsByGroup[log.GroupID][log.KeyHash]++
					}

					hour := log.Timestamp.Truncate(time.Hour)
					adjustKey := fmt.Sprintf("%d:%s", log.GroupID, hour.UTC().Format(time.RFC3339))
					if _, exists := hourlyAdjustments[adjustKey]; !exists {
						hourlyAdjustments[adjustKey] = &probeHourlyAdjustment{
							Time:    hour,
							GroupID: log.GroupID,
						}
					}

					if log.IsSuccess {
						hourlyAdjustments[adjustKey].SuccessCount++
					} else {
						hourlyAdjustments[adjustKey].FailureCount++
					}
				}
				return nil
			}).Error; err != nil {
			return fmt.Errorf("failed to scan probe logs for traffic stat repair: %w", err)
		}

		for groupID, countsByHash := range probeCountsByGroup {
			if len(countsByHash) == 0 {
				continue
			}

			var requestCountCase string
			requestCountCase = "CASE key_hash "
			requestCountArgs := make([]any, 0, len(countsByHash)*3)
			keyHashes := make([]string, 0, len(countsByHash))

			for keyHash, probeCount := range countsByHash {
				requestCountCase += "WHEN ? THEN CASE WHEN request_count >= ? THEN request_count - ? ELSE 0 END "
				requestCountArgs = append(requestCountArgs, keyHash, probeCount, probeCount)
				keyHashes = append(keyHashes, keyHash)
			}
			requestCountCase += "ELSE request_count END"

			if err := tx.Model(&models.APIKey{}).
				Where("group_id = ? AND key_hash IN ?", groupID, keyHashes).
				Update("request_count", gorm.Expr(requestCountCase, requestCountArgs...)).Error; err != nil {
				return fmt.Errorf("failed to repair api_key request_count for group %d: %w", groupID, err)
			}
		}

		for _, adjustment := range hourlyAdjustments {
			updates := map[string]any{}
			if adjustment.SuccessCount > 0 {
				updates["success_count"] = gorm.Expr(
					"CASE WHEN success_count >= ? THEN success_count - ? ELSE 0 END",
					adjustment.SuccessCount,
					adjustment.SuccessCount,
				)
			}
			if adjustment.FailureCount > 0 {
				updates["failure_count"] = gorm.Expr(
					"CASE WHEN failure_count >= ? THEN failure_count - ? ELSE 0 END",
					adjustment.FailureCount,
					adjustment.FailureCount,
				)
			}
			if len(updates) == 0 {
				continue
			}

			if err := tx.Model(&models.GroupHourlyStat{}).
				Where("group_id = ? AND time = ?", adjustment.GroupID, adjustment.Time).
				Updates(updates).Error; err != nil {
				return fmt.Errorf("failed to repair group_hourly_stats for group %d at %s: %w", adjustment.GroupID, adjustment.Time.Format(time.RFC3339), err)
			}
		}

		if err := tx.Where("success_count = 0 AND failure_count = 0").Delete(&models.GroupHourlyStat{}).Error; err != nil {
			return fmt.Errorf("failed to cleanup empty group_hourly_stats rows: %w", err)
		}

		return nil
	})
}

func runOneTimeMigration(db *gorm.DB, name string, fn func(tx *gorm.DB) error) error {
	if err := db.AutoMigrate(&migrationRecord{}); err != nil {
		return fmt.Errorf("failed to prepare migration record table: %w", err)
	}

	return db.Transaction(func(tx *gorm.DB) error {
		var count int64
		if err := tx.Model(&migrationRecord{}).Where("name = ?", name).Count(&count).Error; err != nil {
			return fmt.Errorf("failed to check migration record %s: %w", name, err)
		}
		if count > 0 {
			return nil
		}

		if err := fn(tx); err != nil {
			return err
		}

		return tx.Create(&migrationRecord{
			Name:       name,
			ExecutedAt: time.Now(),
		}).Error
	})
}
