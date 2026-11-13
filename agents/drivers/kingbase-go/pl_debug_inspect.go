package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// plDebugInspectTarget mirrors the launch target dbx-core forwards for
// pl_debug_inspect. Keys are camelCase because the desktop request serializes
// PlDebugInspectTarget directly.
type plDebugInspectTarget struct {
	Schema       string `json:"schema"`
	ObjectType   string `json:"objectType"`
	ObjectName   string `json:"objectName"`
	PackageName  string `json:"packageName"`
	MetadataPrep string `json:"metadataPreparation"`
}

type plDebugInspectArgument struct {
	Name        *string `json:"name"`
	Position    int     `json:"position"`
	Sequence    int     `json:"sequence"`
	DataLevel   int     `json:"dataLevel"`
	Mode        string  `json:"mode"`
	Type        string  `json:"type"`
	TypeOwner   *string `json:"typeOwner"`
	TypeName    *string `json:"typeName"`
	TypeSubname *string `json:"typeSubname"`
	Defaulted   *string `json:"defaulted"`
}

type plDebugInspectSignature struct {
	Schema       string                   `json:"schema"`
	ObjectName   string                   `json:"objectName"`
	PackageName  *string                  `json:"packageName"`
	ObjectType   string                   `json:"objectType"`
	ObjectID     string                   `json:"objectId"`
	SubprogramID int64                    `json:"subprogramId"`
	Status       string                   `json:"status"`
	Arguments    []plDebugInspectArgument `json:"arguments"`
}

// plDebugSourceField is one JSON field the routine body actually reads from a
// json/jsonb parameter. The launch dialog turns exactly these fields into test
// values, so parameters never contain keys the object cannot consume.
type plDebugSourceField struct {
	Parameter string `json:"parameter"`
	Path      string `json:"path"`
	Line      int    `json:"line"`
	Evidence  string `json:"evidence"`
}

type plDebugSourceAnalysis struct {
	Available  bool                 `json:"available"`
	LineCount  int                  `json:"lineCount"`
	JSONFields []plDebugSourceField `json:"jsonFields"`
	Message    string               `json:"message,omitempty"`
}

type plDebugInspectCandidate struct {
	Signature          plDebugInspectSignature  `json:"signature"`
	Parameters         []plDebugInspectArgument `json:"parameters"`
	ReturnValue        *plDebugInspectArgument  `json:"returnValue"`
	ParameterTransport string                   `json:"parameterTransport"`
	SourceAnalysis     *plDebugSourceAnalysis   `json:"sourceAnalysis,omitempty"`
}

type plDebugRoutineRow struct {
	OID        int64
	Name       string
	Kind       string
	Arguments  string
	ReturnType string
	Source     string
}

