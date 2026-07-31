import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import vm from "node:vm";

const authSource = await readFile(new URL("../auth.js", import.meta.url), "utf8");

function loadAuthWithoutWebCrypto(pairingResponse) {
  const fetches = [];
  const storageWrites = [];
  const pairingMessages = [];
  let indexedDBCalls = 0;
  const context = vm.createContext({
    console,
    crypto: {},
    fetch: async (url, init) => {
      fetches.push({ url, init });
      return {
        ok: true,
        async json() { return pairingResponse; },
        async text() { return JSON.stringify(pairingResponse); },
      };
    },
    indexedDB: {
      open() {
        indexedDBCalls++;
        throw new Error("IndexedDB must not be used without WebCrypto");
      },
    },
    localStorage: {
      getItem() { return null; },
      setItem(key, value) { storageWrites.push([key, value]); },
    },
    navigator: { userAgent: "NoCrypto Browser" },
    showPairing(message) { pairingMessages.push(message); },
    state: {},
  });
  const executable = authSource
    .replace(/^import .*;\n/gm, "")
    .replace(/export \{([^}]+)\};\s*$/m, "globalThis.__exports = {$1};");
  vm.runInContext(executable, context, { filename: "auth.js" });
  return {
    context,
    exports: context.__exports,
    fetches,
    indexedDBCalls: () => indexedDBCalls,
    pairingMessages,
    storageWrites,
  };
}

test("crypto-less pairing stores only the device id and ignores hostile credentials", async () => {
  const harness = loadAuthWithoutWebCrypto({
    device_id: "device-123",
    device_secret: "readable-bearer-secret",
    token: "tempting-token",
    credential: { type: "bearer", value: "also-secret" },
  });

  assert.equal(harness.exports.hasWebCrypto(), false);
  await harness.exports.completePairing("pairing-456", "nonce-789");

  assert.equal(harness.fetches.length, 1);
  const call = harness.fetches[0];
  assert.equal(call.url, "/api/pairing/complete");
  assert.equal(call.init.method, "POST");
  assert.equal(call.init.credentials, "include");
  assert.deepEqual(Object.fromEntries(Object.entries(call.init.headers)), { "Content-Type": "application/json" });
  assert.deepEqual(JSON.parse(call.init.body), {
    pairing_id: "pairing-456",
    nonce: "nonce-789",
    device_name: "Browser",
  });
  assert.deepEqual(harness.storageWrites, [["ibkrDeviceID", "device-123"]]);
  assert.equal(harness.indexedDBCalls(), 0);
  assert.match(harness.pairingMessages.at(-1), /cookie credential/i);
});
