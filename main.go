package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image/png"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/kbinani/screenshot"
)

// Konfigurasi sensitif (BOT_TOKEN, ALLOWED_CHAT_ID, ROUTER_API_KEY, dll.) dimuat dari .env / environment variable via loadConfig().

const (
	maxHistoryMsgs   = 16
	maxAgentSteps    = 5
	psDefaultTimeout = 30 * time.Second
	aiRequestTimeout = 90 * time.Second
	pendingActionTTL = 2 * time.Minute
	maxTelegramChars = 3900
	maxToolOutputLen = 3500
	maxUploadBytes   = 48 * 1024 * 1024
)

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type AIResponse struct {
	Reply   string        `json:"reply"`
	Action  string        `json:"action"` // none | screenshot | sysinfo | laragon_start | laragon_stop | lock | stop_bot | send_file | powershell | absen
	Command string        `json:"command"`
	Confirm bool          `json:"confirm"`
	Absen   *AbsenCommand `json:"absen,omitempty"`
}

type pendingAction struct {
	Action    string
	Command   string
	AbsenCmd  *AbsenCommand
	ExpiresAt time.Time
}

type BotApp struct {
	cfg      Config
	bot      *tgbotapi.BotAPI
	aiClient *http.Client
	dlClient *http.Client
	inboxDir string
	db       *AttendanceDB
	dbPath   string

	mu      sync.Mutex
	history []chatMessage
	pending map[string]pendingAction

	webMu       sync.RWMutex
	localWebURL string
	webAppURL   string

	activeMu     sync.Mutex
	activeCancel context.CancelFunc
	activeGen    uint64

	appCancel context.CancelFunc
}

var (
	thinkBlockRe = regexp.MustCompile(`(?is)<think>.*?</think>`)
	dangerRules  = []*regexp.Regexp{
		regexp.MustCompile(`(?i)(^|[;\s|&])(remove-item|del|erase|rmdir|rd)\b`),
		regexp.MustCompile(`(?i)(^|[;\s|&])(stop-computer|restart-computer|shutdown|logoff)\b`),
		regexp.MustCompile(`(?i)(^|[;\s|&])(stop-process|taskkill|kill)\b`),
		regexp.MustCompile(`(?i)(^|[;\s|&])(format-volume|format|clear-disk|diskpart|bcdedit|cipher)\b`),
		regexp.MustCompile(`(?i)(^|[;\s|&])clear-content\b`),
		regexp.MustCompile(`(?i)\breg\s+delete\b`),
		regexp.MustCompile(`(?i)\bnet\s+user\b`),
		regexp.MustCompile(`(?i)\bsc(\.exe)?\s+delete\b`),
	}
)

func main() {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("❌ Konfigurasi tidak valid: %v", err)
	}

	appCtx, appCancel := context.WithCancel(context.Background())
	defer appCancel()

	app := newBotApp(cfg)
	app.appCancel = appCancel
	defer app.db.Close()

	if cfg.ExcelSeedPath != "" {
		if msg, err := app.db.SeedFromExcelIfEmpty(cfg.ExcelSeedPath); err != nil {
			log.Printf("⚠️ Gagal seed Excel awal: %v", err)
		} else if msg != "" {
			log.Printf("📚 %s", msg)
		}
	}

	app.bot = app.connectBotWithRetry()
	app.startWebServerAndTunnel(appCtx)

	log.Printf("✅ Bot terhubung (@%s)! Inbox: %s | DB: %s | Menunggu chat...", app.bot.Self.UserName, app.inboxDir, app.dbPath)

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	updates := app.bot.GetUpdatesChan(u)

	for update := range updates {
		upd := update
		go app.handleUpdate(upd)
	}
}

func newBotApp(cfg Config) *BotApp {
	baseDialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	tgTransport := &http.Transport{
		DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			return baseDialer.DialContext(ctx, "tcp4", addr)
		},
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          10,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   30 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	aiTransport := &http.Transport{
		MaxIdleConns:        10,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 15 * time.Second,
	}

	wd, err := os.Getwd()
	if err != nil {
		wd = "."
	}
	inbox := filepath.Join(wd, "inbox")
	_ = os.MkdirAll(inbox, 0o755)

	dbPath := filepath.Join(wd, dbFileName)
	db, err := OpenAttendanceDB(dbPath)
	if err != nil {
		log.Panicf("❌ Gagal membuka SQLite %s: %v", dbPath, err)
	}

	return &BotApp{
		cfg:      cfg,
		aiClient: &http.Client{Transport: aiTransport, Timeout: aiRequestTimeout},
		dlClient: &http.Client{Transport: tgTransport, Timeout: 75 * time.Second},
		inboxDir: inbox,
		db:       db,
		dbPath:   dbPath,
		pending:  make(map[string]pendingAction),
	}
}

func (a *BotApp) setLocalWebURL(u string) {
	a.webMu.Lock()
	a.localWebURL = u
	a.webMu.Unlock()
}

func (a *BotApp) getLocalWebURL() string {
	a.webMu.RLock()
	defer a.webMu.RUnlock()
	return a.localWebURL
}

func (a *BotApp) setWebAppURL(u string) {
	a.webMu.Lock()
	a.webAppURL = u
	a.webMu.Unlock()
}

func (a *BotApp) getWebAppURL() string {
	a.webMu.RLock()
	defer a.webMu.RUnlock()
	return a.webAppURL
}

