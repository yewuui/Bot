package main

import (
	"archive/zip"
	"bytes"
	"database/sql"
	"encoding/csv"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const (
	dbFileName        = "absensi.db"
	defaultSchoolYear = "2026/2027"
)

var AcademicMonths = []string{
	"Agustus", "September", "Oktober", "November", "Desember",
	"Januari", "Februari", "Maret", "April", "Mei", "Juni", "Juli",
}

type Student struct {
	ID           int64          `json:"id"`
	Peleton      string         `json:"peleton"`
	NamaLengkap  string         `json:"nama_lengkap"`
	NamaLapangan string         `json:"nama_lapangan"`
	Kelas        string         `json:"kelas"`
	Aktif        bool           `json:"aktif"`
	Months       map[string]int `json:"months"`
	Total        int            `json:"total"`
}

type AbsenceLog struct {
	ID           int64  `json:"id"`
	SiswaID      int64  `json:"siswa_id"`
	Peleton      string `json:"peleton"`
	NamaLengkap  string `json:"nama_lengkap"`
	NamaLapangan string `json:"nama_lapangan"`
	Kelas        string `json:"kelas"`
	Bulan        string `json:"bulan"`
	TahunAjaran  string `json:"tahun_ajaran"`
	Keterangan   string `json:"keterangan"`
	Jumlah       int    `json:"jumlah"`
	DicatatAt    string `json:"dicatat_at"`
}

type MonthlyStat struct {
	Bulan string `json:"bulan"`
	Putra int    `json:"putra"`
	Putri int    `json:"putri"`
	Total int    `json:"total"`
}

type DashboardSummary struct {
	TotalSiswa      int           `json:"total_siswa"`
	TotalPutraSiswa int           `json:"total_putra_siswa"`
	TotalPutriSiswa int           `json:"total_putri_siswa"`
	TotalTidakHadir int           `json:"total_tidak_hadir"`
	TotalPutra      int           `json:"total_putra"`
	TotalPutri      int           `json:"total_putri"`
	Monthly         []MonthlyStat `json:"monthly"`
}

type OverviewData struct {
	Summary  DashboardSummary `json:"summary"`
	Students []Student        `json:"students"`
	Logs     []AbsenceLog     `json:"logs"`
}

type AttendanceDB struct {
	sql *sql.DB
}

func OpenAttendanceDB(path string) (*AttendanceDB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // SQLite single-writer best practice

	pragmas := `
PRAGMA journal_mode=WAL;
PRAGMA synchronous=NORMAL;
PRAGMA foreign_keys=ON;
PRAGMA busy_timeout=5000;`
	if _, err := db.Exec(pragmas); err != nil {
		_ = db.Close()
		return nil, err
	}

	schema := `
CREATE TABLE IF NOT EXISTS siswa (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	peleton TEXT NOT NULL CHECK(peleton IN ('PUTRA','PUTRI')),
	nama_lengkap TEXT NOT NULL,
	nama_lapangan TEXT NOT NULL DEFAULT '',
	kelas TEXT NOT NULL DEFAULT '',
	aktif INTEGER NOT NULL DEFAULT 1,
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
	updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS absensi_log (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	siswa_id INTEGER NOT NULL REFERENCES siswa(id) ON DELETE CASCADE,
	bulan TEXT NOT NULL,
	tahun_ajaran TEXT NOT NULL DEFAULT '2026/2027',
	keterangan TEXT NOT NULL DEFAULT 'tidak hadir',
	jumlah INTEGER NOT NULL DEFAULT 1,
	dicatat_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_siswa_peleton ON siswa(peleton, aktif);
CREATE INDEX IF NOT EXISTS idx_log_siswa ON absensi_log(siswa_id, bulan);`
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, err
	}

	return &AttendanceDB{sql: db}, nil
}

func (d *AttendanceDB) Close() error {
	return d.sql.Close()
}

func (d *AttendanceDB) CountStudents() (int, error) {
	var n int
	err := d.sql.QueryRow(`SELECT COUNT(*) FROM siswa WHERE aktif = 1`).Scan(&n)
	return n, err
}

