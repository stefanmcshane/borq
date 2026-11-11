import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { execFileSync, spawnSync } from "node:child_process";
import { mkdtemp, mkdir, readFile, readdir, rm, symlink, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";
import worker from "../worker.mjs";

const template = await readFile(new URL("../public/install.sh", import.meta.url), "utf8");

async function installer(t, {
  path = "/install.sh", method = "GET", tag = "v0.2.1", prerelease = false,
  upstreamStatus = 200, payload = { tag_name: tag, prerelease, draft: false },
} = {}) {
  const requests = [];
  t.mock.method(globalThis, "fetch", async (url) => {
    requests.push(String(url));
    return new Response(JSON.stringify(payload), { status: upstreamStatus });
  });
  const response = await worker.fetch(new Request(`https://install.borq.io${path}`, { method }), {
    ASSETS: {
      async fetch(request) {
        assert.equal(new URL(request.url).pathname, "/install.sh");
        return new Response(template);
      },
    },
  });
  return { response, requests };
}

test("the default installer pins GitHub's latest stable release", async (t) => {
  const { response, requests } = await installer(t);
  assert.equal(response.status, 200);
  assert.deepEqual(requests, ["https://api.github.com/repos/stefanmcshane/borq/releases/latest"]);
  assert.match(response.headers.get("content-type"), /^text\/x-shellscript/);
  assert.match(await response.text(), /tag='v0\.2\.1'/);
});

test("an explicit version selects that exact prerelease on either installer route", async (t) => {
  for (const path of ["/", "/install.sh"]) {
    await t.test(path, async (t) => {
      const { response, requests } = await installer(t, {
        path: `${path}?version=v0.2.2-rc.4`, tag: "v0.2.2-rc.4", prerelease: true,
      });
      assert.equal(response.status, 200);
      assert.deepEqual(requests, ["https://api.github.com/repos/stefanmcshane/borq/releases/tags/v0.2.2-rc.4"]);
      assert.match(await response.text(), /tag='v0\.2\.2-rc\.4'/);
    });
  }
});

test("invalid or duplicate versions never reach GitHub or enter a shell script", async (t) => {
  for (const query of ["version=", "version=latest", "version=v1.0.0&version=v2.0.0", "version=" + encodeURIComponent("v1.0.0'; touch /tmp/injected; #")]) {
    await t.test(query, async (t) => {
      const { response, requests } = await installer(t, { path: `/install.sh?${query}` });
      assert.equal(response.status, 400);
      assert.deepEqual(requests, []);
    });
  }
});

test("release lookup errors and invalid metadata fail without selecting another version", async (t) => {
  for (const scenario of [
    { name: "no stable release", upstreamStatus: 404, status: 404, message: /No stable/ },
    { name: "unknown tag", path: "/?version=v0.2.2-rc.4", upstreamStatus: 404, status: 404, message: /v0\.2\.2-rc\.4 was not found/ },
    { name: "GitHub rate limit", upstreamStatus: 403, status: 502, message: /unavailable/ },
    { name: "prerelease returned as latest", prerelease: true, status: 502, message: /invalid/ },
    { name: "draft returned as latest", payload: { tag_name: "v0.2.1", prerelease: false, draft: true }, status: 502, message: /invalid/ },
    { name: "mismatched tag", path: "/?version=v0.2.2-rc.4", status: 502, message: /invalid/ },
    { name: "unsafe upstream tag", tag: "v0.2.1'; exit 0; #", status: 502, message: /invalid/ },
    { name: "invalid response", payload: null, status: 502, message: /invalid/ },
  ]) {
    await t.test(scenario.name, async (t) => {
      const { response, requests } = await installer(t, scenario);
      assert.equal(response.status, scenario.status);
      assert.match(await response.text(), scenario.message);
      assert.equal(response.headers.get("cache-control"), "no-store");
      assert.equal(requests.length, 1);
    });
  }
});

test("unsupported routes and methods fail before release lookup; HEAD has no script body", async (t) => {
  for (const scenario of [
    { path: "/other", status: 404 },
    { method: "POST", status: 405 },
    { method: "HEAD", status: 200 },
  ]) {
    await t.test(`${scenario.method ?? "GET"} ${scenario.path ?? "/install.sh"}`, async (t) => {
      const { response, requests } = await installer(t, scenario);
      assert.equal(response.status, scenario.status);
      if (scenario.method === "HEAD") {
        assert.equal(await response.text(), "");
      } else {
        assert.deepEqual(requests, []);
      }
      if (scenario.method === "POST") assert.equal(response.headers.get("allow"), "GET, HEAD");
    });
  }
});

async function downloadFixture(t, { platform = "linux_amd64", corruptChecksum = false, failDownload = false, destinationIsDirectory = false } = {}) {
  const directory = await mkdtemp(join(tmpdir(), "borq-installer-"));
  t.after(() => rm(directory, { recursive: true, force: true }));
  const bin = join(directory, "tools");
  const release = join(directory, "release");
  const destination = join(directory, "install dir");
  const temporary = join(directory, "temporary dir");
  await Promise.all([bin, release, destination, temporary].map((path) => mkdir(path)));
  await writeFile(join(release, "borq"), "#!/bin/sh\nprintf 'borq 0.2.2-rc.4\\n'\n", { mode: 0o755 });
  const archiveName = `borq_0.2.2-rc.4_${platform}.tar.gz`;
  execFileSync("tar", ["-czf", join(release, archiveName), "-C", release, "borq"], { timeout: 3000, maxBuffer: 16384 });
  const digest = createHash("sha256").update(await readFile(join(release, archiveName))).digest("hex");
  await writeFile(join(release, "checksums.txt"), `${corruptChecksum ? "0".repeat(64) : digest}  ${archiveName}\n`);
  if (destinationIsDirectory) {
    await mkdir(join(destination, "borq"));
    await writeFile(join(destination, "borq", "keep.txt"), "existing directory content\n");
  } else {
    await writeFile(join(destination, "borq"), "previous binary\n");
  }

  // Only GitHub's network boundary is substituted; tar, hashing, and file replacement run for real.
  await writeFile(join(bin, "curl"), `#!${process.execPath}
import fs from 'node:fs';
const args = process.argv.slice(2);
const url = args.at(-1);
const expected = 'https://github.com/stefanmcshane/borq/releases/download/v0.2.2-rc.4/';
if (!url.startsWith(expected)) process.exit(22);
fs.appendFileSync(process.env.BORQ_FIXTURE + '/downloads', url + '\\n');
if (${failDownload}) { process.stderr.write('Download failed\\n'); process.exit(22); }
fs.copyFileSync(process.env.BORQ_FIXTURE + '/release/' + url.slice(expected.length), args[args.indexOf('--output') + 1]);
`, { mode: 0o755 });
  await writeFile(join(bin, "uname"), `#!/bin/sh
if [ "$1" = -s ]; then printf '%s\\n' '${platform === "darwin_arm64" ? "Darwin" : "Linux"}'; else printf '%s\\n' '${platform === "darwin_arm64" ? "arm64" : "x86_64"}'; fi
`, { mode: 0o755 });
  let path = `${bin}:${process.env.PATH}`;
  if (platform === "darwin_arm64") {
    for (const utility of ["tar", "gzip", "awk", "mktemp", "shasum", "mkdir", "cp", "chmod", "mv", "rm"]) {
      const executable = execFileSync("which", [utility], { encoding: "utf8", timeout: 1000, maxBuffer: 4096 }).trim();
      await symlink(executable, join(bin, utility));
    }
    path = bin;
  }
  const { response } = await installer(t, { path: "/?version=v0.2.2-rc.4", tag: "v0.2.2-rc.4", prerelease: true });
  assert.equal(response.status, 200);
  const result = spawnSync("/bin/sh", [], {
    input: await response.text(), encoding: "utf8", timeout: 5000, maxBuffer: 16384,
    env: { ...process.env, PATH: path, BORQ_FIXTURE: directory, BORQ_INSTALL_DIR: destination, TMPDIR: temporary },
  });
  assert.equal(result.error, undefined);
  return { directory, destination, temporary, result, archiveName };
}

test("the emitted script installs the verified archive without prompts, including macOS shasum fallback", async (t) => {
  for (const platform of ["linux_amd64", "darwin_arm64"]) {
    await t.test(platform, async (t) => {
      const { directory, destination, result, archiveName } = await downloadFixture(t, { platform });
      assert.equal(result.status, 0, result.stderr);
      assert.match(result.stdout, /Installed Borq v0\.2\.2-rc\.4/);
      assert.equal(execFileSync(join(destination, "borq"), ["--version"], { encoding: "utf8", timeout: 1000, maxBuffer: 4096 }), "borq 0.2.2-rc.4\n");
      assert.deepEqual((await readFile(join(directory, "downloads"), "utf8")).trim().split("\n"), [
        `https://github.com/stefanmcshane/borq/releases/download/v0.2.2-rc.4/${archiveName}`,
        "https://github.com/stefanmcshane/borq/releases/download/v0.2.2-rc.4/checksums.txt",
      ]);
    });
  }
});

test("checksum and download failures preserve the existing binary", async (t) => {
  for (const scenario of [
    { name: "checksum mismatch", corruptChecksum: true, error: /Checksum verification failed/ },
    { name: "download failure", failDownload: true, error: /Download failed/ },
  ]) {
    await t.test(scenario.name, async (t) => {
      const { destination, result } = await downloadFixture(t, scenario);
      assert.notEqual(result.status, 0);
      assert.match(result.stderr, scenario.error);
      assert.equal(await readFile(join(destination, "borq"), "utf8"), "previous binary\n");
    });
  }
});

test("a directory at the binary destination is rejected without moving a file inside it", async (t) => {
  const { destination, result } = await downloadFixture(t, { destinationIsDirectory: true });
  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /install destination .* is a directory/);
  assert.deepEqual(await readdir(join(destination, "borq")), ["keep.txt"]);
});
