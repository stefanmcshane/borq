import { createServer } from "node:http";
import { once } from "node:events";
import { writeFile } from "node:fs/promises";
import { join } from "node:path";
import {
  createHash,
  generateKeyPairSync,
  randomBytes,
  sign,
  timingSafeEqual,
} from "node:crypto";

// This fixture runs a signed OIDC exchange and private vault HTTP storage. Its
// explicit approval button represents a test identity, not passkey authentication
// or Kubernetes controller readiness.
export async function startIdentityFixture(directory) {
  const documents = new Map();
  const approvals = new Map();
  const codes = new Map();
  const issuedTokens = new Map();
  const { privateKey, publicKey } = generateKeyPairSync("rsa", {
    modulusLength: 2048,
  });
  const keyId = "browser-identity-fixture";
  const publicJwk = {
    ...publicKey.export({ format: "jwk" }),
    alg: "RS256",
    use: "sig",
    kid: keyId,
  };
  const registration = {
    id: "browser-team-client",
    secret: "browser-team-client-secret",
    callback: "",
  };
  const opaque = () => randomBytes(24).toString("base64url");
  const keep = (map, key, value) => {
    for (const [previousKey, previous] of map) {
      if (previous.expires <= Date.now()) map.delete(previousKey);
    }
    if (map.size >= 64) return false;
    map.set(key, { ...value, expires: Date.now() + 300_000 });
    return true;
  };
  const take = (map, key) => {
    const value = map.get(key);
    map.delete(key);
    return value?.expires > Date.now() ? value : undefined;
  };
  const sameSecret = (actual) => {
    const supplied = Buffer.from(actual);
    const expected = Buffer.from(registration.secret);
    return (
      supplied.length === expected.length && timingSafeEqual(supplied, expected)
    );
  };
  const readBody = async (request, limit) => {
    let body = "";
    for await (const chunk of request) {
      body += chunk;
      if (Buffer.byteLength(body) > limit) throw new Error("body too large");
    }
    return body;
  };
  const server = createServer(async (request, response) => {
    const origin = `http://127.0.0.1:${server.address().port}`;
    const address = new URL(request.url, origin);
    const path = address.pathname;
    response.setHeader("Content-Type", "application/json");
    response.setHeader("Cache-Control", "no-store");
    const reject = (error = "invalid_request", status = 400) => {
      response.statusCode = status;
      response.end(JSON.stringify({ error }));
    };
    try {
      if (path === "/oidc/authorize" && request.method === "GET") {
        const query = address.searchParams;
        const callback = new URL(query.get("redirect_uri"));
        const state = query.get("state");
        const nonce = query.get("nonce");
        const challenge = query.get("code_challenge");
        if (
          query.get("client_id") !== registration.id ||
          query.get("response_type") !== "code" ||
          !query.get("scope")?.split(" ").includes("openid") ||
          query.get("code_challenge_method") !== "S256" ||
          !/^[A-Za-z0-9_-]{43}$/.test(challenge ?? "") ||
          !/^[A-Za-z0-9_-]{16,128}$/.test(state ?? "") ||
          !/^[A-Za-z0-9_-]{16,128}$/.test(nonce ?? "") ||
          callback.protocol !== "http:" ||
          callback.hostname !== "127.0.0.1" ||
          !callback.port ||
          callback.username ||
          callback.password ||
          callback.search ||
          callback.hash ||
          !/^\/api\/auth\/oidc\/callback\/[a-f0-9]{32}$/.test(
            callback.pathname,
          ) ||
          (registration.callback && registration.callback !== callback.href)
        ) {
          reject();
          return;
        }
        // The standalone Borq origin and organization ID are ephemeral. Pin the
        // first exact callback for the fixture's single pre-registered client.
        registration.callback = callback.href;
        const attempt = opaque();
        if (
          !keep(approvals, attempt, {
            state,
            nonce,
            challenge,
            callback: callback.href,
          })
        ) {
          reject("temporarily_unavailable", 503);
          return;
        }
        response.setHeader("Content-Type", "text/html; charset=utf-8");
        response.end(
          `<!doctype html><h1>Identity provider sign-in</h1><p>Browser test identities</p><form method="post" action="/oidc/approve"><input type="hidden" name="attempt" value="${attempt}"><button type="submit" name="identity" value="administrator">Sign in as browser administrator</button><button type="submit" name="identity" value="viewer">Sign in as provider member</button></form>`,
        );
        return;
      }
      if (path === "/oidc/approve" && request.method === "POST") {
        const form = new URLSearchParams(await readBody(request, 8192));
        const approval = take(approvals, form.get("attempt"));
        if (request.headers.origin !== origin || !approval) {
          reject();
          return;
        }
        const code = opaque();
        const identity = form.get("identity");
        if (identity !== "administrator" && identity !== "viewer") {
          reject();
          return;
        }
        if (!keep(codes, code, { ...approval, identity })) {
          reject("temporarily_unavailable", 503);
          return;
        }
        const callback = new URL(approval.callback);
        callback.searchParams.set("code", code);
        callback.searchParams.set("state", approval.state);
        callback.searchParams.set("iss", origin + "/oidc");
        response.writeHead(303, { Location: callback.href });
        response.end();
        return;
      }
      if (path === "/oidc/token" && request.method === "POST") {
        const form = new URLSearchParams(await readBody(request, 8192));
        let clientId = form.get("client_id");
        let secret = form.get("client_secret");
        if (request.headers.authorization?.startsWith("Basic ")) {
          const credentials = Buffer.from(
            request.headers.authorization.slice(6),
            "base64",
          ).toString();
          const colon = credentials.indexOf(":");
          if (colon < 0 || clientId || secret) {
            reject("invalid_client", 401);
            return;
          }
          clientId = decodeURIComponent(credentials.slice(0, colon));
          secret = decodeURIComponent(credentials.slice(colon + 1));
        }
        if (clientId !== registration.id || !sameSecret(secret ?? "")) {
          reject("invalid_client", 401);
          return;
        }
        const authorization = take(codes, form.get("code"));
        const verifier = form.get("code_verifier");
        if (
          form.get("grant_type") !== "authorization_code" ||
          !authorization ||
          form.get("redirect_uri") !== authorization.callback ||
          !/^[A-Za-z0-9._~-]{43,128}$/.test(verifier ?? "") ||
          createHash("sha256").update(verifier).digest("base64url") !==
            authorization.challenge
        ) {
          reject("invalid_grant");
          return;
        }
        const now = Math.floor(Date.now() / 1000);
        const header = Buffer.from(
          JSON.stringify({ alg: "RS256", typ: "JWT", kid: keyId }),
        ).toString("base64url");
        const payload = Buffer.from(
          JSON.stringify({
            iss: origin + "/oidc",
            sub:
              authorization.identity === "viewer"
                ? "browser-viewer"
                : "browser-admin",
            aud: registration.id,
            nonce: authorization.nonce,
            iat: now,
            exp: now + 300,
            name:
              authorization.identity === "viewer"
                ? "Provider member"
                : "Browser administrator",
            email:
              authorization.identity === "viewer"
                ? "browser-viewer@example.test"
                : "browser-admin@example.test",
            email_verified: true,
          }),
        ).toString("base64url");
        const message = header + "." + payload;
        const token =
          message +
          "." +
          sign("RSA-SHA256", Buffer.from(message), privateKey).toString(
            "base64url",
          );
        if (
          !keep(issuedTokens, token, {
            clientId,
            returnURL: new URL("/login", authorization.callback).href,
          })
        ) {
          reject("temporarily_unavailable", 503);
          return;
        }
        response.end(
          JSON.stringify({
            access_token: opaque(),
            token_type: "Bearer",
            expires_in: 300,
            id_token: token,
          }),
        );
        return;
      }
      if (path === "/oidc/end-session" && request.method === "GET") {
        const query = address.searchParams;
        // An exact lookup proves this is the signed token actually issued by the
        // fixture, rather than a fabricated hint accepted by a permissive mock.
        const issued = take(issuedTokens, query.get("id_token_hint"));
        if (
          !issued ||
          query.get("client_id") !== issued.clientId ||
          query.get("post_logout_redirect_uri") !== issued.returnURL
        ) {
          reject();
          return;
        }
        const destination = new URL(issued.returnURL);
        destination.searchParams.set("provider_logout", "complete");
        response.writeHead(303, { Location: destination.href });
        response.end();
        return;
      }
      if (path === "/oidc/.well-known/openid-configuration") {
        response.end(
          JSON.stringify({
            issuer: origin + "/oidc",
            authorization_endpoint: origin + "/oidc/authorize",
            token_endpoint: origin + "/oidc/token",
            jwks_uri: origin + "/oidc/keys",
            end_session_endpoint: origin + "/oidc/end-session",
            response_types_supported: ["code"],
            subject_types_supported: ["public"],
            id_token_signing_alg_values_supported: ["RS256"],
            code_challenge_methods_supported: ["S256"],
            authorization_response_iss_parameter_supported: true,
            token_endpoint_auth_methods_supported: [
              "client_secret_basic",
              "client_secret_post",
            ],
          }),
        );
        return;
      }
      if (path === "/oidc/keys") {
        response.end(JSON.stringify({ keys: [publicJwk] }));
        return;
      }
      if (path.endsWith("/secrets/borq-openbao-admin")) {
        response.end(
          JSON.stringify({
            apiVersion: "v1",
            kind: "Secret",
            metadata: { name: "borq-openbao-admin", namespace: "borq" },
            data: {
              token: Buffer.from("browser-test-admin").toString("base64"),
            },
          }),
        );
        return;
      }
      if (path.endsWith("/sys/seal-status")) {
        response.end(JSON.stringify({ initialized: true, sealed: false }));
        return;
      }
      const marker = "/proxy/v1/secret/data/";
      if (path.includes(marker)) {
        const key = path.split(marker)[1];
        if (request.method === "PUT") {
          if (!documents.has(key) && documents.size >= 256) {
            reject("fixture_storage_limit", 503);
            return;
          }
          documents.set(key, JSON.parse(await readBody(request, 1 << 20)).data);
          response.end(JSON.stringify({ data: { version: 1 } }));
        } else if (documents.has(key)) {
          response.end(
            JSON.stringify({
              data: { data: documents.get(key), metadata: { version: 1 } },
            }),
          );
        } else {
          response.statusCode = 404;
          response.end(JSON.stringify({ errors: ["not found"] }));
        }
        return;
      }
      reject("fixture_endpoint_not_found", 404);
    } catch {
      reject();
    }
  });
  server.listen(0, "127.0.0.1");
  await once(server, "listening");
  const origin = `http://127.0.0.1:${server.address().port}`;
  const kubeconfigPath = join(directory, "identity-kubeconfig");
  await writeFile(
    kubeconfigPath,
    `apiVersion: v1\nkind: Config\nclusters:\n- name: identity\n  cluster:\n    server: ${origin}\ncontexts:\n- name: identity\n  context:\n    cluster: identity\n    user: identity\ncurrent-context: identity\nusers:\n- name: identity\n  user: {}\n`,
    { mode: 0o600 },
  );
  return {
    issuer: origin + "/oidc",
    kubeconfigPath,
    close: () => new Promise((resolve) => server.close(resolve)),
  };
}
