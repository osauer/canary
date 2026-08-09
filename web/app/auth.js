import { showPairing } from "./lifecycle.js";
import { state } from "./state.js";

// A non-secure origin — the plain-http LAN host — has no crypto.subtle, so the
// HttpOnly device cookie the server sets on this response is then its only
async function completePairing(pairingID, nonce) {
  const req = {
    pairing_id: pairingID,
    nonce,
    device_name: navigator.userAgent.includes("iPhone") ? "iPhone" : "Browser",
  };
  let privateKey = null;
  if (hasWebCrypto()) {
    showPairing("Generating a device key and proving QR possession.");
    const keys = await crypto.subtle.generateKey(
      { name: "ECDSA", namedCurve: "P-256" },
      true,
      ["sign", "verify"]
    );
    privateKey = keys.privateKey;
    req.public_key_jwk = await crypto.subtle.exportKey("jwk", keys.publicKey);
    req.signature = await sign(privateKey, nonce);
  } else {
    showPairing("Pairing this device; the Mac issues its cookie credential.");
  }
  const res = await fetch("/api/pairing/complete", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    credentials: "include",
    body: JSON.stringify(req),
  });
  if (!res.ok) {
    showPairing("Pairing failed: " + await res.text());
    throw new Error("pairing failed");
  }
  const paired = await res.json();
  localStorage.setItem("ibkrDeviceID", paired.device_id);
  if (privateKey) {
    await savePrivateKey(privateKey);
  }
}

// The device key must survive for a year or more. iOS evicts IndexedDB
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

// tryDeviceLogin returns "ok" when a fresh session was minted, "repair" when
// the app definitively rejected this device (only a new pairing can help),
// and "retry" for everything transient: network failures, relay 503s while
// transient failure must never read as "please re-pair" — that habit is what
async function tryDeviceLogin() {
  const deviceID = localStorage.getItem("ibkrDeviceID");
  const privateKey = hasWebCrypto() ? await loadPrivateKey() : null;
  // No key means no client-side credential exists — a cookie-only grant, or a
  // the request that 401'd, so re-pairing is the only move left.
  if (!deviceID || !privateKey) return "repair";
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
    return ch.status === 401 ? "repair" : "retry";
  }
  const challenge = await ch.json();
  const body = {
    device_id: deviceID,
    challenge: challenge.challenge,
    signature: await sign(privateKey, challenge.challenge),
  };
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

export { b64urlToBytes, bytesToB64url, completePairing, hasWebCrypto, loadPrivateKey, openDB, savePrivateKey, sign, tryDeviceLogin };
