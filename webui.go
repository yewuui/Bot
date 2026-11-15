package main

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

//go:embed web/index.html
var embeddedWebHTML []byte

var tryCloudflareRe = regexp.MustCompile(`https://[a-zA-Z0-9-]+\.trycloudflare\.com`)

func (a *BotApp) startWebServerAndTunnel(ctx context.Context) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(embeddedWebHTML)
	})

	mux.HandleFunc("/api/overview", a.withWebAuth(a.handleAPIOverview))
	mux.HandleFunc("/api/students", a.withWebAuth(a.handleAPIStudents))
	mux.HandleFunc("/api/students/", a.withWebAuth(a.handleAPIStudentByID))
	mux.HandleFunc("/api/logs", a.withWebAuth(a.handleAPILogs))
	mux.HandleFunc("/api/logs/", a.withWebAuth(a.handleAPILogByID))
	mux.HandleFunc("/api/export.csv", a.withWebAuth(a.handleAPIExportCSV))

	var ln net.Listener
	var port int
	for p := 8090; p <= 8099; p++ {
		l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
		if err == nil {
			ln = l
			port = p
			break
		}
	}
	if ln == nil {
		log.Println("⚠️ Gagal membuka port Web GUI (8090-8099 terpakai)")
		return
	}

	a.setLocalWebURL(fmt.Sprintf("http://127.0.0.1:%d", port))
	log.Printf("🌐 Web GUI Absensi aktif di %s", a.getLocalWebURL())

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("⚠️ Web server berhenti: %v", err)
		}
	}()
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()

	go a.runCloudflareTunnel(ctx, port)
}

func (a *BotApp) runCloudflareTunnel(ctx context.Context, port int) {
	cfBin := `C:\Program Files (x86)\cloudflared\cloudflared.exe`
	if path, err := exec.LookPath("cloudflared"); err == nil {
		cfBin = path
	} else if _, err := exec.LookPath(cfBin); err != nil {
		log.Println("ℹ️ cloudflared tidak ditemukan; GUI hanya tersedia di localhost.")
		return
	}

	for ctx.Err() == nil {
		cmd := exec.CommandContext(ctx, cfBin, "tunnel", "--no-autoupdate", "--url", fmt.Sprintf("http://127.0.0.1:%d", port))
		stderr, err := cmd.StderrPipe()
		if err != nil {
			return
		}
		if err := cmd.Start(); err != nil {
			log.Printf("⚠️ Gagal menjalankan cloudflared: %v", err)
			return
		}

		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			line := scanner.Text()
			if match := tryCloudflareRe.FindString(line); match != "" {
				if a.getWebAppURL() != match {
					a.setWebAppURL(match)
					log.Printf("🔗 Telegram Mini App GUI URL: %s", match)
					a.syncTelegramMenuButton(match)
				}
			}
		}
		_ = cmd.Wait()
		if ctx.Err() != nil {
			return
		}
		time.Sleep(5 * time.Second)
	}
}

func (a *BotApp) syncTelegramMenuButton(webURL string) {
	if a.bot == nil || webURL == "" {
		return
	}
	menuPayload, _ := json.Marshal(map[string]interface{}{
		"type":    "web_app",
		"text":    "📊 Absensi",
		"web_app": map[string]string{"url": webURL},
	})
	_, err := a.bot.MakeRequest("setChatMenuButton", tgbotapi.Params{
		"chat_id":     strconv.FormatInt(a.cfg.AllowedChatID, 10),
		"menu_button": string(menuPayload),
	})
	if err != nil {
		log.Printf("⚠️ Gagal set menu button Telegram: %v", err)
	}
}

func (a *BotApp) sendWebAppButton(chatID int64, text string) {
	a.sendWebAppButtonWithFilter(chatID, text, "", "")
}

func (a *BotApp) sendWebAppButtonWithFilter(chatID int64, text, peleton, bulan string) {
	webURL := a.getWebAppURL()
	localURL := a.getLocalWebURL()

	querySuffix := ""
	params := url.Values{}
	if p := strings.TrimSpace(peleton); p != "" {
		params.Set("tab", strings.ToUpper(p))
	}
	if b := strings.TrimSpace(bulan); b != "" {
		params.Set("bulan", b)
	}
	if enc := params.Encode(); enc != "" {
		querySuffix = "?" + enc
	}

	if webURL != "" {
		markup, _ := json.Marshal(map[string]interface{}{
			"inline_keyboard": [][]map[string]interface{}{
				{
					{"text": "📊 Buka Panel GUI Absensi", "web_app": map[string]string{"url": webURL + querySuffix}},
				},
			},
		})
		_, err := a.bot.MakeRequest("sendMessage", tgbotapi.Params{
			"chat_id":      strconv.FormatInt(chatID, 10),
			"text":         text + "\n\nTekan tombol di bawah untuk membuka tampilan visual langsung di Telegram:",
			"reply_markup": string(markup),
		})
		if err == nil {
			return
		}
	}

	msg := text + "\n\n⏳ Tunnel HTTPS Telegram sedang disiapkan (coba beberapa detik lagi)."
	if localURL != "" {
		msg += "\n💻 Di laptop bisa langsung buka: " + localURL + querySuffix
	}
	a.sendText(chatID, msg)
}

