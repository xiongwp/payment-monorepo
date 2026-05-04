package segment

import (
	"gorm.io/gorm"
)

func Fetch(db *gorm.DB) (int64, error) {
	var maxID int64

	err := db.Transaction(func(tx *gorm.DB) error {
		return tx.Raw("SELECT max_id FROM id_segment LIMIT 1").Scan(&maxID).Error
	})

	if err != nil {
		return 0, err
	}

	return maxID, nil
}
