package main

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"time"

	"gitea.com/kingbase/gokb"
)

const (
	plDebugLogLimit       = 64 * 1024
	plDebugAnonymousOID   = 9999
	plDebugAttachTimeout  = 30 * time.Second
	plDebugNoticePrefix   = "PLDBGBREAK:"
	plDebugFunctionPrefix = "plsql_debug."
)

type plDebugManager struct {
	mu       sync.Mutex
	sessions map[string]*plDebugSession
	server   *server
}

var (
	globalPLDebugManager     *plDebugManager
	globalPLDebugManagerOnce sync.Mutex
)

func initPLDebugManager(s *server) {
	globalPLDebugManagerOnce.Lock()
	defer globalPLDebugManagerOnce.Unlock()
	if globalPLDebugManager == nil {
		globalPLDebugManager = &plDebugManager{sessions: map[string]*plDebugSession{}, server: s}
		return
	}
	if s != nil && s.db != nil {
		globalPLDebugManager.mu.Lock()
		globalPLDebugManager.server = s
		globalPLDebugManager.mu.Unlock()
	}
}

// plsql_pldbgapi is shipped with the server; the extension only needs to exist
// in the target database. It is installed on first use.
func (s *server) plDebugSupported() bool {
	if s == nil || s.db == nil {
		return false
	}
	var count int
	err := s.db.QueryRow("SELECT count(*) FROM sys_catalog.sys_available_extensions WHERE name = 'plsql_pldbgapi'").Scan(&count)
	if err == nil && count > 0 {
		return true
	}
	err = s.db.QueryRow("SELECT count(*) FROM pg_catalog.pg_available_extensions WHERE name = 'plsql_pldbgapi'").Scan(&count)
	return err == nil && count > 0
}

func (s *server) ensurePLDebugExtension(ctx context.Context, conn *sql.Conn) error {
	var installed int
	if err := conn.QueryRowContext(ctx, "SELECT count(*) FROM sys_catalog.sys_extension WHERE extname = 'plsql_pldbgapi'").Scan(&installed); err == nil && installed > 0 {
		return nil
	}
	if _, err := conn.ExecContext(ctx, "CREATE EXTENSION IF NOT EXISTS plsql_pldbgapi"); err != nil {
		return fmt.Errorf("PL/SQL debug requires the plsql_pldbgapi extension in the target database: %w", err)
	}
	return nil
}

type plDebugSession struct {
	mu             sync.Mutex
	ctrlMu         sync.Mutex
	execDB         *sql.DB
	ctrlDB         *sql.DB
	db             *sql.DB
	execConn       *sql.Conn
	ctrlConn       *sql.Conn
	execPID        int
	handle         int64
	port           int
	debugSessionID string
	targetOID      int64
	targetKind     string
	targetSQL      string
	objectType     string
	objectName     string
	schema         string
	lastStatus     string
	currentLine    int
	stopRevision   int64
	targetResult   string
	targetError    string
	targetFinished bool
	logOutput      string
	sources        map[int64]string
	cancel         context.CancelFunc
	targetDone     chan struct{}
	targetStarted  bool
	closed         bool
	portCh         chan int
}

func (m *plDebugManager) getSession(sessionID string) (*plDebugSession, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	sess, ok := m.sessions[sessionID]
	if !ok {
		return nil, fmt.Errorf("pl_debug: unknown debug session %q", sessionID)
	}
	return sess, nil
}

