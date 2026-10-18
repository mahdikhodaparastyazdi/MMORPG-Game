package db

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func ConnectAndMigrate(ctx context.Context, databaseURL string, migrationPath string) (*sql.DB, error) {
	conn, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}

	conn.SetMaxOpenConns(25)
	conn.SetMaxIdleConns(10)
	conn.SetConnMaxLifetime(5 * time.Minute)

	if err := conn.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("ping db: %w", err)
	}

	sqlBytes, err := os.ReadFile(migrationPath)
	if err != nil {
		return nil, fmt.Errorf("read migration file %s: %w", migrationPath, err)
	}

	if _, err := conn.ExecContext(ctx, string(sqlBytes)); err != nil {
		return nil, fmt.Errorf("apply migration: %w", err)
	}

	return conn, nil
}