func NormalizePeleton(p string) (string, error) {
	up := strings.ToUpper(strings.TrimSpace(p))
	switch up {
	case "PUTRA", "L", "LAKI-LAKI", "COWOK", "PA":
		return "PUTRA", nil
	case "PUTRI", "P", "PEREMPUAN", "CEWEK", "PI":
		return "PUTRI", nil
	default:
		return "", fmt.Errorf("peleton tidak valid: %q (harus PUTRA atau PUTRI)", p)
	}
}

func NormalizeMonth(m string) (string, error) {
	low := strings.ToLower(strings.TrimSpace(m))
	if low == "" {
		return CurrentAcademicMonth(), nil
	}
	monthMap := map[string]string{
		"agu": "Agustus", "agust": "Agustus", "agustus": "Agustus", "aug": "Agustus", "august": "Agustus",
		"sep": "September", "sept": "September", "september": "September",
		"okt": "Oktober", "oktober": "Oktober", "oct": "Oktober", "october": "Oktober",
		"nov": "November", "november": "November", "nop": "November",
		"des": "Desember", "desember": "Desember", "dec": "Desember", "december": "Desember",
		"jan": "Januari", "januari": "Januari", "january": "Januari",
		"feb": "Februari", "februari": "Februari", "peb": "Februari", "february": "Februari",
		"mar": "Maret", "maret": "Maret", "march": "Maret",
		"apr": "April", "april": "April",
		"mei": "Mei", "may": "Mei",
		"jun": "Juni", "juni": "Juni", "june": "Juni",
		"jul": "Juli", "juli": "Juli", "july": "Juli",
	}
	if canonical, ok := monthMap[low]; ok {
		return canonical, nil
	}
	return "", fmt.Errorf("bulan tidak dikenali: %q", m)
}

func CurrentAcademicMonth() string {
	idx := (int(time.Now().Month()) - 8 + 12) % 12 // Agustus = 0
	return AcademicMonths[idx]
}

