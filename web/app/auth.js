import { showPairing } from "./lifecycle.js";
import { state } from "./state.js";

async function completePairing(pairingID, nonce) {
  if (!hasWebCrypto()) {
    return completeHTTPPairing(pairingID, nonce);
  }
  showPairing("Generating a device key and proving QR possession.");
  const keys = await crypto.subtle.generateKey(
    { name: "ECDSA", namedCurve: "P-256" },
    true,
    ["sign", "verify"]
  );
  const publicKeyJWK = await crypto.subtle.exportKey("jwk", keys.publicKey);
  const signature = await sign(keys.privateKey, nonce);
  const res = await fetch("/api/pairing/complete", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    credentials: "include",
    body: JSON.stringify({
      pairing_id: pairingID,
      nonce,
      device_name: navigator.userAgent.includes("iPhone") ? "iPhone" : "Browser",
      public_key_jwk: publicKeyJWK,
      signature,
    }),
  });
  if (!res.ok) {
    showPairing("Pairing failed: " + await res.text());
    throw new Error("pairing failed");
  }
  const body = await res.json();
  localStorage.setItem("ibkrDeviceID", body.device_id);
  await savePrivateKey(keys.privateKey);
  localStorage.removeItem("ibkrDeviceSecret");
}

// The device key must survive for a year or more. iOS evicts IndexedDB
// independently of localStorage under storage pressure, so the key (already
// generated extractable) is mirrored to localStorage; losing either store
// alone no longer forces a re-pair. Both stores are same-origin JS-readable,
// so the mirror does not widen the exposure of the extractable key.
const DEVICE_KEY_BACKUP = "ibkrDeviceKeyJWK";

async function backupPrivateKey(key) {
  try {
    const jwk = await crypto.subtle.exportKey("jwk", key);
    localStorage.setItem(DEVICE_KEY_BACKUP, JSON.stringify(jwk));
  } catch {
    // Key export can fail on exotic engines; IndexedDB remains primary.
  }
}

async function restorePrivateKeyFromBackup() {
  const raw = localStorage.getItem(DEVICE_KEY_BACKUP);
  if (!raw) return null;
  try {
    const jwk = JSON.parse(raw);
    const key = await crypto.subtle.importKey(
      "jwk",
      jwk,
      { name: "ECDSA", namedCurve: "P-256" },
      true,
      ["sign"]
    );
    await savePrivateKey(key).catch(() => {});
    return key;
  } catch {
    return null;
  }
}

async function completeHTTPPairing(pairingID, nonce) {
  showPairing("Completing local HTTP pairing.");
  const secret = randomDeviceSecret();
  const res = await fetch("/api/pairing/complete", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    credentials: "include",
    body: JSON.stringify({
      pairing_id: pairingID,
      nonce,
      device_name: navigator.userAgent.includes("iPhone") ? "iPhone" : "Browser",
      device_secret: secret,
    }),
  });
  if (!res.ok) {
    showPairing("Pairing failed: " + await res.text());
    throw new Error("pairing failed");
  }
  const body = await res.json();
  localStorage.setItem("ibkrDeviceID", body.device_id);
  // Cleartext is the only option this path has, and it is the weakest of the
  // three credentials rather than the one that matters. completeHTTPPairing
  // runs only when crypto.subtle is absent — a non-secure-context origin, so
  // the plain-http LAN host — where no client-side encryption exists to apply
  // and this same secret has already crossed the wire in the clear above. The
  // credential a scanner would tell us to use instead is already the primary
  // one: ibkr_app_device is HttpOnly, JS-unreadable, and redeemed on every
  // request by deviceCookieSession. The secret is the third-tier backstop for
  // when cookie eviction leaves a crypto-less origin holding nothing at all,
  // and dropping it turns every eviction into a forced re-pair.
  localStorage.setItem("ibkrDeviceSecret", secret); // codeql[js/clear-text-storage-of-sensitive-data]
}

