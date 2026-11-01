package relational

import "gorm.io/gorm"

// CZGorm hands CodizeLabs fork patch P3 (internal/cz/imaginepost) the GORM
// handle for its own cz_imagine_* tables. P3 never touches upstream tables.
func (d *Database) CZGorm() *gorm.DB { return d.db }
