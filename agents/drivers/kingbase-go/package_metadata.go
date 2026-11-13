package main

import (
	"database/sql"
	"fmt"
	"regexp"
	"strings"
)

// packageCatalogCandidate names one physical layout of the package catalog.
// KES V9 exposes `pg_catalog.sys_package` (a view over pg_package) with
// `pkgspecsrc`/`pkgbodysrc`; older builds expose `sys_catalog.sys_package`
// with a single `pkgsrc`. Trying both keeps package browsing version-proof.
type packageCatalogCandidate struct {
	packageTable   string
	namespaceTable string
	specColumn     string
	bodyColumn     string
}

func packageCatalogCandidates() []packageCatalogCandidate {
	return []packageCatalogCandidate{
		{"pg_catalog.sys_package", "pg_catalog.pg_namespace", "pkgspecsrc", "pkgbodysrc"},
		{"sys_catalog.sys_package", "sys_catalog.sys_namespace", "pkgspecsrc", "pkgbodysrc"},
		{"sys_catalog.sys_package", "sys_catalog.sys_namespace", "pkgsrc", ""},
		{"pg_catalog.pg_package", "pg_catalog.pg_namespace", "pkgsrc", ""},
	}
}

// listPackageObjects returns PACKAGE and/or PACKAGE_BODY object rows for the
// given schema. Kingbase exposes packages in both PG and Oracle compatibility
// modes (sys_package exists regardless of mode).
func (s *server) listPackageObjects(schema string, includeSpec bool, includeBody bool) ([]objectInfo, error) {
	if !includeSpec && !includeBody {
		return nil, nil
	}
	effective, err := s.effectiveSchema(schema)
	if err != nil {
		return nil, err
	}
	var lastErr error
	for _, candidate := range packageCatalogCandidates() {
		query := fmt.Sprintf(
			"SELECT p.pkgname FROM %s p JOIN %s n ON n.oid = p.pkgnamespace WHERE n.nspname = %s ORDER BY p.pkgname",
			candidate.packageTable, candidate.namespaceTable, quoteLiteral(effective),
		)
		rows, queryErr := s.metadataQuery(query)
		if queryErr != nil {
			lastErr = queryErr
			continue
		}
		names, scanErr := scanPackageNames(rows)
		_ = rows.Close()
		if scanErr != nil {
			lastErr = scanErr
			continue
		}
		result := make([]objectInfo, 0, len(names))
		for _, name := range names {
			if includeSpec {
				result = append(result, objectInfo{Name: name, ObjectType: "PACKAGE", Schema: effective})
			}
			if includeBody {
				// 仅在确认包体有源码时补充 PACKAGE_BODY 行（当前树只展示单个
				// PACKAGE 节点，这条分支保留给未来的显式包体视图）。
				if body, bodyErr := s.getPackageSource(effective, name, "PACKAGE_BODY"); bodyErr == nil && strings.TrimSpace(body) != "" {
					result = append(result, objectInfo{Name: name, ObjectType: "PACKAGE_BODY", Schema: effective})
				}
			}
		}
		return result, nil
	}
	return nil, lastErr
}

