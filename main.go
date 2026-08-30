package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io"
	"log"
	"mime/multipart"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	qrcode "github.com/skip2/go-qrcode"
)

const maxUploadSize int64 = 10 << 30

type app struct {
	dir      string
	token    string
	phoneURL string
	qrCode   []byte
	quit     chan struct{}
	quitOnce sync.Once
}

type fileInfo struct {
	Name     string    `json:"name"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
}

func main() {
	defaultDir, err := defaultTransferDir()
	if err != nil {
		log.Fatal(err)
	}
	dir := flag.String("dir", defaultDir, "folder used for uploads and downloads")
	port := flag.Int("port", 0, "TCP port (0 chooses an available port)")
	flag.Parse()

	absDir, err := filepath.Abs(*dir)
	if err != nil {
		log.Fatal(err)
	}
	if err := os.MkdirAll(absDir, 0o755); err != nil {
		log.Fatalf("create transfer folder: %v", err)
	}
	token, err := newToken()
	if err != nil {
		log.Fatalf("create session token: %v", err)
	}
	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", *port))
	if err != nil {
		log.Fatal(err)
	}
	actualPort := listener.Addr().(*net.TCPAddr).Port
	phoneURL := fmt.Sprintf("http://%s:%d/%s/", localIP(), actualPort, token)
	qrCode, err := qrcode.Encode(phoneURL, qrcode.Medium, 384)
	if err != nil {
		log.Fatalf("create QR code: %v", err)
	}
	application := &app{dir: absDir, token: token, phoneURL: phoneURL, qrCode: qrCode, quit: make(chan struct{})}
	server := &http.Server{Handler: application.routes(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}
	dashboardURL := fmt.Sprintf("http://localhost:%d/", actualPort)
	log.Printf("Sharing %s", absDir)
	log.Printf("Scan the QR code at %s", dashboardURL)
	log.Printf("Phone URL: %s", phoneURL)
	log.Print("Keep both devices on the same trusted Wi-Fi network. Press Ctrl+C to stop.")
	if err := openBrowser(dashboardURL); err != nil {
		log.Printf("Could not open browser: %v", err)
	}
	serveErrors := make(chan error, 1)
	go func() { serveErrors <- server.Serve(listener) }()
	select {
	case <-application.quit:
		_ = server.Close()
	case err := <-serveErrors:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}
}

func (a *app) routes() http.Handler {
	mux := http.NewServeMux()
	base := "/" + a.token
	mux.HandleFunc("GET /{$}", a.dashboard)
	mux.HandleFunc("GET /qr.png", a.qrImage)
	mux.HandleFunc("POST /open-folder", a.openFolder)
	mux.HandleFunc("POST /quit", a.quitApp)
	mux.HandleFunc("GET "+base+"/api/files", a.listFiles)
	mux.HandleFunc("POST "+base+"/api/upload", a.uploadFiles)
	mux.HandleFunc("GET "+base+"/files/{name}", a.downloadFile)
	mux.HandleFunc("DELETE "+base+"/files/{name}", a.deleteFile)
	mux.HandleFunc("GET "+base+"/", a.index)
	return securityHeaders(mux)
}

func (a *app) dashboard(w http.ResponseWriter, r *http.Request) {
	if !requestIsLocal(r) {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := dashboardTemplate.Execute(w, a.phoneURL); err != nil {
		log.Printf("render dashboard: %v", err)
	}
}

func (a *app) qrImage(w http.ResponseWriter, r *http.Request) {
	if !requestIsLocal(r) {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	_, _ = w.Write(a.qrCode)
}

func (a *app) openFolder(w http.ResponseWriter, r *http.Request) {
	if !requestIsLocal(r) {
		http.NotFound(w, r)
		return
	}
	if err := openPath(a.dir); err != nil {
		http.Error(w, "Could not open shared folder", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *app) quitApp(w http.ResponseWriter, r *http.Request) {
	if !requestIsLocal(r) {
		http.NotFound(w, r)
		return
	}
	w.WriteHeader(http.StatusNoContent)
	a.quitOnce.Do(func() { close(a.quit) })
}

func (a *app) index(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, indexHTML)
}

func (a *app) listFiles(w http.ResponseWriter, _ *http.Request) {
	entries, err := os.ReadDir(a.dir)
	if err != nil {
		http.Error(w, "Could not read transfer folder", http.StatusInternalServerError)
		return
	}
	files := make([]fileInfo, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err == nil {
			files = append(files, fileInfo{Name: entry.Name(), Size: info.Size(), Modified: info.ModTime()})
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Modified.After(files[j].Modified) })
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(files)
}

func (a *app) uploadFiles(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadSize)
	reader, err := r.MultipartReader()
	if err != nil {
		http.Error(w, "Expected a multipart upload", http.StatusBadRequest)
		return
	}
	uploaded := make([]fileInfo, 0)
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			http.Error(w, "Upload interrupted", http.StatusBadRequest)
			return
		}
		if part.FileName() == "" {
			_ = part.Close()
			continue
		}
		info, err := a.savePart(part)
		_ = part.Close()
		if err != nil {
			http.Error(w, "Could not save file", http.StatusInternalServerError)
			return
		}
		uploaded = append(uploaded, info)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(uploaded)
}

func (a *app) savePart(part *multipart.Part) (fileInfo, error) {
	name := safeName(part.FileName())
	if name == "" {
		name = "upload"
	}
	path, name, err := availablePath(a.dir, name)
	if err != nil {
		return fileInfo{}, err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return fileInfo{}, err
	}
	size, copyErr := io.Copy(file, part)
	closeErr := file.Close()
	if copyErr != nil || closeErr != nil {
		_ = os.Remove(path)
		return fileInfo{}, errors.Join(copyErr, closeErr)
	}
	return fileInfo{Name: name, Size: size, Modified: time.Now()}, nil
}

func (a *app) downloadFile(w http.ResponseWriter, r *http.Request) {
	name := safeName(r.PathValue("name"))
	if name == "" || name != r.PathValue("name") {
		http.NotFound(w, r)
		return
	}
	http.ServeFile(w, r, filepath.Join(a.dir, name))
}

func (a *app) deleteFile(w http.ResponseWriter, r *http.Request) {
	name := safeName(r.PathValue("name"))
	if name == "" || name != r.PathValue("name") {
		http.NotFound(w, r)
		return
	}
	if err := os.Remove(filepath.Join(a.dir, name)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "Could not delete file", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func safeName(name string) string {
	name = strings.TrimSpace(filepath.Base(strings.ReplaceAll(name, "\\", "/")))
	if name == "." || name == "" {
		return ""
	}
	return name
}

func availablePath(dir, name string) (string, string, error) {
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for number := 0; number < 10_000; number++ {
		candidate := name
		if number > 0 {
			candidate = fmt.Sprintf("%s (%d)%s", stem, number, ext)
		}
		path := filepath.Join(dir, candidate)
		_, err := os.Stat(path)
		if errors.Is(err, os.ErrNotExist) {
			return path, candidate, nil
		}
		if err != nil {
			return "", "", err
		}
	}
	return "", "", errors.New("could not choose a unique filename")
}

func newToken() (string, error) {
	value := make([]byte, 18)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func defaultTransferDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Downloads", "Up"), nil
}

func localIP() string {
	connection, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return "localhost"
	}
	defer connection.Close()
	return connection.LocalAddr().(*net.UDPAddr).IP.String()
}

func requestIsLocal(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	return err == nil && net.ParseIP(host).IsLoopback()
}

func openBrowser(url string) error {
	return openPath(url)
}

func openPath(path string) error {
	var command *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		command = exec.Command("open", path)
	case "windows":
		command = exec.Command("rundll32", "url.dll,FileProtocolHandler", path)
	default:
		command = exec.Command("xdg-open", path)
	}
	return command.Start()
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

var dashboardTemplate = template.Must(template.New("dashboard").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Up - Connect phone</title>
<style>:root{--ink:#17211d;--paper:#f4f1e8;--green:#0f715b;--red:#b53c29;--line:#c9c7ba}*{box-sizing:border-box}body{margin:0;min-height:100vh;display:grid;place-items:center;color:var(--ink);background:repeating-linear-gradient(0deg,transparent 0 31px,rgba(23,33,29,.04) 31px 32px),var(--paper);font-family:Georgia,'Times New Roman',serif}main{width:min(520px,calc(100% - 32px));padding:40px 0;text-align:center}h1{margin:0 0 10px;font-size:64px;font-weight:500;letter-spacing:0}p{margin:0 0 24px;font-size:20px;line-height:1.4}.qr{display:block;width:min(384px,100%);margin:auto;border:1px solid var(--line)}.url{margin-top:20px;overflow-wrap:anywhere;color:var(--green);font:13px/1.5 ui-monospace,monospace}.actions{display:flex;gap:10px;margin-top:24px}.actions button{flex:1;min-height:48px;border:0;border-radius:3px;background:var(--green);color:white;font:700 13px ui-monospace,monospace;cursor:pointer}.actions .quit{background:transparent;border:1px solid var(--line);color:var(--red)}</style></head>
<body><main><h1>Up</h1><p>Scan with your Android camera to connect.</p><img class="qr" src="/qr.png" alt="QR code for the phone transfer page"><div class="url">{{.}}</div><div class="actions"><button id="folder">Open shared folder</button><button class="quit" id="quit">Quit Up</button></div></main><script>document.querySelector('#folder').onclick=()=>fetch('/open-folder',{method:'POST'});document.querySelector('#quit').onclick=async()=>{await fetch('/quit',{method:'POST'});document.body.innerHTML='<main><h1>Up</h1><p>Up has stopped. You can close this tab.</p></main>'}</script></body></html>`))