func (a *BotApp) withWebAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !a.isAuthorizedWebRequest(r) {
			writeJSONError(w, http.StatusForbidden, "Akses ditolak: buka dari Telegram pemilik bot atau localhost")
			return
		}
		next(w, r)
	}
}

func (a *BotApp) isAuthorizedWebRequest(r *http.Request) bool {
	initData := r.Header.Get("X-Telegram-Init-Data")
	if initData != "" {
		return verifyTelegramInitData(initData, a.cfg.BotToken, a.cfg.AllowedChatID)
	}

	// Izinkan akses langsung dari browser lokal laptop (bukan lewat proxy/tunnel)
	if r.Header.Get("Cf-Connecting-Ip") == "" && r.Header.Get("X-Forwarded-For") == "" {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err == nil {
			ip := net.ParseIP(host)
			if ip != nil && ip.IsLoopback() {
				return true
			}
		}
	}
	return false
}

func verifyTelegramInitData(initData, botToken string, allowedUserID int64) bool {
	vals, err := url.ParseQuery(initData)
	if err != nil {
		return false
	}
	hashHex := vals.Get("hash")
	if hashHex == "" {
		return false
	}
	vals.Del("hash")

	keys := make([]string, 0, len(vals))
	for k := range vals {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+vals.Get(k))
	}
	dataCheckString := strings.Join(parts, "\n")

	secretMAC := hmac.New(sha256.New, []byte("WebAppData"))
	secretMAC.Write([]byte(botToken))
	secretKey := secretMAC.Sum(nil)

	calcMAC := hmac.New(sha256.New, secretKey)
	calcMAC.Write([]byte(dataCheckString))
	calcHex := hex.EncodeToString(calcMAC.Sum(nil))

	if !hmac.Equal([]byte(strings.ToLower(hashHex)), []byte(calcHex)) {
		return false
	}

	var user struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal([]byte(vals.Get("user")), &user); err != nil {
		return false
	}
	return user.ID == allowedUserID
}

func (a *BotApp) handleAPIOverview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	ov, err := a.db.GetOverview()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, ov)
}

func (a *BotApp) handleAPIStudents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req struct {
		Peleton      string `json:"peleton"`
		NamaLengkap  string `json:"nama_lengkap"`
		NamaLapangan string `json:"nama_lapangan"`
		Kelas        string `json:"kelas"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "JSON tidak valid")
		return
	}
	id, err := a.db.AddStudent(req.Peleton, req.NamaLengkap, req.NamaLapangan, req.Kelas)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]interface{}{"id": id})
}

func (a *BotApp) handleAPIStudentByID(w http.ResponseWriter, r *http.Request) {
	idStr := strings.TrimPrefix(r.URL.Path, "/api/students/")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		writeJSONError(w, http.StatusBadRequest, "ID siswa tidak valid")
		return
	}

	switch r.Method {
	case http.MethodPut:
		var req struct {
			Peleton      string `json:"peleton"`
			NamaLengkap  string `json:"nama_lengkap"`
			NamaLapangan string `json:"nama_lapangan"`
			Kelas        string `json:"kelas"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "JSON tidak valid")
			return
		}
		if err := a.db.UpdateStudent(id, req.Peleton, req.NamaLengkap, req.NamaLapangan, req.Kelas); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})

	case http.MethodDelete:
		if err := a.db.DeleteStudent(id); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})

	default:
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (a *BotApp) handleAPILogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req struct {
		SiswaID    int64  `json:"siswa_id"`
		Bulan      string `json:"bulan"`
		Jumlah     int    `json:"jumlah"`
		Keterangan string `json:"keterangan"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "JSON tidak valid")
		return
	}
	id, err := a.db.AddAbsenceLog(req.SiswaID, req.Bulan, req.Jumlah, req.Keterangan)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]interface{}{"id": id})
}

func (a *BotApp) handleAPILogByID(w http.ResponseWriter, r *http.Request) {
	idStr := strings.TrimPrefix(r.URL.Path, "/api/logs/")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		writeJSONError(w, http.StatusBadRequest, "ID log tidak valid")
		return
	}

	switch r.Method {
	case http.MethodPut:
		var req struct {
			Bulan      string `json:"bulan"`
			Jumlah     int    `json:"jumlah"`
			Keterangan string `json:"keterangan"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "JSON tidak valid")
			return
		}
		if err := a.db.UpdateAbsenceLog(id, req.Bulan, req.Jumlah, req.Keterangan); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})

	case http.MethodDelete:
		if err := a.db.DeleteAbsenceLog(id); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})

	default:
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (a *BotApp) handleAPIExportCSV(w http.ResponseWriter, r *http.Request) {
	data, err := a.db.ExportCSVBytes()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="rekap-absensi-peleton.csv"`)
	_, _ = w.Write(data)
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