func (d *AttendanceDB) GetOverview() (*OverviewData, error) {
	rows, err := d.sql.Query(`
		SELECT id, peleton, nama_lengkap, nama_lapangan, kelas, aktif
		FROM siswa
		WHERE aktif = 1
		ORDER BY peleton ASC, nama_lengkap COLLATE NOCASE ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	students := make([]Student, 0)
	byID := make(map[int64]*Student)
	for rows.Next() {
		var s Student
		var aktif int
		if err := rows.Scan(&s.ID, &s.Peleton, &s.NamaLengkap, &s.NamaLapangan, &s.Kelas, &aktif); err != nil {
			return nil, err
		}
		s.Aktif = aktif == 1
		s.Months = make(map[string]int, len(AcademicMonths))
		students = append(students, s)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range students {
		byID[students[i].ID] = &students[i]
	}

	logRows, err := d.sql.Query(`
		SELECT l.id, l.siswa_id, s.peleton, s.nama_lengkap, s.nama_lapangan, s.kelas,
		       l.bulan, l.tahun_ajaran, l.keterangan, l.jumlah, l.dicatat_at
		FROM absensi_log l
		JOIN siswa s ON s.id = l.siswa_id
		WHERE s.aktif = 1
		ORDER BY l.id DESC`)
	if err != nil {
		return nil, err
	}
	defer logRows.Close()

	logs := make([]AbsenceLog, 0)
	monthlyMap := make(map[string]*MonthlyStat, len(AcademicMonths))
	monthly := make([]MonthlyStat, len(AcademicMonths))
	for i, m := range AcademicMonths {
		monthly[i] = MonthlyStat{Bulan: m}
		monthlyMap[m] = &monthly[i]
	}

	var summary DashboardSummary
	for _, s := range students {
		summary.TotalSiswa++
		if s.Peleton == "PUTRA" {
			summary.TotalPutraSiswa++
		} else {
			summary.TotalPutriSiswa++
		}
	}

	for logRows.Next() {
		var l AbsenceLog
		if err := logRows.Scan(&l.ID, &l.SiswaID, &l.Peleton, &l.NamaLengkap, &l.NamaLapangan, &l.Kelas,
			&l.Bulan, &l.TahunAjaran, &l.Keterangan, &l.Jumlah, &l.DicatatAt); err != nil {
			return nil, err
		}
		logs = append(logs, l)

		if st, ok := byID[l.SiswaID]; ok {
			st.Months[l.Bulan] += l.Jumlah
			st.Total += l.Jumlah
		}
		if ms, ok := monthlyMap[l.Bulan]; ok {
			if l.Peleton == "PUTRA" {
				ms.Putra += l.Jumlah
			} else {
				ms.Putri += l.Jumlah
			}
			ms.Total += l.Jumlah
		}
		if l.Peleton == "PUTRA" {
			summary.TotalPutra += l.Jumlah
		} else {
			summary.TotalPutri += l.Jumlah
		}
		summary.TotalTidakHadir += l.Jumlah
	}
	if err := logRows.Err(); err != nil {
		return nil, err
	}

	summary.Monthly = monthly
	return &OverviewData{
		Summary:  summary,
		Students: students,
		Logs:     logs,
	}, nil
}

type StudentMatchResult struct {
	Direct  []Student
	Partial []Student
	Fuzzy   []Student
}

func (d *AttendanceDB) MatchStudents(query, peleton string) (*StudentMatchResult, error) {
	ov, err := d.GetOverview()
	if err != nil {
		return nil, err
	}
	q := strings.ToLower(strings.TrimSpace(query))
	pelFilter := ""
	if strings.TrimSpace(peleton) != "" {
		pelFilter, _ = NormalizePeleton(peleton)
	}

	res := &StudentMatchResult{}
	if q == "" {
		for _, s := range ov.Students {
			if pelFilter == "" || s.Peleton == pelFilter {
				res.Direct = append(res.Direct, s)
			}
		}
		return res, nil
	}

	qWords := splitNameTokens(q)
	var exactLapangan, tokenLapangan, exactFullName, tokenFullName []Student
	var partialLapangan, partialFullName, fuzzyLapangan, fuzzyFullName []Student

	for _, s := range ov.Students {
		if pelFilter != "" && s.Peleton != pelFilter {
			continue
		}
		nl := strings.ToLower(strings.TrimSpace(s.NamaLengkap))
		np := strings.ToLower(strings.TrimSpace(s.NamaLapangan))
		kl := strings.ToLower(strings.TrimSpace(s.Kelas))

		npTokens := splitNameTokens(np)
		nlTokens := splitNameTokens(nl)

		// Prioritas 1: Nama panggilan (nama_lapangan)
		switch {
		case np != "" && np == q:
			exactLapangan = append(exactLapangan, s)
			continue
		case allTokensMatch(qWords, npTokens):
			tokenLapangan = append(tokenLapangan, s)
			continue
		}

		// Prioritas 2: Nama lengkap (nama_lengkap)
		switch {
		case nl == q:
			exactFullName = append(exactFullName, s)
			continue
		case allTokensMatch(qWords, nlTokens):
			tokenFullName = append(tokenFullName, s)
			continue
		}

		// Prioritas 3: Partial / Fuzzy (utamakan panggilan dulu baru nama lengkap)
		if np != "" && strings.Contains(np, q) {
			partialLapangan = append(partialLapangan, s)
		} else if strings.Contains(nl, q) || strings.Contains(kl, q) {
			partialFullName = append(partialFullName, s)
		} else if isFuzzyMatch(q, npTokens) {
			fuzzyLapangan = append(fuzzyLapangan, s)
		} else if isFuzzyMatch(q, nlTokens) {
			fuzzyFullName = append(fuzzyFullName, s)
		}
	}

	switch {
	case len(exactLapangan) > 0:
		res.Direct = exactLapangan
	case len(tokenLapangan) > 0:
		res.Direct = tokenLapangan
	case len(exactFullName) > 0:
		res.Direct = exactFullName
	case len(tokenFullName) > 0:
		res.Direct = tokenFullName
	}

	res.Partial = append(partialLapangan, partialFullName...)
	res.Fuzzy = append(fuzzyLapangan, fuzzyFullName...)
	return res, nil
}

func (d *AttendanceDB) FindStudents(query, peleton string) ([]Student, error) {
	m, err := d.MatchStudents(query, peleton)
	if err != nil {
		return nil, err
	}
	if len(m.Direct) > 0 {
		return m.Direct, nil
	}
	if len(m.Partial) > 0 {
		return m.Partial, nil
	}
	return nil, nil
}

func splitNameTokens(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return r == ' ' || r == '/' || r == '-' || r == ',' || r == '.'
	})
}

func allTokensMatch(queryWords, candidateTokens []string) bool {
	if len(queryWords) == 0 {
		return false
	}
	for _, qw := range queryWords {
		found := false
		for _, ct := range candidateTokens {
			if ct == qw {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func isFuzzyMatch(q string, candidateTokens []string) bool {
	if len([]rune(q)) < 3 {
		return false
	}
	maxDist := 1
	if len([]rune(q)) >= 5 {
		maxDist = 2
	}
	for _, ct := range candidateTokens {
		if len([]rune(ct)) < 3 {
			continue
		}
		if levenshteinRunes(q, ct) <= maxDist {
			return true
		}
	}
	return false
}

func levenshteinRunes(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	la, lb := len(ra), len(rb)
	if la == 0 {
		return lb
	}
	if lb == 0 {
		return la
	}
	prev := make([]int, lb+1)
	cur := make([]int, lb+1)
	for j := 0; j <= lb; j++ {
		prev[j] = j
	}
	for i := 1; i <= la; i++ {
		cur[0] = i
		for j := 1; j <= lb; j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			del := prev[j] + 1
			ins := cur[j-1] + 1
			sub := prev[j-1] + cost
			m := del
			if ins < m {
				m = ins
			}
			if sub < m {
				m = sub
			}
			cur[j] = m
		}
		prev, cur = cur, prev
	}
	return prev[lb]
}

func (d *AttendanceDB) AddStudent(peleton, namaLengkap, namaLapangan, kelas string) (int64, error) {
	pel, err := NormalizePeleton(peleton)
	if err != nil {
		return 0, err
	}
	nama := strings.TrimSpace(namaLengkap)
	if nama == "" {
		return 0, errors.New("nama lengkap wajib diisi")
	}
	lap := strings.TrimSpace(namaLapangan)
	if lap == "" {
		lap = strings.Fields(nama)[0]
	}
	res, err := d.sql.Exec(`
		INSERT INTO siswa (peleton, nama_lengkap, nama_lapangan, kelas)
		VALUES (?, ?, ?, ?)`,
		pel, nama, lap, strings.TrimSpace(kelas))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (d *AttendanceDB) UpdateStudent(id int64, peleton, namaLengkap, namaLapangan, kelas string) error {
	var curPel, curNama, curLap, curKelas string
	err := d.sql.QueryRow(`SELECT peleton, nama_lengkap, nama_lapangan, kelas FROM siswa WHERE id = ? AND aktif = 1`, id).
		Scan(&curPel, &curNama, &curLap, &curKelas)
	if err != nil {
		return fmt.Errorf("siswa ID %d tidak ditemukan", id)
	}

	if strings.TrimSpace(peleton) != "" {
		p, err := NormalizePeleton(peleton)
		if err != nil {
			return err
		}
		curPel = p
	}
	if strings.TrimSpace(namaLengkap) != "" {
		curNama = strings.TrimSpace(namaLengkap)
	}
	if strings.TrimSpace(namaLapangan) != "" {
		curLap = strings.TrimSpace(namaLapangan)
	}
	if strings.TrimSpace(kelas) != "" {
		curKelas = strings.TrimSpace(kelas)
	}

	_, err = d.sql.Exec(`
		UPDATE siswa
		SET peleton = ?, nama_lengkap = ?, nama_lapangan = ?, kelas = ?, updated_at = CURRENT_TIMESTAMP
		WHERE id = ?`,
		curPel, curNama, curLap, curKelas, id)
	return err
}

func (d *AttendanceDB) DeleteStudent(id int64) error {
	res, err := d.sql.Exec(`DELETE FROM siswa WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("siswa ID %d tidak ditemukan", id)
	}
	return nil
}