const indexHTML = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><meta name="theme-color" content="#f4f1e8"><title>Up - Local file transfer</title>
<style>:root{--ink:#17211d;--paper:#f4f1e8;--accent:#e85234;--green:#0f715b;--line:#c9c7ba}*{box-sizing:border-box}body{margin:0;min-height:100svh;color:var(--ink);background:repeating-linear-gradient(0deg,transparent 0 31px,rgba(23,33,29,.04) 31px 32px),var(--paper);font-family:Georgia,'Times New Roman',serif}main{width:min(720px,calc(100% - 32px));margin:auto;padding:40px 0 60px}header{display:flex;align-items:end;justify-content:space-between;padding-bottom:14px;border-bottom:2px solid var(--ink)}h1{margin:0;font-size:64px;line-height:.8;font-weight:500;letter-spacing:0}h2{margin:0 0 8px;font-size:30px;font-weight:500;letter-spacing:0}p{margin:0 0 20px;line-height:1.45}.status{color:var(--green);font:700 12px ui-monospace,monospace;text-transform:uppercase}.status:before{content:'';display:inline-block;width:8px;height:8px;margin-right:7px;border-radius:50%;background:#28a37e}section{padding:30px 0;border-bottom:1px solid var(--line)}input[type=file]{position:absolute;width:1px;height:1px;opacity:0}.picker{min-height:150px;display:grid;place-items:center;padding:24px;border:2px dashed #777b70;background:rgba(255,255,255,.45);text-align:center;cursor:pointer}.picker:hover,.picker:focus-within{border-color:var(--accent);background:rgba(255,255,255,.75)}.picker strong{display:block;margin-bottom:7px;font-size:20px;font-weight:500}.detail,.meta,.empty,#notice{font:12px/1.45 ui-monospace,monospace;color:#62675f}button{min-height:48px;border:0;border-radius:3px;padding:0 18px;color:white;background:var(--accent);font:700 14px ui-monospace,monospace;cursor:pointer}button:disabled{opacity:.55;cursor:wait}#send{width:100%;margin-top:12px}#notice{min-height:20px;margin-top:10px;color:var(--green)}.files{margin:18px 0 0;padding:0;list-style:none}.file{display:grid;grid-template-columns:minmax(0,1fr) auto auto;gap:10px;align-items:center;padding:13px 0;border-top:1px solid var(--line)}.name{overflow:hidden;text-overflow:ellipsis;white-space:nowrap;font-size:16px}.icon{display:grid;width:42px;min-height:42px;place-items:center;padding:0;background:var(--green);text-decoration:none;color:white;border-radius:3px;font:bold 20px ui-monospace,monospace}.delete{width:42px;padding:0;background:transparent;border:1px solid var(--line);color:var(--accent);font-size:20px}@media(max-width:520px){main{padding-top:28px}h1{font-size:54px}h2{font-size:27px}.file{grid-template-columns:minmax(0,1fr) 42px 42px}}</style></head>
<body><main><header><h1>Up</h1><div class="status">Connected</div></header><section><h2>Send files</h2><p>Choose photos, videos, or documents from this device.</p><form id="upload"><label class="picker"><input id="files" type="file" name="files" multiple required><span><strong id="selection">Choose files</strong><span class="detail" id="detail">Tap to browse this device</span></span></label><button id="send" type="submit">Send to computer</button></form><div id="notice" role="status"></div></section><section><h2>Shared files</h2><p>Download files currently in the computer's Up folder.</p><div id="listing" class="empty">Loading files...</div></section></main>
<script>const base=location.pathname.replace(/\/$/,'');const input=document.querySelector('#files');const form=document.querySelector('#upload');const send=document.querySelector('#send');const notice=document.querySelector('#notice');const listing=document.querySelector('#listing');const formatSize=bytes=>bytes<1024?bytes+' B':bytes<1048576?(bytes/1024).toFixed(1)+' KB':bytes<1073741824?(bytes/1048576).toFixed(1)+' MB':(bytes/1073741824).toFixed(1)+' GB';const escapeHTML=value=>value.replace(/[&<>'"]/g,char=>({'&':'&amp;','<':'&lt;','>':'&gt;',"'":'&#39;','"':'&quot;'}[char]));input.addEventListener('change',()=>{const count=input.files.length;document.querySelector('#selection').textContent=count?count+(count===1?' file selected':' files selected'):'Choose files';document.querySelector('#detail').textContent=count?Array.from(input.files).map(file=>file.name).join(', '):'Tap to browse this device'});async function loadFiles(){const response=await fetch(base+'/api/files');if(!response.ok)throw new Error('Could not load files');const files=await response.json();if(!files.length){listing.className='empty';listing.textContent='No files shared yet.';return}listing.className='';listing.innerHTML='<ul class="files">'+files.map(file=>{const name=escapeHTML(file.name);const encoded=encodeURIComponent(file.name);return '<li class="file"><div><div class="name" title="'+name+'">'+name+'</div><div class="meta">'+formatSize(file.size)+'</div></div><a class="icon" href="'+base+'/files/'+encoded+'" download title="Download">&#8595;</a><button class="delete" data-name="'+name+'" title="Delete">&#215;</button></li>'}).join('')+'</ul>'}form.addEventListener('submit',async event=>{event.preventDefault();if(!input.files.length)return;send.disabled=true;send.textContent='Sending...';notice.textContent='';try{const response=await fetch(base+'/api/upload',{method:'POST',body:new FormData(form)});if(!response.ok)throw new Error(await response.text());const uploaded=await response.json();notice.textContent=uploaded.length+(uploaded.length===1?' file received.':' files received.');form.reset();input.dispatchEvent(new Event('change'));await loadFiles()}catch(error){notice.textContent=error.message||'Upload failed.'}finally{send.disabled=false;send.textContent='Send to computer'}});listing.addEventListener('click',async event=>{const button=event.target.closest('.delete');if(!button||!confirm('Delete "'+button.dataset.name+'"?'))return;const response=await fetch(base+'/files/'+encodeURIComponent(button.dataset.name),{method:'DELETE'});if(response.ok)loadFiles()});loadFiles().catch(error=>{listing.textContent=error.message});</script></body></html>`
