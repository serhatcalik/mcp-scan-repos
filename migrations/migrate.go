package migrations

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed *.sql
var scripts embed.FS

// Apply serializes startup migrations across API, worker, and MCP replicas.
func Apply(ctx context.Context, db *pgxpool.Pool) error {
	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(ctx, "select pg_advisory_xact_lock(734802619)"); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `create table if not exists schema_migrations (version text primary key, applied_at timestamptz not null default now())`); err != nil {
		return err
	}
	files, err := fs.Glob(scripts, "*.sql")
	if err != nil {
		return err
	}
	for _, name := range files {
		var applied bool
		if err := tx.QueryRow(ctx, "select exists(select 1 from schema_migrations where version=$1)", name).Scan(&applied); err != nil {
			return err
		}
		if applied {
			continue
		}
		// Adopt the initial schema when it was already installed manually.
		if strings.HasPrefix(name, "001_") {
			var count int
			if err := tx.QueryRow(ctx, `select count(*) from information_schema.tables where table_schema=current_schema() and table_name=any($1)`, []string{
				"gitlab_groups", "projects", "documentation_jobs", "job_attempts", "ingestion_requests",
				"analysis_snapshots", "evidence_items", "generated_documents", "documentation_artifacts", "publication_records",
			}).Scan(&count); err != nil {
				return err
			}
			if count != 0 && count != 10 {
				return fmt.Errorf("cannot adopt partial initial schema: found %d of 10 tables", count)
			}
			applied = count == 10
		}
		if !applied {
			body, err := scripts.ReadFile(name)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, string(body)); err != nil {
				return fmt.Errorf("migration %s: %w", name, err)
			}
		}
		if _, err := tx.Exec(ctx, "insert into schema_migrations(version) values($1)", name); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
