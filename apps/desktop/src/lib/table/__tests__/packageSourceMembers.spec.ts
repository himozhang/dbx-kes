import { describe, expect, it } from "vitest";
import { addPackageMemberToSources, buildPackageOutline, findPackageOutlineMemberByName, packageOutlineMemberAtLine, parsePackageSourceMembers, removePackageMemberFromSources } from "../packageSourceMembers";

const header = `CREATE OR REPLACE PACKAGE demo_pkg AS
  PROCEDURE create_order(p_id integer, p_note text DEFAULT NULL);
  FUNCTION find_order(p_id integer) RETURN text;
  PROCEDURE missing_impl;
END demo_pkg;`;

const body = `CREATE OR REPLACE PACKAGE BODY demo_pkg AS
  PROCEDURE create_order(p_id integer, p_note text DEFAULT NULL) IS
  BEGIN
    NULL;
  END;

  FUNCTION find_order(p_id integer) RETURN text IS
  BEGIN
    RETURN NULL;
  END;
END demo_pkg;`;

describe("packageSourceMembers", () => {
  it("keeps all declarations from the package header", () => {
    const members = parsePackageSourceMembers(header);
    expect(members.map((member) => `${member.kind}:${member.name}`)).toEqual(["PROCEDURE:create_order", "FUNCTION:find_order", "PROCEDURE:missing_impl"]);
  });

  it("maps header declarations to package body implementation lines", () => {
    const outline = buildPackageOutline(header, body);
    expect(outline.map((member) => [member.name, member.bodyLine])).toEqual([
      ["create_order", 2],
      ["find_order", 7],
    ]);
  });

  it("drops declarations without an implementation when the body is available", () => {
    const outline = buildPackageOutline(header, body);
    expect(outline.some((member) => member.name === "missing_impl")).toBe(false);
  });

  it("falls back to header line numbers when the body cannot be read", () => {
    const outline = buildPackageOutline(header, "");
    expect(outline.map((member) => [member.name, member.bodyLine, member.line])).toEqual([
      ["create_order", undefined, 2],
      ["find_order", undefined, 3],
      ["missing_impl", undefined, 4],
    ]);
  });

  it("matches overloads by signature before falling back to name only", () => {
    const overloadHeader = `PACKAGE p AS
  PROCEDURE run(p_id integer);
  PROCEDURE run(p_name text);
END p;`;
    const overloadBody = `PACKAGE BODY p AS
  PROCEDURE run(p_name text) IS
  BEGIN NULL; END;
  PROCEDURE run(p_id integer) IS
  BEGIN NULL; END;
END p;`;
    const outline = buildPackageOutline(overloadHeader, overloadBody);
    expect(outline.map((member) => [member.signature, member.bodyLine])).toEqual([
      ["p_id integer", 4],
      ["p_name text", 2],
    ]);
  });

  it("finds the member that owns a package body line", () => {
    const outline = buildPackageOutline(header, body);
    expect(packageOutlineMemberAtLine(outline, 2)?.name).toBe("create_order");
    expect(packageOutlineMemberAtLine(outline, 5)?.name).toBe("create_order");
    expect(packageOutlineMemberAtLine(outline, 7)?.name).toBe("find_order");
    expect(packageOutlineMemberAtLine(outline, 12)?.name).toBe("find_order");
    expect(packageOutlineMemberAtLine(outline, 1)).toBeUndefined();
  });

  it("ignores PROCEDURE/FUNCTION words inside comments", () => {
    const commented = `CREATE OR REPLACE PACKAGE demo_pkg AS
  -- PROCEDURE ghost(p_id integer); must not become a member
  /* FUNCTION ghost_fn RETURN text; */
  PROCEDURE real_one(p_id integer);
END demo_pkg;`;
    expect(parsePackageSourceMembers(commented).map((member) => member.name)).toEqual(["real_one"]);
  });

  it("maps overloads when the header spells parameter modes and the body does not", () => {
    const modeHeader = `PACKAGE p AS
  PROCEDURE run(p_id IN integer);
  PROCEDURE run(p_name IN OUT text);
END p;`;
    const modeBody = `PACKAGE BODY p AS
  PROCEDURE run(p_name text) IS
  BEGIN NULL; END;
  PROCEDURE run(p_id integer) IS
  BEGIN NULL; END;
END p;`;
    const outline = buildPackageOutline(modeHeader, modeBody);
    expect(outline.map((member) => [member.signature, member.bodyLine])).toEqual([
      ["p_id IN integer", 4],
      ["p_name IN OUT text", 2],
    ]);
  });

  it("returns no outline when the body is present but exposes no implementation", () => {
    expect(buildPackageOutline(header, "-- nothing here\nEND demo_pkg;")).toEqual([]);
  });

  it("locates a member by name case-insensitively", () => {
    const outline = buildPackageOutline(header, body);
    expect(findPackageOutlineMemberByName(outline, "FIND_ORDER")?.name).toBe("find_order");
    expect(findPackageOutlineMemberByName(outline, "absent")).toBeUndefined();
  });

  it("adds a procedure declaration to the header and a stub to the body", () => {
    const result = addPackageMemberToSources(header, body, { kind: "PROCEDURE", name: "new_proc", parameters: "p_id integer" });
    expect(parsePackageSourceMembers(result.headerSource).map((member) => member.name)).toContain("new_proc");
    expect(result.headerSource.trimEnd().endsWith("END demo_pkg;")).toBe(true);
    expect(result.bodySource.trimEnd().endsWith("END demo_pkg;")).toBe(true);
    expect(result.bodySource).toContain("PROCEDURE new_proc(p_id integer) IS");
    expect(result.bodySource).toContain("END new_proc;");
    expect(parsePackageSourceMembers(result.bodySource).map((member) => member.name)).toContain("new_proc");
  });

  it("adds a function declaration with its return type", () => {
    const result = addPackageMemberToSources(header, body, { kind: "FUNCTION", name: "new_fn", parameters: "", returnType: "integer" });
    expect(result.headerSource).toContain("FUNCTION new_fn() RETURN integer;");
    expect(result.bodySource).toContain("FUNCTION new_fn() RETURN integer IS");
    expect(result.bodySource).toContain("RETURN NULL;");
  });

  it("removes a member from both the header and the body", () => {
    const result = removePackageMemberFromSources(header, body, { name: "create_order", kind: "PROCEDURE", signature: "p_id integer, p_note text DEFAULT NULL" });
    expect(result.headerSource).not.toContain("create_order");
    expect(result.bodySource).not.toContain("create_order");
    expect(result.bodySource).toContain("FUNCTION find_order");
    expect(result.bodySource.trimEnd().endsWith("END demo_pkg;")).toBe(true);
  });

  it("removes a body block containing nested IF/LOOP blocks", () => {
    const nestedBody = `CREATE OR REPLACE PACKAGE BODY demo_pkg AS
  PROCEDURE walk(p_id integer) IS
  BEGIN
    IF p_id > 0 THEN
      FOR i IN 1..p_id LOOP
        NULL;
      END LOOP;
    ELSE
      CASE p_id
        WHEN 0 THEN NULL;
        ELSE NULL;
      END CASE;
    END IF;
  END walk;

  PROCEDURE keep_me IS
  BEGIN
    NULL;
  END keep_me;
END demo_pkg;`;
    const nestedHeader = `CREATE OR REPLACE PACKAGE demo_pkg AS
  PROCEDURE walk(p_id integer);
  PROCEDURE keep_me;
END demo_pkg;`;
    const result = removePackageMemberFromSources(nestedHeader, nestedBody, { name: "walk", kind: "PROCEDURE", signature: "p_id integer" });
    expect(result.headerSource).not.toContain("walk");
    expect(result.bodySource).not.toContain("walk");
    expect(result.bodySource).toContain("PROCEDURE keep_me");
    expect(result.bodySource.trimEnd().endsWith("END demo_pkg;")).toBe(true);
  });

  it("inserts new members before the final END even when the source ends with a SQL*Plus slash", () => {
    const slashHeader = `CREATE OR REPLACE PACKAGE slash_pkg AS
  PROCEDURE existing;
END slash_pkg;
/`;
    const slashBody = `CREATE OR REPLACE PACKAGE BODY slash_pkg AS
  PROCEDURE existing IS
  BEGIN
    NULL;
  END existing;
END slash_pkg;
/`;
    const result = addPackageMemberToSources(slashHeader, slashBody, { kind: "PROCEDURE", name: "added", parameters: "" });
    // 新成员必须出现在 END 之前，不能被追加到末尾的 / 之后。
    const headerEndIndex = result.headerSource.indexOf("END slash_pkg;");
    const addedHeaderIndex = result.headerSource.indexOf("PROCEDURE added");
    expect(addedHeaderIndex).toBeGreaterThan(0);
    expect(addedHeaderIndex).toBeLessThan(headerEndIndex);
    expect(result.headerSource.trimEnd().endsWith("/")).toBe(true);

    const bodyEndIndex = result.bodySource.indexOf("END slash_pkg;");
    const addedBodyIndex = result.bodySource.indexOf("PROCEDURE added() IS");
    expect(addedBodyIndex).toBeGreaterThan(0);
    expect(addedBodyIndex).toBeLessThan(bodyEndIndex);
    expect(result.bodySource.trimEnd().endsWith("/")).toBe(true);
  });
});