func (a *BotApp) connectBotWithRetry() *tgbotapi.BotAPI {
	backoff := 2 * time.Second
	for {
		bot, err := tgbotapi.NewBotAPIWithClient(a.cfg.BotToken, tgbotapi.APIEndpoint, a.dlClient)
		if err == nil {
			return bot
		}
		log.Printf("⚠️ Gagal terhubung ke Telegram (%v), mencoba lagi dalam %s...", err, backoff)
		time.Sleep(backoff)
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func (a *BotApp) handleUpdate(update tgbotapi.Update) {
	if update.CallbackQuery != nil {
		a.handleCallbackQuery(update.CallbackQuery)
		return
	}
	if update.Message == nil {
		return
	}

	chatID := update.Message.Chat.ID
	if chatID != a.cfg.AllowedChatID {
		a.sendText(chatID, "⛔ Akses ditolak.")
		return
	}

	if update.Message.Document != nil || len(update.Message.Photo) > 0 {
		a.handleIncomingMedia(update.Message)
		return
	}

	text := strings.TrimSpace(update.Message.Text)
	if text == "" {
		return
	}

	if strings.HasPrefix(text, "/") {
		a.handleSlashCommand(chatID, text)
		return
	}

	ctx, cancel := a.beginActiveTask()
	defer cancel()
	a.runAIChat(ctx, chatID, text)
}

func (a *BotApp) beginActiveTask() (context.Context, context.CancelFunc) {
	a.activeMu.Lock()
	if a.activeCancel != nil {
		a.activeCancel()
	}
	a.activeGen++
	myGen := a.activeGen
	ctx, cancel := context.WithCancel(context.Background())
	a.activeCancel = cancel
	a.activeMu.Unlock()

	return ctx, func() {
		cancel()
		a.activeMu.Lock()
		if a.activeGen == myGen {
			a.activeCancel = nil
		}
		a.activeMu.Unlock()
	}
}

func (a *BotApp) cancelCurrentTask() bool {
	a.activeMu.Lock()
	defer a.activeMu.Unlock()
	if a.activeCancel == nil {
		return false
	}
	a.activeCancel()
	a.activeCancel = nil
	return true
}

func (a *BotApp) handleSlashCommand(chatID int64, rawText string) {
	cmd, arg := parseSlashCommand(rawText)

	switch cmd {
	case "/start", "/menu":
		a.sendMenu(chatID)

	case "/rekap":
		if strings.TrimSpace(arg) == "" {
			a.sendRecapPhoto(chatID, "", "", "")
			return
		}
		pel, bul, err := parseRecapArgs(arg)
		if err != nil {
			a.sendText(chatID, "⚠️ "+err.Error())
			return
		}
		a.sendRecapPhoto(chatID, "", pel, bul)

	case "/absen":
		if strings.TrimSpace(arg) != "" {
			pel, bul, err := parseRecapArgs(arg)
			if err != nil {
				a.sendText(chatID, "⚠️ "+err.Error())
				return
			}
			a.sendRecapPhoto(chatID, "", pel, bul)
			return
		}
		a.sendAttendanceSummary(chatID)

	case "/gui":
		a.sendWebAppButton(chatID, "📊 Panel GUI Absensi Peleton")

	case "/export":
		a.sendAttendanceCSV(chatID)

	case "/backup":
		a.sendDatabaseBackup(chatID)

	case "/ss":
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		a.executeAction(ctx, chatID, "screenshot", "", "")

	case "/sys":
		ctx, cancel := a.beginActiveTask()
		defer cancel()
		a.executeAction(ctx, chatID, "sysinfo", "", "")

	case "/laragon":
		a.executeAction(context.Background(), chatID, "laragon_start", "", "")

	case "/laragon_stop":
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		a.executeAction(ctx, chatID, "laragon_stop", "", "")

	case "/lock":
		a.executeAction(context.Background(), chatID, "lock", "", "")

	case "/get":
		if arg == "" {
			a.sendText(chatID, "📁 Gunakan: /get C:\\path\\ke\\file.ext")
			return
		}
		a.executeAction(context.Background(), chatID, "send_file", arg, "")

	case "/cmd":
		if arg == "" {
			a.sendText(chatID, "💻 Gunakan: /cmd <perintah PowerShell>")
			return
		}
		ctx, cancel := a.beginActiveTask()
		defer cancel()
		a.executeAction(ctx, chatID, "powershell", arg, "")

	case "/cancel":
		if a.cancelCurrentTask() {
			a.sendText(chatID, "🛑 Tugas yang sedang berjalan dibatalkan.")
		} else {
			a.sendText(chatID, "ℹ️ Tidak ada tugas aktif yang berjalan.")
		}

	case "/reset":
		a.clearHistory()
		a.sendText(chatID, "🧹 Memori percakapan AI sudah direset.")

	case "/off":
		a.executeAction(context.Background(), chatID, "stop_bot", "", "")

	default:
		a.sendText(chatID, "Perintah tidak dikenali. Ketik /menu atau langsung ngobrol tanpa garis miring.")
	}
}

func parseSlashCommand(raw string) (string, string) {
	raw = strings.TrimSpace(raw)
	fields := strings.SplitN(raw, " ", 2)
	cmd := strings.ToLower(fields[0])
	if atIdx := strings.Index(cmd, "@"); atIdx != -1 {
		cmd = cmd[:atIdx]
	}
	arg := ""
	if len(fields) > 1 {
		arg = strings.TrimSpace(fields[1])
	}
	return cmd, arg
}

func (a *BotApp) sendMenu(chatID int64) {
	text := "🤖 *Asisten Laptop & Absensi Peleton AI Aktif!*\n\n" +
		"Langsung chat pakai bahasa sehari-hari untuk kontrol laptop atau kelola absensi (misal: _\"Abi hari ini sakit\"_, _\"ubah panggilan lana jadi Raffa\"_, _\"cek rekap Oktober\"_), atau gunakan perintah cepat:\n\n" +
		"• `/rekap [bulan] [peleton]` - Gambar kartu ringkasan visual (tanpa download)\n" +
		"• `/absen` - Menu absensi & catat cepat\n" +
		"• `/gui` - Buka tampilan GUI visual di Telegram\n" +
		"• `/export` - Download rekap absensi (CSV/Excel)\n" +
		"• `/backup` - Backup file database SQLite\n" +
		"• `/ss` - Screenshot semua layar\n" +
		"• `/sys` - Info CPU, RAM, Disk, Baterai\n" +
		"• `/get <path>` - Ambil file dari laptop\n" +
		"• `/cmd <perintah>` - Jalankan PowerShell\n" +
		"• `/cancel` - Batalkan perintah yang macet\n" +
		"• `/reset` - Hapus memori chat AI\n" +
		"• `/off` - Matikan bot"

	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("📋 Absensi Peleton", "abs:menu"),
			tgbotapi.NewInlineKeyboardButtonData("🌐 Panel GUI", "abs:gui"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("📸 Screenshot", "act:screenshot"),
			tgbotapi.NewInlineKeyboardButtonData("💻 Info Sistem", "act:sysinfo"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🚀 Start Laragon", "act:laragon_start"),
			tgbotapi.NewInlineKeyboardButtonData("🛑 Stop Laragon", "act:laragon_stop"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🔒 Lock Layar", "act:lock"),
			tgbotapi.NewInlineKeyboardButtonData("🧹 Reset AI", "act:reset"),
		),
	)

	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = "Markdown"
	msg.ReplyMarkup = keyboard
	if _, err := a.bot.Send(msg); err != nil {
		// Fallback tanpa Markdown jika ada karakter spesial
		msg.ParseMode = ""
		_, _ = a.bot.Send(msg)
	}
}

func (a *BotApp) handleCallbackQuery(q *tgbotapi.CallbackQuery) {
	var chatID int64
	if q.Message != nil {
		chatID = q.Message.Chat.ID
	} else if q.From != nil {
		chatID = q.From.ID
	}

	if chatID != a.cfg.AllowedChatID {
		_, _ = a.bot.Request(tgbotapi.NewCallback(q.ID, "⛔ Akses ditolak."))
		return
	}

	data := q.Data
	switch {
	case strings.HasPrefix(data, "abs:"):
		a.handleAbsenCallback(q, chatID)

	case strings.HasPrefix(data, "act:"):
		act := strings.TrimPrefix(data, "act:")
		_, _ = a.bot.Request(tgbotapi.NewCallback(q.ID, "Diproses..."))
		if act == "reset" {
			a.clearHistory()
			a.sendText(chatID, "🧹 Memori percakapan AI sudah direset.")
			return
		}
		ctx, cancel := a.beginActiveTask()
		defer cancel()
		a.executeAction(ctx, chatID, act, "", "")

	case strings.HasPrefix(data, "confirm:"):
		id := strings.TrimPrefix(data, "confirm:")
		pa, ok := a.popPending(id)
		if !ok {
			_, _ = a.bot.Request(tgbotapi.NewCallback(q.ID, "⏳ Konfirmasi sudah kedaluwarsa."))
			a.clearInlineMarkup(q)
			return
		}
		_, _ = a.bot.Request(tgbotapi.NewCallback(q.ID, "✅ Dikonfirmasi"))
		a.editCallbackMessage(q, fmt.Sprintf("✅ Dikonfirmasi: menjalankan %s...", pa.Action))
		if pa.Action == "absen" && pa.AbsenCmd != nil {
			res, _, err := a.runAbsenTool(pa.AbsenCmd)
			if err != nil {
				a.sendText(chatID, "❌ Gagal menjalankan aksi absensi: "+err.Error())
			} else {
				a.sendText(chatID, "✅ "+res)
			}
			return
		}
		ctx, cancel := a.beginActiveTask()
		defer cancel()
		a.executeAction(ctx, chatID, pa.Action, pa.Command, "")

	case strings.HasPrefix(data, "cancel:"):
		id := strings.TrimPrefix(data, "cancel:")
		a.popPending(id)
		_, _ = a.bot.Request(tgbotapi.NewCallback(q.ID, "❌ Dibatalkan"))
		a.editCallbackMessage(q, "❌ Aksi dibatalkan oleh pengguna.")

	default:
		_, _ = a.bot.Request(tgbotapi.NewCallback(q.ID, ""))
	}
}

func (a *BotApp) editCallbackMessage(q *tgbotapi.CallbackQuery, text string) {
	if q.Message == nil {
		return
	}
	edit := tgbotapi.NewEditMessageText(q.Message.Chat.ID, q.Message.MessageID, text)
	_, _ = a.bot.Send(edit)
}

func (a *BotApp) clearInlineMarkup(q *tgbotapi.CallbackQuery) {
	if q.Message == nil {
		return
	}
	empty := tgbotapi.NewInlineKeyboardMarkup([]tgbotapi.InlineKeyboardButton{})
	edit := tgbotapi.NewEditMessageReplyMarkup(q.Message.Chat.ID, q.Message.MessageID, empty)
	_, _ = a.bot.Send(edit)
}

func (a *BotApp) handleIncomingMedia(msg *tgbotapi.Message) {
	chatID := msg.Chat.ID
	var fileID, fileName string

	if msg.Document != nil {
		fileID = msg.Document.FileID
		fileName = msg.Document.FileName
	} else if len(msg.Photo) > 0 {
		best := msg.Photo[len(msg.Photo)-1]
		fileID = best.FileID
		fileName = fmt.Sprintf("foto_%s.jpg", time.Now().Format("20060102_150405"))
	}

	if fileID == "" {
		return
	}

	savedPath, err := a.downloadTelegramFile(fileID, fileName)
	if err != nil {
		a.sendText(chatID, "❌ Gagal mengunduh file: "+err.Error())
		return
	}

	caption := strings.TrimSpace(msg.Caption)
	if caption == "" {
		a.sendText(chatID, fmt.Sprintf("📥 File tersimpan di:\n%s", savedPath))
		return
	}

	a.sendText(chatID, fmt.Sprintf("📥 File tersimpan di %s. Memproses instruksi...", savedPath))
	ctx, cancel := a.beginActiveTask()
	defer cancel()
	prompt := fmt.Sprintf("[Pengguna baru mengirim file dan sudah tersimpan di laptop pada path: %s]\nInstruksi pengguna: %s", savedPath, caption)
	a.runAIChat(ctx, chatID, prompt)
}

func (a *BotApp) downloadTelegramFile(fileID, origName string) (string, error) {
	fileURL, err := a.bot.GetFileDirectURL(fileID)
	if err != nil {
		return "", err
	}

	resp, err := a.dlClient.Get(fileURL)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	base := filepath.Base(strings.TrimSpace(origName))
	if base == "" || base == "." || base == string(filepath.Separator) {
		base = fmt.Sprintf("file_%d", time.Now().Unix())
	}

	target := filepath.Join(a.inboxDir, base)
	if _, err := os.Stat(target); err == nil {
		ext := filepath.Ext(base)
		stem := strings.TrimSuffix(base, ext)
		target = filepath.Join(a.inboxDir, fmt.Sprintf("%s_%s%s", stem, time.Now().Format("150405"), ext))
	}

	f, err := os.Create(target)
	if err != nil {
		return "", err
	}
	defer f.Close()

	if _, err := io.Copy(f, resp.Body); err != nil {
		return "", err
	}
	return target, nil
}

func (a *BotApp) runAIChat(ctx context.Context, chatID int64, userPrompt string) {
	stopTyping := a.startTypingLoop(ctx, chatID)
	defer stopTyping()

	historySnap := a.getHistorySnapshot()
	loopMessages := make([]chatMessage, 0, len(historySnap)+maxAgentSteps*2+2)
	loopMessages = append(loopMessages, chatMessage{Role: "system", Content: a.buildSystemPrompt()})
	loopMessages = append(loopMessages, historySnap...)
	loopMessages = append(loopMessages, chatMessage{Role: "user", Content: userPrompt})

	var lastToolOutput string
	var finalAssistantReply string

	for step := 1; step <= maxAgentSteps; step++ {
		if ctx.Err() != nil {
			return
		}

		aiRes, rawContent, err := a.ask9RouterWithRetry(ctx, loopMessages)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return
			}
			a.sendText(chatID, "⚠️ Gagal menghubungi 9router: "+err.Error())
			return
		}

		action := normalizeAction(aiRes.Action)

		// 1. Jawaban langsung (tidak ada aksi)
		if action == "none" {
			reply := strings.TrimSpace(aiRes.Reply)
			if reply == "" {
				reply = "✅ Selesai."
			}
			finalAssistantReply = reply
			a.sendLongMessage(chatID, reply)
			a.appendHistory(userPrompt, finalAssistantReply)
			return
		}

		// 2. Cek apakah butuh konfirmasi tombol Ya/Batal
		isRiskyAbsen := action == "absen" && aiRes.Absen != nil && strings.EqualFold(aiRes.Absen.Op, "delete_student")
		if aiRes.Confirm || action == "stop_bot" || isRiskyAbsen || (action == "powershell" && isDangerousCommand(aiRes.Command)) {
			desc := aiRes.Command
			if isRiskyAbsen && aiRes.Absen != nil {
				desc = fmt.Sprintf("Hapus siswa: %s (ID=%d)", firstNonEmpty(aiRes.Absen.Nama, aiRes.Absen.NamaLengkap, aiRes.Absen.Query), aiRes.Absen.SiswaID)
			}
			a.askConfirmation(chatID, aiRes.Reply, action, desc, aiRes.Absen)
			summary := fmt.Sprintf("%s (menunggu konfirmasi untuk %s: %s)", aiRes.Reply, action, desc)
			a.appendHistory(userPrompt, summary)
			return
		}

		// 3. Aksi absensi peleton (SQLite + GUI)
		if action == "absen" {
			res, isTerminal, execErr := a.runAbsenTool(aiRes.Absen)
			if isTerminal {
				switch {
				case res == "GUI_READY":
					pel, bul := "", ""
					if aiRes.Absen != nil {
						pel = aiRes.Absen.Peleton
						bul = aiRes.Absen.Bulan
					}
					a.sendWebAppButtonWithFilter(chatID, firstNonEmpty(aiRes.Reply, "📊 Panel GUI Absensi Peleton"), pel, bul)
				case strings.HasPrefix(res, "RECAP_IMAGE|"):
					parts := strings.SplitN(res, "|", 3)
					pel, bul := "", ""
					if len(parts) >= 2 {
						pel = parts[1]
					}
					if len(parts) >= 3 {
						bul = parts[2]
					}
					a.sendRecapPhoto(chatID, aiRes.Reply, pel, bul)
				case res == "EXPORT_CSV":
					a.sendAttendanceCSV(chatID)
				case res == "BACKUP_DB":
					a.sendDatabaseBackup(chatID)
				}
				a.appendHistory(userPrompt, firstNonEmpty(aiRes.Reply, res))
				return
			}

			formattedResult := formatToolResult("absen", "", res, execErr)
			lastToolOutput = formattedResult
			if step == maxAgentSteps {
				a.sendLongMessage(chatID, formattedResult)
				a.appendHistory(userPrompt, formattedResult)
				return
			}

			loopMessages = append(loopMessages,
				chatMessage{Role: "assistant", Content: rawContent},
				chatMessage{
					Role:    "user",
					Content: fmt.Sprintf("[HASIL DATABASE ABSENSI]\n%s\n\nGunakan hasil di atas untuk menjawab pengguna dengan ramah & akurat (set \"action\": \"none\"), atau panggil action=\"absen\" lagi jika perlu langkah lanjutan.", truncateRunes(formattedResult, maxToolOutputLen)),
				},
			)
			continue
		}

		// 4. Aksi inspeksi sistem yang hasilnya dibaca kembali oleh AI (agentic loop)
		if action == "powershell" || action == "sysinfo" {
			var output string
			var execErr error

			if action == "sysinfo" {
				output, execErr = getSystemInfo(ctx)
			} else {
				if strings.TrimSpace(aiRes.Command) == "" {
					a.sendLongMessage(chatID, aiRes.Reply)
					a.appendHistory(userPrompt, aiRes.Reply)
					return
				}
				output, execErr = runPowerShell(ctx, aiRes.Command)
			}

			if errors.Is(execErr, context.Canceled) {
				return
			}

			formattedResult := formatToolResult(action, aiRes.Command, output, execErr)
			lastToolOutput = formattedResult

			// Jika sudah di langkah terakhir, kirim hasilnya langsung agar tidak hilang
			if step == maxAgentSteps {
				msg := strings.TrimSpace(aiRes.Reply)
				if msg != "" {
					msg += "\n\n" + formattedResult
				} else {
					msg = formattedResult
				}
				a.sendLongMessage(chatID, msg)
				a.appendHistory(userPrompt, msg)
				return
			}

			loopMessages = append(loopMessages,
				chatMessage{Role: "assistant", Content: rawContent},
				chatMessage{
					Role:    "user",
					Content: fmt.Sprintf("[HASIL EKSEKUSI]\n%s\n\nGunakan hasil di atas untuk menjawab pengguna secara ringkas & jelas (set \"action\": \"none\"), atau jalankan langkah lanjutan jika masih perlu.", truncateRunes(formattedResult, maxToolOutputLen)),
				},
			)
			continue
		}

		// 4. Aksi terminal (screenshot, laragon, lock, send_file)
		a.executeAction(ctx, chatID, action, aiRes.Command, aiRes.Reply)
		summary := aiRes.Reply
		if summary == "" {
			summary = fmt.Sprintf("[Menjalankan aksi %s]", action)
		}
		a.appendHistory(userPrompt, summary)
		return
	}

	if lastToolOutput != "" {
		a.sendLongMessage(chatID, lastToolOutput)
	}
}

func (a *BotApp) askConfirmation(chatID int64, reason, action, command string, absenCmd *AbsenCommand) {
	id := newShortID()
	a.storePending(id, pendingAction{
		Action:    action,
		Command:   command,
		AbsenCmd:  absenCmd,
		ExpiresAt: time.Now().Add(pendingActionTTL),
	})

	var sb strings.Builder
	sb.WriteString("⚠️ *Konfirmasi Aksi Berisiko*\n")
	if strings.TrimSpace(reason) != "" {
		sb.WriteString(reason)
		sb.WriteString("\n\n")
	}
	sb.WriteString(fmt.Sprintf("*Aksi:* `%s`\n", action))
	if strings.TrimSpace(command) != "" {
		sb.WriteString(fmt.Sprintf("*Perintah:*\n```powershell\n%s\n```", truncateRunes(command, 1000)))
	}

	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("✅ Ya, Jalankan", "confirm:"+id),
			tgbotapi.NewInlineKeyboardButtonData("❌ Batal", "cancel:"+id),
		),
	)

	msg := tgbotapi.NewMessage(chatID, sb.String())
	msg.ParseMode = "Markdown"
	msg.ReplyMarkup = keyboard
	if _, err := a.bot.Send(msg); err != nil {
		msg.ParseMode = ""
		_, _ = a.bot.Send(msg)
	}
}

