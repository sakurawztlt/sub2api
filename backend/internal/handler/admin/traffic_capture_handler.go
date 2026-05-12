// 2026-05-12 R29 P6 backup-only traffic capture admin endpoint + 简单 HTML 查询页.
package admin

import (
	"net/http"
	"strconv"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// TrafficCaptureHandler — admin endpoint 查 traffic_captures 表 (backup 调试用).
type TrafficCaptureHandler struct {
	svc *service.TrafficCaptureService
}

func NewTrafficCaptureHandler(svc *service.TrafficCaptureService) *TrafficCaptureHandler {
	return &TrafficCaptureHandler{svc: svc}
}

// ListRecent GET /api/v1/admin/traffic-captures/recent?limit=100
func (h *TrafficCaptureHandler) ListRecent(c *gin.Context) {
	limit := 100
	if v := c.Query("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 500 {
			limit = n
		}
	}
	rows, err := h.svc.ListRecent(c.Request.Context(), limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	written, dropped, failed := h.svc.Stats()
	c.JSON(http.StatusOK, gin.H{
		"items":   rows,
		"count":   len(rows),
		"stats":   gin.H{"written": written, "dropped": dropped, "failed": failed},
		"enabled": h.svc.Enabled(),
	})
}

// SearchByRequestID GET /api/v1/admin/traffic-captures?request_id=xxx
func (h *TrafficCaptureHandler) SearchByRequestID(c *gin.Context) {
	reqID := c.Query("request_id")
	if reqID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "request_id required"})
		return
	}
	if !service.IsValidCaptureRequestID(reqID) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request_id (allowed: A-Za-z0-9 _-:., max 128)"})
		return
	}
	rows, err := h.svc.GetByRequestID(c.Request.Context(), reqID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": rows, "count": len(rows)})
}

