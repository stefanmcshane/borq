import { isMap, isScalar, parseDocument, visit, type Document } from "yaml";

export const defaultPostgresConfiguration = `instances: 1
storage: 1Gi
backup:
  enabled: true
  restoreCheck:
    schedule: 0 3 * * 0
`;

type ConfigurationDocument = Document;
export type PostgresSettings = {
  backups: boolean;
  restoreSchedule: string;
  instances: string;
  storage: string;
};
export type PostgresConfiguration =
  | {
      kind: "valid";
      document: ConfigurationDocument;
      settings: PostgresSettings;
    }
  | { kind: "invalid"; problem: string };
export type PostgresSettingEdit =
  | { name: "backups"; value: boolean }
  | { name: "restoreSchedule" | "instances" | "storage"; value: string };

const settingPaths = {
  backups: ["backup", "enabled"],
  restoreSchedule: ["backup", "restoreCheck", "schedule"],
  instances: ["instances"],
  storage: ["storage"],
};

export function parsePostgresConfiguration(
  source: string,
): PostgresConfiguration {
  try {
    const document: ConfigurationDocument = parseDocument(source, {
      version: "1.1",
    });
    if (document.errors.length) throw document.errors[0];
    if (document.warnings.length) throw document.warnings[0];
    if (document.contents === null) document.contents = document.createNode({});
    if (!isMap(document.contents)) {
      throw new Error("Enter a YAML configuration object.");
    }
    visit(document, {
      Alias() {
        throw new Error("Use explicit values instead of YAML aliases.");
      },
      Pair(_key, pair) {
        if (!isScalar(pair.key) || typeof pair.key.value !== "string") {
          throw new Error(
            "Configuration keys must be strings. Use explicit values instead of YAML merge keys.",
          );
        }
      },
    });
    for (const path of [["backup"], ["backup", "restoreCheck"]]) {
      const node = document.getIn(path, true);
      if (node !== undefined && !isMap(node)) {
        throw new Error(path.join(".") + " must be a YAML object.");
      }
    }
    function booleanValue(path: string[], fallback: boolean): boolean {
      const node = document.getIn(path, true);
      if (node === undefined) return fallback;
      if (!isScalar(node) || typeof node.value !== "boolean") {
        throw new Error(path.join(".") + " must be a boolean.");
      }
      return node.value;
    }
    function stringValue(path: string[], fallback: string): string {
      const node = document.getIn(path, true);
      if (node === undefined) return fallback;
      if (!isScalar(node) || typeof node.value !== "string") {
        throw new Error(path.join(".") + " must be a string.");
      }
      return node.value;
    }
    const instances = document.getIn(settingPaths.instances, true);
    if (
      instances !== undefined &&
      (!isScalar(instances) ||
        typeof instances.value !== "number" ||
        !Number.isInteger(instances.value) ||
        instances.value < 1 ||
        instances.value > 16)
    ) {
      throw new Error("Instance count must be between 1 and 16.");
    }
    return {
      kind: "valid",
      document,
      settings: {
        backups: booleanValue(settingPaths.backups, true),
        restoreSchedule: stringValue(settingPaths.restoreSchedule, "0 3 * * 0"),
        instances: isScalar(instances) ? String(instances.value) : "1",
        storage: stringValue(settingPaths.storage, "1Gi"),
      },
    };
  } catch (problem) {
    return {
      kind: "invalid",
      problem:
        problem instanceof Error
          ? problem.message
          : "Correct the YAML configuration before continuing.",
    };
  }
}

export function editPostgresConfiguration(
  configuration: Extract<PostgresConfiguration, { kind: "valid" }>,
  edit: PostgresSettingEdit,
): string {
  const document = configuration.document.clone();
  const path = settingPaths[edit.name];
  if (edit.value === "") {
    const parent =
      path.length === 1
        ? document.contents
        : document.getIn(path.slice(0, -1), true);
    if (isMap(parent)) {
      const pair = parent.items.find(
        (item) => isScalar(item.key) && item.key.value === path.at(-1),
      );
      if (pair) {
        const key = isScalar(pair.key) ? pair.key : undefined;
        const value =
          isScalar(pair.value) || isMap(pair.value) ? pair.value : undefined;
        parent.commentBefore = [
          parent.commentBefore,
          key?.commentBefore,
          key?.comment,
          value?.commentBefore,
          value?.comment,
        ]
          .filter(Boolean)
          .join("\n");
      }
    }
    document.deleteIn(path);
  } else {
    document.setIn(
      path,
      edit.name === "instances" ? Number(edit.value) : edit.value,
    );
  }
  return document.toString();
}
