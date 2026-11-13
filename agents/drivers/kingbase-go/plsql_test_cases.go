package main

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// dbxTestCasesSchema controls which schema the metadata table lives in.
// Defaulting to public keeps the table visible to the current user without
// requiring explicit schema-qualification in every CRUD call. DBA installations
// that want a dedicated schema can flip the env var KINGBASE_DBX_TESTCASE_SCHEMA.
var dbxTestCasesSchema = "public"

// ensureTestCasesTableIdempotent creates the metadata table if it doesn't
// exist. Safe to call on every test-case RPC — CREATE TABLE IF NOT EXISTS is
// cheap and the statement is memoized by the connection pool after the first
// run. The schema parameter is accepted so future upgrades can support
// non-public placement via KINGBASE_DBX_TESTCASE_SCHEMA.
func (s *server) ensureTestCasesTable(ctx context.Context) error {
	schema := dbxTestCasesSchema
	safeSchema := quoteIdentifier(schema)
	ddl := fmt.Sprintf(`
CREATE TABLE IF NOT EXISTS %s.dbx_plsql_test_cases (
	id               BIGSERIAL PRIMARY KEY,
	case_name        TEXT NOT NULL,
	schema_name      TEXT NOT NULL,
	object_name      TEXT NOT NULL,
	object_type      TEXT NOT NULL,
	package_name     TEXT,
	pre_sql          TEXT,
	call_sql         TEXT NOT NULL,
	params_json      TEXT NOT NULL,
	expected_return  TEXT,
	note             TEXT,
	creator          TEXT DEFAULT CURRENT_USER,
	created_at       TIMESTAMPTZ DEFAULT now(),
	updated_at       TIMESTAMPTZ DEFAULT now()
)`, safeSchema)
	if _, err := s.db.ExecContext(ctx, ddl); err != nil {
		return fmt.Errorf("ensure plsql test case table: %w", err)
	}
	idx := fmt.Sprintf(
		`CREATE INDEX IF NOT EXISTS idx_dbx_testcases_object ON %s.dbx_plsql_test_cases(schema_name, object_name, object_type)`,
		safeSchema,
	)
	_, _ = s.db.ExecContext(ctx, idx) // index failure is non-fatal
	return nil
}

type plsqlTestCase struct {
	ID              int64   `json:"id"`
	CaseName        string  `json:"case_name"`
	SchemaName      string  `json:"schema_name"`
	ObjectName      string  `json:"object_name"`
	ObjectType      string  `json:"object_type"`
	PackageName     *string `json:"package_name,omitempty"`
	PreSQL          *string `json:"pre_sql,omitempty"`
	CallSQL         string  `json:"call_sql"`
	ParamsJSON      string  `json:"params_json"`
	ExpectedReturn  *string `json:"expected_return,omitempty"`
	Note            *string `json:"note,omitempty"`
	Creator         string  `json:"creator"`
	CreatedAt       string  `json:"created_at"`
	UpdatedAt       string  `json:"updated_at"`
}

// listTestCases returns every test case bound to (schema, object, type).
// packageName is optional — when set it further narrows down to a specific
// package member, otherwise all cases for the object (including non-member
// ones) are returned so the UI can show "all tests for this routine" even
// when no package filter applies.
func (s *server) listTestCases(schema, objectName, objectType string, packageName *string) ([]plsqlTestCase, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := s.ensureTestCasesTable(ctx); err != nil {
		return nil, err
	}
	q := fmt.Sprintf(
		`SELECT id, case_name, schema_name, object_name, object_type, package_name, pre_sql, call_sql, params_json, expected_return, note, creator, created_at, updated_at
		 FROM %s.dbx_plsql_test_cases
		 WHERE schema_name = $1 AND object_name = $2 AND object_type = $3`,
		quoteIdentifier(dbxTestCasesSchema),
	)
	args := []any{schema, objectName, objectType}
	if packageName != nil && *packageName != "" {
		q += " AND (package_name = $4 OR package_name IS NULL)"
		args = append(args, *packageName)
	}
	q += " ORDER BY updated_at DESC, id DESC"
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []plsqlTestCase{}
	for rows.Next() {
		var (
			tc      plsqlTestCase
			created sql.NullTime
			updated sql.NullTime
		)
		if err := rows.Scan(
			&tc.ID, &tc.CaseName, &tc.SchemaName, &tc.ObjectName, &tc.ObjectType,
			&tc.PackageName, &tc.PreSQL, &tc.CallSQL, &tc.ParamsJSON, &tc.ExpectedReturn,
			&tc.Note, &tc.Creator, &created, &updated,
		); err != nil {
			return nil, err
		}
		if created.Valid {
			tc.CreatedAt = created.Time.Format(time.RFC3339)
		}
		if updated.Valid {
			tc.UpdatedAt = updated.Time.Format(time.RFC3339)
		}
		result = append(result, tc)
	}
	return result, rows.Err()
}

