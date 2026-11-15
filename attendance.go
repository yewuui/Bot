package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

type AbsenItem struct {
	SiswaID    int64  `json:"siswa_id"`
	Nama       string `json:"nama"`
	Peleton    string `json:"peleton"`
	Bulan      string `json:"bulan"`
	Jumlah     int    `json:"jumlah"`
	Keterangan string `json:"keterangan"`
}

type AbsenCommand struct {
	Op           string      `json:"op"` // query | add_logs | update_log | delete_log | set_month | add_student | update_student | delete_student | open_gui | export_csv | backup_db
	Query        string      `json:"query"`
	Peleton      string      `json:"peleton"`
	Bulan        string      `json:"bulan"`
	SiswaID      int64       `json:"siswa_id"`
	Nama         string      `json:"nama"`
	NamaLengkap  string      `json:"nama_lengkap"`
	NamaLapangan string      `json:"nama_lapangan"`
	Kelas        string      `json:"kelas"`
	LogID        int64       `json:"log_id"`
	Jumlah       int         `json:"jumlah"`
	Keterangan   string      `json:"keterangan"`
	Items        []AbsenItem `json:"items"`
}

func (a *BotApp) runAbsenTool(cmd *AbsenCommand) (result string, terminal bool, err error) {
	if cmd == nil {
		return "", false, fmt.Errorf("objek absen kosong")
	}

	op := strings.ToLower(strings.TrimSpace(cmd.Op))
	switch op {
	case "query", "cari", "list", "rekap":
		res, err := a.execAbsenQuery(cmd)
		return res, false, err

	case "add_logs", "add_log", "catat":
		res, err := a.execAbsenAddLogs(cmd)
		return res, false, err

	case "update_log", "edit_log":
		if cmd.LogID <= 0 {
			return "", false, fmt.Errorf("log_id wajib diisi (cari dulu pakai op=query)")
		}
		if err := a.db.UpdateAbsenceLog(cmd.LogID, cmd.Bulan, cmd.Jumlah, cmd.Keterangan); err != nil {
			return "", false, err
		}
		return fmt.Sprintf("Berhasil update log ID %d (bulan=%s, jumlah=%d, ket=%s)", cmd.LogID, cmd.Bulan, cmd.Jumlah, cmd.Keterangan), false, nil

	case "delete_log", "hapus_log":
		if cmd.LogID <= 0 {
			return "", false, fmt.Errorf("log_id wajib diisi (cari dulu pakai op=query)")
		}
		if err := a.db.DeleteAbsenceLog(cmd.LogID); err != nil {
			return "", false, err
		}
		return fmt.Sprintf("Berhasil hapus log ID %d", cmd.LogID), false, nil

	case "set_month":
		st, err := a.resolveSingleStudent(cmd.SiswaID, firstNonEmpty(cmd.Nama, cmd.Query, cmd.NamaLengkap), cmd.Peleton)
		if err != nil {
			return "", false, err
		}
		if err := a.db.SetStudentMonthCount(st.ID, cmd.Bulan, cmd.Jumlah, cmd.Keterangan); err != nil {
			return "", false, err
		}
		return fmt.Sprintf("Jumlah tidak hadir %s (%s) bulan %s diset menjadi %d", st.NamaLengkap, st.NamaLapangan, cmd.Bulan, cmd.Jumlah), false, nil

	case "add_student", "tambah_siswa":
		id, err := a.db.AddStudent(cmd.Peleton, firstNonEmpty(cmd.NamaLengkap, cmd.Nama), cmd.NamaLapangan, cmd.Kelas)
		if err != nil {
			return "", false, err
		}
		return fmt.Sprintf("Berhasil menambahkan siswa baru ID=%d: %s (panggilan: %s, kelas: %s, peleton: %s)",
			id, firstNonEmpty(cmd.NamaLengkap, cmd.Nama), cmd.NamaLapangan, cmd.Kelas, strings.ToUpper(cmd.Peleton)), false, nil

	case "update_student", "edit_siswa":
		st, err := a.resolveSingleStudent(cmd.SiswaID, firstNonEmpty(cmd.Nama, cmd.Query), cmd.Peleton)
		if err != nil {
			return "", false, err
		}
		if err := a.db.UpdateStudent(st.ID, cmd.Peleton, cmd.NamaLengkap, cmd.NamaLapangan, cmd.Kelas); err != nil {
			return "", false, err
		}
		return fmt.Sprintf("Data siswa ID=%d (%s) berhasil diperbarui.", st.ID, st.NamaLengkap), false, nil

	case "delete_student", "hapus_siswa":
		st, err := a.resolveSingleStudent(cmd.SiswaID, firstNonEmpty(cmd.Nama, cmd.Query, cmd.NamaLengkap), cmd.Peleton)
		if err != nil {
			return "", false, err
		}
		if err := a.db.DeleteStudent(st.ID); err != nil {
			return "", false, err
		}
		return fmt.Sprintf("Siswa ID=%d (%s) berhasil dihapus.", st.ID, st.NamaLengkap), false, nil

	case "open_gui", "gui":
		return "GUI_READY", true, nil

	case "recap_image", "visual_recap", "rekap_gambar", "ringkasan_gambar", "card", "gambar":
		pel := ""
		if strings.TrimSpace(cmd.Peleton) != "" {
			p, err := NormalizePeleton(cmd.Peleton)
			if err != nil {
				return "", false, err
			}
			pel = p
		}
		bul := ""
		if strings.TrimSpace(cmd.Bulan) != "" {
			b, err := NormalizeMonth(cmd.Bulan)
			if err != nil {
				return "", false, err
			}
			bul = b
		}
		return fmt.Sprintf("RECAP_IMAGE|%s|%s", pel, bul), true, nil

	case "export_csv", "export":
		return "EXPORT_CSV", true, nil

	case "backup_db", "backup":
		return "BACKUP_DB", true, nil

	default:
		return "", false, fmt.Errorf("operasi absen tidak dikenali: %q", cmd.Op)
	}
}