func (a *BotApp) executeAction(ctx context.Context, chatID int64, action, command, aiReply string) {
	switch normalizeAction(action) {
	case "screenshot":
		if err := a.sendScreenshots(chatID, aiReply); err != nil {
			a.sendText(chatID, "❌ Gagal mengambil screenshot: "+err.Error())
		}

	case "sysinfo":
		info, err := getSystemInfo(ctx)
		if err != nil {
			a.sendText(chatID, "❌ Gagal mengambil info sistem: "+err.Error())
			return
		}
		a.sendLongMessage(chatID, info)

	case "laragon_start":
		if err := exec.Command(a.cfg.LaragonExePath).Start(); err != nil {
			a.sendText(chatID, "❌ Gagal menyalakan Laragon: "+err.Error())
			return
		}
		msg := "🚀 Laragon berhasil dijalankan!"
		if strings.TrimSpace(aiReply) != "" {
			msg = aiReply + "\n" + msg
		}
		a.sendText(chatID, msg)

	case "laragon_stop":
		out, err := runPowerShell(ctx, "Get-Process laragon,httpd,mysqld,nginx -ErrorAction SilentlyContinue | Stop-Process -Force")
		if err != nil {
			a.sendText(chatID, fmt.Sprintf("⚠️ Gagal menghentikan sebagian proses Laragon:\n%s\n%v", out, err))
			return
		}
		a.sendText(chatID, "🛑 Layanan Laragon berhasil dihentikan.")

	case "lock":
		if err := exec.Command("rundll32.exe", "user32.dll,LockWorkStation").Run(); err != nil {
			a.sendText(chatID, "❌ Gagal mengunci layar: "+err.Error())
			return
		}
		a.sendText(chatID, "🔒 Layar laptop berhasil dikunci.")

	case "send_file":
		a.sendFileFromLaptop(chatID, command, aiReply)

	case "stop_bot":
		a.sendText(chatID, "👋 Bot dimatikan. Sampai jumpa!")
		if a.appCancel != nil {
			a.appCancel()
		}
		time.Sleep(300 * time.Millisecond)
		os.Exit(0)

	case "powershell":
		if strings.TrimSpace(command) == "" {
			return
		}
		out, err := runPowerShell(ctx, command)
		if errors.Is(err, context.Canceled) {
			return
		}
		if errors.Is(err, context.DeadlineExceeded) {
			a.sendText(chatID, fmt.Sprintf("⏱️ Perintah timeout (>%s). Gunakan `/cancel` jika perlu.\n%s", psDefaultTimeout, truncateRunes(out, 1500)))
			return
		}
		if err != nil {
			a.sendLongMessage(chatID, fmt.Sprintf("⚠️ Error eksekusi:\n%s\n%v", out, err))
			return
		}
		if strings.TrimSpace(out) == "" {
			a.sendText(chatID, "✅ Perintah selesai dijalankan.")
			return
		}
		a.sendLongMessage(chatID, "📟 Output:\n"+out)
	}
}

