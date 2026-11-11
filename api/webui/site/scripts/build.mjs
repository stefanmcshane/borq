import { build } from "astro";
import { mkdir, mkdtemp, rename, rm } from "node:fs/promises";
import { fileURLToPath } from "node:url";
import path from "node:path";

const root = fileURLToPath(new URL("../", import.meta.url));
// Astro uses .astro itself as temporary output when outDir is outside the site.
// Keep staging inside the site so it cannot remove its own generated metadata.
const temporaryRoot = path.join(root, ".astro");
await mkdir(temporaryRoot, { recursive: true });
const staging = await mkdtemp(path.join(temporaryRoot, "ui-build-"));
const output = path.join(staging, "output");
const previous = path.join(staging, "previous");
const published = path.join(root, "dist");

try {
  await build({ root, outDir: output });
  // Keep the last complete site when Astro fails. The shared build lock also
  // excludes Go embedding while the complete output directory is replaced.
  let hadPrevious = false;
  try {
    await rename(published, previous);
    hadPrevious = true;
  } catch (error) {
    if (error.code !== "ENOENT") throw error;
  }
  try {
    await rename(output, published);
  } catch (error) {
    if (hadPrevious) await rename(previous, published);
    throw error;
  }
} finally {
  await rm(staging, { recursive: true, force: true });
}