func formatStudentCandidates(list []Student, limit int) string {
	var cands []string
	for i, m := range list {
		if i >= limit {
			cands = append(cands, fmt.Sprintf("...dan %d lainnya", len(list)-limit))
			break
		}
		cands = append(cands, fmt.Sprintf("ID=%d | %s (panggilan: %s, kelas: %s, %s)",
			m.ID, m.NamaLengkap, m.NamaLapangan, m.Kelas, m.Peleton))
	}
	return strings.Join(cands, "; ")
}

func (a *BotApp) resolveSingleStudent(id int64, nameQuery, peleton string) (*Student, error) {
	ov, err := a.db.GetOverview()
	if err != nil {
		return nil, err
	}
	if id > 0 {
		for i := range ov.Students {
			if ov.Students[i].ID == id {
				return &ov.Students[i], nil
			}
		}
		return nil, fmt.Errorf("siswa ID %d tidak ditemukan di database", id)
	}

	q := strings.TrimSpace(nameQuery)
	if q == "" {
		return nil, fmt.Errorf("siswa_id atau nama siswa wajib diisi")
	}

	m, err := a.db.MatchStudents(q, peleton)
	if err != nil {
		return nil, err
	}

	if len(m.Direct) == 1 {
		return &m.Direct[0], nil
	}
	if len(m.Direct) > 1 {
		return nil, fmt.Errorf("AMBIGU: nama %q cocok dengan %d siswa -> %s. Tanyakan ke pengguna yang mana yang dimaksud (sebutkan nama lengkap/kelas/peleton)",
			q, len(m.Direct), formatStudentCandidates(m.Direct, 8))
	}

	// Tidak ada kecocokan langsung: cek partial atau fuzzy typo
	var suggestions []Student
	seen := make(map[int64]bool)
	for _, s := range append(append([]Student{}, m.Partial...), m.Fuzzy...) {
		if !seen[s.ID] {
			seen[s.ID] = true
			suggestions = append(suggestions, s)
		}
	}
	if len(suggestions) > 0 {
		return nil, fmt.Errorf("TIDAK_PERSIS: nama/panggilan %q tidak ditemukan persis di database. Kemungkinan mirip -> %s. Jangan langsung diubah/dicatat, konfirmasi dulu ke pengguna apakah salah satu nama ini yang dimaksud",
			q, formatStudentCandidates(suggestions, 6))
	}

	return nil, fmt.Errorf("TIDAK_DITEMUKAN: nama/panggilan %q sama sekali tidak ada di database siswa (putra maupun putri). Beri tahu pengguna bahwa nama tersebut tidak terdaftar", q)
}