func (a *BotApp) sendScreenshots(chatID int64, caption string) error {
	n := screenshot.NumActiveDisplays()
	if n <= 0 {
		return errors.New("tidak ada layar aktif terdeteksi")
	}

	for i := 0; i < n; i++ {
		bounds := screenshot.GetDisplayBounds(i)
		img, err := screenshot.CaptureRect(bounds)
		if err != nil {
			return fmt.Errorf("monitor %d: %w", i+1, err)
		}

		var buf bytes.Buffer
		if err := png.Encode(&buf, img); err != nil {
			return err
		}

		photoMsg := tgbotapi.NewPhoto(chatID, tgbotapi.FileBytes{
			Name:  fmt.Sprintf("layar_%d.png", i+1),
			Bytes: buf.Bytes(),
		})
		capText := fmt.Sprintf("📸 Layar %d (%dx%d)", i+1, bounds.Dx(), bounds.Dy())
		if i == 0 && strings.TrimSpace(caption) != "" {
			capText = truncateRunes(caption, 900) + "\n" + capText
		}
		photoMsg.Caption = capText
		if _, err := a.bot.Send(photoMsg); err != nil {
			return err
		}
	}
	return nil
}

func (a *BotApp) sendFileFromLaptop(chatID int64, rawPath, caption string) {
	path := strings.Trim(strings.TrimSpace(rawPath), `"'`)
	if path == "" {
		a.sendText(chatID, "❌ Path file kosong.")
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		a.sendText(chatID, "❌ File tidak ditemukan: "+err.Error())
		return
	}
	if info.IsDir() {
		a.sendText(chatID, "❌ Path mengarah ke folder, bukan file.")
		return
	}
	if info.Size() > maxUploadBytes {
		a.sendText(chatID, fmt.Sprintf("❌ Ukuran file %.1f MB melebihi batas Telegram (~48 MB).", float64(info.Size())/(1024*1024)))
		return
	}

	doc := tgbotapi.NewDocument(chatID, tgbotapi.FilePath(path))
	if strings.TrimSpace(caption) != "" {
		doc.Caption = truncateRunes(caption, 900)
	}
	if _, err := a.bot.Send(doc); err != nil {
		a.sendText(chatID, "❌ Gagal mengirim file: "+err.Error())
	}
}

