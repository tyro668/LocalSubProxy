// Package saas is the mock business service (the role of the missing
// sophorn-mock-server): user login, the "link a device" page where a signed-in
// user types the code shown by the desktop connector, a device list, and a
// chat page whose backend proxies the gateway's OpenAI-compatible stream.
// The tenant API key stays server-side; browsers only hold a session cookie.
package saas

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type Config struct {
	PublicURL        string // this service's external origin (used in link_url)
	GatewayURL       string // gateway as reached from this backend
	PublicGatewayURL string // gateway as reached from desktops (defaults to GatewayURL)
	TenantID         string
	TenantAPIKey     string
	Users            map[string]string // user → password (mock only)
}

type Server struct {
	cfg      Config
	mu       sync.Mutex
	sessions map[string]string
	// pins records, per user, the device key thumbprint (jkt) received through
	// the pairing link; it anchors trust in the device's E2EE key.
	pins   map[string]map[string]string
	client *http.Client
}

func New(cfg Config) *Server {
	if cfg.PublicGatewayURL == "" {
		cfg.PublicGatewayURL = cfg.GatewayURL
	}
	return &Server{cfg: cfg, sessions: map[string]string{}, pins: map[string]map[string]string{}, client: &http.Client{}}
}

func (s *Server) Handler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("/.well-known/lsp-connector", s.discovery)
	m.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"status":"ok"}`)) })
	m.HandleFunc("/login", s.login)
	m.HandleFunc("/logout", s.logout)
	m.HandleFunc("/", s.auth(s.home))
	m.HandleFunc("/link", s.auth(s.link))
	m.HandleFunc("/api/devices", s.auth(s.proxy("GET", "/lmbp/v1/tenant/devices")))
	m.HandleFunc("/api/models", s.auth(s.proxy("GET", "/v1/models")))
	m.HandleFunc("/api/chat", s.auth(s.proxy("POST", "/v1/chat/completions")))
	m.HandleFunc("/api/chat/e2ee", s.auth(s.e2eeChat))
	m.HandleFunc("/api/devices/revoke", s.auth(s.revoke))
	return m
}

// discovery is public and contains no secrets and no codes.
func (s *Server) discovery(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"gateway_url": s.cfg.PublicGatewayURL, "tenant": s.cfg.TenantID, "link_url": strings.TrimRight(s.cfg.PublicURL, "/") + "/link", "protocol": "lmbp"})
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		page.ExecuteTemplate(w, "login", map[string]string{"Next": r.URL.Query().Get("next")})
		return
	}
	u, p := r.FormValue("user"), r.FormValue("password")
	want, ok := s.cfg.Users[u]
	if !ok || subtle.ConstantTimeCompare([]byte(want), []byte(p)) != 1 {
		w.WriteHeader(401)
		page.ExecuteTemplate(w, "login", map[string]string{"Error": "用户名或密码错误"})
		return
	}
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	sid := hex.EncodeToString(b)
	s.mu.Lock()
	s.sessions[sid] = u
	s.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: "sid", Value: sid, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, Expires: time.Now().Add(12 * time.Hour)})
	next := r.FormValue("next")
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		next = "/"
	}
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie("sid"); err == nil {
		s.mu.Lock()
		delete(s.sessions, c.Value)
		s.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: "sid", Value: "", Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

type userHandler func(w http.ResponseWriter, r *http.Request, user string)

func (s *Server) auth(h userHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user := ""
		if c, err := r.Cookie("sid"); err == nil {
			s.mu.Lock()
			user = s.sessions[c.Value]
			s.mu.Unlock()
		}
		if user == "" {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				http.Error(w, `{"error":{"message":"login required","code":"UNAUTHENTICATED"}}`, 401)
				return
			}
			http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
			return
		}
		// CSRF: SameSite=Strict cookie plus an Origin check on state-changing requests.
		if r.Method != http.MethodGet {
			if o := r.Header.Get("Origin"); o != "" && o != strings.TrimRight(s.cfg.PublicURL, "/") {
				http.Error(w, "cross-origin request rejected", 403)
				return
			}
		}
		h(w, r, user)
	}
}

func (s *Server) gw(r *http.Request, method, path string, body io.Reader, user string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(r.Context(), method, s.cfg.GatewayURL+path, body)
	if err != nil {
		return nil, err
	}
	for _, h := range []string{"Idempotency-Key", "Last-Event-ID"} {
		if v := r.Header.Get(h); v != "" {
			req.Header.Set(h, v)
		}
	}
	req.Header.Set("Authorization", "Bearer "+s.cfg.TenantAPIKey)
	req.Header.Set("X-End-User", user) // identity comes from our session, never from the browser
	req.Header.Set("Content-Type", "application/json")
	return s.client.Do(req)
}

// proxy forwards to the gateway and streams the response through unbuffered.
func (s *Server) proxy(method, path string) userHandler {
	return func(w http.ResponseWriter, r *http.Request, user string) {
		if r.Method != method {
			http.Error(w, "method not allowed", 405)
			return
		}
		var body io.Reader
		if method == http.MethodPost {
			body = http.MaxBytesReader(w, r.Body, 40<<20) // images travel inline as data: URIs
		}
		resp, err := s.gw(r, method, path, body, user)
		if err != nil {
			http.Error(w, `{"error":{"message":"gateway unreachable","code":"DEVICE_OFFLINE"}}`, 502)
			return
		}
		defer resp.Body.Close()
		for _, h := range []string{"Content-Type", "Cache-Control", "X-Task-Id", "Retry-After"} {
			if v := resp.Header.Get(h); v != "" {
				w.Header().Set(h, v)
			}
		}
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(resp.StatusCode)
		f, _ := w.(http.Flusher)
		buf := make([]byte, 4096)
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				_, _ = w.Write(buf[:n])
				if f != nil {
					f.Flush()
				}
			}
			if err != nil {
				return
			}
		}
	}
}

func (s *Server) link(w http.ResponseWriter, r *http.Request, user string) {
	data := map[string]any{"User": user, "Code": r.URL.Query().Get("user_code"), "JKT": r.URL.Query().Get("jkt")}
	if r.Method == http.MethodPost {
		jkt := r.FormValue("jkt")
		b, _ := json.Marshal(map[string]any{"user_code": r.FormValue("user_code"), "deny": r.FormValue("deny") == "1", "jkt": jkt})
		resp, err := s.gw(r, "POST", "/lmbp/v1/tenant/device-approvals", strings.NewReader(string(b)), user)
		if err != nil {
			data["Error"] = "网关不可达"
		} else {
			var out map[string]any
			_ = json.NewDecoder(resp.Body).Decode(&out)
			resp.Body.Close()
			if resp.StatusCode == 200 {
				data["Result"] = out
				if dev, _ := out["device_id"].(string); dev != "" && jkt != "" && out["status"] == "approved" {
					s.mu.Lock()
					if s.pins[user] == nil {
						s.pins[user] = map[string]string{}
					}
					s.pins[user][dev] = jkt // the gateway verified it matches the paired key
					s.mu.Unlock()
				}
			} else {
				e, _ := out["error"].(map[string]any)
				data["Error"] = e["message"]
			}
			if wantsJSON(r) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(resp.StatusCode)
				_ = json.NewEncoder(w).Encode(out)
				return
			}
		}
	}
	page.ExecuteTemplate(w, "link", data)
}

func (s *Server) revoke(w http.ResponseWriter, r *http.Request, user string) {
	id := r.FormValue("device_id")
	if r.Method != http.MethodPost || !strings.HasPrefix(id, "dev_") {
		http.Error(w, "bad request", 400)
		return
	}
	s.proxy("POST", "/lmbp/v1/tenant/devices/"+id+"/revoke")(w, r, user)
}

func (s *Server) home(w http.ResponseWriter, r *http.Request, user string) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	page.ExecuteTemplate(w, "home", map[string]string{"User": user, "Tenant": s.cfg.TenantID, "Public": s.cfg.PublicURL})
}

func wantsJSON(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "application/json")
}

var page = template.Must(template.New("").Parse(`
{{define "head"}}<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><title>Mock SaaS · 本地模型</title>
<style>body{font-family:"PingFang SC","Microsoft YaHei","Noto Sans CJK SC",sans-serif;max-width:860px;margin:32px auto;padding:0 20px;color:#1f2328}
input,textarea,select,button{font:inherit;padding:6px 10px}button{background:#2f6feb;color:#fff;border:0;border-radius:6px;cursor:pointer}
.card{border:1px solid #d0d7de;border-radius:10px;padding:16px;margin:16px 0}.code{font-size:28px;letter-spacing:4px;font-family:monospace}
pre{background:#0d1117;color:#e6edf3;padding:12px;border-radius:8px;white-space:pre-wrap;min-height:60px}.err{color:#c62828}.ok{color:#2e7d32}</style></head><body>{{end}}
{{define "login"}}{{template "head"}}<h2>登录 Mock SaaS</h2>{{if .Error}}<p class="err">{{.Error}}</p>{{end}}
<form method="post" action="/login"><input type="hidden" name="next" value="{{.Next}}"><p><input name="user" placeholder="用户名"></p>
<p><input name="password" type="password" placeholder="密码"></p><button>登录</button></form></body></html>{{end}}
{{define "link"}}{{template "head"}}<p>当前用户：<b>{{.User}}</b> · <a href="/">返回</a></p>
<div class="card"><h3>连接本机设备</h3><p>在桌面连接器上会显示一个 8 位代码（如 <span class="code">BCDF-GHJK</span>），请在这里输入并确认。只有你登录的账号能认领这台设备。</p>
{{if .Result}}<p class="ok">已{{if eq (index .Result "status") "approved"}}连接{{else}}拒绝{{end}}设备：{{index .Result "device_name"}} {{index .Result "device_id"}}</p>{{end}}
{{if .Error}}<p class="err">{{.Error}}</p>{{end}}
<form method="post" action="/link"><input type="hidden" name="jkt" value="{{.JKT}}"><input name="user_code" value="{{.Code}}" placeholder="XXXX-XXXX" class="code" size="10" autocomplete="off">
<button>确认连接</button> <button name="deny" value="1" style="background:#888">拒绝</button></form></div></body></html>{{end}}
{{define "home"}}{{template "head"}}<p>当前用户：<b>{{.User}}</b>（租户 {{.Tenant}}）· <a href="/link">连接新设备</a> · <form style="display:inline" method="post" action="/logout"><button style="background:#888">退出</button></form></p>
<div class="card"><h3>我的设备</h3><div id="devices">加载中…</div>
<p style="color:#666">桌面连接器里填写服务地址 <code>{{.Public}}</code> 即可开始配对。</p></div>
<div class="card"><h3>对话（经网关路由到你的本机模型）</h3><p><select id="model"></select></p>
<textarea id="prompt" rows="3" style="width:100%">你好，请介绍一下你自己</textarea><p><button id="send">发送</button> <button id="stop" style="background:#888">停止</button></p><pre id="out"></pre></div>
<script>
async function load(){const d=await (await fetch('/api/devices')).json();const el=document.getElementById('devices');
el.innerHTML=(d.devices||[]).map(x=>'<div>'+(x.online?'🟢':'⚪')+' <b>'+x.name+'</b> <code>'+x.device_id+'</code> '+x.status+' · 模型: '+(x.models||[]).join(', ')+'</div>').join('')||'还没有设备';
const m=await (await fetch('/api/models')).json();document.getElementById('model').innerHTML=(m.data||[]).map(x=>'<option>'+x.id+'</option>').join('');}
let ctl;document.getElementById('send').onclick=async()=>{const out=document.getElementById('out');out.textContent='';ctl=new AbortController();
const r=await fetch('/api/chat',{method:'POST',signal:ctl.signal,headers:{'Content-Type':'application/json'},body:JSON.stringify({model:document.getElementById('model').value,stream:true,messages:[{role:'user',content:document.getElementById('prompt').value}]})});
if(!r.ok){out.textContent='错误: '+await r.text();return}const rd=r.body.getReader();const dec=new TextDecoder();let buf='';
for(;;){const {value,done}=await rd.read();if(done)break;buf+=dec.decode(value,{stream:true});let i;while((i=buf.indexOf('\n\n'))>=0){const ev=buf.slice(0,i);buf=buf.slice(i+2);
if(!ev.startsWith('data: '))continue;const p=ev.slice(6);if(p==='[DONE]')return;const j=JSON.parse(p);if(j.error){out.textContent+='\n[错误 '+j.error.code+'] '+j.error.message;return}
out.textContent+=(j.choices[0].delta.content||'');}}};
document.getElementById('stop').onclick=()=>ctl&&ctl.abort();load();setInterval(load,5000);
</script></body></html>{{end}}`))