func (d *AttendanceDB) AddAbsenceLog(siswaID int64, bulan string, jumlah int, keterangan string) (int64, error) {
	b, err := NormalizeMonth(bulan)
	if err != nil {
		return 0, err
	}
	if jumlah <= 0 {
		jumlah = 1
	}
	ket := strings.TrimSpace(keterangan)
	if ket == "" {
		ket = "tidak hadir"
	}

	var exists int
	if err := d.sql.QueryRow(`SELECT COUNT(*) FROM siswa WHERE id = ? AND aktif = 1`, siswaID).Scan(&exists); err != nil || exists == 0 {
		return 0, fmt.Errorf("siswa ID %d tidak ditemukan", siswaID)
	}

	res, err := d.sql.Exec(`
		INSERT INTO absensi_log (siswa_id, bulan, tahun_ajaran, keterangan, jumlah)
		VALUES (?, ?, ?, ?, ?)`,
		siswaID, b, defaultSchoolYear, ket, jumlah)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (d *AttendanceDB) UpdateAbsenceLog(id int64, bulan string, jumlah int, keterangan string) error {
	var curBulan, curKet string
	var curJml int
	err := d.sql.QueryRow(`SELECT bulan, jumlah, keterangan FROM absensi_log WHERE id = ?`, id).
		Scan(&curBulan, &curJml, &curKet)
	if err != nil {
		return fmt.Errorf("catatan log ID %d tidak ditemukan", id)
	}

	if strings.TrimSpace(bulan) != "" {
		b, err := NormalizeMonth(bulan)
		if err != nil {
			return err
		}
		curBulan = b
	}
	if jumlah > 0 {
		curJml = jumlah
	}
	if strings.TrimSpace(keterangan) != "" {
		curKet = strings.TrimSpace(keterangan)
	}

	_, err = d.sql.Exec(`UPDATE absensi_log SET bulan = ?, jumlah = ?, keterangan = ? WHERE id = ?`,
		curBulan, curJml, curKet, id)
	return err
}

func (d *AttendanceDB) DeleteAbsenceLog(id int64) error {
	res, err := d.sql.Exec(`DELETE FROM absensi_log WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("catatan log ID %d tidak ditemukan", id)
	}
	return nil
}

func (d *AttendanceDB) SetStudentMonthCount(siswaID int64, bulan string, target int, keterangan string) error {
	b, err := NormalizeMonth(bulan)
	if err != nil {
		return err
	}
	if target < 0 {
		target = 0
	}
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`DELETE FROM absensi_log WHERE siswa_id = ? AND bulan = ?`, siswaID, b); err != nil {
		return err
	}
	if target > 0 {
		ket := strings.TrimSpace(keterangan)
		if ket == "" {
			ket = "penyesuaian manual"
		}
		if _, err := tx.Exec(`
			INSERT INTO absensi_log (siswa_id, bulan, tahun_ajaran, keterangan, jumlah)
			VALUES (?, ?, ?, ?, ?)`,
			siswaID, b, defaultSchoolYear, ket, target); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (d *AttendanceDB) ExportCSVBytes() ([]byte, error) {
	ov, err := d.GetOverview()
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	buf.Write([]byte{0xEF, 0xBB, 0xBF}) // UTF-8 BOM agar rapi dibuka di Excel Windows
	w := csv.NewWriter(&buf)

	header := append([]string{"No", "Peleton", "Nama Lengkap", "Nama Lapangan", "Kelas"}, AcademicMonths...)
	header = append(header, "Total")
	_ = w.Write(header)

	for i, s := range ov.Students {
		row := []string{
			strconv.Itoa(i + 1),
			s.Peleton,
			s.NamaLengkap,
			s.NamaLapangan,
			s.Kelas,
		}
		for _, m := range AcademicMonths {
			row = append(row, strconv.Itoa(s.Months[m]))
		}
		row = append(row, strconv.Itoa(s.Total))
		_ = w.Write(row)
	}

	_ = w.Write([]string{})
	_ = w.Write([]string{"LOG KETIDAKHADIRAN"})
	_ = w.Write([]string{"ID Log", "Peleton", "Nama Lengkap", "Nama Lapangan", "Kelas", "Bulan", "Jumlah", "Keterangan", "Dicatat"})
	for _, l := range ov.Logs {
		_ = w.Write([]string{
			strconv.FormatInt(l.ID, 10),
			l.Peleton,
			l.NamaLengkap,
			l.NamaLapangan,
			l.Kelas,
			l.Bulan,
			strconv.Itoa(l.Jumlah),
			l.Keterangan,
			l.DicatatAt,
		})
	}

	w.Flush()
	return buf.Bytes(), w.Error()
}

// ================= IMPORTER XLSX STANDAR (STDLIB ONLY) =================

func (d *AttendanceDB) SeedFromExcelIfEmpty(xlsxPath string) (string, error) {
	count, err := d.CountStudents()
	if err != nil {
		return "", err
	}
	if count > 0 {
		return "", nil
	}
	if _, err := os.Stat(xlsxPath); err != nil {
		return "", nil
	}
	return d.ImportFromXLSX(xlsxPath, false)
}

func (d *AttendanceDB) ImportFromXLSX(xlsxPath string, replaceExisting bool) (string, error) {
	sheets, err := readXLSXSheets(xlsxPath)
	if err != nil {
		return "", err
	}

	putraRows := sheets["ABSENSI PUTRA"]
	putriRows := sheets["ABSENSI PUTRI"]
	logRows := sheets["LOG ABSENSI"]
	if len(putraRows) < 4 && len(putriRows) < 4 {
		return "", errors.New("sheet ABSENSI PUTRA / ABSENSI PUTRI tidak ditemukan atau kosong")
	}

	tx, err := d.sql.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()

	if replaceExisting {
		if _, err := tx.Exec(`DELETE FROM absensi_log; DELETE FROM siswa;`); err != nil {
			return "", err
		}
	}

	type importedStudent struct {
		id         int64
		peleton    string
		nama       string
		monthSheet map[string]int
	}
	studentIndex := make(map[string]*importedStudent)
	var allImported []*importedStudent

	parsePeleton := func(peleton string, rows []map[string]string) error {
		monthCols := []struct {
			col   string
			bulan string
		}{
			{"E", "Agustus"}, {"F", "September"}, {"G", "Oktober"}, {"H", "November"},
			{"I", "Desember"}, {"J", "Januari"}, {"K", "Februari"}, {"L", "Maret"},
			{"M", "April"}, {"N", "Mei"}, {"O", "Juni"}, {"P", "Juli"},
		}
		for i, r := range rows {
			if i < 3 {
				continue // Lewati judul & header
			}
			nama := strings.TrimSpace(r["B"])
			if nama == "" {
				continue
			}
			lap := strings.TrimSpace(r["C"])
			kelas := strings.TrimSpace(r["D"])

			res, err := tx.Exec(`INSERT INTO siswa (peleton, nama_lengkap, nama_lapangan, kelas) VALUES (?, ?, ?, ?)`,
				peleton, nama, lap, kelas)
			if err != nil {
				return err
			}
			sid, _ := res.LastInsertId()

			mMap := make(map[string]int)
			for _, mc := range monthCols {
				if v := parseIntFloat(r[mc.col]); v > 0 {
					mMap[mc.bulan] = v
				}
			}
			st := &importedStudent{id: sid, peleton: peleton, nama: nama, monthSheet: mMap}
			studentIndex[peleton+"|"+strings.ToLower(nama)] = st
			allImported = append(allImported, st)
		}
		return nil
	}

	if err := parsePeleton("PUTRA", putraRows); err != nil {
		return "", err
	}
	if err := parsePeleton("PUTRI", putriRows); err != nil {
		return "", err
	}

	loggedPerStudentMonth := make(map[string]int)
	importedLogs := 0

	for i, r := range logRows {
		if i < 2 {
			continue // Lewati judul & header LOG ABSENSI
		}
		pelRaw := strings.TrimSpace(r["A"])
		nama := strings.TrimSpace(r["B"])
		if pelRaw == "" || nama == "" {
			continue
		}
		pel, err := NormalizePeleton(pelRaw)
		if err != nil {
			continue
		}
		st := studentIndex[pel+"|"+strings.ToLower(nama)]
		if st == nil {
			continue
		}
		bulan, err := NormalizeMonth(r["E"])
		if err != nil {
			continue
		}
		ket := strings.TrimSpace(r["F"])
		if ket == "" {
			ket = "tidak hadir"
		}
		jml := parseIntFloat(r["G"])
		if jml <= 0 {
			jml = 1
		}
		ts := excelSerialToTimeStr(r["H"])
		if ts != "" {
			_, err = tx.Exec(`INSERT INTO absensi_log (siswa_id, bulan, tahun_ajaran, keterangan, jumlah, dicatat_at) VALUES (?, ?, ?, ?, ?, ?)`,
				st.id, bulan, defaultSchoolYear, ket, jml, ts)
		} else {
			_, err = tx.Exec(`INSERT INTO absensi_log (siswa_id, bulan, tahun_ajaran, keterangan, jumlah) VALUES (?, ?, ?, ?, ?)`,
				st.id, bulan, defaultSchoolYear, ket, jml)
		}
		if err != nil {
			return "", err
		}
		loggedPerStudentMonth[fmt.Sprintf("%d|%s", st.id, bulan)] += jml
		importedLogs++
	}

	// Rekonsiliasi jika di tab ABSENSI PUTRA/PUTRI ada angka bulanan yang belum tercatat di LOG ABSENSI
	for _, st := range allImported {
		for bulan, sheetVal := range st.monthSheet {
			logged := loggedPerStudentMonth[fmt.Sprintf("%d|%s", st.id, bulan)]
			if sheetVal > logged {
				diff := sheetVal - logged
				if _, err := tx.Exec(`INSERT INTO absensi_log (siswa_id, bulan, tahun_ajaran, keterangan, jumlah) VALUES (?, ?, ?, ?, ?)`,
					st.id, bulan, defaultSchoolYear, "riwayat rekap sheet", diff); err != nil {
					return "", err
				}
				importedLogs++
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return "", err
	}
	return fmt.Sprintf("Berhasil impor %d siswa dan %d catatan ketidakhadiran dari %s", len(allImported), importedLogs, filepath.Base(xlsxPath)), nil
}

func parseIntFloat(s string) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return int(math.Round(f))
}

func excelSerialToTimeStr(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f < 30000 {
		return ""
	}
	epoch := time.Date(1899, 12, 30, 0, 0, 0, 0, time.Local)
	t := epoch.Add(time.Duration(f * float64(24*time.Hour)))
	return t.Format("2006-01-02 15:04:05")
}

func readXLSXSheets(path string) (map[string][]map[string]string, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return nil, err
	}
	defer zr.Close()

	files := make(map[string]*zip.File, len(zr.File))
	for _, f := range zr.File {
		files[strings.TrimPrefix(f.Name, "/")] = f
	}

	readFileBytes := func(name string) ([]byte, error) {
		f := files[strings.TrimPrefix(name, "/")]
		if f == nil {
			return nil, fmt.Errorf("file %s tidak ada di arsip xlsx", name)
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		return io.ReadAll(rc)
	}

	var sharedStrings []string
	if ssBytes, err := readFileBytes("xl/sharedStrings.xml"); err == nil {
		sharedStrings = parseSharedStrings(ssBytes)
	}

	relsBytes, err := readFileBytes("xl/_rels/workbook.xml.rels")
	if err != nil {
		return nil, err
	}
	relMap := parseWorkbookRels(relsBytes)

	wbBytes, err := readFileBytes("xl/workbook.xml")
	if err != nil {
		return nil, err
	}
	sheetTargets := parseWorkbookSheets(wbBytes, relMap)

	out := make(map[string][]map[string]string, len(sheetTargets))
	for name, target := range sheetTargets {
		if strings.Contains(strings.ToUpper(name), "PELETON") {
			continue // Skip tab dummy PELETON PUTRA/PUTRI 2026
		}
		shBytes, err := readFileBytes(target)
		if err != nil {
			continue
		}
		out[name] = parseWorksheetRows(shBytes, sharedStrings)
	}
	return out, nil
}

func parseSharedStrings(data []byte) []string {
	var sst struct {
		SI []struct {
			T string `xml:"t"`
			R []struct {
				T string `xml:"t"`
			} `xml:"r"`
		} `xml:"si"`
	}
	_ = xml.Unmarshal(data, &sst)
	res := make([]string, len(sst.SI))
	for i, si := range sst.SI {
		if si.T != "" {
			res[i] = si.T
		} else {
			var sb strings.Builder
			for _, r := range si.R {
				sb.WriteString(r.T)
			}
			res[i] = sb.String()
		}
	}
	return res
}

func parseWorkbookRels(data []byte) map[string]string {
	var rels struct {
		Rel []struct {
			ID     string `xml:"Id,attr"`
			Target string `xml:"Target,attr"`
		} `xml:"Relationship"`
	}
	_ = xml.Unmarshal(data, &rels)
	m := make(map[string]string, len(rels.Rel))
	for _, r := range rels.Rel {
		t := strings.TrimPrefix(r.Target, "/")
		if !strings.HasPrefix(t, "xl/") {
			t = "xl/" + t
		}
		m[r.ID] = t
	}
	return m
}

func parseWorkbookSheets(data []byte, relMap map[string]string) map[string]string {
	var wb struct {
		Sheets []struct {
			Name string `xml:"name,attr"`
			RID  string `xml:"http://schemas.openxmlformats.org/officeDocument/2006/relationships id,attr"`
		} `xml:"sheets>sheet"`
	}
	_ = xml.Unmarshal(data, &wb)
	m := make(map[string]string, len(wb.Sheets))
	for _, sh := range wb.Sheets {
		if target, ok := relMap[sh.RID]; ok {
			m[sh.Name] = target
		}
	}
	return m
}

func parseWorksheetRows(data []byte, ss []string) []map[string]string {
	var ws struct {
		Rows []struct {
			Num   int `xml:"r,attr"`
			Cells []struct {
				Ref   string `xml:"r,attr"`
				Type  string `xml:"t,attr"`
				Value string `xml:"v"`
				IS    struct {
					T string `xml:"t"`
				} `xml:"is"`
			} `xml:"c"`
		} `xml:"sheetData>row"`
	}
	_ = xml.Unmarshal(data, &ws)

	// Pastikan urut berdasarkan nomor baris dan simpan pada slice terindeks (0-based = row 1)
	maxRow := 0
	for _, r := range ws.Rows {
		if r.Num > maxRow {
			maxRow = r.Num
		}
	}
	if maxRow == 0 {
		return nil
	}

	rows := make([]map[string]string, maxRow)
	for _, r := range ws.Rows {
		if r.Num <= 0 {
			continue
		}
		rowMap := make(map[string]string)
		for _, c := range r.Cells {
			val := c.Value
			switch c.Type {
			case "s":
				if idx, err := strconv.Atoi(val); err == nil && idx >= 0 && idx < len(ss) {
					val = ss[idx]
				}
			case "inlineStr":
				val = c.IS.T
			}
			if val != "" {
				col := strings.TrimRight(c.Ref, "0123456789")
				rowMap[col] = val
			}
		}
		rows[r.Num-1] = rowMap
	}

	// Trim trailing empty rows
	last := len(rows)
	for last > 0 && len(rows[last-1]) == 0 {
		last--
	}
	res := rows[:last]
	// Sort check no-op; map keys are columns
	_ = sort.StringsAreSorted
	return res
}
