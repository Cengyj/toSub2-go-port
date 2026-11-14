export const TRASH_PAGE_SIZE = 20;

export function canRestoreTrash(entry) {
  return entry?.state === "trashed" && entry.canRestore === true;
}

const timestamp = value => typeof value === "string" && value ? Date.parse(value) : NaN;
const compareID = (a, b) => String(a.id).localeCompare(String(b.id));

export function selectTrashPage(entries, { query = "", filter = "all", order = "newest", page = 1, pageSize = TRASH_PAGE_SIZE } = {}) {
  const needle = query.trim().toLowerCase();
  const matches = entries.filter(entry => (filter === "all" || filter === "restorable" && canRestoreTrash(entry) || filter === "purging" && entry.state === "purging")
    && (!needle || [entry.email, entry.id, ...entry.jobs.map(job => job.id)].some(value => String(value || "").toLowerCase().includes(needle))));
  matches.sort((a, b) => {
    if (order === "expiry") {
      const expiry = item => Number.isFinite(timestamp(item.expiresAt)) ? timestamp(item.expiresAt) : Infinity;
      if (expiry(a) !== expiry(b)) return expiry(a) - expiry(b);
    }
    const difference = timestamp(a.deletedAt) - timestamp(b.deletedAt);
    return (Number.isFinite(difference) && difference ? order === "oldest" ? difference : -difference : compareID(a, b));
  });
  const size = Number.isSafeInteger(pageSize) && pageSize > 0 ? Math.min(pageSize, 100) : TRASH_PAGE_SIZE;
  const totalPages = Math.max(1, Math.ceil(matches.length / size));
  const currentPage = Math.max(1, Math.min(Number.isSafeInteger(page) ? page : 1, totalPages));
  const start = (currentPage - 1) * size;
  return { rows: matches.slice(start, start + size), total: matches.length, page: currentPage, totalPages, start: matches.length ? start + 1 : 0, end: Math.min(start + size, matches.length) };
}

export function describeTrashDeadline(entry, settings, { uncertain = false, now = Date.now() } = {}) {
  if (entry.state === "purging") return { label: "清理未完成", detail: "剩余资料保留供重试，已不能恢复", tone: "danger", at: null };
  if (!settings) return { label: "等待保留策略", detail: "读取后确认清理时间", tone: "muted", at: null };
  if (uncertain || settings.persistenceWarning) return { label: "自动清理暂停", detail: "需在保留策略中重新确认保存", tone: "warning", at: null };
  if (settings.retentionDays === 0) return { label: "持续保留", detail: "自动清理未启用", tone: "muted", at: null };
  const expiry = timestamp(entry.expiresAt);
  if (!Number.isFinite(expiry)) return { label: "清理时间待确认", detail: "尚未获得有效到期时间", tone: "warning", at: null };
  const remaining = expiry - now;
  if (remaining <= 0) return { label: "已到期，待清理", detail: "等待后台检查，尚不表示已删除", tone: "warning", at: entry.expiresAt };
  const label = remaining < 86400000 ? "不足 1 天" : "剩余 " + Math.ceil(remaining / 86400000) + " 天";
  return { label, detail: "预计自动清理时间", tone: remaining < 86400000 ? "warning" : "muted", at: entry.expiresAt };
}

export function trashSelectionScope(entries, selected, visibleRows) {
  const selectedEntries = entries.filter(entry => selected.has(entry.id));
  const visible = new Set(visibleRows.map(entry => entry.id));
  return { selectedEntries, hiddenCount: selectedEntries.filter(entry => !visible.has(entry.id)).length,
    restorableCount: selectedEntries.filter(canRestoreTrash).length,
    allVisibleSelected: visibleRows.length > 0 && visibleRows.every(entry => selected.has(entry.id)),
    someVisibleSelected: visibleRows.some(entry => selected.has(entry.id)) };
}

// Confirm against the latest authoritative snapshot, not the list captured
// before a dialog was opened. A changed group must be reviewed again.
export function trashTargetsUnchanged(expected, entries, action) {
  const signature = entry => JSON.stringify([entry.email, entry.deletedAt, entry.state, entry.recordCount, entry.canRestore, entry.jobs.map(job => job.id).sort()]);
  return expected.every(record => {
    const current = entries.find(entry => entry.id === record.id);
    return current && signature(record) === signature(current) && (action !== "restore" || canRestoreTrash(current));
  });
}
