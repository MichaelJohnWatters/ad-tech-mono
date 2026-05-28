// cmd/migrate runs database migrations using goose.
//
// Usage:
//
//	go run ./cmd/migrate           # run all pending migrations (up)
//	go run ./cmd/migrate up        # same as above
//	go run ./cmd/migrate down      # rollback last migration
//	go run ./cmd/migrate status    # show migration status
//	go run ./cmd/migrate reset     # rollback all migrations
package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"

	_ "github.com/lib/pq"
	"github.com/pressly/goose/v3"
)

func main() {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://adtech:adtech-local-dev@localhost:5432/adtech?sslmode=disable"
	}

	migrationsDir := os.Getenv("MIGRATIONS_DIR")
	if migrationsDir == "" {
		migrationsDir = "migrations"
	}

	db, err := sql.Open("postgres", dbURL)
	if err != nil {
		log.Fatalf("failed to open database: %v", err)
	}
	defer db.Close()

	if err := db.PingContext(context.Background()); err != nil {
		log.Fatalf("failed to ping database: %v", err)
	}

	command := "up"
	if len(os.Args) > 1 {
		command = os.Args[1]
	}

	if err := goose.RunContext(context.Background(), command, db, migrationsDir); err != nil {
		log.Fatalf("migration %s failed: %v", command, err)
	}

	fmt.Printf("Migration %s completed successfully.\n", command)
}
