import assert from "node:assert/strict";
import { test } from "node:test";
import { parse } from "yaml";
import {
  editPostgresConfiguration,
  parsePostgresConfiguration,
} from "../src/lib/postgres-editor.ts";
import { generateWordName } from "../src/lib/name-generator.ts";

function configuration(source) {
  const result = parsePostgresConfiguration(source);
  assert.equal(
    result.kind,
    "valid",
    result.kind === "invalid" ? result.problem : "",
  );
  return result;
}

function edit(source, update) {
  return editPostgresConfiguration(configuration(source), update);
}

test("Settings preserve comments, unrelated YAML keys, and disabled backup policy", () => {
  let source =
    "# Keep the database policy\ninstances: 4\nstorage: 6Gi\nimage: postgres:17\nbackup:\n  schedule: 0 1 * * *\n  enabled: false\n  restoreCheck:\n    timeoutSeconds: 3600\n    schedule: 0 5 * * 3\n";
  const settings = configuration(source).settings;
  assert.equal(settings.backups, false);
  assert.equal(settings.restoreSchedule, "0 5 * * 3");
  source = edit(source, { name: "instances", value: "2" });
  source = edit(source, { name: "storage", value: "8Gi" });
  source = edit(source, { name: "restoreSchedule", value: "0 6 * * 1" });
  source = edit(source, { name: "backups", value: true });
  source = edit(source, { name: "backups", value: false });
  assert.deepEqual(parse(source), {
    instances: 2,
    storage: "8Gi",
    image: "postgres:17",
    backup: {
      schedule: "0 1 * * *",
      enabled: false,
      restoreCheck: { timeoutSeconds: 3600, schedule: "0 6 * * 1" },
    },
  });
  assert.ok(source.includes("# Keep the database policy"));
});

test("Clearing optional settings restores omission while retaining field comments", () => {
  let source =
    "# Instance policy\ninstances: 3 # Retain this note\n# Storage policy\nstorage: 2Gi # Preserve this too\nimage: postgres:17\n";
  source = edit(source, { name: "instances", value: "" });
  source = edit(source, { name: "storage", value: "" });
  assert.deepEqual(parse(source), { image: "postgres:17" });
  for (const comment of [
    "Instance policy",
    "Retain this note",
    "Storage policy",
    "Preserve this too",
  ]) {
    assert.ok(
      source.includes(comment),
      `Cleared configuration retains ${comment}`,
    );
  }
});

test("Blank and comment-only documents submit as objects and accept Settings edits", () => {
  for (const source of [" \n\t", "# Keep defaults\n"]) {
    const parsed = configuration(source);
    assert.deepEqual(parse(parsed.document.toString()), {});
    assert.equal(parsed.settings.backups, true);
    const changed = edit(source, { name: "storage", value: "2Gi" });
    assert.equal(parse(changed).storage, "2Gi");
    const cleared = edit(changed, { name: "storage", value: "" });
    assert.deepEqual(parse(cleared), {});
    assert.ok(!source.includes("#") || cleared.includes("# Keep defaults"));
    assert.deepEqual(parse(edit(source, { name: "instances", value: "" })), {});
  }
});

test("Invalid YAML fails at the named configuration guard", () => {
  const cases = [
    ["backup: [", /flow sequence/i],
    ["backup: false", /backup must be a YAML object/],
    [
      "backup:\n  restoreCheck: false",
      /backup.restoreCheck must be a YAML object/,
    ],
    ['backup:\n  enabled: "false"', /backup.enabled must be a boolean/],
    [
      "backup:\n  restoreCheck:\n    schedule: false",
      /backup.restoreCheck.schedule must be a string/,
    ],
    ["instances: 1\ninstances: 2", /unique/],
    ["{}\n---\n{}", /multiple documents/],
    ["backup: &policy {enabled: true}\nresources: *policy", /aliases/],
    ["backup:\n  <<: {enabled: false}", /merge keys/],
    ["storage: false", /storage must be a string/],
    ["instances: 0", /between 1 and 16/],
    ["instances: 17", /between 1 and 16/],
    ["instances: 1.5", /between 1 and 16/],
    ["[]", /configuration object/],
  ];
  for (const [source, message] of cases) {
    const result = parsePostgresConfiguration(source);
    assert.equal(result.kind, "invalid", `Reject ${source}`);
    assert.match(result.problem, message);
  }
});

test("Suggested names use two whole words within the configured total length", () => {
  for (const maximum of [6, 10, 18, 47]) {
    for (let index = 0; index < 100; index++) {
      const name = generateWordName(maximum);
      assert.match(name, /^[a-z]+-[a-z]+$/);
      assert.ok(
        name.length <= maximum,
        `Suggested name fits ${maximum} characters`,
      );
    }
  }
  for (const maximum of [0, 4, -1, 10.5, Infinity, NaN]) {
    assert.throws(() => generateWordName(maximum), RangeError);
  }
});
