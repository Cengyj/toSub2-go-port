import React, { useEffect, useMemo, useRef, useState } from "react";
import { Download, ExternalLink, RotateCcw, X } from "lucide-react";
import { DialogFrame } from "./ui.jsx";
import { pelicanSizeMessage, preparePelicanPreview } from "../lib/pelicanPreview.js";

export function PelicanArtworkPreview({ output, mode = "fit" }) {
  const prepared = useMemo(() => preparePelicanPreview(output), [output]);
  const container = useRef(null), frame = useRef(null);
  const [available, setAvailable] = useState({ width: 0, height: 0 });
  const [measured, setMeasured] = useState(null);
  const artwork = measured?.channel === prepared.channel ? measured : prepared.viewport;
  useEffect(() => {
    let updates = 0;
    const resize = () => { if (container.current) setAvailable({ width: container.current.clientWidth, height: container.current.clientHeight }); };
    const receive = event => {
      const size = pelicanSizeMessage(event, frame.current?.contentWindow, prepared.channel);
      if (size && updates++ < 120) setMeasured({ ...size, channel: prepared.channel });
    };
    const observer = typeof ResizeObserver === "undefined" ? null : new ResizeObserver(resize);
    if (container.current) observer?.observe(container.current);
    window.addEventListener("resize", resize); window.addEventListener("message", receive); resize();
    return () => { observer?.disconnect(); window.removeEventListener("resize", resize); window.removeEventListener("message", receive); };
  }, [prepared]);
  const scale = mode === "actual" ? 1 : Math.max(0, Math.min(1, available.width / artwork.width, available.height / artwork.height));
  const innerScale = Math.min(1, prepared.viewport.width / artwork.width, prepared.viewport.height / artwork.height);
  return <div ref={container} className={`pelican-artwork-preview is-${mode}`}>
    <div className="pelican-preview-surface" style={{ minWidth: artwork.width * scale, minHeight: artwork.height * scale }}>
      <div className="pelican-preview-bounds" style={{ width: artwork.width * scale, height: artwork.height * scale }}>
        <iframe ref={frame} title="鹈鹕骑自行车作品预览" tabIndex={0} srcDoc={prepared.srcDoc} sandbox="allow-scripts" referrerPolicy="no-referrer" allow="camera 'none'; microphone 'none'; geolocation 'none'; clipboard-read 'none'; clipboard-write 'none'" scrolling="no" style={{ width: prepared.viewport.width, height: prepared.viewport.height, transform: `scale(${scale / innerScale})` }} />
      </div>
    </div>
  </div>;
}

export function PelicanPreview({ output }) {
  const [expanded, setExpanded] = useState(false), [mode, setMode] = useState("fit");
  function download() {
    const htmlDocument = /^<svg(?:\s|>)/i.test(output) ? `<!doctype html><html><head><meta charset="utf-8"><title>鹈鹕骑自行车</title></head><body>${output}</body></html>` : output;
    const url = URL.createObjectURL(new Blob([htmlDocument], { type: "text/html;charset=utf-8" }));
    const anchor = document.createElement("a");
    anchor.href = url; anchor.download = "pelican-bicycle.html"; anchor.click();
    window.setTimeout(() => URL.revokeObjectURL(url), 1000);
  }
  return <>
    <div className="pelican-preview-toolbar"><span>作品预览<small>HTML</small></span><div><button type="button" className="text-action" onClick={download}><Download size={13} />下载 HTML</button><button type="button" className="text-action" onClick={() => setExpanded(true)}><ExternalLink size={13} />放大</button></div></div>
    {expanded ? <div className="pelican-preview-suspended">作品已在放大窗口中打开</div> : <PelicanArtworkPreview output={output} />}
    <details className="pelican-source"><summary>查看源码<span>{(new TextEncoder().encode(output).length / 1024).toFixed(1)} KB</span></summary><pre><code>{output}</code></pre></details>
    {expanded && <DialogFrame onClose={() => setExpanded(false)}><section className="batch-dialog pelican-expanded-dialog" role="dialog" aria-modal="true" aria-labelledby="pelican-preview-title"><div className="pelican-expanded-header"><h2 id="pelican-preview-title">鹈鹕骑自行车<span>作品预览</span></h2><div><button type="button" className="text-action" onClick={() => setMode(mode === "fit" ? "actual" : "fit")}>{mode === "fit" ? <ExternalLink size={13} /> : <RotateCcw size={13} />}{mode === "fit" ? "原始大小" : "适应窗口"}</button><button type="button" className="icon-button" aria-label="关闭作品预览" onClick={() => setExpanded(false)}><X size={18} /></button></div></div><PelicanArtworkPreview output={output} mode={mode} /></section></DialogFrame>}
  </>;
}