func (m *plDebugManager) openPLDebugSession(agentSessionID, compileID, paramsJSON, targetKind, targetSQL, targetSignature string, generation int64, targetOID int64) (string, error) {
	m.mu.Lock()
	srv := m.server
	m.mu.Unlock()
	if srv == nil || srv.db == nil {
		return "", fmt.Errorf("pl_debug: server is not connected")
	}
	sessionID := compileID
	if sessionID == "" {
		sessionID = agentSessionID
	}
	if sessionID == "" {
		sessionID = fmt.Sprintf("pldbg-%d", time.Now().UnixNano())
	}

	s := &plDebugSession{
		targetKind:     targetKind,
		targetSQL:      targetSQL,
		targetOID:      targetOID,
		lastStatus:     "starting",
		targetDone:     make(chan struct{}),
		portCh:         make(chan int, 1),
		sources:        map[int64]string{},
		debugSessionID: sessionID,
	}
	if s.targetOID == 0 {
		s.targetOID = plDebugAnonymousOID
	}

	// The caller and controller channels are two dedicated backends: pldbgapi
	// pins both for the whole trace, so a shared one-connection pool would
	// deadlock. They are opened concurrently because each connect pays the full
	// network round-trip to the server.
	type openedConn struct {
		db   *sql.DB
		conn *sql.Conn
		err  error
	}
	execCh := make(chan openedConn, 1)
	ctrlCh := make(chan openedConn, 1)
	go func() {
		db, err := openPLDebugExecDB(srv)
		if err != nil {
			execCh <- openedConn{err: err}
			return
		}
		conn, err := db.Conn(context.Background())
		if err != nil {
			_ = db.Close()
			execCh <- openedConn{err: fmt.Errorf("pl_debug: acquire exec conn: %w", err)}
			return
		}
		execCh <- openedConn{db: db, conn: conn}
	}()
	go func() {
		db, err := openPLDebugCtrlDB(srv)
		if err != nil {
			ctrlCh <- openedConn{err: err}
			return
		}
		db.SetMaxOpenConns(1)
		db.SetMaxIdleConns(1)
		conn, err := db.Conn(context.Background())
		if err != nil {
			_ = db.Close()
			ctrlCh <- openedConn{err: fmt.Errorf("pl_debug: acquire ctrl conn: %w", err)}
			return
		}
		ctrlCh <- openedConn{db: db, conn: conn}
	}()
	execOpened := <-execCh
	ctrlOpened := <-ctrlCh
	if execOpened.err != nil || ctrlOpened.err != nil {
		if execOpened.db != nil {
			_ = execOpened.db.Close()
		}
		if ctrlOpened.db != nil {
			_ = ctrlOpened.db.Close()
		}
		if execOpened.err != nil {
			return "", execOpened.err
		}
		return "", ctrlOpened.err
	}
	s.execDB, s.execConn = execOpened.db, execOpened.conn
	s.ctrlDB, s.ctrlConn, s.db = ctrlOpened.db, ctrlOpened.conn, ctrlOpened.db
	execConn, ctrlConn := s.execConn, s.ctrlConn

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.ensurePLDebugExtension(ctx, ctrlConn); err != nil {
		s.closeResources()
		return "", fmt.Errorf("pl_debug: %w", err)
	}
	if err := execConn.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&s.execPID); err != nil {
		s.execPID = 0
	}

	// Arm the target-side listener before the target statement runs; the entry
	// breakpoint is the implicit line -1 watched by plsql_oid_debug.
	if _, err := execConn.ExecContext(ctx, "SELECT "+plDebugFunctionPrefix+"plsql_oid_debug($1)", s.targetOID); err != nil {
		s.closeResources()
		return "", fmt.Errorf("pl_debug: plsql_oid_debug(%d): %w", s.targetOID, err)
	}

	m.mu.Lock()
	m.sessions[sessionID] = s
	m.mu.Unlock()

	if err := s.startTarget(); err != nil {
		m.mu.Lock()
		delete(m.sessions, sessionID)
		m.mu.Unlock()
		s.Close()
		return "", err
	}
	return sessionID, nil
}

func openPLDebugExecDB(srv *server) (*sql.DB, error) {
	noticeHandler := func(e *gokb.Error) {
		if e == nil {
			return
		}
		// The handler closes over the session via the manager registration.
		deliverPLDebugNotice(e.Message)
	}
	modes := []string{effectiveSSLMode(srv.params)}
	if modes[0] == "prefer" {
		// gokb only understands disable/require/verify-*; mimic "prefer" by
		// trying plain TCP first and only then TLS.
		modes = []string{"disable", "require"}
	}
	var lastErr error
	for _, mode := range modes {
		connector, err := gokb.NewConnector(buildDSNWithSSLMode(srv.params, mode))
		if err != nil {
			lastErr = err
			continue
		}
		db := sql.OpenDB(gokb.ConnectorWithNoticeHandler(connector, noticeHandler))
		db.SetMaxOpenConns(1)
		db.SetMaxIdleConns(1)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err = db.PingContext(ctx)
		cancel()
		if err == nil {
			return db, nil
		}
		_ = db.Close()
		lastErr = err
	}
	return nil, fmt.Errorf("pl_debug: open exec connection: %w", lastErr)
}