func (a *BotApp) execAbsenQuery(cmd *AbsenCommand) (string, error) {
	ov, err := a.db.GetOverview()
	if err != nil {
		return "", err
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("RINGKASAN: Total Siswa=%d (Putra=%d, Putri=%d) | Total Ketidakhadiran=%d (Putra=%d, Putri=%d) | Bulan aktif=%s\n",
		ov.Summary.TotalSiswa, ov.Summary.TotalPutraSiswa, ov.Summary.TotalPutriSiswa,
		ov.Summary.TotalTidakHadir, ov.Summary.TotalPutra, ov.Summary.TotalPutri, CurrentAcademicMonth()))

	if strings.TrimSpace(cmd.Query) == "" && strings.TrimSpace(cmd.Peleton) == "" && cmd.SiswaID == 0 && strings.TrimSpace(cmd.Bulan) == "" {
		sb.WriteString("PER BULAN:\n")
		for _, m := range ov.Summary.Monthly {
			if m.Total > 0 {
				sb.WriteString(fmt.Sprintf("- %s: Total=%d (Putra=%d, Putri=%d)\n", m.Bulan, m.Total, m.Putra, m.Putri))
			}
		}
		sorted := append([]Student(nil), ov.Students...)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i].Total > sorted[j].Total })
		sb.WriteString("TOP KETIDAKHADIRAN:\n")
		for i := 0; i < len(sorted) && i < 12; i++ {
			if sorted[i].Total == 0 {
				break
			}
			sb.WriteString(fmt.Sprintf("- ID=%d | %s (%s | %s | %s) => %d kali\n",
				sorted[i].ID, sorted[i].NamaLengkap, sorted[i].NamaLapangan, sorted[i].Kelas, sorted[i].Peleton, sorted[i].Total))
		}
		return sb.String(), nil
	}

	var matched []Student
	var fuzzySuggestions []Student
	if cmd.SiswaID > 0 {
		for _, s := range ov.Students {
			if s.ID == cmd.SiswaID {
				matched = append(matched, s)
				break
			}
		}
	} else {
		mr, err := a.db.MatchStudents(cmd.Query, cmd.Peleton)
		if err != nil {
			return "", err
		}
		if len(mr.Direct) > 0 {
			matched = mr.Direct
		} else if len(mr.Partial) > 0 {
			matched = mr.Partial
		} else {
			fuzzySuggestions = mr.Fuzzy
		}
	}

	sb.WriteString(fmt.Sprintf("SISWA DITEMUKAN (%d):\n", len(matched)))
	if len(matched) == 0 && len(fuzzySuggestions) > 0 {
		sb.WriteString("NAMA MIRIP / SARAN TYPO:\n")
		sb.WriteString("- " + formatStudentCandidates(fuzzySuggestions, 6) + "\n")
	}
	for i, s := range matched {
		if i >= 40 {
			sb.WriteString(fmt.Sprintf("...dan %d siswa lainnya.\n", len(matched)-40))
			break
		}
		var mParts []string
		for _, m := range AcademicMonths {
			if v := s.Months[m]; v > 0 {
				mParts = append(mParts, fmt.Sprintf("%s:%d", m[:3], v))
			}
		}
		sb.WriteString(fmt.Sprintf("- ID=%d | %s | Panggilan=%s | %s | %s | Total=%d [%s]\n",
			s.ID, s.NamaLengkap, s.NamaLapangan, s.Kelas, s.Peleton, s.Total, strings.Join(mParts, ", ")))
	}

	// Sertakan log terkait hanya jika ada siswa yang cocok (atau filter bulan tanpa query nama)
	bulanFilter := ""
	if strings.TrimSpace(cmd.Bulan) != "" {
		bulanFilter, _ = NormalizeMonth(cmd.Bulan)
	}
	idSet := make(map[int64]bool, len(matched))
	for _, s := range matched {
		idSet[s.ID] = true
	}

	var relLogs []AbsenceLog
	if len(matched) > 0 || (strings.TrimSpace(cmd.Query) == "" && cmd.SiswaID == 0) {
		for _, l := range ov.Logs {
			if bulanFilter != "" && l.Bulan != bulanFilter {
				continue
			}
			if len(matched) > 0 && !idSet[l.SiswaID] {
				continue
			}
			relLogs = append(relLogs, l)
		}
	}

	if len(relLogs) > 0 && (len(matched) <= 10 || bulanFilter != "") {
		sb.WriteString(fmt.Sprintf("LOG TERKAIT (%d):\n", len(relLogs)))
		for i, l := range relLogs {
			if i >= 25 {
				sb.WriteString(fmt.Sprintf("...dan %d log lainnya.\n", len(relLogs)-25))
				break
			}
			sb.WriteString(fmt.Sprintf("- LogID=%d | %s (%s) | %s | %dx | Ket: %s | %s\n",
				l.ID, l.NamaLengkap, l.NamaLapangan, l.Bulan, l.Jumlah, l.Keterangan, l.DicatatAt))
		}
	}

	return sb.String(), nil
}