// 简单 HTML 查询页 (跟 cc-lb /capture/ui 同款) — GET /api/v1/admin/traffic-captures/ui
// 全 textContent 渲染防 XSS, 复用 admin auth 不另作.
const trafficCaptureUIHTML = `<!doctype html>
<html lang="zh">
<head>
<meta charset="utf-8">
<title>sub2api traffic capture</title>
<style>
body{font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif;margin:20px;max-width:1400px;color:#222;background:#0f1115;color:#e4e4e7}
h1{font-size:18px;margin:0 0 12px}
input[type=text]{padding:6px 9px;font-family:ui-monospace,Menlo,monospace;border:1px solid #444;border-radius:4px;background:#181b22;color:#e4e4e7}
#reqid{width:480px}
button{padding:6px 14px;cursor:pointer;border:1px solid #444;background:#181b22;color:#e4e4e7;border-radius:4px;font-weight:500}
button:hover{background:#252830}
.row{display:flex;gap:8px;align-items:center;margin-bottom:12px;flex-wrap:wrap}
table{border-collapse:collapse;width:100%;margin-top:14px;font-size:12px;background:#13161c}
th,td{border:1px solid #252830;padding:5px 8px;text-align:left;vertical-align:top}
th{background:#181b22;font-weight:600;color:#a78bfa;font-size:11px;text-transform:uppercase;letter-spacing:0.05em}
tr.r{cursor:pointer}
tr.r:hover{background:#1f2430}
.detail{margin-top:24px;border-top:2px solid #252830;padding-top:14px}
.detail h3{font-size:13px;margin:14px 0 6px;color:#a78bfa;text-transform:uppercase;letter-spacing:0.05em}
pre{background:#181b22;border:1px solid #252830;padding:10px;border-radius:4px;font-size:11px;overflow:auto;max-height:420px;white-space:pre-wrap;word-break:break-word;color:#e4e4e7;font-family:ui-monospace,Menlo,monospace}
.error{color:#f87171;padding:8px;background:#2c0e10;border-radius:4px;border:1px solid #5a1e24}
.meta{color:#8b90a0;font-size:11px;font-family:ui-monospace,Menlo,monospace}
.tag{display:inline-block;padding:1px 7px;font-size:10px;background:#252830;color:#c7c9d1;border-radius:99px;margin:1px 3px 1px 0;font-family:ui-monospace,Menlo,monospace}
.tag-warn{background:#553a10;color:#fde68a}
.tag-danger{background:#5a1e24;color:#fecaca}
.tag-success{background:#155248;color:#86efac}
code{font-family:ui-monospace,Menlo,monospace}
</style>
</head>
<body>
<h1>sub2api traffic capture · backup 调试</h1>
<div class="row">
  <input type="text" id="reqid" placeholder="request_id (X-Newapi-Request-Id 或 X-Oneapi-Request-Id)" autocomplete="off">
  <button type="button" id="btn-search">查 request_id</button>
  <button type="button" id="btn-recent">最近 100 条</button>
  <span id="status" class="meta"></span>
</div>
<table id="results" style="display:none">
  <thead><tr>
    <th>ts</th><th>req_id</th><th>upstream_id</th><th>account</th><th>platform/type</th>
    <th>model</th><th>status</th><th>stream</th><th>use_time</th>
    <th>in/out/resp bytes</th>
  </tr></thead>
  <tbody id="resultsBody"></tbody>
</table>
<div id="detail" class="detail" style="display:none">
  <h2 id="detailTitle"></h2>
  <div id="detailMeta"></div>
  <h3>inbound body (客户原始)</h3>
  <pre id="detailInbound"></pre>
  <h3>outbound body (sub2api 改完发上游的)</h3>
  <pre id="detailOutbound"></pre>
  <h3>response body</h3>
  <pre id="detailResponse"></pre>
  <h3>outbound headers (sub2api → upstream, 已脱敏)</h3>
  <pre id="detailOutboundHdr"></pre>
  <h3>response headers</h3>
  <pre id="detailResponseHdr"></pre>
</div>
<script>
function setText(el, v){el.textContent=(v==null)?"":String(v)}
function clr(el){while(el.firstChild)el.removeChild(el.firstChild)}
function fmtTime(ms){if(!ms)return"-";return ms<1000?ms+"ms":(ms/1000).toFixed(1)+"s"}
function tag(label, kind){const s=document.createElement("span");s.className="tag"+(kind?" tag-"+kind:"");s.textContent=label;return s}
function truncMid(s,n){if(!s)return"-";if(s.length<=n)return s;return s.slice(0,Math.floor(n/2))+"…"+s.slice(-Math.floor(n/2))}
async function load(url){
  const status=document.getElementById("status");const table=document.getElementById("results");const tbody=document.getElementById("resultsBody");const detail=document.getElementById("detail");
  setText(status,"查询中...");table.style.display="none";detail.style.display="none";clr(tbody);
  try{
    const r=await fetch(url,{credentials:"same-origin"});
    if(!r.ok){const err=await r.json().catch(()=>({error:"HTTP "+r.status}));status.className="error";setText(status,err.error||"请求失败");return}
    const d=await r.json();const items=d.items||[];
    if(d.enabled===false){setText(status,"⚠ traffic capture 未启用 (env SUB2API_TRAFFIC_CAPTURE_ENABLED 缺) · 共 "+items.length+" 条历史")}
    else{setText(status,"共 "+items.length+" 条" + (d.stats?(" · written="+d.stats.written+" dropped="+d.stats.dropped+" failed="+d.stats.failed):""))}
    status.className="meta";
    if(items.length===0)return;
    table.style.display="";
    for(const c of items){
      const tr=document.createElement("tr");tr.className="r";tr.dataset.id=c.id;
      const cells=[
        new Date(c.ts).toLocaleTimeString(),
        truncMid(c.request_id||"",24),
        truncMid(c.upstream_request_id||"",24),
        c.account_id?("a"+c.account_id):"-",
        (c.platform||"-")+"/"+(c.account_type||"-"),
        truncMid(c.model||"",24),
        c.upstream_status,
        c.stream?"✓":"",
        fmtTime(c.use_time_ms),
        (c.inbound_body_bytes||0)+"/"+(c.outbound_body_bytes||0)+"/"+(c.response_body_bytes||0)
      ];
      for(const v of cells){const td=document.createElement("td");td.textContent=String(v);tr.appendChild(td)}
      tr.addEventListener("click",()=>renderDetail(c));tbody.appendChild(tr)
    }
  }catch(err){status.className="error";setText(status,err.message||"请求失败")}
}
function renderDetail(c){
  const d=document.getElementById("detail");d.style.display="";
  setText(document.getElementById("detailTitle"),"capture #"+c.id);
  const meta=document.getElementById("detailMeta");clr(meta);
  meta.appendChild(tag("req_id "+(c.request_id||"-")));
  meta.appendChild(tag("upstream_id "+(c.upstream_request_id||"-")));
  meta.appendChild(tag("model "+(c.model||"-")));
  meta.appendChild(tag("platform "+(c.platform||"-")));
  meta.appendChild(tag("type "+(c.account_type||"-")));
  meta.appendChild(tag("status "+c.upstream_status, c.upstream_status>=500?"danger":c.upstream_status>=400?"warn":"success"));
  meta.appendChild(tag("stream "+(c.stream?"yes":"no")));
  meta.appendChild(tag("use_time "+fmtTime(c.use_time_ms)));
  meta.appendChild(tag("client_ip "+(c.client_ip||"-")));
  if(c.inbound_body_truncated)meta.appendChild(tag("inbound ⚠ truncated","warn"));
  if(c.outbound_body_truncated)meta.appendChild(tag("outbound ⚠ truncated","warn"));
  if(c.response_body_truncated)meta.appendChild(tag("response ⚠ truncated","warn"));
  setText(document.getElementById("detailInbound"),c.inbound_body||"(无)");
  setText(document.getElementById("detailOutbound"),c.outbound_body||"(无)");
  setText(document.getElementById("detailResponse"),c.response_body||"(无)");
  setText(document.getElementById("detailOutboundHdr"),JSON.stringify(c.outbound_headers||{},null,2));
  setText(document.getElementById("detailResponseHdr"),JSON.stringify(c.response_headers||{},null,2));
  d.scrollIntoView({behavior:"smooth"})
}
document.getElementById("btn-search").addEventListener("click",()=>{const id=document.getElementById("reqid").value.trim();if(!id){alert("填 request_id");return}load("/api/v1/admin/traffic-captures?request_id="+encodeURIComponent(id))});
document.getElementById("btn-recent").addEventListener("click",()=>load("/api/v1/admin/traffic-captures/recent?limit=100"));
load("/api/v1/admin/traffic-captures/recent?limit=20");
</script>
</body></html>`

// UI GET /api/v1/admin/traffic-captures/ui
func (h *TrafficCaptureHandler) UI(c *gin.Context) {
	c.Header("Content-Type", "text/html; charset=utf-8")
	c.Header("X-Content-Type-Options", "nosniff")
	c.String(http.StatusOK, trafficCaptureUIHTML)
}