// plDebugInspect answers the launch dialog with the routines that match the
// requested target. The dialog needs a signature plus the argument list before
// it can build the CALL; the source analysis then drives parameter generation.
func (r *runtimeServer) plDebugInspect(srv *server, params map[string]json.RawMessage) (any, bool, error) {
	target := plDebugInspectTarget{}
	if raw, ok := params["target"]; ok && strings.TrimSpace(string(raw)) != "null" {
		if err := json.Unmarshal(raw, &target); err != nil {
			return nil, false, fmt.Errorf("pl_debug_inspect: invalid target: %w", err)
		}
	}
	result := map[string]any{
		"ok":     true,
		"target": json.RawMessage(params["target"]),
		"capabilities": map[string]any{
			"engine":          "kingbase",
			"supported":       true,
			"canAttemptStart": true,
			"startVerified":   false,
			"status":          "supported",
			"warnings":        []string{},
		},
	}
	if strings.TrimSpace(target.ObjectName) == "" {
		return result, false, nil
	}
	if strings.EqualFold(target.MetadataPrep, "type_only") {
		// Kingbase resolves argument types from the catalog directly, so typed
		// metadata preparation is a no-op rather than an error.
		return result, false, nil
	}
	rows, err := srv.plDebugRoutineRows(target.Schema, target.PackageName, target.ObjectName)
	if target.PackageName != "" && (err != nil || len(rows) == 0) {
		// KES V9 keeps package members only in sys_package.pkgspecsrc: sys_proc
		// has no propackageid and pkgbodyfuncs stays empty. Fall back to the
		// package spec so package members stay debuggable instead of appearing
		// as "not_visible".
		fallback, fallbackErr := srv.plDebugPackageMemberRows(target.Schema, target.PackageName, target.ObjectName)
		switch {
		case fallbackErr == nil && len(fallback) > 0:
			rows, err = fallback, nil
		case err == nil:
			err = fallbackErr
		}
	}
	if err != nil {
		return map[string]any{
			"ok":           true,
			"target":       json.RawMessage(params["target"]),
			"capabilities": result["capabilities"],
			"targetPreview": map[string]any{
				"schema":     target.Schema,
				"state":      "not_visible",
				"message":    fmt.Sprintf("无法读取例程元数据：%v", err),
				"candidates": []any{},
			},
		}, false, nil
	}
	candidates := make([]plDebugInspectCandidate, 0, len(rows))
	for _, row := range rows {
		if target.ObjectType != "" && !strings.EqualFold(row.Kind, target.ObjectType) {
			continue
		}
		arguments := parsePLDebugArguments(row.Arguments)
		signature := plDebugInspectSignature{
			Schema:       target.Schema,
			ObjectName:   row.Name,
			ObjectType:   row.Kind,
			ObjectID:     strconv.FormatInt(row.OID, 10),
			SubprogramID: 0,
			Status:       "VALID",
			Arguments:    arguments,
		}
		if target.PackageName != "" {
			pkg := target.PackageName
			signature.PackageName = &pkg
		}
		var returnValue *plDebugInspectArgument
		if strings.EqualFold(row.Kind, "FUNCTION") {
			resultType := strings.TrimSpace(row.ReturnType)
			if resultType != "" && !strings.EqualFold(resultType, "void") && !strings.EqualFold(resultType, "trigger") {
				returnValue = &plDebugInspectArgument{Position: 0, Sequence: 0, DataLevel: 0, Mode: "OUT", Type: resultType}
			}
		}
		source := row.Source
		sourceMessage := ""
		if strings.TrimSpace(source) == "" && target.PackageName != "" {
			// Package members on some builds keep the body only in the package
			// body catalog row. The whole body is a safe superset for field
			// detection because analysis is filtered by parameter name.
			if body, bodyErr := srv.getPackageSource(target.Schema, target.PackageName, "PACKAGE_BODY"); bodyErr == nil {
				source = body
			} else {
				sourceMessage = fmt.Sprintf("无法读取包体源码：%v", bodyErr)
			}
		}
		candidates = append(candidates, plDebugInspectCandidate{
			Signature:          signature,
			Parameters:         arguments,
			ReturnValue:        returnValue,
			ParameterTransport: "implemented",
			SourceAnalysis:     analyzePLDebugSource(source, arguments, sourceMessage),
		})
	}
	preview := map[string]any{
		"schema":     target.Schema,
		"state":      "available",
		"candidates": candidates,
	}
	if len(candidates) == 0 {
		preview["state"] = "not_visible"
		preview["message"] = fmt.Sprintf("未找到可调试的例程 %s.%s", target.Schema, target.ObjectName)
	}
	result["targetPreview"] = preview
	lineCount := 0
	for _, row := range rows {
		lineCount += len(strings.Split(strings.ReplaceAll(row.Source, "\r\n", "\n"), "\n"))
	}
	result["source"] = map[string]any{"lineOffset": 0, "lineCount": lineCount}
	return result, false, nil
}

