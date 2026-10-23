import { useEffect, useState } from "react";
import { api } from "../api";
import {
  intelligenceLabels,
  type IntelligenceRun,
} from "../lib/intelligenceCheck";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from "./ui/dialog";
import { Button } from "./ui/button";
import { getErrorMessage } from "../utils/error";

export default function IntelligenceRunDialog({
  id,
  onClose,
}: {
  id: number | null;
  onClose: () => void;
}) {
  const [run, setRun] = useState<IntelligenceRun | null>(null);
  const [error, setError] = useState("");
  const [retry, setRetry] = useState(0);
  useEffect(() => {
    setRun(null);
    setError("");
    if (!id) return;
    const controller = new AbortController();
    let timer: ReturnType<typeof window.setTimeout> | undefined;
    const load = async () => {
      try {
        const { run } = await api.getIntelligenceRun(id, controller.signal);
        if (controller.signal.aborted) return;
        setRun(run);
        setError("");
        if (run.status === "queued" || run.status === "running")
          timer = window.setTimeout(() => {
            void load();
          }, 4000);
      } catch (e) {
        if (!controller.signal.aborted) setError(getErrorMessage(e));
      }
    };
    void load();
    return () => {
      controller.abort();
      window.clearTimeout(timer);
    };
  }, [id, retry]);
  return (
    <Dialog
      open={!!id}
      onOpenChange={(open) => {
        if (!open) onClose();
      }}
    >
      <DialogContent className="sm:max-w-3xl">
        <DialogHeader>
          <DialogTitle>检测详情 #{id}</DialogTitle>
          <DialogDescription>
            保存本次题目、判定配置和最终可见答案。
          </DialogDescription>
        </DialogHeader>
        {error && (
          <div role="alert" className="text-destructive">
            {error}
            <Button variant="outline" onClick={() => setRetry((n) => n + 1)}>
              重试
            </Button>
          </div>
        )}
        {!run && !error && <p className="text-muted-foreground">正在加载…</p>}
        {run && (
          <div className="space-y-4 text-sm">
            <dl className="grid grid-cols-2 gap-3 sm:grid-cols-3">
              {[
                ["账号", `${run.account_name} (#${run.account_id})`],
                ["结果", intelligenceLabels[run.status]],
                ["请求模型", run.model],
                ["响应模型", run.response_model || "—"],
                ["推理强度", run.reasoning_effort || "默认"],
                [
                  "判定方式",
                  { answer: "仅答案", latency: "仅耗时", both: "答案和耗时" }[
                    run.judge_mode
                  ],
                ],
                [
                  "耗时 / 阈值",
                  `${run.duration_ms ?? 0} / ${run.question.max_duration_ms} ms`,
                ],
                ["配置版本", String(run.config_revision)],
                ["发起方式", run.source === "manual" ? "手动" : "自动"],
                [
                  "输入 / 输出 / 推理 token",
                  `${run.input_tokens} / ${run.output_tokens} / ${run.reasoning_tokens}`,
                ],
                [
                  "开始时间",
                  run.started_at
                    ? new Date(run.started_at).toLocaleString()
                    : "尚未开始",
                ],
                ["处置", intelligenceLabels[run.action] || run.action || "无"],
              ].map(([label, value]) => (
                <div key={label}>
                  <dt className="text-xs text-muted-foreground">{label}</dt>
                  <dd className="mt-1 break-words">{value}</dd>
                </div>
              ))}
            </dl>
            <div>
              <h3 className="mb-1 font-medium">{run.question.name}</h3>
              <pre className="whitespace-pre-wrap break-words rounded-lg border bg-muted/30 p-3 font-sans">
                {run.question.prompt}
              </pre>
            </div>
            <p>
              预期答案：<code>{run.question.expected_answer}</code> · 匹配方式：
              {run.question.match_mode}
            </p>
            <div>
              <h3 className="mb-1 font-medium">最终答案</h3>
              <pre className="max-h-72 overflow-auto whitespace-pre-wrap break-words rounded-lg border bg-muted/30 p-3 font-sans">
                {run.output || "暂无答案"}
              </pre>
            </div>
            {run.action_message && (
              <p className="text-muted-foreground">{run.action_message}</p>
            )}
            {run.error && (
              <p
                role="alert"
                className="whitespace-pre-wrap break-words text-destructive"
              >
                {run.error}
              </p>
            )}
            {run.effective_context && (
              <details>
                <summary className="cursor-pointer text-muted-foreground">
                  生效出站配置
                </summary>
                <pre className="mt-2 whitespace-pre-wrap break-words text-xs">
                  {JSON.stringify(run.effective_context, null, 2)}
                </pre>
              </details>
            )}
          </div>
        )}
      </DialogContent>
    </Dialog>
  );
}
