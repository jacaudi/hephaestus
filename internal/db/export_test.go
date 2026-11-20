package db

import "context"

// MigrateDownTo rolls the database back to version, for the Down migration
// test, which runs in package db_test.
func (d *DB) MigrateDownTo(ctx context.Context, version int64) error {
	p, err := d.provider()
	if err != nil {
		return err
	}
	if _, err := p.DownTo(ctx, version); err != nil {
		return err
	}
	return nil
}
