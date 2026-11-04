#!/usr/bin/env node
// Cloudflare's content endpoint preserves config/metadata, including bindings,
// secrets and the migration tag. This helper never creates or migrates a Worker.
// API contracts and operating limits are linked in README.md.
import { readFile } from "node:fs/promises";
import { isDeepStrictEqual } from "node:util";
import { pathToFileURL } from "node:url";

const apiOrigin = "https://api.cloudflare.com/client/v4";
const config = JSON.parse(await readFile(new URL("./deploy.json", import.meta.url), "utf8"));
const binding = config.durable_objects.bindings[0];
const migration = config.migrations[0];

class DeploymentError extends Error {}
function requireState(ok, message) {
  if (!ok) throw new DeploymentError(message);
}

function verifySettings(settings) {
  requireState(settings && Array.isArray(settings.bindings), "Worker bindings are unavailable; nothing can be safely deployed.");
  const matches = settings.bindings.filter((item) => item.name === binding.name);
  const relay = matches[0];
  requireState(matches.length === 1 && relay.type === "durable_object_namespace" &&
    relay.class_name === binding.class_name && typeof relay.namespace_id === "string" && relay.namespace_id.length > 0 &&
    (!relay.script_name || relay.script_name === config.name) && !relay.environment && !relay.dispatch_namespace,
  "The relay binding does not identify the expected local Durable Object; deployment refused.");
  requireState(settings.compatibility_date === config.compatibility_date &&
    isDeepStrictEqual(settings.compatibility_flags?.toSorted(), config.compatibility_flags.toSorted()),
  "Worker compatibility settings differ from deploy.json; review them separately before deployment.");
}

// fetchImpl is a test seam, not a CLI endpoint override. Production credentials
// are sent only to the fixed HTTPS API origin, with redirects disabled.
export async function deploy({ action, accountId, token, fetchImpl = fetch }) {
  requireState(["--check", "--deploy"].includes(action), "usage: node deploy.js --check|--deploy");
  requireState(typeof accountId === "string" && /^[a-f0-9]{32}$/i.test(accountId), "Set CLOUDFLARE_ACCOUNT_ID to the intended account's 32-character ID.");
  requireState(typeof token === "string" && token.length > 0 && !/\s/.test(token), "Set CLOUDFLARE_API_TOKEN to an account-scoped Workers Scripts token.");
  const root = `/accounts/${accountId}/workers/scripts`;
  const script = `${root}/${config.name}`;

  async function api(method, path, body) {
    let response;
    let envelope;
    try {
      response = await fetchImpl(`${apiOrigin}${path}`, {
        method, body, headers: { Authorization: `Bearer ${token}` },
        redirect: "error", signal: AbortSignal.timeout(30_000),
      });
      envelope = await response.json();
    } catch {
      // Do not echo response bodies, URLs, exception causes or credentials.
      throw new DeploymentError(`Cloudflare ${method} failed (network, redirect, timeout or invalid JSON).`);
    }
    if (!response.ok || envelope?.success !== true || envelope.result == null ||
        (Array.isArray(envelope.errors) && envelope.errors.length > 0)) {
      const codes = Array.isArray(envelope?.errors)
        ? envelope.errors.map((error) => error?.code).filter(Number.isSafeInteger).join(", ") : "";
      throw new DeploymentError(`Cloudflare ${method} failed (HTTP ${response.status}${codes ? `; codes ${codes}` : ""}).`);
    }
    return envelope.result;
  }

  const scripts = await api("GET", root);
  requireState(Array.isArray(scripts) && scripts.every((item) => item && typeof item.id === "string"),
    "Cloudflare returned an invalid Worker inventory; deployment refused.");
  const matches = scripts.filter((item) => item.id === config.name);
  requireState(matches.length <= 1, "Cloudflare returned ambiguous Worker identity; deployment refused.");
  const current = matches[0];
  requireState(current, "Worker is missing; deployment refused. Provisioning is a separate operator action.");
  requireState(current.migration_tag === migration.tag && current.has_modules === true,
    "Worker migration tag or module format differs; deployment refused. Review migration history separately.");
  const settings = await api("GET", `${script}/settings`);
  verifySettings(settings);
  if (action === "--check") return "Existing relay configuration matches; no writes performed.";

  const metadata = { main_module: "worker.js" };
  const form = new FormData();
  form.set("metadata", new Blob([JSON.stringify(metadata)], { type: "application/json" }));
  form.set("worker.js", new Blob([await readFile(new URL(config.main, import.meta.url))], { type: "application/javascript+module" }), "worker.js");
  try {
    const uploaded = await api("PUT", `${script}/content`, form);
    requireState(uploaded.migration_tag === migration.tag, "Upload response did not confirm the expected migration tag.");
    const after = await api("GET", `${script}/settings`);
    verifySettings(after);
    requireState(isDeepStrictEqual(settings, after), "Worker settings changed during deployment; inspect the remote state.");
  } catch (error) {
    const detail = error instanceof DeploymentError ? error.message : "Deployment verification failed.";
    throw new DeploymentError(`${detail} The write may have applied; inspect Cloudflare before retrying. No automatic retry was made.`);
  }
  return "Relay content uploaded; existing settings and namespace verified unchanged.";
}

export async function main(args, env, options = {}) {
  const { stdout = console.log, stderr = console.error, fetchImpl = fetch } = options;
  try {
    requireState(args.length === 1, "usage: node deploy.js --check|--deploy");
    stdout(await deploy({ action: args[0], accountId: env.CLOUDFLARE_ACCOUNT_ID, token: env.CLOUDFLARE_API_TOKEN, fetchImpl }));
    return 0;
  } catch (error) {
    stderr(`relay-deploy: ${error instanceof DeploymentError ? error.message : "Failed to read local inputs or validate the API response."}`);
    return 1;
  }
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  process.exitCode = await main(process.argv.slice(2), process.env);
}
