import { useState } from "react";
import { normalizeUploadTarget, readUploadTargetPreference, saveUploadTargetPreference } from "../lib/uploadTarget.js";

// Preference persistence does not authorize or start an upload.
export function useUploadPreference() {
  const [uploadTarget, setUploadTarget] = useState(readUploadTargetPreference);
  const [uploadTargetSaved, setUploadTargetSaved] = useState(true);
  function selectUploadTarget(value) {
    const target = normalizeUploadTarget(value);
    const saved = saveUploadTargetPreference(target);
    setUploadTarget(target);
    setUploadTargetSaved(saved);
    return saved;
  }
  return { uploadTarget, setUploadTarget, uploadTargetSaved, setUploadTargetSaved, selectUploadTarget };
}
