package db

import "gorm.io/gorm"

func V1_2_3_EnsureRequestLogsUTF8MB4(db *gorm.DB) error {
	return runOneTimeMigration(db, "v1_2_3_ensure_request_logs_utf8mb4", func(tx *gorm.DB) error {
		if tx.Dialector.Name() != "mysql" {
			return nil
		}

		return tx.Exec("ALTER TABLE request_logs CONVERT TO CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci").Error
	})
}