func (s *server) getTestCase(id int) (*plsqlTestCase, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := s.ensureTestCasesTable(ctx); err != nil {
		return nil, err
	}
	var tc plsqlTestCase
	var created, updated sql.NullTime
	q := fmt.Sprintf(
		`SELECT id, case_name, schema_name, object_name, object_type, package_name, pre_sql, call_sql, params_json, expected_return, note, creator, created_at, updated_at
		 FROM %s.dbx_plsql_test_cases WHERE id = $1`,
		quoteIdentifier(dbxTestCasesSchema),
	)
	err := s.db.QueryRowContext(ctx, q, id).Scan(
		&tc.ID, &tc.CaseName, &tc.SchemaName, &tc.ObjectName, &tc.ObjectType,
		&tc.PackageName, &tc.PreSQL, &tc.CallSQL, &tc.ParamsJSON, &tc.ExpectedReturn,
		&tc.Note, &tc.Creator, &created, &updated,
	)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("test case id=%d not found", id)
	}
	if err != nil {
		return nil, err
	}
	if created.Valid {
		tc.CreatedAt = created.Time.Format(time.RFC3339)
	}
	if updated.Valid {
		tc.UpdatedAt = updated.Time.Format(time.RFC3339)
	}
	return &tc, nil
}

// saveTestCase upserts a row by id. id <= 0 means "insert"; id > 0 means
// "update". The caller decides which mode to use so we don't have to
// introspect the row to figure out intent.
func (s *server) saveTestCase(tc plsqlTestCase) (int64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := s.ensureTestCasesTable(ctx); err != nil {
		return 0, err
	}
	safeSchema := quoteIdentifier(dbxTestCasesSchema)
	if tc.ID > 0 {
		q := fmt.Sprintf(
			`UPDATE %s.dbx_plsql_test_cases SET case_name = $2, schema_name = $3, object_name = $4, object_type = $5,
			 package_name = $6, pre_sql = $7, call_sql = $8, params_json = $9, expected_return = $10, note = $11, updated_at = now()
			 WHERE id = $1`,
			safeSchema,
		)
		res, err := s.db.ExecContext(ctx, q, tc.ID, tc.CaseName, tc.SchemaName, tc.ObjectName, tc.ObjectType,
			tc.PackageName, tc.PreSQL, tc.CallSQL, tc.ParamsJSON, tc.ExpectedReturn, tc.Note)
		if err != nil {
			return 0, err
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return 0, fmt.Errorf("test case id=%d not found for update", tc.ID)
		}
		return tc.ID, nil
	}
	q := fmt.Sprintf(
		`INSERT INTO %s.dbx_plsql_test_cases (case_name, schema_name, object_name, object_type, package_name, pre_sql, call_sql, params_json, expected_return, note)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) RETURNING id`,
		safeSchema,
	)
	var id int64
	err := s.db.QueryRowContext(ctx, q, tc.CaseName, tc.SchemaName, tc.ObjectName, tc.ObjectType,
		tc.PackageName, tc.PreSQL, tc.CallSQL, tc.ParamsJSON, tc.ExpectedReturn, tc.Note).Scan(&id)
	return id, err
}

func (s *server) deleteTestCase(id int) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := s.ensureTestCasesTable(ctx); err != nil {
		return err
	}
	q := fmt.Sprintf(`DELETE FROM %s.dbx_plsql_test_cases WHERE id = $1`, quoteIdentifier(dbxTestCasesSchema))
	res, err := s.db.ExecContext(ctx, q, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("test case id=%d not found", id)
	}
	return nil
}

// runTestCase executes pre_sql (if any) and then call_sql, returning the
// command-level query result. The execution path reuses the existing
// paged_query machinery so production safety and truncation rules stay
// identical to the normal query editor.
func (s *server) runTestCase(id int) (*queryResult, error) {
	tc, err := s.getTestCase(id)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if tc.PreSQL != nil && strings.TrimSpace(*tc.PreSQL) != "" {
		if _, err := s.db.ExecContext(ctx, *tc.PreSQL); err != nil {
			return nil, fmt.Errorf("test case pre_sql failed: %w", err)
		}
	}
	if strings.TrimSpace(tc.CallSQL) == "" {
		return nil, fmt.Errorf("test case %d has empty call_sql", id)
	}
	res, err := s.executeQuery(queryOptions{SQL: tc.CallSQL, Database: ""})
	return &res, err
}
