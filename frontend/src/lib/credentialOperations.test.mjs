import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";

test("2FA parallelism has a dedicated persistent system settings control", () => {
  const settings = readFileSync(new URL("../pages/Settings.tsx", import.meta.url), "utf8");
  const control = readFileSync(new URL("../components/CredentialOperationsSettings.tsx", import.meta.url), "utf8");
  assert.match(settings, /<CredentialOperationsSettings/);
  assert.match(control, /api.getCredentialOperationsSettings/);
  assert.match(control, /api.updateCredentialOperationsSettings/);
  assert.match(control, /Number.isInteger/);
  assert.doesNotMatch(control, /localStorage|sessionStorage/);
});

const panel = readFileSync(
  new URL("../components/TwoFAImport.tsx", import.meta.url),
  "utf8",
);
const page = readFileSync(
  new URL("../pages/Accounts.tsx", import.meta.url),
  "utf8",
);
test("2FA importer is reachable and does not persist secrets in browser storage", () => {
  assert.match(page, /<TwoFAImport/);
  assert.match(panel, /setContent[(]["']["'][)]/);
  assert.doesNotMatch(panel, /localStorage|sessionStorage|console./);
  assert.match(panel, /enrollment_pending/);
  assert.match(panel, /login_failed/);
  assert.match(panel, /worker_ready/);
});
