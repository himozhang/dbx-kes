package main

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// db_stats.go exposes the live-statistics read paths the desktop side panel
// consumes. All of them read from in-memory server catalogs (pg/sys_stat_statements,
// pg/sys_stat_activity) which require no writes on our side — we are pure
// observers here.
//
// Every query is prefixed with a dual-catalog probe so it keeps working across
// Kingbase V7/V8 releases: sys_catalog first, pg_catalog as fallback.

func (s *server) statCatalog() string {
	if s.mode.postgresCatalog {
		return "pg_catalog"
	}
	return "sys_catalog"
}

// listTopStatements pulls the heaviest SQL statements by mean execution time.
// limit defaults to 50 when <= 0.
func (s *server) listTopStatements(schema string, limit int) ([]map[string]any, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if limit <= 0 {
		limit = 50
	}
	// Detect whether stat_statements extension is present.
	var count int
	cat := s.statCatalog()
	extPrefix := "pg"
	if cat == "sys_catalog" {
		extPrefix = "sys"
	}
	if err := s.db.QueryRowContext(ctx, fmt.Sprintf(
		`SELECT COUNT(*) FROM %s.%s_available_extensions WHERE extname IN ('sys_stat_statements', 'pg_stat_statements')`,
		cat, extPrefix,
	)).Scan(&count); err != nil || count == 0 {
		return nil, fmt.Errorf("stat_statements 扩展未安装。请执行 CREATE EXTENSION sys_stat_statements 后重试")
	}

	// Kingbase V8 exposes sys_stat_statements, V7 may use pg_stat_statements.
	catalog := s.statCatalog()
	table := "sys_stat_statements"
	altTable := "pg_stat_statements"
	// Prefer sys_stat_statements but fall back to pg_ if missing.
	var query string
	args := []any{}
	if schema != "" {
		query = fmt.Sprintf(
			`SELECT query, calls, total_exec_time, mean_exec_time, rows, shared_blks_hit, shared_blks_read, created
			 FROM %s.%s WHERE query LIKE $1 ORDER BY mean_exec_time DESC LIMIT %d`,
			catalog, table, limit,
		)
		args = append(args, schema+".%")
	} else {
		query = fmt.Sprintf(
			`SELECT query, calls, total_exec_time, mean_exec_time, rows, shared_blks_hit, shared_blks_read, created
			 FROM %s.%s ORDER BY mean_exec_time DESC LIMIT %d`,
			catalog, table, limit,
		)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		// Try alt catalog.
		if catalog == "sys_catalog" {
			query = fmt.Sprintf(
				`SELECT query, calls, total_exec_time, mean_exec_time, rows, shared_blks_hit, shared_blks_read, created
				 FROM pg_catalog.%s ORDER BY mean_exec_time DESC LIMIT %d`,
				altTable, limit,
			)
			rows, err = s.db.QueryContext(ctx, query, args...)
		}
		if err != nil {
			return nil, fmt.Errorf("top statements query: %w (stat_statements may not be enabled — check shared_preload_libraries)", err)
		}
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	result := []map[string]any{}
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		row := map[string]any{}
		for i, c := range cols {
			row[c] = vals[i]
		}
		result = append(result, row)
	}
	return result, rows.Err()
}

// listActivity lists current connections from sys/pg_stat_activity.
func (s *server) listActivity() ([]map[string]any, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	catalog := s.statCatalog()
	statPrefix := "pg_"
	if catalog == "sys_catalog" {
		statPrefix = "sys_"
	}
	query := fmt.Sprintf(
		`SELECT pid, usename, application_name, state, query_start, wait_event_type, wait_event, backend_type, query
		 FROM %s.%s_stat_activity
		 WHERE datname = current_database()
		 ORDER BY query_start ASC NULLS LAST`,
		catalog, statPrefix,
	)
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		// pg_stat_activity fallback.
		query = `SELECT pid, usename, application_name, state, query_start, wait_event_type, wait_event, backend_type, query
				 FROM pg_catalog.pg_stat_activity WHERE datname = current_database() ORDER BY query_start ASC NULLS LAST`
		rows, err = s.db.QueryContext(ctx, query)
		if err != nil {
			return nil, fmt.Errorf("list activity: %w", err)
		}
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	result := []map[string]any{}
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		row := map[string]any{}
		for i, c := range cols {
			row[c] = vals[i]
		}
		result = append(result, row)
	}
	return result, rows.Err()
}

// killActivity terminates a session by pid. Requires matching role (SUPERUSER or
// same user) — the database will reject otherwise, we just surface the error.
func (s *server) killActivity(pid int) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	catalog := s.statCatalog()
	_, err := s.db.ExecContext(ctx, fmt.Sprintf("SELECT %s_terminate_backend($1)", catalog), pid)
	if err != nil {
		// sys_catalog.sys_terminate_backend — Kingbase uses sys_ prefix.
		_, err = s.db.ExecContext(ctx, "SELECT pg_terminate_backend($1)", pid)
	}
	return err
}

// getServerDataDirectory returns the filesystem path of the Kingbase data
// directory — useful as a hint for the log root directory default in the UI.
func (s *server) getServerDataDirectory() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var dir sql.NullString
	// Kingbase V8 exposes kingbase_data_directory(); V7 falls back to
	// data_directory which is PG-compat.
	if err := s.db.QueryRowContext(ctx, "SELECT current_setting('kingbase_data_directory', true)").Scan(&dir); err == nil && dir.Valid {
		return dir.String, nil
	}
	if err := s.db.QueryRowContext(ctx, "SELECT current_setting('data_directory', true)").Scan(&dir); err == nil && dir.Valid {
		return dir.String, nil
	}
	return "", fmt.Errorf("无法读取数据目录，请手动填写日志根目录")
}