func (a *BotApp) execAbsenAddLogs(cmd *AbsenCommand) (string, error) {
	items := cmd.Items
	if len(items) == 0 && (cmd.SiswaID > 0 || strings.TrimSpace(cmd.Nama) != "" || strings.TrimSpace(cmd.Query) != "") {
		items = []AbsenItem{{
			SiswaID:    cmd.SiswaID,
			Nama:       firstNonEmpty(cmd.Nama, cmd.Query, cmd.NamaLengkap),
			Peleton:    cmd.Peleton,
			Bulan:      cmd.Bulan,
			Jumlah:     cmd.Jumlah,
			Keterangan: cmd.Keterangan,
		}}
	}
	if len(items) == 0 {
		return "", fmt.Errorf("tidak ada siswa yang disebutkan untuk dicatat absennya")
	}

	type resolvedEntry struct {
		st    *Student
		bulan string
		jml   int
		ket   string
	}

	var validEntries []resolvedEntry
	var errMsgs []string

	// Validasi SEMUA nama dulu sebelum menulis ke database agar tidak tersimpan setengah-setengah
	for _, it := range items {
		st, err := a.resolveSingleStudent(it.SiswaID, it.Nama, firstNonEmpty(it.Peleton, cmd.Peleton))
		if err != nil {
			errMsgs = append(errMsgs, err.Error())
			continue
		}
		bulan := firstNonEmpty(it.Bulan, cmd.Bulan, CurrentAcademicMonth())
		jml := it.Jumlah
		if jml <= 0 {
			jml = 1
		}
		ket := firstNonEmpty(it.Keterangan, cmd.Keterangan, "tidak hadir")
		validEntries = append(validEntries, resolvedEntry{st: st, bulan: bulan, jml: jml, ket: ket})
	}

	if len(errMsgs) > 0 {
		var sb strings.Builder
		sb.WriteString("DIBATALKAN SEMENTARA (belum ada data yang disimpan karena ada nama yang tidak ditemukan/ambigu):\n")
		sb.WriteString(strings.Join(errMsgs, "\n"))
		if len(validEntries) > 0 {
			var ready []string
			for _, e := range validEntries {
				ready = append(ready, fmt.Sprintf("%s (%s)", e.st.NamaLengkap, e.st.NamaLapangan))
			}
			sb.WriteString("\nNama lain yang sudah valid & siap dicatat setelah konfirmasi: " + strings.Join(ready, ", "))
		}
		return "", fmt.Errorf("%s", sb.String())
	}

	var okMsgs []string
	for _, e := range validEntries {
		logID, err := a.db.AddAbsenceLog(e.st.ID, e.bulan, e.jml, e.ket)
		if err != nil {
			errMsgs = append(errMsgs, fmt.Sprintf("%s: %v", e.st.NamaLengkap, err))
			continue
		}
		okMsgs = append(okMsgs, fmt.Sprintf("LogID=%d: %s (%s, %s) +%d [%s, %s]", logID, e.st.NamaLengkap, e.st.NamaLapangan, e.st.Kelas, e.jml, e.bulan, e.ket))
	}

	var sb strings.Builder
	if len(okMsgs) > 0 {
		sb.WriteString("BERHASIL DICATAT:\n" + strings.Join(okMsgs, "\n") + "\n")
	}
	if len(errMsgs) > 0 {
		sb.WriteString("PERLU PERHATIAN:\n" + strings.Join(errMsgs, "\n"))
	}
	return sb.String(), nil
}

// ================= TELEGRAM CHAT UI & EXPORT =================

