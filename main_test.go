package main

import (
	"bytes"
	"image/png"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestParseSlashCommand(t *testing.T) {
	tests := []struct {
		in      string
		wantCmd string
		wantArg string
	}{
		{"/start", "/start", ""},
		{"/ss@MyLaptopBot", "/ss", ""},
		{"/cmd", "/cmd", ""},
		{"/cmd   Get-Date ", "/cmd", "Get-Date"},
		{"/get@MyBot C:\\Users\\test\\file name.txt", "/get", `C:\Users\test\file name.txt`},
	}

	for _, tc := range tests {
		cmd, arg := parseSlashCommand(tc.in)
		if cmd != tc.wantCmd || arg != tc.wantArg {
			t.Errorf("parseSlashCommand(%q) = (%q, %q), want (%q, %q)", tc.in, cmd, arg, tc.wantCmd, tc.wantArg)
		}
	}
}

func TestParseAIJSON(t *testing.T) {
	raw := "<think>berpikir {sementara}...</think>\n```json\n{\"reply\":\"Halo {bro}!\",\"action\":\"powershell\",\"command\":\"Get-Date\",\"confirm\":false}\n```\ncatatan tambahan"
	res, err := parseAIJSON(raw)
	if err != nil {
		t.Fatalf("parseAIJSON failed: %v", err)
	}
	if res.Reply != "Halo {bro}!" || res.Action != "powershell" || res.Command != "Get-Date" || res.Confirm {
		t.Fatalf("unexpected parsed result: %+v", res)
	}

	// Prefix teks berisi kurung kurawal tidak valid diikuti JSON valid
	raw2 := "lihat {ini bukan json} lalu:\n{\"reply\":\"Siap\",\"action\":\"none\",\"command\":\"\",\"confirm\":false}"
	res2, err := parseAIJSON(raw2)
	if err != nil {
		t.Fatalf("parseAIJSON fallback failed: %v", err)
	}
	if res2.Reply != "Siap" || res2.Action != "none" {
		t.Fatalf("unexpected parsed result 2: %+v", res2)
	}
}

func TestExtractRouterContent(t *testing.T) {
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"{\\\"reply\\\":\\\"ok\\\",\"}}]}\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"\\\"action\\\":\\\"none\\\"}\"}}]}\n\ndata: [DONE]\n"
	content, err := extractRouterContent([]byte(sse), strings.TrimSpace(sse))
	if err != nil {
		t.Fatalf("SSE extract failed: %v", err)
	}
	res, err := parseAIJSON(content)
	if err != nil {
		t.Fatalf("SSE JSON parse failed: %v", err)
	}
	if res.Reply != "ok" || res.Action != "none" {
		t.Fatalf("unexpected SSE result: %+v", res)
	}

	std := `{"choices":[{"message":{"content":"{\"reply\":\"hai\",\"action\":\"screenshot\"}"}}]}`
	content2, err := extractRouterContent([]byte(std), std)
	if err != nil {
		t.Fatalf("standard JSON extract failed: %v", err)
	}
	if !strings.Contains(content2, "screenshot") {
		t.Fatalf("unexpected standard content: %q", content2)
	}
}

func TestIsDangerousCommand(t *testing.T) {
	safe := []string{
		"Get-Process",
		"Get-ChildItem D:\\bot",
		"Get-CimInstance Win32_Battery",
		"ipconfig",
	}
	for _, cmd := range safe {
		if isDangerousCommand(cmd) {
			t.Errorf("expected safe command, got dangerous: %q", cmd)
		}
	}

	risky := []string{
		"Remove-Item D:\\temp\\* -Recurse",
		"Get-Process notepad | Stop-Process -Force",
		"shutdown /s /t 0",
		"del C:\\foo.txt",
		"taskkill /IM chrome.exe /F",
		"reg delete HKCU\\Software\\Test",
	}
	for _, cmd := range risky {
		if !isDangerousCommand(cmd) {
			t.Errorf("expected dangerous command, got safe: %q", cmd)
		}
	}
}

func TestSplitAndTruncateRunesUTF8(t *testing.T) {
	emojis := strings.Repeat("🔥", 50)
	chunks := splitRunes(emojis, 20)
	if len(chunks) != 3 {
		t.Fatalf("expected 3 chunks, got %d", len(chunks))
	}
	for _, c := range chunks {
		if !utf8.ValidString(c) {
			t.Fatalf("chunk contains invalid UTF-8: %q", c)
		}
	}

	trunc := truncateRunes(emojis, 10)
	if !utf8.ValidString(trunc) {
		t.Fatalf("truncated string contains invalid UTF-8")
	}
	if !strings.HasSuffix(trunc, "...(dipotong)") {
		t.Fatalf("missing truncation suffix: %q", trunc)
	}
}

func TestHistoryAndPendingStore(t *testing.T) {
	app := &BotApp{pending: make(map[string]pendingAction)}
	for i := 0; i < 20; i++ {
		app.appendHistory("u", "a")
	}
	if got := len(app.getHistorySnapshot()); got != maxHistoryMsgs {
		t.Fatalf("history length = %d, want %d", got, maxHistoryMsgs)
	}
	app.clearHistory()
	if got := len(app.getHistorySnapshot()); got != 0 {
		t.Fatalf("expected empty history after clear, got %d", got)
	}

	app.storePending("valid", pendingAction{Action: "lock", ExpiresAt: time.Now().Add(time.Minute)})
	app.storePending("expired", pendingAction{Action: "lock", ExpiresAt: time.Now().Add(-time.Minute)})

	if _, ok := app.popPending("expired"); ok {
		t.Fatalf("expired action should not pop")
	}
	act, ok := app.popPending("valid")
	if !ok || act.Action != "lock" {
		t.Fatalf("valid action failed to pop: %+v, ok=%v", act, ok)
	}
	if _, ok := app.popPending("valid"); ok {
		t.Fatalf("valid action should only pop once")
	}
}

func requireTestExcelSeedPath(t *testing.T) string {
	t.Helper()
	loadDotEnv(".env")
	excelPath := strings.TrimSpace(os.Getenv("EXCEL_SEED_PATH"))
	if excelPath == "" {
		t.Skip("EXCEL_SEED_PATH not set; skipping local Excel seed test")
	}
	if _, err := os.Stat(excelPath); err != nil {
		t.Skipf("local Excel seed file not found (%s): %v", excelPath, err)
	}
	return excelPath
}

func TestExcelImportAndAttendanceCRUD(t *testing.T) {
	excelPath := requireTestExcelSeedPath(t)

	dbPath := t.TempDir() + `\test_absensi.db`
	db, err := OpenAttendanceDB(dbPath)
	if err != nil {
		t.Fatalf("OpenAttendanceDB: %v", err)
	}
	defer db.Close()

	msg, err := db.SeedFromExcelIfEmpty(excelPath)
	if err != nil {
		t.Fatalf("SeedFromExcelIfEmpty: %v", err)
	}
	if msg == "" {
		t.Fatalf("expected non-empty seed message from %s", excelPath)
	}

	ov, err := db.GetOverview()
	if err != nil {
		t.Fatalf("GetOverview: %v", err)
	}
	if ov.Summary.TotalSiswa != 70 || ov.Summary.TotalPutraSiswa != 35 || ov.Summary.TotalPutriSiswa != 35 {
		t.Fatalf("unexpected student counts: %+v", ov.Summary)
	}
	if ov.Summary.TotalTidakHadir != 131 || ov.Summary.TotalPutra != 96 || ov.Summary.TotalPutri != 35 {
		t.Fatalf("unexpected absence totals: %+v", ov.Summary)
	}

	// Test update student nama_lapangan
	matches, err := db.FindStudents("Raffa Cahya Maulana", "PUTRA")
	if err != nil || len(matches) != 1 {
		t.Fatalf("FindStudents Raffa: len=%d err=%v", len(matches), err)
	}
	if err := db.UpdateStudent(matches[0].ID, "", "", "Raffa", ""); err != nil {
		t.Fatalf("UpdateStudent: %v", err)
	}
	updated, _ := db.FindStudents("Raffa", "PUTRA")
	if len(updated) == 0 || updated[0].NamaLapangan != "Raffa" {
		t.Fatalf("expected updated panggilan 'Raffa', got %+v", updated)
	}

	// Test add + delete absence log
	lid, err := db.AddAbsenceLog(matches[0].ID, "November", 2, "sakit")
	if err != nil {
		t.Fatalf("AddAbsenceLog: %v", err)
	}
	ov2, _ := db.GetOverview()
	if ov2.Summary.TotalTidakHadir != 133 {
		t.Fatalf("expected 133 total after +2, got %d", ov2.Summary.TotalTidakHadir)
	}
	if err := db.DeleteAbsenceLog(lid); err != nil {
		t.Fatalf("DeleteAbsenceLog: %v", err)
	}
	ov3, _ := db.GetOverview()
	if ov3.Summary.TotalTidakHadir != 131 {
		t.Fatalf("expected 131 total after delete, got %d", ov3.Summary.TotalTidakHadir)
	}
}

func TestStudentResolutionSafety(t *testing.T) {
	excelPath := requireTestExcelSeedPath(t)

	dbPath := t.TempDir() + `\test_safety.db`
	db, err := OpenAttendanceDB(dbPath)
	if err != nil {
		t.Fatalf("OpenAttendanceDB: %v", err)
	}
	defer db.Close()

	if _, err := db.SeedFromExcelIfEmpty(excelPath); err != nil {
		t.Fatalf("SeedFromExcelIfEmpty: %v", err)
	}
	app := &BotApp{db: db}

	// 1. Panggilan utama lebih diprioritaskan daripada kata di nama lengkap orang lain
	st, err := app.resolveSingleStudent(0, "Abi", "")
	if err != nil || st.NamaLapangan != "Abi" {
		t.Fatalf("expected Abi to resolve cleanly, got st=%+v err=%v", st, err)
	}
	tegar, err := app.resolveSingleStudent(0, "Tegar", "")
	if err != nil || tegar.NamaLengkap != "Tegar Satrya Akbar" {
		t.Fatalf("expected 'Tegar' to prioritize nama_lapangan -> Tegar Satrya Akbar, got st=%+v err=%v", tegar, err)
	}
	vino, err := app.resolveSingleStudent(0, "Vino", "")
	if err != nil || vino.NamaLengkap != "Alvino Rendra Saputra" {
		t.Fatalf("expected 'Vino' to prioritize nama_lapangan -> Alvino Rendra Saputra, got st=%+v err=%v", vino, err)
	}

	// 1b. Nama lengkap tetap bisa dipakai jika tidak ada di panggilan
	tegarPri, err := app.resolveSingleStudent(0, "Tegar Priyanto", "")
	if err != nil || tegarPri.NamaLengkap != "Tegar Priyanto" {
		t.Fatalf("expected full name Tegar Priyanto to resolve, got st=%+v err=%v", tegarPri, err)
	}

	// 2. Nama ambigu (kata nama lengkap dimiliki >1 orang, atau 2 orang punya panggilan sama)
	if _, err := app.resolveSingleStudent(0, "Setiawan", ""); err == nil || !strings.Contains(err.Error(), "AMBIGU") {
		t.Fatalf("expected AMBIGU error for 'Setiawan', got %v", err)
	}
	_, _ = db.AddStudent("PUTRA", "Kembar Satu", "Kembar", "X A")
	_, _ = db.AddStudent("PUTRA", "Kembar Dua", "Kembar", "X B")
	if _, err := app.resolveSingleStudent(0, "Kembar", ""); err == nil || !strings.Contains(err.Error(), "AMBIGU") {
		t.Fatalf("expected AMBIGU error for duplicate panggilan 'Kembar', got %v", err)
	}

	// 3. Typo / tidak persis ("Hanip" mirip "Hanif") -> beri saran tanpa langsung eksekusi
	_, err = app.resolveSingleStudent(0, "Hanip", "")
	if err == nil || !strings.Contains(err.Error(), "TIDAK_PERSIS") || !strings.Contains(err.Error(), "Hanif") {
		t.Fatalf("expected TIDAK_PERSIS with Hanif suggestion, got %v", err)
	}

	// 4. Nama benar-benar tidak ada di database -> TIDAK_DITEMUKAN
	_, err = app.resolveSingleStudent(0, "Zorgon", "")
	if err == nil || !strings.Contains(err.Error(), "TIDAK_DITEMUKAN") {
		t.Fatalf("expected TIDAK_DITEMUKAN for Zorgon, got %v", err)
	}

	// 5. Batch add_logs: 1 benar ("Abi") + 1 ambigu ("Setiawan") -> batal atomik, tidak menyimpan setengah-setengah
	before, _ := db.GetOverview()
	_, err = app.execAbsenAddLogs(&AbsenCommand{
		Op: "add_logs",
		Items: []AbsenItem{
			{Nama: "Abi", Bulan: "November", Jumlah: 1, Keterangan: "sakit"},
			{Nama: "Setiawan", Bulan: "November", Jumlah: 1, Keterangan: "izin"},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "DIBATALKAN SEMENTARA") {
		t.Fatalf("expected atomic batch abort error, got %v", err)
	}
	after, _ := db.GetOverview()
	if after.Summary.TotalTidakHadir != before.Summary.TotalTidakHadir {
		t.Fatalf("expected zero writes on failed batch, total changed %d -> %d",
			before.Summary.TotalTidakHadir, after.Summary.TotalTidakHadir)
	}
}

func TestRecapImageAndArgParsing(t *testing.T) {
	// 1. Test parseRecapArgs
	pel, bul, err := parseRecapArgs("bulan oktober peleton putra")
	if err != nil || pel != "PUTRA" || bul != "Oktober" {
		t.Fatalf("parseRecapArgs got (%q, %q, %v)", pel, bul, err)
	}
	pel2, bul2, err := parseRecapArgs("pi agu")
	if err != nil || pel2 != "PUTRI" || bul2 != "Agustus" {
		t.Fatalf("parseRecapArgs(pi agu) got (%q, %q, %v)", pel2, bul2, err)
	}
	if _, _, err := parseRecapArgs("zimbabwe"); err == nil {
		t.Fatalf("expected error for invalid filter token")
	}

	// 2. Seed test DB & test runAbsenTool op=recap_image + RenderRecapCardPNG
	dbPath := t.TempDir() + `\test_recap_img.db`
	db, err := OpenAttendanceDB(dbPath)
	if err != nil {
		t.Fatalf("OpenAttendanceDB: %v", err)
	}
	defer db.Close()

	excelPath := requireTestExcelSeedPath(t)
	if _, err := db.SeedFromExcelIfEmpty(excelPath); err != nil {
		t.Fatalf("SeedFromExcelIfEmpty: %v", err)
	}
	app := &BotApp{db: db}

	res, term, err := app.runAbsenTool(&AbsenCommand{Op: "recap_image", Peleton: "putra", Bulan: "oktober"})
	if err != nil || !term || res != "RECAP_IMAGE|PUTRA|Oktober" {
		t.Fatalf("runAbsenTool recap_image got (%q, %v, %v)", res, term, err)
	}

	ov, err := db.GetOverview()
	if err != nil {
		t.Fatalf("GetOverview: %v", err)
	}

	cases := []struct {
		pel string
		bul string
	}{
		{"PUTRA", "Oktober"},
		{"", ""},
		{"PUTRI", "Juli"},
	}
	for _, tc := range cases {
		pngBytes, caption, err := RenderRecapCardPNG(ov, tc.pel, tc.bul)
		if err != nil {
			t.Fatalf("RenderRecapCardPNG(%q, %q): %v", tc.pel, tc.bul, err)
		}
		if strings.TrimSpace(caption) == "" {
			t.Fatalf("empty caption for (%q, %q)", tc.pel, tc.bul)
		}
		cfg, err := png.DecodeConfig(bytes.NewReader(pngBytes))
		if err != nil {
			t.Fatalf("invalid PNG output for (%q, %q): %v", tc.pel, tc.bul, err)
		}
		if cfg.Width != 1080 || cfg.Height < 300 {
			t.Fatalf("unexpected dimensions for (%q, %q): %dx%d", tc.pel, tc.bul, cfg.Width, cfg.Height)
		}
	}
}
