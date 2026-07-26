package gateway

import (
	"fmt"
	"html"
	"net/http"
)

// pageShell is the gateway's own look: the same night-stable palette as the
// admin console, so an error in front of an app still looks like donkey.
const pageShell = `<!doctype html>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>%s — donkey</title>
<style>
  :root{color-scheme:dark}
  body{margin:0;min-height:100vh;display:flex;align-items:center;justify-content:center;
       background:#16120e;color:#ece5dc;
       font:15px/1.6 ui-sans-serif,system-ui,-apple-system,"Segoe UI",sans-serif}
  .card{max-width:26rem;padding:2.4rem 2.6rem;border:1px solid #332a22;border-radius:14px;
        background:#1c1712;box-shadow:0 20px 60px rgba(0,0,0,.45);text-align:center}
  .mark{font-size:2rem}
  h1{font-size:1.05rem;margin:.8rem 0 .4rem;font-weight:600}
  p{margin:.3rem 0;color:#a89c8e;font-size:.88rem}
  code{font-family:ui-monospace,"SF Mono",Menlo,monospace;font-size:.85em;color:#e8a33d}
  .foot{margin-top:1.5rem;font-size:.72rem;color:#776c60;letter-spacing:.08em;text-transform:uppercase}
  a{color:#e8a33d;text-decoration:none}
</style>
<div class="card">
  <div class="mark">🫏</div>
  <h1>%s</h1>
  <p>%s</p>
  <div class="foot">donkey gateway</div>
</div>`

func writePage(w http.ResponseWriter, status int, title, detail string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	fmt.Fprintf(w, pageShell, html.EscapeString(title), html.EscapeString(title), html.EscapeString(detail))
}

func (s *Server) errorPage(w http.ResponseWriter, status int, title, detail string) {
	writePage(w, status, title, detail)
}

func (s *Server) messagePage(w http.ResponseWriter, status int, title, detail string) {
	writePage(w, status, title, detail)
}

func (s *Server) deniedPage(w http.ResponseWriter, who string) {
	writePage(w, http.StatusForbidden,
		"This app is restricted.",
		fmt.Sprintf("You're signed in as %s, but this app's access list doesn't include you. Ask whoever runs it to add you — or sign out at /oauth2/logout.", who))
}
