import { useState } from "react";
import { Shuffle, X } from "lucide-react";
import { useTranslation } from "react-i18next";
import { api } from "../api";
import type { AccountModelMismatch, AccountRow } from "../types";
import { formatRelativeTime } from "../utils/time";
import { Button } from "@/components/ui/button";

// 「上游回显的 model 与实际发给上游的 model 不一致」的只读标记。
// 纯展示：不影响调度、冷却或计费，只帮管理员发现上游悄悄换模型。

function describeMismatch(item: AccountModelMismatch, times: string): string {
  const seen = item.last_seen_at ? ` · ${formatRelativeTime(item.last_seen_at)}` : "";
  return `${item.model} → ${item.upstream_model} (${times})${seen}`;
}

export function ModelMismatchBadge({ account }: { account: AccountRow }) {
  const { t } = useTranslation();
  const items = account.model_mismatches ?? [];
  if (items.length === 0) return null;
  const title = [
    t("accounts.modelMismatchTooltip"),
    ...items.map((item) =>
      describeMismatch(item, t("accounts.modelMismatchHits", { count: item.hit_count })),
    ),
  ].join("\n");
  return (
    <span
      className="inline-flex h-5 shrink-0 items-center gap-1 rounded-md border border-fuchsia-500/30 bg-fuchsia-500/10 px-1.5 text-[11px] font-medium tabular-nums text-fuchsia-700 dark:text-fuchsia-300"
      title={title}
      aria-label={t("accounts.modelMismatchBadge")}
    >
      <Shuffle className="size-3" />
      {items.length}
    </span>
  );
}

export function ModelMismatchList({
  account,
  onCleared,
}: {
  account: AccountRow;
  onCleared?: () => void;
}) {
  const { t } = useTranslation();
  const [clearedFor, setClearedFor] = useState<number | null>(null);
  const [clearing, setClearing] = useState(false);
  const items = account.model_mismatches ?? [];
  if (items.length === 0 || clearedFor === account.id) return null;

  const handleClear = async () => {
    setClearing(true);
    try {
      await api.clearAccountModelMismatches(account.id);
      setClearedFor(account.id);
      onCleared?.();
    } catch {
      // 清除失败时标记保持原样，下次刷新仍可见；这里不需要额外提示。
    } finally {
      setClearing(false);
    }
  };

  return (
    <div className="rounded-lg bg-fuchsia-500/10 px-2.5 py-2 text-[12px] text-fuchsia-700 dark:text-fuchsia-300">
      <div className="flex items-center justify-between gap-2">
        <span className="inline-flex items-center gap-1.5 font-medium">
          <Shuffle className="size-3.5 shrink-0" />
          {t("accounts.modelMismatchBadge")}
        </span>
        <Button
          type="button"
          variant="ghost"
          size="xs"
          disabled={clearing}
          onClick={() => void handleClear()}
          className="h-6 text-[11px] text-fuchsia-700 dark:text-fuchsia-300"
        >
          <X className="size-3" />
          {t("accounts.clearModelMismatches")}
        </Button>
      </div>
      <p className="mt-1 text-[11px] leading-snug opacity-80">{t("accounts.modelMismatchTooltip")}</p>
      <ul className="mt-1.5 space-y-1">
        {items.map((item) => (
          <li key={item.model} className="break-all font-mono text-[11px]">
            {describeMismatch(item, t("accounts.modelMismatchHits", { count: item.hit_count }))}
          </li>
        ))}
      </ul>
    </div>
  );
}