func (a *BotApp) sendAttendanceSummary(chatID int64) {
	ov, err := a.db.GetOverview()
	if err != nil {
		a.sendText(chatID, "❌ Gagal memuat rekap absensi: "+err.Error())
		return
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("📊 *REKAP PELETON 2026/2027*\n"))
	sb.WriteString(fmt.Sprintf("👥 Total Siswa: *%d* (Putra: %d | Putri: %d)\n",
		ov.Summary.TotalSiswa, ov.Summary.TotalPutraSiswa, ov.Summary.TotalPutriSiswa))
	sb.WriteString(fmt.Sprintf("📌 Total Tidak Hadir: *%d* (Putra: %d | Putri: %d)\n\n",
		ov.Summary.TotalTidakHadir, ov.Summary.TotalPutra, ov.Summary.TotalPutri))

	sb.WriteString("*Per Bulan:*\n")
	for _, m := range ov.Summary.Monthly {
		if m.Total > 0 || m.Bulan == CurrentAcademicMonth() {
			sb.WriteString(fmt.Sprintf("• %s: *%d* (Pa: %d | Pi: %d)\n", m.Bulan, m.Total, m.Putra, m.Putri))
		}
	}

	sorted := append([]Student(nil), ov.Students...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Total > sorted[j].Total })
	sb.WriteString("\n*Top Ketidakhadiran:*\n")
	shown := 0
	for _, s := range sorted {
		if s.Total == 0 || shown >= 8 {
			break
		}
		sb.WriteString(fmt.Sprintf("%d. %s (%s) — *%dx*\n", shown+1, s.NamaLapangan, s.Kelas, s.Total))
		shown++
	}

	msg := tgbotapi.NewMessage(chatID, sb.String())
	msg.ParseMode = "Markdown"
	msg.ReplyMarkup = a.buildAbsenMenuKeyboard()
	if _, err := a.bot.Send(msg); err != nil {
		msg.ParseMode = ""
		_, _ = a.bot.Send(msg)
	}
}

func (a *BotApp) buildAbsenMenuKeyboard() tgbotapi.InlineKeyboardMarkup {
	return tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🖼 Kartu Rekap Visual", "abs:img::"),
			tgbotapi.NewInlineKeyboardButtonData("🌐 Buka GUI WebApp", "abs:gui"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("👦 Absen Putra", "abs:list:PUTRA:0"),
			tgbotapi.NewInlineKeyboardButtonData("👧 Absen Putri", "abs:list:PUTRI:0"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("📥 Export CSV", "abs:csv"),
			tgbotapi.NewInlineKeyboardButtonData("💾 Backup DB", "abs:backup"),
		),
	)
}

func parseRecapArgs(raw string) (peleton, bulan string, err error) {
	tokens := strings.Fields(strings.ToLower(strings.TrimSpace(raw)))
	for _, t := range tokens {
		switch t {
		case "peleton", "bulan", "bln", "rekap", "rekapan", "ringkasan", "absen", "absensi", "data":
			continue
		case "semua", "all", "total", "tahun", "setahun":
			continue
		}
		if p, pErr := NormalizePeleton(t); pErr == nil {
			peleton = p
			continue
		}
		if b, bErr := NormalizeMonth(t); bErr == nil {
			bulan = b
			continue
		}
		return "", "", fmt.Errorf("filter tidak dikenali: %q (contoh: `/rekap oktober putra`)", t)
	}
	return peleton, bulan, nil
}

func (a *BotApp) buildRecapKeyboard(pel, bul string) tgbotapi.InlineKeyboardMarkup {
	mark := func(active bool, label string) string {
		if active {
			return "• " + label
		}
		return label
	}

	curMonth := CurrentAcademicMonth()
	row1 := tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData(mark(pel == "", "📊 Semua"), fmt.Sprintf("abs:img::%s", bul)),
		tgbotapi.NewInlineKeyboardButtonData(mark(pel == "PUTRA", "👦 Putra"), fmt.Sprintf("abs:img:PUTRA:%s", bul)),
		tgbotapi.NewInlineKeyboardButtonData(mark(pel == "PUTRI", "👧 Putri"), fmt.Sprintf("abs:img:PUTRI:%s", bul)),
	)

	var row2 []tgbotapi.InlineKeyboardButton
	if bul == "" {
		row2 = tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("📅 "+curMonth, fmt.Sprintf("abs:img:%s:%s", pel, curMonth)),
			tgbotapi.NewInlineKeyboardButtonData("📅 Oktober", fmt.Sprintf("abs:img:%s:Oktober", pel)),
			tgbotapi.NewInlineKeyboardButtonData("• 🗓 Setahun", fmt.Sprintf("abs:img:%s:", pel)),
		)
	} else {
		idx := 0
		for i, m := range AcademicMonths {
			if m == bul {
				idx = i
				break
			}
		}
		prevM := AcademicMonths[(idx-1+len(AcademicMonths))%len(AcademicMonths)]
		nextM := AcademicMonths[(idx+1)%len(AcademicMonths)]
		row2 = tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("◀ "+prevM[:3], fmt.Sprintf("abs:img:%s:%s", pel, prevM)),
			tgbotapi.NewInlineKeyboardButtonData("🗓 Setahun", fmt.Sprintf("abs:img:%s:", pel)),
			tgbotapi.NewInlineKeyboardButtonData(nextM[:3]+" ▶", fmt.Sprintf("abs:img:%s:%s", pel, nextM)),
		)
	}

	row3 := tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData("🌐 Buka & Edit di GUI", fmt.Sprintf("abs:gui:%s:%s", pel, bul)),
	)

	return tgbotapi.NewInlineKeyboardMarkup(row1, row2, row3)
}

