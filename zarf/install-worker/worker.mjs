const repository = "stefanmcshane/borq";
const versionPattern = /^v\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$/;

function problem(message, status) {
  return new Response(`${message}\n`, {
    status,
    headers: {
      "Content-Type": "text/plain; charset=utf-8",
      "Cache-Control": "no-store",
      "X-Content-Type-Options": "nosniff",
    },
  });
}

export default {
  async fetch(request, env) {
    const url = new URL(request.url);
    if (url.pathname !== "/" && url.pathname !== "/install.sh") {
      return problem("Not found.", 404);
    }
    if (request.method !== "GET" && request.method !== "HEAD") {
      const response = problem("Use GET or HEAD to download the installer.", 405);
      response.headers.set("Allow", "GET, HEAD");
      return response;
    }

    const versions = url.searchParams.getAll("version");
    const version = versions[0] ?? null;
    if (versions.length > 1 || (version !== null && !versionPattern.test(version))) {
      return problem("Specify one release tag, for example ?version=v0.2.2-rc.4.", 400);
    }

    try {
      const selector = version === null ? "latest" : `tags/${encodeURIComponent(version)}`;
      const upstream = await fetch(`https://api.github.com/repos/${repository}/releases/${selector}`, {
        headers: {
          Accept: "application/vnd.github+json",
          "User-Agent": "borq-installer",
          "X-GitHub-Api-Version": "2022-11-28",
        },
        signal: AbortSignal.timeout(10000),
        cf: { cacheEverything: true, cacheTtl: 300 },
      });
      if (upstream.status === 404) {
        return problem(version === null
          ? "No stable Borq release is published. Specify ?version=vX.Y.Z for a published prerelease."
          : `Borq release ${version} was not found.`, 404);
      }
      if (!upstream.ok) {
        return problem("GitHub release lookup is unavailable. Try again shortly.", 502);
      }

      const release = await upstream.json();
      if (release === null || typeof release !== "object" ||
          typeof release.tag_name !== "string" || !versionPattern.test(release.tag_name) ||
          release.draft !== false || typeof release.prerelease !== "boolean" ||
          (version === null && release.prerelease) ||
          (version !== null && release.tag_name !== version)) {
        return problem("GitHub returned an invalid Borq release.", 502);
      }

      const template = await env.ASSETS.fetch(new Request(new URL("/install.sh", request.url)));
      if (!template.ok) {
        return problem("The Borq installer is unavailable.", 500);
      }
      const script = (await template.text()).replace("__BORQ_TAG__", release.tag_name);
      return new Response(request.method === "HEAD" ? null : script, {
        headers: {
          "Content-Type": "text/x-shellscript; charset=utf-8",
          "Cache-Control": "public, max-age=300",
          "X-Content-Type-Options": "nosniff",
        },
      });
    } catch {
      return problem("Could not prepare the Borq installer. Try again shortly.", 502);
    }
  },
};
