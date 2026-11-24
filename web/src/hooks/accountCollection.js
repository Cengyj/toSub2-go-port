import { useEffect, useMemo, useState } from "react";
import { normalizeAuthorizationMethod } from "../lib/authorizationMethod.js";
import { uploadBlockedJob } from "../lib/jobPresentation.js";

// The server supplies account capabilities. This hook only aggregates the
// current selection, including accounts outside the visible page.
export function useAccountCollection() {
  const [jobs, setJobs] = useState([]);
  const [expandedJobId, setExpandedJobId] = useState(null);
  const [selectedJobIds, setSelectedJobIds] = useState(() => new Set());
  const [jobSelectionIndex, setJobSelectionIndex] = useState([]);
  const [page, setPage] = useState(1);
  const [pagination, setPagination] = useState({ page: 1, pageSize: 20, total: 0, totalPages: 1 });
  const [stats, setStats] = useState({ active: 0, queued: 0, completed: 0 });
  const pageJobIds = useMemo(() => jobs.map((job) => job.id), [jobs]);
  const selectedJobs = useMemo(
    () => jobSelectionIndex.filter((job) => selectedJobIds.has(job.id)),
    [jobSelectionIndex, selectedJobIds],
  );
  const downloadableSelectedCount = selectedJobs.filter((job) => job.canDownload).length;
  const selectedAuthorizationMethod = selectedJobs.length && selectedJobs.every((job) => normalizeAuthorizationMethod(job.authorizationMethod) === normalizeAuthorizationMethod(selectedJobs[0].authorizationMethod)) ? normalizeAuthorizationMethod(selectedJobs[0].authorizationMethod) : "chatgpt";
  const allPageSelected = pageJobIds.length > 0 && pageJobIds.every((id) => selectedJobIds.has(id));
  const canDownloadSelected = selectedJobs.length > 0 && selectedJobs.length === selectedJobIds.size
    && downloadableSelectedCount > 0 && selectedJobs.every((job) => !job.totpExportBlocked);
  const canReauthorizeSelected = selectedJobs.length > 0 && selectedJobs.length === selectedJobIds.size
    && selectedJobs.every((job) => job.canRegenerate || job.canRetry);
  const hasImportedSelected = selectedJobs.some((job) => job.status === "imported");
  const forceReloginSelectedCount = selectedJobs.filter((job) => job.canForceRelogin).length;
  const canForceReloginSelected = selectedJobs.length > 0 && selectedJobs.length === selectedJobIds.size
    && forceReloginSelectedCount > 0;
  const uploadBlockedSelected = selectedJobs.some(uploadBlockedJob);
  const canUploadSelected = selectedJobs.length > 0 && selectedJobs.length === selectedJobIds.size
    && downloadableSelectedCount > 0 && !uploadBlockedSelected;
  const totpSetupSelectedCount = selectedJobs.filter((job) => job.canSetupTotp).length;
  const canSetupTotpSelected = selectedJobs.length > 0 && selectedJobs.length === selectedJobIds.size
    && totpSetupSelectedCount > 0;
  const totpResetSelectedCount = selectedJobs.filter((job) => job.canResetTotp).length;
  const canResetTotpSelected = selectedJobs.length > 0 && selectedJobs.length === selectedJobIds.size
    && totpResetSelectedCount > 0;
  const passwordAddSelectedCount = selectedJobs.filter((job) => job.canAddPassword).length;
  const canAddPasswordSelected = selectedJobs.length > 0 && selectedJobs.length === selectedJobIds.size
    && passwordAddSelectedCount > 0;
  useEffect(() => {
    const valid = new Set(jobSelectionIndex.map((job) => job.id));
    setSelectedJobIds((current) => {
      const next = new Set([...current].filter((id) => valid.has(id)));
      if (next.size === current.size && [...next].every((id) => current.has(id))) return current;
      return next;
    });
  }, [jobSelectionIndex]);

  useEffect(() => {
    setExpandedJobId(null);
  }, [page]);

  function toggleJobSelection(jobId) {
    setSelectedJobIds((current) => {
      const next = new Set(current);
      if (next.has(jobId)) next.delete(jobId);
      else next.add(jobId);
      return next;
    });
  }

  function toggleAllOnPage() {
    setSelectedJobIds((current) => {
      const next = new Set(current);
      pageJobIds.forEach((id) => {
        if (allPageSelected) next.delete(id);
        else next.add(id);
      });
      return next;
    });
  }

  return { jobs, setJobs, expandedJobId, setExpandedJobId, selectedJobIds, setSelectedJobIds, jobSelectionIndex, setJobSelectionIndex, page, setPage, pagination, setPagination, stats, setStats,
    pageJobIds, selectedJobs, downloadableSelectedCount, selectedAuthorizationMethod, allPageSelected, canDownloadSelected, canReauthorizeSelected, hasImportedSelected, forceReloginSelectedCount, canForceReloginSelected, uploadBlockedSelected, canUploadSelected, totpSetupSelectedCount, canSetupTotpSelected, totpResetSelectedCount, canResetTotpSelected, passwordAddSelectedCount, canAddPasswordSelected, toggleJobSelection, toggleAllOnPage };
}
