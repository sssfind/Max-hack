package main

import (
	"database/sql"
	"embed"
	"log/slog"
	"os"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

//go:embed migrations/*.sql
var embedMigrations embed.FS

func main() {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		slog.Error("DATABASE_URL is not set")
		os.Exit(1)
	}

	db, err := sql.Open("pgx", dbURL)
	if err != nil {
		slog.Error("Failed to open db for migrations", "error", err)
		os.Exit(1)
	}
	defer func() {
		if err := db.Close(); err != nil {
			slog.Warn("Failed to close migration database", "error", err)
		}
	}()

	if err := db.Ping(); err != nil {
		slog.Error("Database is unreachable", "error", err)
		os.Exit(1)
	}

	// Настраиваем Goose на использование встроенной файловой системы
	goose.SetBaseFS(embedMigrations)
	if err := goose.SetDialect("postgres"); err != nil {
		slog.Error("Failed to set goose dialect", "error", err)
		os.Exit(1)
	}

	// Запускаем миграции
	if err := goose.Up(db, "migrations"); err != nil {
		slog.Error("Migration failed", "error", err)
		os.Exit(1)
	}

	slog.Info("Successfully applied database migrations")
}