func openPLDebugCtrlDB(srv *server) (*sql.DB, error) {
	modes := []string{effectiveSSLMode(srv.params)}
	if modes[0] == "prefer" {
		// Match the exec connection: plain TCP first, TLS only if needed.
		modes = []string{"disable", "require"}
	}
	var lastErr error
	for _, mode := range modes {
		db, err := srv.openDatabase(srv.params, mode)
		if err != nil {
			lastErr = err
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		err = db.PingContext(ctx)
		cancel()
		if err == nil {
			return db, nil
		}
		_ = db.Close()
		lastErr = err
	}
	return nil, fmt.Errorf("pl_debug: open control connection: %w", lastErr)
}

// Notices arrive from the exec connection; they are routed to the session that
// owns the target call. Only one debug session per agent process is active.
var plDebugNoticeSink struct {
	sync.Mutex
	target *plDebugSession
}

func deliverPLDebugNotice(message string) {
	plDebugNoticeSink.Lock()
	sess := plDebugNoticeSink.target
	plDebugNoticeSink.Unlock()
	if sess == nil {
		return
	}
	sess.appendLog(message)
	if port := plDebugNoticePort(message); port > 0 {
		select {
		case sess.portCh <- port:
		default:
		}
	}
}

func plDebugNoticePort(message string) int {
	idx := strings.Index(message, plDebugNoticePrefix)
	if idx < 0 {
		return 0
	}
	var port int
	if _, err := fmt.Sscanf(strings.TrimSpace(message[idx+len(plDebugNoticePrefix):]), "%d", &port); err != nil {
		return 0
	}
	return port
}

func (s *plDebugSession) appendLog(message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.logOutput != "" {
		s.logOutput += "\n"
	}
	s.logOutput += message
	if len(s.logOutput) > plDebugLogLimit {
		s.logOutput = s.logOutput[len(s.logOutput)-plDebugLogLimit:]
	}
}

func (s *plDebugSession) startTarget() error {
	plDebugNoticeSink.Lock()
	plDebugNoticeSink.target = s
	plDebugNoticeSink.Unlock()

	s.mu.Lock()
	if s.targetStarted {
		s.mu.Unlock()
		return nil
	}
	s.targetStarted = true
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	s.cancel = cancel
	s.lastStatus = "running"
	s.mu.Unlock()

	go func() {
		defer close(s.targetDone)
		var result string
		var err error
		if strings.EqualFold(s.targetKind, "function") || strings.EqualFold(s.targetKind, "FUNCTION") {
			err = s.execConn.QueryRowContext(ctx, s.targetSQL).Scan(&result)
		} else {
			_, err = s.execConn.ExecContext(ctx, s.targetSQL)
		}
		s.recordOutcome(result, err)
	}()

	// Wait for the entry breakpoint notice emitted when the target starts.
	select {
	case port := <-s.portCh:
		s.mu.Lock()
		s.port = port
		s.mu.Unlock()
	case <-s.targetDone:
		s.mu.Lock()
		s.lastStatus = "finished"
		s.mu.Unlock()
		return nil
	case <-time.After(plDebugAttachTimeout):
		return fmt.Errorf("pl_debug: target did not reach the debugger entry breakpoint within %s", plDebugAttachTimeout)
	}

	attachCtx, attachCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer attachCancel()
	var handle int64
	if err := s.ctrlConn.QueryRowContext(attachCtx, "SELECT "+plDebugFunctionPrefix+"plsql_pldbg_attach_to_port($1)", s.port).Scan(&handle); err != nil {
		return fmt.Errorf("pl_debug: attach_to_port(%d): %w", s.port, err)
	}
	s.mu.Lock()
	s.handle = handle
	s.mu.Unlock()

	line := s.topStackLine(attachCtx)
	s.mu.Lock()
	if s.lastStatus != "finished" && s.lastStatus != "error" {
		if line > 0 {
			s.lastStatus = "paused"
			s.currentLine = line
		} else {
			s.lastStatus = "running"
		}
	}
	s.mu.Unlock()
	return nil
}

func (s *plDebugSession) recordOutcome(result string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.targetResult = result
	if err != nil {
		// A close cancels the target statement on purpose; that is not a failure.
		if !s.closed {
			s.targetError = err.Error()
			if s.lastStatus != "error" {
				s.lastStatus = "error"
			}
		}
		return
	}
	if !s.closed && s.lastStatus != "error" {
		s.lastStatus = "finished"
		s.targetFinished = true
	}
}

func (s *plDebugSession) topStackLine(ctx context.Context) int {
	rows, err := s.query(ctx, "SELECT levels, targetname, func, linenumber, args FROM "+plDebugFunctionPrefix+"plsql_pldbg_get_stack($1)", s.handle)
	if err != nil {
		return 0
	}
	line := 0
	for _, row := range rows {
		if value, ok := row["linenumber"]; ok {
			line = int64Value(value)
		}
		break
	}
	return line
}

func (s *plDebugSession) query(ctx context.Context, query string, args ...any) ([]map[string]any, error) {
	s.ctrlMu.Lock()
	defer s.ctrlMu.Unlock()
	rows, err := s.ctrlConn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	result := make([]map[string]any, 0, 8)
	for rows.Next() {
		values := make([]any, len(columns))
		ptrs := make([]any, len(columns))
		for i := range values {
			ptrs[i] = &values[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		item := make(map[string]any, len(columns))
		for i, name := range columns {
			item[name] = normalizePLDebugValue(values[i])
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func normalizePLDebugValue(value any) any {
	switch typed := value.(type) {
	case []byte:
		return string(typed)
	default:
		return value
	}
}

func int64Value(value any) int {
	switch typed := value.(type) {
	case int64:
		return int(typed)
	case int32:
		return int(typed)
	case int:
		return typed
	case float64:
		return int(typed)
	case string:
		var parsed int
		if _, err := fmt.Sscanf(typed, "%d", &parsed); err == nil {
			return parsed
		}
	}
	return 0
}

type plDebugBreakpoint struct {
	OID  int64
	Line int
}

func (s *plDebugSession) setBreakpoint(ctx context.Context, funcOID int64, line int) (int, error) {
	if s.controlBlocked() {
		return line, nil
	}
	if funcOID == 0 {
		funcOID = s.targetOID
	}
	if _, err := s.query(ctx, "SELECT "+plDebugFunctionPrefix+"plsql_pldbg_set_breakpoint($1, $2, $3)", s.handle, funcOID, line); err != nil {
		return 0, err
	}
	// pldbgapi has no numeric breakpoint id; the line is the stable handle the
	// UI uses to delete it again.
	return line, nil
}

func (s *plDebugSession) dropBreakpoint(ctx context.Context, line int) error {
	return s.dropBreakpointOn(ctx, s.targetOID, line)
}

func (s *plDebugSession) dropBreakpointOn(ctx context.Context, funcOID int64, line int) error {
	if s.controlBlocked() {
		return nil
	}
	if funcOID == 0 {
		funcOID = s.targetOID
	}
	_, err := s.query(ctx, "SELECT "+plDebugFunctionPrefix+"plsql_pldbg_drop_breakpoint($1, $2, $3)", s.handle, funcOID, line)
	return err
}

func (s *plDebugSession) listBreakpoints(ctx context.Context) ([]map[string]any, error) {
	if s.controlBlocked() {
		return nil, nil
	}
	rows, err := s.query(ctx, "SELECT func, linenumber, targetname FROM "+plDebugFunctionPrefix+"plsql_pldbg_get_breakpoints($1)", s.handle)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	owner := s.schema
	name := s.objectName
	kind := s.objectType
	if kind == "" {
		kind = s.targetKind
	}
	targetOID := s.targetOID
	s.mu.Unlock()
	result := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		line := int64Value(row["linenumber"])
		if line <= 0 {
			continue // implicit entry breakpoint
		}
		oid := int64(int64Value(row["func"]))
		if oid == 0 {
			oid = targetOID
		}
		program := plDebugProgramName(stringValue(row["targetname"]))
		if program == "" {
			program = name
		}
		refID := plDebugSourceRefID(oid)
		result = append(result, map[string]any{
			"owner":            owner,
			"name":             program,
			"line":             line,
			"kind":             strings.ToUpper(kind),
			"breakpointNumber": line,
			"registrationId":   fmt.Sprintf("%s:%d", refID, line),
			"sourceRefId":      refID,
			"sourceRevision":   1,
			"nativeFrameId":    0,
			"localActive":      true,
			"enabled":          true,
			"owned":            true,
		})
	}
	return result, nil
}

func stringValue(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case []byte:
		return string(typed)
	}
	return ""
}

func (s *plDebugSession) step(ctx context.Context, function string) (int, error) {
	if s.controlBlocked() {
		return 0, nil
	}
	s.mu.Lock()
	handle := s.handle
	s.lastStatus = "running"
	s.mu.Unlock()
	if handle == 0 {
		return 0, fmt.Errorf("pl_debug: session is not attached")
	}

	stepCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	type stepResult struct {
		line int
		err  error
	}
	resultCh := make(chan stepResult, 1)
	go func() {
		rows, err := s.query(stepCtx, "SELECT * FROM "+plDebugFunctionPrefix+"plsql_pldbg_"+function+"($1)", handle)
		if err != nil {
			resultCh <- stepResult{err: err}
			return
		}
		line := 0
		if len(rows) > 0 {
			for _, key := range []string{"linenumber", "line"} {
				if value, ok := rows[0][key]; ok {
					line = int64Value(value)
					break
				}
			}
		}
		resultCh <- stepResult{line: line}
	}()

	select {
	case result := <-resultCh:
		return result.line, result.err
	case <-s.targetDone:
		// pldbgapi keeps the control call blocked once the target has finished,
		// waiting for the next execution. Cancel it and report completion.
		cancel()
		select {
		case <-resultCh:
		case <-time.After(5 * time.Second):
		}
		return 0, nil
	}
}

func (s *plDebugSession) continueExec(ctx context.Context) (int, error) {
	return s.step(ctx, "continue")
}
func (s *plDebugSession) stepOver(ctx context.Context) (int, error) { return s.step(ctx, "step_over") }
func (s *plDebugSession) stepInto(ctx context.Context) (int, error) { return s.step(ctx, "step_into") }
func (s *plDebugSession) stepOut(ctx context.Context) (int, error)  { return s.step(ctx, "step_out") }

func (s *plDebugSession) getStack(ctx context.Context) ([]map[string]any, error) {
	if s.controlBlocked() {
		return nil, nil
	}
	rows, err := s.query(ctx, "SELECT levels, targetname, func, linenumber, args FROM "+plDebugFunctionPrefix+"plsql_pldbg_get_stack($1)", s.handle)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	stopRevision := s.stopRevision
	owner := s.schema
	kind := strings.ToUpper(s.objectType)
	if kind == "" {
		kind = strings.ToUpper(s.targetKind)
	}
	targetOID := s.targetOID
	s.mu.Unlock()
	if kind == "" {
		kind = "PROCEDURE"
	}
	result := make([]map[string]any, 0, len(rows))
	for index, row := range rows {
		oid := int64(int64Value(row["func"]))
		if oid == 0 {
			oid = targetOID
		}
		line := int64Value(row["linenumber"])
		name := plDebugProgramName(stringValue(row["targetname"]))
		source := s.sourceFor(ctx, oid)
		result = append(result, map[string]any{
			"frameIndex":    index,
			"nativeFrameId": index,
			"stackDepth":    int64Value(row["levels"]),
			"line":          line,
			"identityState": "confirmed",
			"sourceState":   "resolved",
			"program":       name,
			"args":          stringValue(row["args"]),
			"sourceRef": map[string]any{
				"id":             plDebugSourceRefID(oid),
				"sourceRevision": 1,
				"stopRevision":   stopRevision,
				"nativeFrameId":  index,
				"lineBasis":      "source",
				"identityState":  "confirmed",
				"evidenceKind":   "native",
				"objectType":     kind,
				"source":         source,
				"owner":          owner,
				"name":           name,
				"objectId":       oid,
			},
		})
	}
	if len(rows) > 0 {
		if line := int64Value(rows[0]["linenumber"]); line > 0 {
			s.mu.Lock()
			s.currentLine = line
			s.mu.Unlock()
		}
	}
	return result, nil
}

// plDebugProgramName strips the argument signature pldbgapi appends to a
// program name, so the name matches the object the editor knows.
func plDebugProgramName(targetName string) string {
	if idx := strings.Index(targetName, "("); idx > 0 {
		return strings.TrimSpace(targetName[:idx])
	}
	return strings.TrimSpace(targetName)
}

func plDebugSourceRefID(oid int64) string {
	return fmt.Sprintf("kes:%d", oid)
}

// sourceFor returns the text whose line numbers pldbgapi reports. It is read
// once per object through the debug handle and cached for later frames.
func (s *plDebugSession) sourceFor(ctx context.Context, oid int64) string {
	s.mu.Lock()
	if text, ok := s.sources[oid]; ok {
		s.mu.Unlock()
		return text
	}
	targetSQL := s.targetSQL
	s.mu.Unlock()
	text := ""
	if oid != plDebugAnonymousOID {
		rows, err := s.query(ctx, "SELECT "+plDebugFunctionPrefix+"plsql_pldbg_get_source($1, $2) AS source", s.handle, oid)
		if err == nil && len(rows) > 0 {
			text = stringValue(rows[0]["source"])
		}
		// The catalog definition is a faithful fallback when the extension cannot
		// hand out source (e.g. a compiled object without debug info).
		if text == "" {
			rows, err = s.query(ctx, "SELECT pg_get_functiondef($1) AS source", oid)
			if err == nil && len(rows) > 0 {
				text = stringValue(rows[0]["source"])
			}
		}
	}
	if text == "" && strings.TrimSpace(targetSQL) != "" {
		text = strings.TrimSuffix(strings.TrimSpace(targetSQL), ";")
	}
	if text != "" {
		s.mu.Lock()
		if s.sources == nil {
			s.sources = map[int64]string{}
		}
		s.sources[oid] = text
		s.mu.Unlock()
	}
	return text
}

func (s *plDebugSession) getVariables(ctx context.Context, frame int) ([]map[string]any, error) {
	if s.controlBlocked() {
		return nil, nil
	}
	if _, err := s.query(ctx, "SELECT "+plDebugFunctionPrefix+"plsql_pldbg_select_frame($1, $2)", s.handle, frame); err != nil {
		return nil, err
	}
	rows, err := s.query(ctx, "SELECT name, varclass, linenumber, isunique, isconst, isnotnull, dtype, value FROM "+plDebugFunctionPrefix+"plsql_pldbg_get_variables($1)", s.handle)
	if err != nil {
		return nil, err
	}
	result := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		name := stringValue(row["name"])
		if name == "" {
			continue
		}
		value := stringValue(row["value"])
		state := "value"
		if value == "" {
			if boolValue(row["isnotnull"]) {
				state = "empty"
			} else {
				state = "null"
			}
		}
		result = append(result, map[string]any{
			"name":  name,
			"state": state,
			"value": value,
			"type":  stringValue(row["dtype"]),
			"class": stringValue(row["varclass"]),
		})
	}
	return result, nil
}

func boolValue(value any) bool {
	switch typed := value.(type) {
	case bool:
		return typed
	case int64:
		return typed != 0
	case int:
		return typed != 0
	case float64:
		return typed != 0
	case []byte:
		if len(typed) == 1 && (typed[0] == 0 || typed[0] == 1) {
			return typed[0] == 1
		}
		return boolValue(string(typed))
	case string:
		switch strings.ToLower(strings.TrimSpace(typed)) {
		case "t", "true", "y", "yes", "1":
			return true
		}
	}
	return false
}

func (s *plDebugSession) getVar(ctx context.Context, name string, frame int) (string, error) {
	rows, err := s.getVariables(ctx, frame)
	if err != nil {
		return "", err
	}
	for _, row := range rows {
		if strings.EqualFold(stringValue(row["name"]), name) {
			return stringValue(row["value"]), nil
		}
	}
	return "", fmt.Errorf("pl_debug: variable %q is not in scope", name)
}

func (s *plDebugSession) setVar(ctx context.Context, name, value string, frame int) error {
	_, err := s.query(ctx, "SELECT "+plDebugFunctionPrefix+"plsql_pldbg_deposit_value($1, $2, $3, $4)", s.handle, name, frame, value)
	return err
}

func (s *plDebugSession) abort(ctx context.Context) error {
	s.mu.Lock()
	handle := s.handle
	status := s.lastStatus
	s.mu.Unlock()
	if handle == 0 || status != "paused" {
		return nil
	}
	abortCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err := s.query(abortCtx, "SELECT "+plDebugFunctionPrefix+"plsql_pldbg_abort_target($1)", handle)
	return err
}

func (s *plDebugSession) isPaused() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastStatus == "paused"
}

// identity reports the debugged program and its routine OID, used to describe
// breakpoints the desktop lists for the editor gutter.
func (s *plDebugSession) identity() (owner, name, kind string, targetOID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	kind = strings.ToUpper(s.objectType)
	if kind == "" {
		kind = strings.ToUpper(s.targetKind)
	}
	return s.schema, s.objectName, kind, s.targetOID
}

// controlBlocked reports whether pldbgapi would keep a control call blocked
// forever: once the target has finished (or the session is closed) it waits for
// the next execution of the object instead of returning.
func (s *plDebugSession) controlBlocked() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch s.lastStatus {
	case "finished", "error", "closed", "starting":
		return true
	}
	return s.handle == 0
}

// snapshot renders the status view the desktop store consumes. It is the single
// source for start/status/control/close payloads so the toolbar sees a coherent
// lifecycle (starting → paused/running → ended, then released).
func (s *plDebugSession) snapshot() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.lastStatus
	ended := false
	switch state {
	case "finished":
		state = "ended"
		ended = true
	case "error":
		state = "ended"
		ended = true
	case "closed":
		ended = true
	}
	if state == "starting" {
		state = "starting"
	}
	outcome := "pending"
	if ended {
		outcome = "unknown"
		switch {
		case s.targetError != "":
			outcome = "failed"
		case s.targetFinished:
			outcome = "succeeded"
		}
	}
	release := "open"
	if ended {
		release = "released"
	}
	program := s.objectName
	if program == "" {
		program = plDebugProgramName(s.targetKind)
	}
	snap := map[string]any{
		"debugId":            s.debugSessionID,
		"nativeDebugId":      s.debugSessionID,
		"state":              state,
		"line":               s.currentLine,
		"stopRevision":       s.stopRevision,
		"startState":         "succeeded",
		"targetDispatched":   s.targetStarted,
		"targetReturned":     ended,
		"operationBusy":      state == "running",
		"executionStopped":   ended,
		"executionOutcome":   outcome,
		"releaseState":       release,
		"transactionOutcome": "not_started",
		"transactionScope":   "uncommitted_work_only",
		"sourceMappingState": "verified",
		"frameMappingState":  "verified",
		"program":            program,
		"logAvailable":       s.logOutput != "",
	}
	if s.targetError != "" {
		snap["startError"] = s.targetError
		snap["message"] = s.targetError
	}
	return snap
}