func runPowerShell(parentCtx context.Context, command string) (string, error) {
	ctx, cancel := context.WithTimeout(parentCtx, psDefaultTimeout)
	defer cancel()

	script := "[Console]::OutputEncoding = [System.Text.Encoding]::UTF8; $OutputEncoding = [System.Text.Encoding]::UTF8; " + command
	cmd := exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command", script)
	out, err := cmd.CombinedOutput()
	result := strings.TrimSpace(string(out))
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	return result, err
}

func getSystemInfo(ctx context.Context) (string, error) {
	psScript := `
$os = Get-CimInstance Win32_OperatingSystem
$cpu = Get-CimInstance Win32_Processor | Select-Object -First 1
$bat = Get-CimInstance Win32_Battery -ErrorAction SilentlyContinue | Select-Object -First 1
$disks = Get-CimInstance Win32_LogicalDisk -Filter "DriveType=3" | ForEach-Object {
  $free = [math]::Round($_.FreeSpace / 1GB, 1)
  $total = [math]::Round($_.Size / 1GB, 1)
  "$($_.DeviceID) $free GB free / $total GB"
}
$ramTotal = [math]::Round($os.TotalVisibleMemorySize / 1MB, 1)
$ramFree = [math]::Round($os.FreePhysicalMemory / 1MB, 1)
$ramUsed = [math]::Round($ramTotal - $ramFree, 1)
$uptime = (Get-Date) - $os.LastBootUpTime
$batText = if ($bat) { "$($bat.EstimatedChargeRemaining)% (Status: $($bat.BatteryStatus))" } else { "Tidak ada / Desktop" }
"CPU: $($cpu.Name) (Load: $($cpu.LoadPercentage)%)"
"RAM: $ramUsed GB / $ramTotal GB (Sisa: $ramFree GB)"
"Baterai: $batText"
"Uptime: $([int]$uptime.TotalHours)j $($uptime.Minutes)m"
"Disk: " + ($disks -join " | ")
`
	psOut, err := runPowerShell(ctx, psScript)
	if err != nil && psOut == "" {
		return "", err
	}

	host, _ := os.Hostname()
	ips := localIPv4s()
	ipStr := strings.Join(ips, ", ")
	if ipStr == "" {
		ipStr = "-"
	}

	return fmt.Sprintf("💻 Info Sistem (%s | %s/%s)\nIP Lokal: %s\n%s",
		host, runtime.GOOS, runtime.GOARCH, ipStr, psOut), nil
}

