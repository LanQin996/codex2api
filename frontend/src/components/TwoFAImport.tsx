import { useEffect, useState } from "react";
import { api, type CredentialOperation } from "../api";
import { Button } from "./ui/button";

const stateLabels: Record<string, string> = {
  queued: "等待登录",
  enrollment_pending: "登录成功，等待完成登记",
  enrolled: "已导入并启用凭证运营",
  login_failed: "未完成，需要重试",
};

export default function TwoFAImport({ onUpdated }: { onUpdated: () => void }) {
  const [content, setContent] = useState("");
  const [proxyURL, setProxyURL] = useState("");
  const [replaceExisting, setReplaceExisting] = useState(false);
  const [rows, setRows] = useState<CredentialOperation[]>([]);
  const [ready, setReady] = useState(false);
  const [notice, setNotice] = useState("正在检查加密和 Worker 状态…");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const refresh = async () => {
    const result = await api.credentialOperations();
    setRows(result.items);
    setReady(result.encryption_ready && result.worker_ready);
    setNotice(
      !result.encryption_ready
        ? "请配置 CREDENTIAL_OPS_KEY（Base64 编码的 32 字节密钥）并重启服务。"
        : !result.worker_ready
          ? "本地 Worker 未就绪：请配置 TOSUB2_ROOT、安装 Node.js 并重启服务。"
          : "密码和 TOTP 加密保存；导入后默认启用巡检和自动重登。",
    );
  };
  useEffect(() => {
    let active = true;
    let previous: string | null = null;
    const poll = async () => {
      try {
        const result = await api.credentialOperations();
        if (!active) return;
        setRows(result.items);
        setReady(result.encryption_ready && result.worker_ready);
        setNotice(
          !result.encryption_ready
            ? "请配置 CREDENTIAL_OPS_KEY（Base64 编码的 32 字节密钥）并重启服务。"
            : !result.worker_ready
              ? "本地 Worker 未就绪：请配置 TOSUB2_ROOT、安装 Node.js 并重启服务。"
              : "密码和 TOTP 加密保存；导入后默认启用巡检和自动重登。",
        );
        const signature = result.items
          .filter((row) => row.state === "enrolled")
          .map((row) => row.id)
          .join(",");
        if (previous !== null && signature !== previous) onUpdated();
        previous = signature;
      } catch {
        if (active) setError("无法读取任务状态，请稍后刷新。");
      }
    };
    void poll();
    const timer = setInterval(() => void poll(), 5000);
    return () => {
      active = false;
      clearInterval(timer);
    };
  }, []);
  const submit = async () => {
    setBusy(true);
    setError("");
    try {
      await api.importCredentialOperations(content, proxyURL, replaceExisting);
      setContent("");
      setReplaceExisting(false);
      await refresh();
    } catch {
      setError(
        "提交未完成，请检查格式并刷新任务列表。已登记的行不会重复建号。",
      );
      await refresh().catch(() => {});
    } finally {
      setBusy(false);
    }
  };
  const control = async (id: string, action: "pause" | "resume" | "retry") => {
    setBusy(true);
    setError("");
    try {
      await api.controlCredentialOperation(id, action);
      await refresh();
      onUpdated();
    } catch {
      setError("操作未完成，任务可能正在执行；请刷新后重试。");
    } finally {
      setBusy(false);
    }
  };
  return (
    <section className="space-y-4">
      <p className="text-sm text-muted-foreground">{notice}</p>
      <label className="block text-sm font-medium" htmlFor="twofa-content">
        每行：邮箱----密码----TOTP 密钥（不是六位验证码）
      </label>
      <textarea
        id="twofa-content"
        className="w-full min-h-32 rounded-xl border border-input bg-background p-3 font-mono text-sm"
        value={content}
        onChange={(event) => setContent(event.target.value)}
        autoComplete="off"
        autoCorrect="off"
        spellCheck={false}
        placeholder="user@example.com----your-password----BASE32_SECRET"
        disabled={busy}
      />
      <label className="block text-sm" htmlFor="twofa-proxy">
        首次登录代理（可选，留空使用全局代理）
      </label>
      <input
        id="twofa-proxy"
        className="w-full rounded-xl border border-input bg-background p-2 text-sm"
        value={proxyURL}
        onChange={(event) => setProxyURL(event.target.value)}
        autoComplete="off"
        disabled={busy}
      />
      <p className="text-xs text-muted-foreground">
        最多 100
        行。提交任务不代表导入成功；刷新页面后可继续查看任务和补登记。重复邮箱保留原登录资料，不会静默覆盖。暂停会阻止后续执行及在途结果写回。
      </p>
      <label className="flex items-center gap-2 text-sm">
        <input
          type="checkbox"
          checked={replaceExisting}
          disabled={busy}
          onChange={(event) => setReplaceExisting(event.target.checked)}
        />
        更新已有邮箱的登录资料并重新登录（保留账号，不重复创建）
      </label>
      {error && (
        <p role="alert" className="text-sm text-destructive">
          {error}
        </p>
      )}
      <Button
        disabled={!ready || busy || !content.trim()}
        onClick={() => void submit()}
      >
        {busy ? "处理中…" : "加密登记并导入"}
      </Button>
      <div className="max-h-72 space-y-2 overflow-auto" aria-live="polite">
        {rows.map((row) => (
          <div
            key={row.id}
            className="rounded-xl border border-border p-3 text-sm"
          >
            <div className="break-all font-medium">
              {row.email}
              {row.account_id > 0 ? ` · #${row.account_id}` : ""}
            </div>
            <p>
              {!row.enabled
                ? "已暂停"
                : row.lease_until > Date.now() / 1000
                  ? "Worker 执行中 / 等待租约释放"
                  : (stateLabels[row.state] ?? row.state)}
            </p>
            {row.message && (
              <p className="text-xs text-muted-foreground">{row.message}</p>
            )}
            <div className="mt-2 flex gap-2">
              <Button
                size="sm"
                variant="outline"
                disabled={busy}
                onClick={() =>
                  void control(row.id, row.enabled ? "pause" : "resume")
                }
              >
                {row.enabled ? "暂停" : "恢复"}
              </Button>
              {(row.state === "login_failed" ||
                row.state === "enrollment_pending") && (
                <Button
                  size="sm"
                  variant="outline"
                  disabled={
                    busy ||
                    !ready ||
                    !row.enabled ||
                    row.lease_until > Date.now() / 1000
                  }
                  onClick={() => void control(row.id, "retry")}
                >
                  重试 / 补登记
                </Button>
              )}
            </div>
          </div>
        ))}
      </div>
    </section>
  );
}