// plDebugRoutineRows reads the routines that match name/package. The query is
// tried twice so that older Kingbase builds without prokind or
// sys_get_function_arguments still produce candidates.
func (s *server) plDebugRoutineRows(schema, packageName, objectName string) ([]plDebugRoutineRow, error) {
	effective, err := s.effectiveSchema(schema)
	if err != nil {
		return nil, err
	}
	catalog, prefix := s.routineCatalog()
	packageFilter := "(p.propackageid IS NULL OR p.propackageid = 0)"
	if packageName != "" {
		packageFilter = fmt.Sprintf(
			"p.propackageid = (SELECT pk.oid FROM %s.%s_package pk WHERE pk.pkgname = %s AND pk.pkgnamespace = n.oid)",
			catalog, prefix, quoteLiteral(packageName),
		)
	}
	build := func(kindExpression, argumentsFunction, filter string) string {
		return fmt.Sprintf(
			`SELECT p.oid::bigint, p.proname, %s, COALESCE(%s(p.oid), ''), COALESCE(format_type(p.prorettype, NULL), ''), COALESCE(p.prosrc, '')
FROM %s.%s_proc p JOIN %s.%s_namespace n ON n.oid = p.pronamespace
WHERE n.nspname = %s AND p.proname = %s AND %s AND %s
ORDER BY p.oid`,
			kindExpression, argumentsFunction,
			catalog, prefix, catalog, prefix,
			quoteLiteral(effective), quoteLiteral(objectName), filter, packageFilter,
		)
	}
	attempts := []string{
		build("CASE WHEN p.prokind = 'p' THEN 'PROCEDURE' ELSE 'FUNCTION' END", prefix+"_get_function_arguments", "p.prokind IN ('p','f')"),
		build("CASE WHEN p.prorettype = 2278 THEN 'PROCEDURE' ELSE 'FUNCTION' END", "pg_get_function_arguments", "true"),
	}
	var lastErr error
	for _, query := range attempts {
		rows, queryErr := s.metadataQuery(query)
		if queryErr != nil {
			lastErr = queryErr
			continue
		}
		result, scanErr := scanPLDebugRoutineRows(rows)
		_ = rows.Close()
		if scanErr != nil {
			lastErr = scanErr
			continue
		}
		return result, nil
	}
	return nil, lastErr
}

// plDebugPackageMemberRows resolves package members from the stored package
// spec. KES V9 does not link members to sys_proc/propackageid, so the spec is
// the only catalog-independent source of member names, modes and signatures.
func (s *server) plDebugPackageMemberRows(schema, packageName, objectName string) ([]plDebugRoutineRow, error) {
	effective, err := s.effectiveSchema(schema)
	if err != nil {
		return nil, err
	}
	spec, err := s.getPackageSource(effective, packageName, "PACKAGE")
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(spec) == "" {
		return nil, nil
	}
	// The whole body is the source-analysis superset; analysis filters fields by
	// parameter name, so unrelated members cannot leak values.
	body, _ := s.getPackageSource(effective, packageName, "PACKAGE_BODY")
	return packageMemberDebugRows(spec, body, objectName), nil
}

// packageMemberDebugRows selects one member (all overloads) from a package spec.
func packageMemberDebugRows(spec, body, objectName string) []plDebugRoutineRow {
	cleaned := stripSQLComments(spec)
	rows := []plDebugRoutineRow{}
	for _, match := range pkgDeclHeadRe.FindAllStringSubmatch(cleaned, -1) {
		name := strings.Trim(match[2], `"`)
		if !strings.EqualFold(name, objectName) {
			continue
		}
		kind := strings.ToUpper(match[1])
		returnType := ""
		if kind == "FUNCTION" {
			returnType = packageFunctionReturnType(cleaned, name)
		}
		rows = append(rows, plDebugRoutineRow{
			Name:       name,
			Kind:       kind,
			Arguments:  normalizePackageDebugArguments(match[3]),
			ReturnType: returnType,
			Source:     body,
		})
	}
	return rows
}

// normalizePackageDebugArguments converts a spec parameter list into the shape
// pg_get_function_arguments uses while preserving modes:
// `(p_line OUT text, numlines IN OUT integer)` -> `OUT p_line text, IN OUT numlines integer`.
func normalizePackageDebugArguments(raw string) string {
	inner := strings.TrimSpace(raw)
	inner = strings.TrimSuffix(strings.TrimPrefix(inner, "("), ")")
	if strings.TrimSpace(inner) == "" {
		return ""
	}
	result := make([]string, 0, 4)
	for _, part := range splitPLDebugArguments(inner) {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if match := plDebugArgDefaultRe.FindStringIndex(part); match != nil {
			part = strings.TrimSpace(part[:match[0]])
		}
		mode := ""
		if match := plDebugArgModeRe.FindStringSubmatch(part); match != nil {
			mode = strings.ToUpper(strings.Join(strings.Fields(match[1]), " "))
			part = strings.TrimSpace(match[2])
		}
		if match := plDebugArgNameRe.FindStringSubmatch(part); match != nil {
			name := match[1]
			rest := strings.TrimSpace(match[2])
			if restMode := plDebugArgModeRe.FindStringSubmatch(rest); restMode != nil {
				mode = strings.ToUpper(strings.Join(strings.Fields(restMode[1]), " "))
				rest = strings.TrimSpace(restMode[2])
			}
			part = name + " " + rest
		}
		if mode != "" && mode != "IN" {
			part = mode + " " + part
		}
		result = append(result, part)
	}
	return strings.Join(result, ", ")
}

