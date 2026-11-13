package main

import (
	"strings"
	"testing"
)

// Sample taken from a KES V9 `sys.dbms_output` package specification: the
// stored text has no `CREATE PACKAGE ... AS` header and uses Oracle-style
// parameter mode keywords.
const packageSpecSample = `
    isenabled       boolean := false;
    hasgetmsg       boolean := false;
    type chararr is table of varchar2(32767) index by int;
    /*内部函数*/
    procedure clear_cache();

    /*send_print(linenum int) 不对用户可见*/
    function  send_print(linenum int) returns text;

    /*外部接口*/
    procedure enable(buffer_size in integer default 20000);
    procedure disable();
    procedure put(item in text);
    procedure put_line(item in text);
    procedure get_line(line out text, status out integer);
    procedure get_lines(lines out chararr, numlines in out integer);
    procedure get_lines(lines out dbmsoutput_linesarray, numlines in out integer);
end dbms_output
`

func TestParsePackageDeclarationsFromOracleSpec(t *testing.T) {
	declarations := parsePackageDeclarations(packageSpecSample, 100)
	got := make(map[string]string, len(declarations))
	for _, declaration := range declarations {
		got[declaration.name] = declaration.kind + "(" + declaration.signature + ")"
	}
	want := map[string]string{
		"clear_cache": "PROCEDURE()",
		"send_print":  "FUNCTION(linenum int)",
		"enable":      "PROCEDURE(buffer_size integer)",
		"disable":     "PROCEDURE()",
		"put":         "PROCEDURE(item text)",
		"put_line":    "PROCEDURE(item text)",
		"get_line":    "PROCEDURE(line text, status integer)",
	}
	for name, expected := range want {
		if got[name] != expected {
			t.Errorf("%s = %q, want %q", name, got[name], expected)
		}
	}
	// Overloads keep distinct parameter lists instead of collapsing into one row.
	overloads := map[string]bool{}
	for _, declaration := range declarations {
		if declaration.name != "get_lines" {
			continue
		}
		if !strings.Contains(declaration.signature, "numlines integer") {
			t.Errorf("get_lines signature = %q", declaration.signature)
		}
		overloads[declaration.signature] = true
	}
	if len(overloads) != 2 {
		t.Errorf("get_lines overloads = %d, want 2", len(overloads))
	}
}

func TestParsePackageDeclarationsSkipsCommentedOutMembers(t *testing.T) {
	source := `
-- procedure dropped_member(p_id integer);
/* procedure old_member(); */
procedure kept(p_id in integer);
end demo
`
	declarations := parsePackageDeclarations(source, 10)
	if len(declarations) != 1 || declarations[0].name != "kept" || declarations[0].signature != "p_id integer" {
		t.Fatalf("declarations = %#v", declarations)
	}
}

func TestNormalizePackageDebugArgumentsKeepsModes(t *testing.T) {
	cases := map[string]string{
		"p_id integer, p_note text DEFAULT NULL":   "p_id integer, p_note text",
		"p_line OUT text, numlines IN OUT integer": "OUT p_line text, IN OUT numlines integer",
		"p_item IN text":                         "p_item text",
		"status OUT integer, note IN varchar2":   "OUT status integer, note varchar2",
		"p_in_json jsonb, p_count integer := 10": "p_in_json jsonb, p_count integer",
	}
	for input, want := range cases {
		if got := normalizePackageDebugArguments("(" + input + ")"); got != want {
			t.Errorf("normalizePackageDebugArguments(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestPackageMemberDebugRowsSelectsMembersAndArguments(t *testing.T) {
	spec := `CREATE OR REPLACE PACKAGE demo_pkg AS
  PROCEDURE put_line(p_item text);
  PROCEDURE get_line(p_line OUT text, status OUT integer);
  FUNCTION find_order(p_id integer, p_note text DEFAULT NULL) RETURN text;
  PROCEDURE find_order(p_id integer, p_note text);
END demo_pkg;`
	body := `PROCEDURE put_line(p_item text) IS BEGIN NULL; END;`
	rows := packageMemberDebugRows(spec, body, "get_line")
	if len(rows) != 1 {
		t.Fatalf("get_line rows = %d, want 1", len(rows))
	}
	if rows[0].Kind != "PROCEDURE" || rows[0].Arguments != "OUT p_line text, OUT status integer" || rows[0].Source != body {
		t.Fatalf("unexpected get_line row: %#v", rows[0])
	}
	arguments := parsePLDebugArguments(rows[0].Arguments)
	if arguments[0].Mode != "OUT" || arguments[1].Mode != "OUT" {
		t.Fatalf("OUT modes lost: %#v", arguments)
	}
	overloads := packageMemberDebugRows(spec, body, "find_order")
	if len(overloads) != 2 {
		t.Fatalf("find_order overloads = %d, want 2", len(overloads))
	}
	if overloads[0].Kind != "FUNCTION" || overloads[0].ReturnType != "text" {
		t.Fatalf("function return type not parsed: %#v", overloads[0])
	}
	if overloads[1].Kind != "PROCEDURE" || overloads[1].Arguments != "p_id integer, p_note text" {
		t.Fatalf("unexpected overload row: %#v", overloads[1])
	}
}

func TestCompletePackageDdlWrapsCatalogFragment(t *testing.T) {
	server := &server{}
	spec := " \n-- Package: \"kesplus_manage\".\"auth_code\"\n    PROCEDURE add_sys_auth_code(code varchar2);\nend auth_code\n"
	got := server.completePackageDdl("kesplus_manage", "auth_code", "PACKAGE", spec)
	if !strings.HasPrefix(got, `CREATE OR REPLACE PACKAGE "kesplus_manage"."auth_code" AS`) {
		t.Fatalf("spec header missing: %q", got)
	}
	if !strings.HasSuffix(got, "end auth_code;") {
		t.Fatalf("spec terminator missing: %q", got)
	}

	body := "\n    PROCEDURE add_sys_auth_code(code varchar2) AS\n    BEGIN\n        NULL;\n    END;\nEND auth_code"
	gotBody := server.completePackageDdl("kesplus_manage", "auth_code", "PACKAGE_BODY", body)
	if !strings.HasPrefix(gotBody, `CREATE OR REPLACE PACKAGE BODY "kesplus_manage"."auth_code" AS`) {
		t.Fatalf("body header missing: %q", gotBody)
	}
	if !strings.HasSuffix(gotBody, "END auth_code;") {
		t.Fatalf("body terminator missing: %q", gotBody)
	}
}

func TestCompletePackageDdlKeepsCompleteSource(t *testing.T) {
	server := &server{}
	complete := "CREATE OR REPLACE PACKAGE BODY \"s\".\"p\" AS\n  PROCEDURE a IS BEGIN NULL; END;\nEND p;\n"
	if got := server.completePackageDdl("s", "p", "PACKAGE_BODY", complete); strings.Count(got, "CREATE OR REPLACE") != 1 {
		t.Fatalf("complete source was wrapped again: %q", got)
	}
	withoutSemicolon := "CREATE OR REPLACE PACKAGE \"s\".\"p\" AS\n  PROCEDURE a;\nEND p"
	got := server.completePackageDdl("s", "p", "PACKAGE", withoutSemicolon)
	if !strings.HasSuffix(got, "END p;") || strings.Count(got, "CREATE OR REPLACE") != 1 {
		t.Fatalf("unexpected package ddl: %q", got)
	}
}