func (s *plDebugSession) closeResources() {
	if s.execConn != nil {
		_ = s.execConn.Close()
	}
	if s.ctrlConn != nil {
		_ = s.ctrlConn.Close()
	}
	if s.execDB != nil {
		_ = s.execDB.Close()
	}
	if s.ctrlDB != nil {
		_ = s.ctrlDB.Close()
	}
	if s.cancel != nil {
		s.cancel()
	}
}

func (s *plDebugSession) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	status := s.lastStatus
	handle := s.handle
	execPID := s.execPID
	s.lastStatus = "closed"
	s.mu.Unlock()

	// Terminating the target backend first releases a blocked CALL (and with it
	// any pldbgapi control call waiting on the controller connection); aborting
	// first could block for the whole cleanup budget.
	if execPID > 0 && globalPLDebugManager != nil {
		globalPLDebugManager.mu.Lock()
		managerServer := globalPLDebugManager.server
		globalPLDebugManager.mu.Unlock()
		if managerServer != nil && managerServer.db != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			_, _ = managerServer.db.ExecContext(ctx, "SELECT pg_terminate_backend($1)", execPID)
			cancel()
		}
	}
	if handle != 0 && status == "paused" {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, _ = s.query(ctx, "SELECT "+plDebugFunctionPrefix+"plsql_pldbg_abort_target($1)", handle)
		cancel()
	}
	s.closeResources()
	plDebugNoticeSink.Lock()
	if plDebugNoticeSink.target == s {
		plDebugNoticeSink.target = nil
	}
	plDebugNoticeSink.Unlock()
	return nil
}

func (m *plDebugManager) closePLDebugSession(sessionID string) error {
	m.mu.Lock()
	sess, ok := m.sessions[sessionID]
	if ok {
		delete(m.sessions, sessionID)
	}
	m.mu.Unlock()
	if !ok {
		return nil
	}
	return sess.Close()
}

// releaseSession tears the debug resources down but keeps the session record so
// status/log/results stay readable while the desktop confirms the exit. The
// record disappears with the agent process.
func (m *plDebugManager) releaseSession(sessionID string) *plDebugSession {
	m.mu.Lock()
	sess := m.sessions[sessionID]
	m.mu.Unlock()
	if sess == nil {
		return nil
	}
	_ = sess.Close()
	return sess
}

func (m *plDebugManager) forgetSession(sessionID string) *plDebugSession {
	m.mu.Lock()
	sess := m.sessions[sessionID]
	delete(m.sessions, sessionID)
	m.mu.Unlock()
	return sess
}