func (a *BotApp) sendRecapPhoto(chatID int64, customIntro, peleton, bulan string) {
	ov, err := a.db.GetOverview()
	if err != nil {
		a.sendText(chatID, "❌ Gagal memuat data absensi: "+err.Error())
		return
	}

	pngBytes, caption, err := RenderRecapCardPNG(ov, peleton, bulan)
	if err != nil {
		a.sendText(chatID, "❌ Gagal membuat gambar rekap: "+err.Error())
		return
	}
	if strings.TrimSpace(customIntro) != "" {
		caption = truncateRunes(strings.TrimSpace(customIntro), 300) + "\n\n" + caption
	}

	pelNorm, _ := NormalizePeleton(peleton)
	bulNorm := ""
	if strings.TrimSpace(bulan) != "" {
		bulNorm, _ = NormalizeMonth(bulan)
	}

	photo := tgbotapi.NewPhoto(chatID, tgbotapi.FileBytes{
		Name:  "rekap_absensi.png",
		Bytes: pngBytes,
	})
	photo.Caption = caption
	photo.ParseMode = "Markdown"
	photo.ReplyMarkup = a.buildRecapKeyboard(pelNorm, bulNorm)
	if _, err := a.bot.Send(photo); err != nil {
		photo.ParseMode = ""
		_, _ = a.bot.Send(photo)
	}
}

func (a *BotApp) editRecapPhoto(q *tgbotapi.CallbackQuery, peleton, bulan string) {
	if q.Message == nil {
		return
	}
	ov, err := a.db.GetOverview()
	if err != nil {
		return
	}
	pngBytes, caption, err := RenderRecapCardPNG(ov, peleton, bulan)
	if err != nil {
		return
	}

	pelNorm, _ := NormalizePeleton(peleton)
	bulNorm := ""
	if strings.TrimSpace(bulan) != "" {
		bulNorm, _ = NormalizeMonth(bulan)
	}

	kb := a.buildRecapKeyboard(pelNorm, bulNorm)
	media := tgbotapi.NewInputMediaPhoto(tgbotapi.FileBytes{
		Name:  "rekap_absensi.png",
		Bytes: pngBytes,
	})
	media.Caption = caption
	media.ParseMode = "Markdown"

	edit := tgbotapi.EditMessageMediaConfig{
		BaseEdit: tgbotapi.BaseEdit{
			ChatID:      q.Message.Chat.ID,
			MessageID:   q.Message.MessageID,
			ReplyMarkup: &kb,
		},
		Media: media,
	}
	if _, err := a.bot.Send(edit); err != nil {
		// Fallback jika pesan asal bukan media (misalnya dari tombol menu teks /absen)
		a.sendRecapPhoto(q.Message.Chat.ID, "", pelNorm, bulNorm)
	}
}