func localIPv4s() []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var ips []string
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			if ipNet, ok := addr.(*net.IPNet); ok && ipNet.IP.To4() != nil {
				ips = append(ips, ipNet.IP.String())
			}
		}
	}
	return ips
}

func (a *BotApp) buildSystemPrompt() string {
	cwd, _ := os.Getwd()
	host, _ := os.Hostname()
	user := os.Getenv("USERNAME")
	now := time.Now().Format("2006-01-02 15:04:05 MST")
	curMonth := CurrentAcademicMonth()

	return fmt.Sprintf(`Kamu asisten pengendali laptop Windows & pengelola absensi peleton (PUTRA/PUTRI) via Telegram.
Konteks saat ini:
- Host/User: %s / %s
- Direktori kerja: %s | Folder inbox: %s
- Waktu lokal: %s | Bulan ajaran aktif: %s

Balas HANYA 1 objek JSON valid tanpa markdown backtick:
{
  "reply": "balasan singkat, santai, bahasa Indonesia",
  "action": "none | screenshot | sysinfo | laragon_start | laragon_stop | lock | stop_bot | send_file | powershell | absen",
  "command": "diisi jika action=powershell atau send_file",
  "confirm": false,
  "absen": {
    "op": "query | recap_image | add_logs | update_log | delete_log | set_month | add_student | update_student | delete_student | open_gui | export_csv | backup_db",
    "query": "", "peleton": "", "bulan": "", "siswa_id": 0,
    "nama": "", "nama_lengkap": "", "nama_lapangan": "", "kelas": "",
    "log_id": 0, "jumlah": 1, "keterangan": "",
    "items": [{"nama":"", "peleton":"", "bulan":"", "jumlah":1, "keterangan":""}]
  }
}

Aturan:
- "none": obrolan biasa atau menyampaikan jawaban akhir ke pengguna.
- "absen": semua urusan data siswa & absensi peleton (cari data, kartu rekap gambar visual, catat absen 1 atau banyak siswa via items, ubah nama lengkap/lapangan/kelas/peleton, tambah/hapus siswa, ubah/hapus log, buka GUI WebApp, export CSV, backup DB).
  * TAMPILAN RINGKASAN VISUAL (WAJIB): Jika pengguna meminta rekapan, ringkasan, laporan, atau tampilan rekap per bulan/peleton (contoh: "minta rekapan bulan oktober peleton putra", "lihat ringkasan bulan ini", "tampilkan rekap putri"), LANGSUNG pakai op="recap_image" dengan mengisi "peleton" ("PUTRA"/"PUTRI"/"" jika semua) dan "bulan" (nama bulan Indonesia seperti "Oktober", atau "" jika semua bulan). Ini langsung mengirim gambar dashboard visual menarik di chat Telegram (JANGAN kirim file CSV/unduhan kecuali diminta "export"/"csv"/"backup").
  * Jika ingin mencari 1 siswa tertentu atau riwayat log detail untuk menjawab pertanyaan teks/ID, pakai op="query".
  * Jika tidak yakin ID/nama lengkap/log_id, panggil op="query" dulu!
  * Jika bulan tidak disebut saat catat absen, pakai "%s".
  * Untuk mengubah nama/kelas siswa: pakai op="update_student" dengan siswa_id atau nama, lalu isi field baru (nama_lengkap / nama_lapangan / kelas / peleton).
  * Jangan pernah mengarang data siswa/angka — selalu baca dari op="query".
  * Patokan pencocokan siswa: UTAMA pakai nama panggilan (nama_lapangan), lalu kedua pakai nama lengkap (nama_lengkap).
  * KEAMANAN NAMA: Jika hasil database menyebut AMBIGU, TIDAK_PERSIS, atau TIDAK_DITEMUKAN, JANGAN menebak atau memaksakan siswa_id lain! Segera hentikan perubahan (action="none") dan jelaskan ke pengguna dengan sopan (misal: nama panggilan tsb tidak ada di database, atau cocok dengan beberapa siswa berikut lengkap dengan nama lengkap, panggilan, dan kelasnya) lalu minta konfirmasi pengguna.
- "screenshot", "sysinfo", "laragon_start", "laragon_stop", "lock", "stop_bot", "send_file", "powershell": kontrol laptop seperti biasa.
- Set "confirm": true untuk perintah sistem berisiko/merusak.`, host, user, cwd, a.inboxDir, now, curMonth, curMonth)
}