func scanPackageNames(rows *sql.Rows) ([]string, error) {
	names := []string{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

// getPackageSource returns the DDL source for a PACKAGE or PACKAGE_BODY.
// kind must be "PACKAGE" or "PACKAGE_BODY".
func (s *server) getPackageSource(schema, name, kind string) (string, error) {
	effective, err := s.effectiveSchema(schema)
	if err != nil {
		return "", err
	}

	kind = strings.ToUpper(strings.TrimSpace(kind))
	if kind != "PACKAGE" && kind != "PACKAGE_BODY" {
		return "", fmt.Errorf("unsupported package kind: %s", kind)
	}
	var lastErr error
	for _, candidate := range packageCatalogCandidates() {
		column := candidate.specColumn
		if kind == "PACKAGE_BODY" {
			column = candidate.bodyColumn
		}
		if column == "" {
			continue
		}
		query := fmt.Sprintf(
			"SELECT p.%s FROM %s p JOIN %s n ON n.oid = p.pkgnamespace WHERE n.nspname = %s AND p.pkgname = %s",
			column, candidate.packageTable, candidate.namespaceTable, quoteLiteral(effective), quoteLiteral(name),
		)
		var source sql.NullString
		if queryErr := s.requireDBQueryRow(query, &source); queryErr != nil {
			lastErr = queryErr
			continue
		}
		if source.Valid && strings.TrimSpace(source.String) != "" {
			return s.completePackageDdl(effective, name, kind, source.String), nil
		}
	}
	// Legacy layout: body lives in a separate sys_package_body relation.
	if kind == "PACKAGE_BODY" {
		for _, table := range []string{"sys_catalog.sys_package_body", "pg_catalog.sys_package_body"} {
			query := fmt.Sprintf(
				"SELECT b.body FROM %s b JOIN sys_catalog.sys_namespace n ON n.oid = b.pkgnamespace WHERE n.nspname = %s AND b.pkgname = %s",
				table, quoteLiteral(effective), quoteLiteral(name),
			)
			var source sql.NullString
			if queryErr := s.requireDBQueryRow(query, &source); queryErr != nil {
				lastErr = queryErr
				continue
			}
			if source.Valid && strings.TrimSpace(source.String) != "" {
				return s.completePackageDdl(effective, name, kind, source.String), nil
			}
		}
	}
	// Fallback: DBMS_METADATA.GET_DDL — available on Kingbase Oracle compat.
	dbmsFunc := "DBMS_METADATA.GET_DDL('PACKAGE', " + quoteLiteral(name) + ", " + quoteLiteral(effective) + ")"
	if kind == "PACKAGE_BODY" {
		dbmsFunc = "DBMS_METADATA.GET_DDL('PACKAGE BODY', " + quoteLiteral(name) + ", " + quoteLiteral(effective) + ")"
	}
	fallback := fmt.Sprintf("SELECT %s FROM DUAL", dbmsFunc)
	var source sql.NullString
	if err := s.requireDBQueryRow(fallback, &source); err == nil && source.Valid && strings.TrimSpace(source.String) != "" {
		return s.completePackageDdl(effective, name, kind, source.String), nil
	}
	if lastErr != nil {
		return "", fmt.Errorf("package %q.%q source is unavailable (%s): %w", effective, name, kind, lastErr)
	}
	return "", fmt.Errorf("package %q.%q source is unavailable (%s)", effective, name, kind)
}

var (
	packageCreateHeaderRe = regexp.MustCompile(`(?is)^\s*(?:(?:--[^\n]*\n)|(?:/\*.*?\*/\s*))*CREATE\s+(?:OR\s+REPLACE\s+)?(?:EDITIONABLE\s+)?PACKAGE\b`)
	packageBodyHeaderRe   = regexp.MustCompile(`(?is)^\s*(?:(?:--[^\n]*\n)|(?:/\*.*?\*/\s*))*CREATE\s+(?:OR\s+REPLACE\s+)?(?:EDITIONABLE\s+)?PACKAGE\s+BODY\b`)
	packageEndRe          = regexp.MustCompile(`(?is)\bEND(?:\s+(?:"(?:[^"]|"")*"|[A-Za-z_][A-Za-z0-9_$#]*))?\s*$`)
)

// completePackageDdl rebuilds a full CREATE OR REPLACE PACKAGE [BODY]
// statement around the source fragment KES stores in its catalog
// (`pkgspecsrc`/`pkgbodysrc`). Those columns only keep the declaration or
// implementation text: no CREATE header and no trailing semicolon. Without
// this the DDL page and the save path operate on an illegal fragment.
func (s *server) completePackageDdl(schema, name, kind, source string) string {
	trimmed := strings.TrimSpace(source)
	if trimmed == "" {
		return source
	}
	if packageDdlIsComplete(trimmed, kind) {
		return ensurePackageDdlTerminator(trimmed, name)
	}
	keyword := "PACKAGE"
	if strings.EqualFold(kind, "PACKAGE_BODY") {
		keyword = "PACKAGE BODY"
	}
	header := fmt.Sprintf("CREATE OR REPLACE %s %s.%s AS", keyword, quoteIdentifier(schema), quoteIdentifier(name))
	return header + "\n" + ensurePackageDdlTerminator(trimmed, name)
}

func packageDdlIsComplete(source, kind string) bool {
	if !packageCreateHeaderRe.MatchString(source) {
		return false
	}
	if strings.EqualFold(kind, "PACKAGE_BODY") {
		return packageBodyHeaderRe.MatchString(source)
	}
	// A PACKAGE BODY header also satisfies a plain PACKAGE match, so require the
	// header not to be followed by BODY.
	return !packageBodyHeaderRe.MatchString(source)
}

func ensurePackageDdlTerminator(source, name string) string {
	trimmed := strings.TrimSpace(source)
	if packageEndRe.MatchString(trimmed) {
		if strings.HasSuffix(trimmed, ";") {
			return trimmed
		}
		return trimmed + ";"
	}
	return trimmed + "\nEND " + name + ";"
}

// listPackageMembers returns PROCEDURE/FUNCTION members of the given package.
// Used by completion_assistant_search_v1 when request.ParentName is set.
//
// The specification source is authoritative: KES V9 has no
// sys_proc.propackageid, so its stored spec is the only place package members
// exist. Older builds that keep members in the catalog are still supported as a
// fallback; that lookup is skipped after the first failure so a missing column
// is not retried on every expansion.
func (s *server) listPackageMembers(schema, pkgName string, limit int) ([]objectInfo, error) {
	if limit <= 0 {
		limit = 100
	}
	members, err := s.listPackageMembersFromSource(schema, pkgName, limit)
	if len(members) > 0 {
		return members, nil
	}
	if s.packageMemberCatalogUnsupported {
		return members, err
	}
	fromCatalog, catalogErr := s.listPackageMembersFromCatalog(schema, pkgName, limit)
	if catalogErr != nil {
		s.packageMemberCatalogUnsupported = true
		return members, err
	}
	return fromCatalog, nil
}

func (s *server) listPackageMembersFromCatalog(schema, pkgName string, limit int) ([]objectInfo, error) {
	effective, err := s.effectiveSchema(schema)
	if err != nil {
		return nil, err
	}
	catalog, prefix := "sys_catalog", "sys"
	if s.mode.postgresCatalog {
		catalog, prefix = "pg_catalog", "pg"
	}
	// 展示签名需要带参数名（如 `create_order(p_in_json jsonb)`），因此用
	// *_get_function_arguments 而不是 identity arguments。
	argumentsFunction := prefix + "_get_function_arguments"
	if s.routineIdentityFallback > 0 || s.mode.postgresCatalog {
		argumentsFunction = "pg_get_function_arguments"
	}
	query := fmt.Sprintf(
		"SELECT f.proname, CASE f.prokind WHEN 'p' THEN 'PROCEDURE' ELSE 'FUNCTION' END, %s(f.oid), COALESCE(format_type(f.prorettype, NULL), '') FROM %s.%s_proc f JOIN %s.%s_package p ON p.oid = f.propackageid JOIN %s.%s_namespace n ON n.oid = p.pkgnamespace WHERE n.nspname = %s AND p.pkgname = %s AND f.prokind IN ('f','p') ORDER BY f.proname, f.oid LIMIT %d",
		argumentsFunction, catalog, prefix, catalog, prefix, catalog, prefix, quoteLiteral(effective), quoteLiteral(pkgName), limit,
	)
	rows, err := s.metadataQuery(query)
	if err != nil {
		// sys_proc may be missing propackageid on older versions — pg_proc fallback.
		if catalog == "sys_catalog" {
			query = fmt.Sprintf(
				"SELECT f.proname, CASE f.prokind WHEN 'p' THEN 'PROCEDURE' ELSE 'FUNCTION' END, pg_get_function_arguments(f.oid), COALESCE(format_type(f.prorettype, NULL), '') FROM pg_catalog.pg_proc f JOIN pg_catalog.pg_package p ON p.oid = f.propackageid JOIN pg_catalog.pg_namespace n ON n.oid = p.pkgnamespace WHERE n.nspname = %s AND p.pkgname = %s AND f.prokind IN ('f','p') ORDER BY f.proname, f.oid LIMIT %d",
				quoteLiteral(effective), quoteLiteral(pkgName), limit,
			)
			rows, err = s.metadataQuery(query)
		}
		if err != nil {
			return nil, err
		}
	}
	defer rows.Close()
	result := []objectInfo{}
	for rows.Next() {
		var name, kind, signature, returnType string
		if err := rows.Scan(&name, &kind, &signature, &returnType); err != nil {
			return nil, err
		}
		result = append(result, objectInfo{
			Name: name, ObjectType: kind, Schema: effective, Signature: stringPtr(signature),
			ParentSchema: stringPtr(effective), ParentName: stringPtr(pkgName),
		})
	}
	return result, rows.Err()
}

// listPackageMembersFromSource parses PROCEDURE/FUNCTION declarations from the
// package specification text. Names and parameter lists are enough for the tree
// and the debugger entry points.
func (s *server) listPackageMembersFromSource(schema, pkgName string, limit int) ([]objectInfo, error) {
	effective, err := s.effectiveSchema(schema)
	if err != nil {
		return nil, err
	}
	source, err := s.getPackageSource(effective, pkgName, "PACKAGE")
	if err != nil || strings.TrimSpace(source) == "" {
		// An unreadable spec is a browsing miss, not a hard failure.
		return []objectInfo{}, nil
	}
	declarations := parsePackageDeclarations(source, limit)
	result := make([]objectInfo, 0, len(declarations))
	for _, declaration := range declarations {
		result = append(result, objectInfo{
			Name: declaration.name, ObjectType: declaration.kind, Schema: effective,
			Signature:    stringPtr(declaration.signature),
			ParentSchema: stringPtr(effective), ParentName: stringPtr(pkgName),
		})
	}
	return result, nil
}

type packageDeclaration struct {
	name      string
	kind      string
	signature string
}

// pkgDeclHeadRe matches one package-spec declaration head anywhere in the text,
// e.g. `PROCEDURE create_order(p_id IN NUMBER)` or
// `FUNCTION calc_total(p_id IN NUMBER) RETURN NUMBER`. It intentionally does not
// anchor to the start of a statement: KES stores the spec without a
// `CREATE PACKAGE ... AS` prefix, while other builds keep the header inline
// ahead of the first declaration.
var pkgDeclHeadRe = regexp.MustCompile(`(?is)\b(PROCEDURE|FUNCTION)\s+("?[A-Za-z_][A-Za-z0-9_$#]*"?)\s*(\([^()]*(?:\([^()]*\)[^()]*)*\))?`)

func parsePackageDeclarations(source string, limit int) []packageDeclaration {
	cleaned := stripSQLComments(source)
	seen := make(map[string]struct{})
	result := make([]packageDeclaration, 0, limit)
	for _, matches := range pkgDeclHeadRe.FindAllStringSubmatch(cleaned, -1) {
		name := strings.Trim(matches[2], `"`)
		kind := strings.ToUpper(matches[1])
		signature := normalizePackageSignature(matches[3])
		key := strings.ToLower(kind) + "\x00" + strings.ToLower(name) + "\x00" + strings.ToLower(signature)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, packageDeclaration{name: name, kind: kind, signature: signature})
		if len(result) >= limit {
			break
		}
	}
	return result
}

// normalizePackageSignature turns `(p_id IN NUMBER, p_name VARCHAR2 DEFAULT 'x')`
// into `p_id NUMBER, p_name VARCHAR2`, matching the tree label style
// `create_order(p_in_json jsonb)`.
func normalizePackageSignature(raw string) string {
	inner := strings.TrimSpace(raw)
	inner = strings.TrimSuffix(strings.TrimPrefix(inner, "("), ")")
	if strings.TrimSpace(inner) == "" {
		return ""
	}
	parts := splitPLDebugArguments(inner)
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if normalized := normalizePackageArgument(part); normalized != "" {
			result = append(result, normalized)
		}
	}
	return strings.Join(result, ", ")
}