func (a *BotApp) handleAbsenCallback(q *tgbotapi.CallbackQuery, chatID int64) {
	parts := strings.Split(q.Data, ":")
	if len(parts) < 2 {
		return
	}

	switch parts[1] {
	case "gui":
		pel, bul := "", ""
		if len(parts) >= 3 {
			pel = parts[2]
		}
		if len(parts) >= 4 {
			bul = parts[3]
		}
		_, _ = a.bot.Request(tgbotapi.NewCallback(q.ID, "Membuka GUI..."))
		a.sendWebAppButtonWithFilter(chatID, "📊 Panel GUI Absensi Peleton", pel, bul)

	case "img":
		// abs:img:<peleton>:<bulan>
		pel, bul := "", ""
		if len(parts) >= 3 {
			pel = parts[2]
		}
		if len(parts) >= 4 {
			bul = parts[3]
		}
		_, _ = a.bot.Request(tgbotapi.NewCallback(q.ID, "Memperbarui kartu rekap..."))
		a.editRecapPhoto(q, pel, bul)

	case "csv":
		_, _ = a.bot.Request(tgbotapi.NewCallback(q.ID, "Menyiapkan CSV..."))
		a.sendAttendanceCSV(chatID)

	case "backup":
		_, _ = a.bot.Request(tgbotapi.NewCallback(q.ID, "Mengirim backup..."))
		a.sendDatabaseBackup(chatID)

	case "menu":
		_, _ = a.bot.Request(tgbotapi.NewCallback(q.ID, ""))
		a.editAbsenMenu(q)

	case "list":
		// abs:list:PUTRA:0
		if len(parts) < 4 {
			return
		}
		peleton := parts[2]
		page, _ := strconv.Atoi(parts[3])
		_, _ = a.bot.Request(tgbotapi.NewCallback(q.ID, ""))
		a.editAbsenStudentList(q, peleton, page)

	case "st":
		// abs:st:<id>
		if len(parts) < 3 {
			return
		}
		id, _ := strconv.ParseInt(parts[2], 10, 64)
		_, _ = a.bot.Request(tgbotapi.NewCallback(q.ID, ""))
		a.editAbsenStudentCard(q, id)

	case "add":
		// abs:add:<id>:<ket>
		if len(parts) < 4 {
			return
		}
		id, _ := strconv.ParseInt(parts[2], 10, 64)
		ket := parts[3]
		bulan := CurrentAcademicMonth()
		if _, err := a.db.AddAbsenceLog(id, bulan, 1, ket); err != nil {
			_, _ = a.bot.Request(tgbotapi.NewCallback(q.ID, "❌ "+err.Error()))
			return
		}
		_, _ = a.bot.Request(tgbotapi.NewCallback(q.ID, fmt.Sprintf("✅ +1 %s (%s)", ket, bulan)))
		a.editAbsenStudentCard(q, id)

	case "dellog":
		// abs:dellog:<siswa_id>:<log_id>
		if len(parts) < 4 {
			return
		}
		sid, _ := strconv.ParseInt(parts[2], 10, 64)
		lid, _ := strconv.ParseInt(parts[3], 10, 64)
		if err := a.db.DeleteAbsenceLog(lid); err != nil {
			_, _ = a.bot.Request(tgbotapi.NewCallback(q.ID, "❌ "+err.Error()))
			return
		}
		_, _ = a.bot.Request(tgbotapi.NewCallback(q.ID, "🗑 Log terakhir dihapus"))
		a.editAbsenStudentCard(q, sid)
	}
}

func (a *BotApp) editAbsenMenu(q *tgbotapi.CallbackQuery) {
	if q.Message == nil {
		return
	}
	edit := tgbotapi.NewEditMessageText(q.Message.Chat.ID, q.Message.MessageID, "📋 Pilih menu absensi peleton:")
	kb := a.buildAbsenMenuKeyboard()
	edit.ReplyMarkup = &kb
	_, _ = a.bot.Send(edit)
}

func (a *BotApp) editAbsenStudentList(q *tgbotapi.CallbackQuery, peleton string, page int) {
	if q.Message == nil {
		return
	}
	list, err := a.db.FindStudents("", peleton)
	if err != nil {
		return
	}
	const perPage = 10
	if page < 0 {
		page = 0
	}
	maxPage := (len(list) - 1) / perPage
	if page > maxPage {
		page = maxPage
	}
	start := page * perPage
	end := start + perPage
	if end > len(list) {
		end = len(list)
	}

	var rows [][]tgbotapi.InlineKeyboardButton
	for i := start; i < end; i += 2 {
		var row []tgbotapi.InlineKeyboardButton
		for j := i; j < i+2 && j < end; j++ {
			s := list[j]
			label := fmt.Sprintf("%s (%d)", s.NamaLapangan, s.Total)
			row = append(row, tgbotapi.NewInlineKeyboardButtonData(label, fmt.Sprintf("abs:st:%d", s.ID)))
		}
		rows = append(rows, row)
	}

	var nav []tgbotapi.InlineKeyboardButton
	if page > 0 {
		nav = append(nav, tgbotapi.NewInlineKeyboardButtonData("◀ Prev", fmt.Sprintf("abs:list:%s:%d", peleton, page-1)))
	}
	nav = append(nav, tgbotapi.NewInlineKeyboardButtonData("🏠 Menu", "abs:menu"))
	if page < maxPage {
		nav = append(nav, tgbotapi.NewInlineKeyboardButtonData("Next ▶", fmt.Sprintf("abs:list:%s:%d", peleton, page+1)))
	}
	rows = append(rows, nav)

	kb := tgbotapi.NewInlineKeyboardMarkup(rows...)
	edit := tgbotapi.NewEditMessageText(
		q.Message.Chat.ID, q.Message.MessageID,
		fmt.Sprintf("Daftar Peleton *%s* (Hal %d/%d) — Tap nama untuk catat/lihat:", peleton, page+1, maxPage+1),
	)
	edit.ParseMode = "Markdown"
	edit.ReplyMarkup = &kb
	_, _ = a.bot.Send(edit)
}