func (a *BotApp) ask9RouterWithRetry(ctx context.Context, messages []chatMessage) (*AIResponse, string, error) {
	res, raw, err := a.ask9Router(ctx, messages)
	if err == nil {
		return res, raw, nil
	}
	// Retry sekali jika respons AI gagal di-parse sebagai JSON
	if strings.Contains(err.Error(), "JSON") {
		retryMsgs := append(append([]chatMessage{}, messages...),
			chatMessage{Role: "assistant", Content: raw},
			chatMessage{Role: "user", Content: "Format balasanmu tadi bukan JSON valid. Ulangi HANYA dengan 1 objek JSON valid sesuai schema."},
		)
		return a.ask9Router(ctx, retryMsgs)
	}
	return nil, raw, err
}

func (a *BotApp) ask9Router(ctx context.Context, messages []chatMessage) (*AIResponse, string, error) {
	payload := map[string]interface{}{
		"model":       a.cfg.RouterModel,
		"messages":    messages,
		"temperature": 0.2,
		"stream":      false,
	}

	jsonData, err := json.Marshal(payload)
	if err != nil {
		return nil, "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.cfg.RouterEndpoint, bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if a.cfg.RouterAPIKey != "" {
		req.Header.Set("Authorization", "Bearer "+a.cfg.RouterAPIKey)
	}

	resp, err := a.aiClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", fmt.Errorf("gagal membaca respons 9router: %w", err)
	}
	bodyStr := strings.TrimSpace(string(body))

	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("status %d: %s", resp.StatusCode, truncateRunes(bodyStr, 500))
	}

	fullContent, err := extractRouterContent(body, bodyStr)
	if err != nil {
		return nil, "", err
	}

	aiRes, err := parseAIJSON(fullContent)
	if err != nil {
		return nil, fullContent, err
	}
	return aiRes, fullContent, nil
}

func extractRouterContent(body []byte, bodyStr string) (string, error) {
	var fullContent string

	if strings.HasPrefix(bodyStr, "data:") {
		for _, line := range strings.Split(bodyStr, "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			dataPart := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if dataPart == "[DONE]" || dataPart == "" {
				continue
			}
			var chunk struct {
				Choices []struct {
					Delta struct {
						Content string `json:"content"`
					} `json:"delta"`
					Message struct {
						Content string `json:"content"`
					} `json:"message"`
				} `json:"choices"`
			}
			if err := json.Unmarshal([]byte(dataPart), &chunk); err == nil && len(chunk.Choices) > 0 {
				if chunk.Choices[0].Delta.Content != "" {
					fullContent += chunk.Choices[0].Delta.Content
				} else if chunk.Choices[0].Message.Content != "" {
					fullContent += chunk.Choices[0].Message.Content
				}
			}
		}
	} else {
		var rawResult struct {
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(body, &rawResult); err != nil {
			return "", fmt.Errorf("gagal membaca format 9router: %v | Isi: %s", err, truncateRunes(bodyStr, 400))
		}
		if len(rawResult.Choices) > 0 {
			fullContent = rawResult.Choices[0].Message.Content
		}
	}

	if strings.TrimSpace(fullContent) == "" {
		return "", fmt.Errorf("respons kosong dari 9router: %s", truncateRunes(bodyStr, 400))
	}
	return fullContent, nil
}

