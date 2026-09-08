package engine

import (
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

var DB *gorm.DB

// Connect initializes the database connection.
// databaseURL can be:
//   - postgres://user:pass@host:5432/dbname?sslmode=disable
//   - sqlite:///path/to/file.db  or  sqlite://data/graph.db  or  just a file path ending in .db
func Connect(databaseURL string) (*gorm.DB, error) {
	var dialector gorm.Dialector

	if strings.HasPrefix(databaseURL, "postgres") {
		dialector = postgres.Open(databaseURL)
	} else {
		// SQLite — strip prefix if present
		path := databaseURL
		path = strings.TrimPrefix(path, "sqlite:///")
		path = strings.TrimPrefix(path, "sqlite://")
		if path == "" {
			path = "data/graph.db"
		}
		dialector = sqlite.Open(path)
	}

	db, err := gorm.Open(dialector, &gorm.Config{
		Logger: logger.Default.LogMode(logger.Warn),
	})
	if err != nil {
		return nil, fmt.Errorf("connect to database: %w", err)
	}

	DB = db
	if sqlDB, err := db.DB(); err == nil {
		sqlDB.SetMaxOpenConns(50)
		sqlDB.SetMaxIdleConns(10)
		sqlDB.SetConnMaxLifetime(30 * time.Minute)
	}
	log.Printf("graph database connected: %s", databaseURL)
	return db, nil
}

// IsPostgres returns true if the database is PostgreSQL
func IsPostgres() bool {
	if DB == nil {
		return false
	}
	return DB.Name() == "postgres"
}