func (a *BotApp) editAbsenStudentCard(q *tgbotapi.CallbackQuery, siswaID int64) {
	if q.Message == nil {
		return
	}
	ov, err := a.db.GetOverview()
	if err != nil {
		return
	}
	var st *Student
	for i := range ov.Students {
		if ov.Students[i].ID == siswaID {
			st = &ov.Students[i]
			break
		}
	}
	if st == nil {
		return
	}

	var latestLog *AbsenceLog
	var histLines []string
	for i := range ov.Logs {
		if ov.Logs[i].SiswaID == siswaID {
			if latestLog == nil {
				latestLog = &ov.Logs[i]
			}
			if len(histLines) < 5 {
				histLines = append(histLines, fmt.Sprintf("• %s: %dx (%s)", ov.Logs[i].Bulan, ov.Logs[i].Jumlah, ov.Logs[i].Keterangan))
			}
		}
	}

	text := fmt.Sprintf("👤 *%s*\nPanggilan: %s | Kelas: %s | %s\nTotal tidak hadir: *%dx*\n\nRiwayat terbaru:\n%s\n\nCatat +1 di bulan *%s*:",
		st.NamaLengkap, st.NamaLapangan, st.Kelas, st.Peleton, st.Total,
		firstNonEmpty(strings.Join(histLines, "\n"), "• Belum ada ketidakhadiran"),
		CurrentAcademicMonth(),
	)

	rows := [][]tgbotapi.InlineKeyboardButton{
		{
			tgbotapi.NewInlineKeyboardButtonData("🤒 +1 Sakit", fmt.Sprintf("abs:add:%d:sakit", st.ID)),
			tgbotapi.NewInlineKeyboardButtonData("📝 +1 Izin", fmt.Sprintf("abs:add:%d:izin", st.ID)),
		},
		{
			tgbotapi.NewInlineKeyboardButtonData("❌ +1 Alfa", fmt.Sprintf("abs:add:%d:alfa", st.ID)),
			tgbotapi.NewInlineKeyboardButtonData("📌 +1 TM", fmt.Sprintf("abs:add:%d:tm", st.ID)),
		},
	}
	if latestLog != nil {
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🗑 Hapus Log Terakhir", fmt.Sprintf("abs:dellog:%d:%d", st.ID, latestLog.ID)),
		))
	}
	rows = append(rows, tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData("⬅ Kembali", fmt.Sprintf("abs:list:%s:0", st.Peleton)),
	))

	kb := tgbotapi.NewInlineKeyboardMarkup(rows...)
	edit := tgbotapi.NewEditMessageText(q.Message.Chat.ID, q.Message.MessageID, text)
	edit.ParseMode = "Markdown"
	edit.ReplyMarkup = &kb
	_, _ = a.bot.Send(edit)
}

func (a *BotApp) sendAttendanceCSV(chatID int64) {
	data, err := a.db.ExportCSVBytes()
	if err != nil {
		a.sendText(chatID, "❌ Gagal export CSV: "+err.Error())
		return
	}
	doc := tgbotapi.NewDocument(chatID, tgbotapi.FileBytes{
		Name:  "rekap-absensi-peleton.csv",
		Bytes: data,
	})
	doc.Caption = "📊 Rekap Absensi Peleton 2026/2027 (CSV)"
	if _, err := a.bot.Send(doc); err != nil {
		a.sendText(chatID, "❌ Gagal mengirim CSV: "+err.Error())
	}
}

func (a *BotApp) sendDatabaseBackup(chatID int64) {
	dbPath := a.dbPath
	if _, err := os.Stat(dbPath); err != nil {
		a.sendText(chatID, "❌ File database tidak ditemukan.")
		return
	}
	doc := tgbotapi.NewDocument(chatID, tgbotapi.FilePath(dbPath))
	doc.Caption = "💾 Backup SQLite Database (" + filepath.Base(dbPath) + ")"
	if _, err := a.bot.Send(doc); err != nil {
		a.sendText(chatID, "❌ Gagal mengirim backup DB: "+err.Error())
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
