package db

import "gorm.io/gorm"

func V1_2_2_EnableWeightedKeyFailures(db *gorm.DB) error {
	return runOneTimeMigration(db, "v1_2_2_enable_weighted_key_failures", func(tx *gorm.DB) error {
		if tx.Dialector.Name() == "mysql" {
			return tx.Exec("ALTER TABLE api_keys MODIFY COLUMN failure_count DOUBLE NOT NULL DEFAULT 0").Error
		}
		return nil
	})
}
