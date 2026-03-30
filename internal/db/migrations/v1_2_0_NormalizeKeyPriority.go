package db

import "gorm.io/gorm"

func V1_2_0_NormalizeKeyPriority(db *gorm.DB) error {
	return db.Exec("UPDATE api_keys SET priority = 100 WHERE priority IS NULL OR priority <= 0").Error
}