func packageFunctionReturnType(source, name string) string {
	pattern := regexp.MustCompile(`(?is)\bFUNCTION\s+"?` + regexp.QuoteMeta(name) + `"?\s*\([^;]*?\)\s*RETURN\s+([A-Za-z0-9_.$#%]+)`)
	match := pattern.FindStringSubmatch(source)
	if match == nil {
		return ""
	}
	return strings.TrimSpace(match[1])
}

func scanPLDebugRoutineRows(rows *sql.Rows) ([]plDebugRoutineRow, error) {
	result := []plDebugRoutineRow{}
	for rows.Next() {
		var row plDebugRoutineRow
		if err := rows.Scan(&row.OID, &row.Name, &row.Kind, &row.Arguments, &row.ReturnType, &row.Source); err != nil {
			return nil, err
		}
		result = append(result, row)
	}
	return result, rows.Err()
}

var plDebugArgModeRe = regexp.MustCompile(`(?is)^(INOUT|IN\s+OUT|IN|OUT|VARIADIC)\s+(.+)$`)
var plDebugArgNameRe = regexp.MustCompile(`^"?([A-Za-z_][A-Za-z0-9_$#]*)"?\s+(.+)$`)
var plDebugArgDefaultRe = regexp.MustCompile(`(?is)\s+(?:DEFAULT|:=)\s+`)

// parsePLDebugArguments turns `pg_get_function_arguments` output such as
// `p_id integer, p_in_json jsonb, OUT p_total numeric` into dialog arguments.
func parsePLDebugArguments(raw string) []plDebugInspectArgument {
	segments := splitPLDebugArguments(raw)
	result := make([]plDebugInspectArgument, 0, len(segments))
	for index, segment := range segments {
		segment = strings.TrimSpace(segment)
		if segment == "" {
			continue
		}
		mode := "IN"
		if match := plDebugArgModeRe.FindStringSubmatch(segment); match != nil {
			switch strings.ToUpper(strings.Join(strings.Fields(match[1]), " ")) {
			case "OUT":
				mode = "OUT"
			case "INOUT", "IN OUT":
				mode = "IN OUT"
			default:
				mode = "IN"
			}
			segment = strings.TrimSpace(match[2])
		}
		defaulted := false
		if match := plDebugArgDefaultRe.FindStringIndex(segment); match != nil {
			defaulted = true
			segment = strings.TrimSpace(segment[:match[0]])
		}
		name := ""
		typeName := segment
		if match := plDebugArgNameRe.FindStringSubmatch(segment); match != nil {
			name = match[1]
			typeName = strings.TrimSpace(match[2])
		}
		argument := plDebugInspectArgument{
			Position:  index + 1,
			Sequence:  index + 1,
			DataLevel: 0,
			Mode:      mode,
			Type:      typeName,
		}
		if name != "" {
			argument.Name = stringPtr(name)
		}
		if defaulted {
			yes := "Y"
			argument.Defaulted = &yes
		}
		result = append(result, argument)
	}
	return result
}

func splitPLDebugArguments(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var result []string
	depth := 0
	start := 0
	inString := false
	for index := 0; index < len(raw); index++ {
		switch raw[index] {
		case '\'':
			if !inString {
				inString = true
			} else if index+1 >= len(raw) || raw[index+1] != '\'' {
				inString = false
			} else {
				index++
			}
		case '(':
			if !inString {
				depth++
			}
		case ')':
			if !inString && depth > 0 {
				depth--
			}
		case ',':
			if !inString && depth == 0 {
				result = append(result, raw[start:index])
				start = index + 1
			}
		}
	}
	return append(result, raw[start:])
}