// tryDeviceLogin returns "ok" when a fresh session was minted, "repair" when
// the app definitively rejected this device (only a new pairing can help),
// and "retry" for everything transient: network failures, relay 503s while
// the Mac restarts, or a challenge that died with the old app process. A
// transient failure must never read as "please re-pair" — that habit is what
// buries the state store in orphaned device grants.
async function tryDeviceLogin() {
  const deviceID = localStorage.getItem("ibkrDeviceID");
  const privateKey = hasWebCrypto() ? await loadPrivateKey() : null;
  const deviceSecret = localStorage.getItem("ibkrDeviceSecret") || "";
  if (!deviceID || (!privateKey && !deviceSecret)) return "repair";
  let ch;
  try {
    ch = await fetch("/api/auth/challenge", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ device_id: deviceID }),
    });
  } catch {
    return "retry";
  }
  if (!ch.ok) {
    // 401 means the device grant is gone server-side; anything else is the
    // relay or app being temporarily unavailable.
    return ch.status === 401 ? "repair" : "retry";
  }
  const challenge = await ch.json();
  const body = privateKey
    ? { device_id: deviceID, challenge: challenge.challenge, signature: await sign(privateKey, challenge.challenge) }
    : { device_id: deviceID, challenge: challenge.challenge, device_secret: deviceSecret };
  let session;
  try {
    session = await fetch("/api/auth/session", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      credentials: "include",
      body: JSON.stringify(body),
    });
  } catch {
    return "retry";
  }
  if (session.ok) return "ok";
  if (session.status !== 401) return "retry";
  const err = await session.json().catch(() => ({}));
  if (err.error === "unknown challenge" || err.error === "challenge expired") {
    // The app restarted between challenge and session; the credential is fine.
    return "retry";
  }
  if (deviceSecret) {
    localStorage.removeItem("ibkrDeviceSecret");
  }
  return "repair";
}

async function sign(privateKey, value) {
  if (!hasWebCrypto()) {
    throw new Error("WebCrypto is unavailable on this origin");
  }
  const sig = await crypto.subtle.sign(
    { name: "ECDSA", hash: "SHA-256" },
    privateKey,
    new TextEncoder().encode(value)
  );
  return bytesToB64url(new Uint8Array(sig));
}

function hasWebCrypto() {
  return !!globalThis.crypto?.subtle;
}

function randomDeviceSecret() {
  const bytes = new Uint8Array(32);
  if (!globalThis.crypto?.getRandomValues) {
    throw new Error("secure random is unavailable in this browser");
  }
  globalThis.crypto.getRandomValues(bytes);
  return bytesToB64url(bytes);
}

async function savePrivateKey(key) {
  await backupPrivateKey(key);
  const db = await openDB();
  return new Promise((resolve, reject) => {
    const tx = db.transaction("keys", "readwrite");
    tx.objectStore("keys").put(key, "device");
    tx.oncomplete = resolve;
    tx.onerror = () => reject(tx.error);
  });
}

async function loadPrivateKey() {
  let key = null;
  try {
    const db = await openDB();
    key = await new Promise((resolve) => {
      const tx = db.transaction("keys", "readonly");
      const req = tx.objectStore("keys").get("device");
      req.onsuccess = () => resolve(req.result || null);
      req.onerror = () => resolve(null);
    });
  } catch {
    key = null;
  }
  if (key) return key;
  return restorePrivateKeyFromBackup();
}

function openDB() {
  return new Promise((resolve, reject) => {
    const req = indexedDB.open("ibkr-app", 1);
    req.onupgradeneeded = () => req.result.createObjectStore("keys");
    req.onsuccess = () => resolve(req.result);
    req.onerror = () => reject(req.error);
  });
}

function b64urlToBytes(input) {
  const pad = "=".repeat((4 - (input.length % 4)) % 4);
  const raw = atob((input + pad).replaceAll("-", "+").replaceAll("_", "/"));
  return Uint8Array.from(raw, (c) => c.charCodeAt(0));
}

function bytesToB64url(bytes) {
  let raw = "";
  bytes.forEach((b) => raw += String.fromCharCode(b));
  return btoa(raw).replaceAll("+", "-").replaceAll("/", "_").replaceAll("=", "");
}

export { b64urlToBytes, bytesToB64url, completeHTTPPairing, completePairing, hasWebCrypto, loadPrivateKey, openDB, randomDeviceSecret, savePrivateKey, sign, tryDeviceLogin };