func parseAIJSON(raw string) (*AIResponse, error) {
	cleaned := stripThinkingAndFences(raw)
	var lastErr error
	for offset := 0; offset < len(cleaned); {
		rel := strings.IndexByte(cleaned[offset:], '{')
		if rel == -1 {
			break
		}
		start := offset + rel
		jsonObj, end, ok := extractBalancedJSONAt(cleaned, start)
		if !ok {
			break
		}
		var aiRes AIResponse
		if err := json.Unmarshal([]byte(jsonObj), &aiRes); err == nil {
			return &aiRes, nil
		} else {
			lastErr = err
		}
		offset = end + 1
	}

	if lastErr != nil {
		return nil, fmt.Errorf("gagal membaca JSON AI: %v | Teks: %s", lastErr, truncateRunes(cleaned, 400))
	}
	return nil, fmt.Errorf("AI tidak mengembalikan JSON: %s", truncateRunes(cleaned, 400))
}

func stripThinkingAndFences(s string) string {
	s = thinkBlockRe.ReplaceAllString(s, "")
	if idx := strings.LastIndex(strings.ToLower(s), "</think>"); idx != -1 {
		s = s[idx+len("</think>"):]
	}
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	return strings.TrimSpace(s)
}

func extractBalancedJSONAt(s string, start int) (string, int, bool) {
	if start < 0 || start >= len(s) || s[start] != '{' {
		return "", -1, false
	}

	depth := 0
	inString := false
	escaped := false

	for i := start; i < len(s); i++ {
		ch := s[i]
		if escaped {
			escaped = false
			continue
		}
		if inString {
			if ch == '\\' {
				escaped = true
			} else if ch == '"' {
				inString = false
			}
			continue
		}
		switch ch {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[start : i+1], i, true
			}
		}
	}
	return "", -1, false
}

func normalizeAction(action string) string {
	act := strings.ToLower(strings.TrimSpace(action))
	switch act {
	case "", "none":
		return "none"
	case "laragon":
		return "laragon_start"
	default:
		return act
	}
}

func isDangerousCommand(command string) bool {
	if strings.TrimSpace(command) == "" {
		return false
	}
	for _, re := range dangerRules {
		if re.MatchString(command) {
			return true
		}
	}
	return false
}

func formatToolResult(action, command, output string, err error) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Aksi: %s\n", action))
	if command != "" {
		sb.WriteString(fmt.Sprintf("Perintah: %s\n", command))
	}
	if err != nil {
		sb.WriteString(fmt.Sprintf("Status: ERROR (%v)\n", err))
	} else {
		sb.WriteString("Status: OK\n")
	}
	if strings.TrimSpace(output) != "" {
		sb.WriteString("Output:\n")
		sb.WriteString(truncateRunes(output, maxToolOutputLen))
	} else {
		sb.WriteString("Output: (kosong)")
	}
	return sb.String()
}

func (a *BotApp) startTypingLoop(ctx context.Context, chatID int64) func() {
	tCtx, cancel := context.WithCancel(ctx)
	go func() {
		_, _ = a.bot.Request(tgbotapi.NewChatAction(chatID, tgbotapi.ChatTyping))
		ticker := time.NewTicker(4 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-tCtx.Done():
				return
			case <-ticker.C:
				_, _ = a.bot.Request(tgbotapi.NewChatAction(chatID, tgbotapi.ChatTyping))
			}
		}
	}()
	return cancel
}

func (a *BotApp) sendText(chatID int64, text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	_, _ = a.bot.Send(tgbotapi.NewMessage(chatID, text))
}

func (a *BotApp) sendLongMessage(chatID int64, text string) {
	for _, chunk := range splitRunes(text, maxTelegramChars) {
		a.sendText(chatID, chunk)
	}
}

func splitRunes(text string, limit int) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	if utf8.RuneCountInString(text) <= limit {
		return []string{text}
	}

	runes := []rune(text)
	var chunks []string
	for len(runes) > 0 {
		if len(runes) <= limit {
			chunks = append(chunks, string(runes))
			break
		}
		cut := limit
		// Cari pemisah baris baru/spasi agar tidak memotong kata di tengah
		for i := limit; i > limit/2; i-- {
			if runes[i-1] == '\n' || runes[i-1] == ' ' {
				cut = i
				break
			}
		}
		chunks = append(chunks, strings.TrimSpace(string(runes[:cut])))
		runes = runes[cut:]
	}
	return chunks
}

func truncateRunes(s string, maxRunes int) string {
	if maxRunes <= 0 || utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	runes := []rune(s)
	return string(runes[:maxRunes]) + "\n...(dipotong)"
}

func (a *BotApp) getHistorySnapshot() []chatMessage {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]chatMessage, len(a.history))
	copy(out, a.history)
	return out
}

func (a *BotApp) appendHistory(userMsg, assistantMsg string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.history = append(a.history,
		chatMessage{Role: "user", Content: truncateRunes(userMsg, 1200)},
		chatMessage{Role: "assistant", Content: truncateRunes(assistantMsg, 1200)},
	)
	if len(a.history) > maxHistoryMsgs {
		a.history = append([]chatMessage(nil), a.history[len(a.history)-maxHistoryMsgs:]...)
	}
}

func (a *BotApp) clearHistory() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.history = nil
}

func (a *BotApp) storePending(id string, act pendingAction) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	for k, v := range a.pending {
		if now.After(v.ExpiresAt) {
			delete(a.pending, k)
		}
	}
	a.pending[id] = act
}

func (a *BotApp) popPending(id string) (pendingAction, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	act, ok := a.pending[id]
	if ok {
		delete(a.pending, id)
	}
	if !ok || time.Now().After(act.ExpiresAt) {
		return pendingAction{}, false
	}
	return act, true
}

func newShortID() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