var plDebugJSONPathRe = regexp.MustCompile(`(?i)\s*(->>|->|#>>|#>)\s*'([^']*)'`)

// analyzePLDebugSource scans the routine body and reports every JSON path it
// reads from a json/jsonb parameter. Field lists stay empty for parameters the
// body never touches.
func analyzePLDebugSource(source string, arguments []plDebugInspectArgument, message string) *plDebugSourceAnalysis {
	analysis := &plDebugSourceAnalysis{Available: strings.TrimSpace(source) != "", JSONFields: []plDebugSourceField{}}
	if !analysis.Available {
		analysis.Message = message
		if analysis.Message == "" {
			analysis.Message = "无法读取例程源码，调试参数需要手动填写"
		}
		return analysis
	}
	normalized := strings.ReplaceAll(source, "\r\n", "\n")
	lines := strings.Split(normalized, "\n")
	analysis.LineCount = len(lines)
	jsonParams := map[string]string{}
	for _, argument := range arguments {
		if argument.Name == nil || !isJSONDebugType(argument.Type) {
			continue
		}
		jsonParams[strings.ToLower(*argument.Name)] = *argument.Name
	}
	if len(jsonParams) == 0 {
		return analysis
	}
	seen := map[string]struct{}{}
	for lineIndex, line := range lines {
		lower := strings.ToLower(line)
		for key, original := range jsonParams {
			offset := 0
			for {
				position := strings.Index(lower[offset:], key)
				if position < 0 {
					break
				}
				position += offset
				offset = position + len(key)
				if position > 0 && isDebugIdentifierByte(lower[position-1]) {
					continue
				}
				if offset < len(lower) && isDebugIdentifierByte(lower[offset]) {
					continue
				}
				paths := plDebugJSONPaths(lower[offset:])
				// Chained access starts right after the parameter name.
				if len(paths) > 1 {
					combined := strings.Join(paths, ".")
					recordPLDebugField(analysis, seen, original, combined, lineIndex+1, line)
				} else if len(paths) == 1 {
					recordPLDebugField(analysis, seen, original, paths[0], lineIndex+1, line)
				}
			}
		}
	}
	return analysis
}

func recordPLDebugField(analysis *plDebugSourceAnalysis, seen map[string]struct{}, parameter, path string, line int, evidence string) {
	path = strings.TrimSpace(path)
	if path == "" {
		return
	}
	key := strings.ToLower(parameter) + "\x00" + strings.ToLower(path)
	if _, ok := seen[key]; ok {
		return
	}
	seen[key] = struct{}{}
	analysis.JSONFields = append(analysis.JSONFields, plDebugSourceField{
		Parameter: parameter,
		Path:      path,
		Line:      line,
		Evidence:  strings.TrimSpace(evidence),
	})
}

// plDebugJSONPaths reads the operators immediately following a parameter name.
// It stops at the first non-operator so only chained accesses are collected.
func plDebugJSONPaths(rest string) []string {
	var paths []string
	for {
		match := plDebugJSONPathRe.FindStringSubmatchIndex(rest)
		if match == nil || match[0] != 0 {
			break
		}
		operator := strings.ToLower(rest[match[2]:match[3]])
		key := strings.TrimSpace(rest[match[4]:match[5]])
		rest = rest[match[1]:]
		if strings.Contains(operator, "#") {
			key = strings.Trim(strings.TrimSpace(key), "{}")
			for _, part := range strings.Split(key, ",") {
				part = strings.Trim(strings.TrimSpace(part), `"`)
				if part != "" {
					paths = append(paths, part)
				}
			}
			continue
		}
		if key != "" {
			paths = append(paths, key)
		}
	}
	return paths
}

func isJSONDebugType(typeName string) bool {
	upper := strings.ToUpper(strings.TrimSpace(typeName))
	return strings.HasPrefix(upper, "JSON") || upper == "JSONB"
}

func isDebugIdentifierByte(ch byte) bool {
	return ch == '_' || ch == '$' || ch == '#' || (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9')
}