// normalizePackageArgument renders one declaration argument as `name type`.
// Package specs spell parameters Oracle-style (`p_id IN NUMBER`,
// `p_total OUT NUMERIC DEFAULT 0`); the catalog spells them PostgreSQL-style
// (`p_id integer`). Both orders are accepted so the tree label stays uniform.
func normalizePackageArgument(part string) string {
	part = strings.TrimSpace(part)
	if part == "" {
		return ""
	}
	if match := plDebugArgDefaultRe.FindStringIndex(part); match != nil {
		part = strings.TrimSpace(part[:match[0]])
	}
	// Mode first: `IN p_id integer`.
	if match := plDebugArgModeRe.FindStringSubmatch(part); match != nil {
		part = strings.TrimSpace(match[2])
	}
	// Name first: `p_id IN integer` / `p_id integer`.
	if match := plDebugArgNameRe.FindStringSubmatch(part); match != nil {
		name := match[1]
		rest := strings.TrimSpace(match[2])
		if mode := plDebugArgModeRe.FindStringSubmatch(rest); mode != nil {
			rest = strings.TrimSpace(mode[2])
		}
		part = name + " " + rest
	}
	return strings.Join(strings.Fields(part), " ")
}

// stripSQLComments removes `-- line` and `/* block */` comments while leaving
// string literals untouched, so declaration parsing cannot pick up commented-out
// procedures.
func stripSQLComments(source string) string {
	var builder strings.Builder
	builder.Grow(len(source))
	inLineComment, inBlockComment, inString := false, false, false
	for index := 0; index < len(source); index++ {
		ch := source[index]
		switch {
		case inLineComment:
			if ch == '\n' {
				inLineComment = false
				builder.WriteByte(ch)
			}
		case inBlockComment:
			if ch == '*' && index+1 < len(source) && source[index+1] == '/' {
				inBlockComment = false
				index++
			}
		case inString:
			builder.WriteByte(ch)
			if ch == '\'' {
				inString = false
			}
		default:
			switch {
			case ch == '-' && index+1 < len(source) && source[index+1] == '-':
				inLineComment = true
				index++
			case ch == '/' && index+1 < len(source) && source[index+1] == '*':
				inBlockComment = true
				index++
			case ch == '\'':
				inString = true
				builder.WriteByte(ch)
			default:
				builder.WriteByte(ch)
			}
		}
	}
	return builder.String()
}
