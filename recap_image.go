package main

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
	"golang.org/x/image/vector"
)

var (
	uiFontsOnce sync.Once
	uiFontReg   *opentype.Font
	uiFontSemi  *opentype.Font
	uiFontBold  *opentype.Font
)

func loadUIFonts() {
	uiFontsOnce.Do(func() {
		loadOne := func(paths ...string) *opentype.Font {
			for _, p := range paths {
				data, err := os.ReadFile(p)
				if err != nil {
					continue
				}
				f, err := opentype.Parse(data)
				if err == nil {
					return f
				}
			}
			return nil
		}
		uiFontReg = loadOne(`C:\Windows\Fonts\segoeui.ttf`, `C:\Windows\Fonts\arial.ttf`)
		uiFontSemi = loadOne(`C:\Windows\Fonts\seguisb.ttf`, `C:\Windows\Fonts\segoeuisl.ttf`, `C:\Windows\Fonts\segoeuib.ttf`)
		uiFontBold = loadOne(`C:\Windows\Fonts\segoeuib.ttf`, `C:\Windows\Fonts\arialbd.ttf`)
		if uiFontSemi == nil {
			uiFontSemi = uiFontBold
		}
		if uiFontBold == nil {
			uiFontBold = uiFontReg
		}
		if uiFontSemi == nil {
			uiFontSemi = uiFontReg
		}
	})
}

type uiFaceSet struct {
	eyebrow  font.Face
	title    font.Face
	sub      font.Face
	kpiLabel font.Face
	kpiVal   font.Face
	secTitle font.Face
	body     font.Face
	bodyBold font.Face
	small    font.Face
	closers  []func()
}

func newUIFaceSet() *uiFaceSet {
	loadUIFonts()
	fs := &uiFaceSet{}
	makeFace := func(f *opentype.Font, size float64) font.Face {
		if f != nil {
			face, err := opentype.NewFace(f, &opentype.FaceOptions{
				Size:    size,
				DPI:     72,
				Hinting: font.HintingFull,
			})
			if err == nil {
				fs.closers = append(fs.closers, func() { _ = face.Close() })
				return face
			}
		}
		return basicfont.Face7x13
	}

	fs.eyebrow = makeFace(uiFontBold, 13)
	fs.title = makeFace(uiFontBold, 28)
	fs.sub = makeFace(uiFontReg, 14)
	fs.kpiLabel = makeFace(uiFontSemi, 12.5)
	fs.kpiVal = makeFace(uiFontBold, 32)
	fs.secTitle = makeFace(uiFontBold, 16)
	fs.body = makeFace(uiFontReg, 14)
	fs.bodyBold = makeFace(uiFontBold, 15.5)
	fs.small = makeFace(uiFontReg, 12.5)
	return fs
}

func (fs *uiFaceSet) Close() {
	for _, fn := range fs.closers {
		fn()
	}
}

type recapRow struct {
	Student     Student
	Count       int
	NoteSummary string
}

type recapBarItem struct {
	Label      string
	PutraCount int
	PutriCount int
	Total      int
}

// RenderRecapCardPNG menghasilkan gambar dashboard ringkasan absensi berformat PNG
// beserta caption ringkas untuk dikirim langsung di chat Telegram tanpa file unduhan.
func RenderRecapCardPNG(ov *OverviewData, peletonFilter, bulanFilter string) ([]byte, string, error) {
	if ov == nil {
		return nil, "", fmt.Errorf("data absensi kosong")
	}

	pelNorm := ""
	if strings.TrimSpace(peletonFilter) != "" {
		p, err := NormalizePeleton(peletonFilter)
		if err != nil {
			return nil, "", err
		}
		pelNorm = p
	}

	bulNorm := ""
	if strings.TrimSpace(bulanFilter) != "" {
		b, err := NormalizeMonth(bulanFilter)
		if err != nil {
			return nil, "", err
		}
		bulNorm = b
	}

	// Kumpulkan statistik siswa & keterangan log sesuai filter
	var scopedStudents []Student
	for _, s := range ov.Students {
		if pelNorm == "" || s.Peleton == pelNorm {
			scopedStudents = append(scopedStudents, s)
		}
	}

	notesByStudent := make(map[int64]map[string]int)
	reasonCounts := make(map[string]int)
	var scopedLogs []AbsenceLog
	for _, l := range ov.Logs {
		if pelNorm != "" && l.Peleton != pelNorm {
			continue
		}
		if bulNorm != "" && l.Bulan != bulNorm {
			continue
		}
		scopedLogs = append(scopedLogs, l)
		ket := normalizeReasonLabel(l.Keterangan)
		reasonCounts[ket] += l.Jumlah

		if notesByStudent[l.SiswaID] == nil {
			notesByStudent[l.SiswaID] = make(map[string]int)
		}
		notesByStudent[l.SiswaID][ket] += l.Jumlah
	}

	totalAbs := 0
	putraAbs := 0
	putriAbs := 0
	absentCount := 0
	redCount := 0
	yellowCount := 0
	classCounts := make(map[string]int)

	var absentRows []recapRow
	for _, s := range scopedStudents {
		cnt := s.Total
		if bulNorm != "" {
			cnt = s.Months[bulNorm]
		}
		totalAbs += cnt
		if s.Peleton == "PUTRA" {
			putraAbs += cnt
		} else {
			putriAbs += cnt
		}
		if cnt > 0 {
			absentCount++
			if cnt > 3 {
				redCount++
			} else if cnt == 3 {
				yellowCount++
			}
			cls := strings.TrimSpace(s.Kelas)
			if cls == "" {
				cls = "Lainnya"
			}
			classCounts[cls] += cnt

			absentRows = append(absentRows, recapRow{
				Student:     s,
				Count:       cnt,
				NoteSummary: buildStudentNoteSummary(s, bulNorm, notesByStudent[s.ID]),
			})
		}
	}
	fullAttendCount := len(scopedStudents) - absentCount

	sort.Slice(absentRows, func(i, j int) bool {
		if absentRows[i].Count != absentRows[j].Count {
			return absentRows[i].Count > absentRows[j].Count
		}
		if absentRows[i].Student.Total != absentRows[j].Student.Total {
			return absentRows[i].Student.Total > absentRows[j].Student.Total
		}
		return strings.ToLower(firstNonEmpty(absentRows[i].Student.NamaLapangan, absentRows[i].Student.NamaLengkap)) <
			strings.ToLower(firstNonEmpty(absentRows[j].Student.NamaLapangan, absentRows[j].Student.NamaLengkap))
	})

	// Siapkan data chart utama (per bulan jika tahunan, atau kelas + keterangan jika per bulan)
	var chartBars []recapBarItem
	if bulNorm == "" {
		for _, m := range ov.Summary.Monthly {
			val := m.Total
			pVal, wVal := m.Putra, m.Putri
			if pelNorm == "PUTRA" {
				val = m.Putra
				wVal = 0
			} else if pelNorm == "PUTRI" {
				val = m.Putri
				pVal = 0
			}
			if val > 0 || m.Bulan == CurrentAcademicMonth() {
				chartBars = append(chartBars, recapBarItem{
					Label:      m.Bulan,
					PutraCount: pVal,
					PutriCount: wVal,
					Total:      val,
				})
			}
		}
	}

	type kvCount struct {
		Key string
		Val int
	}
	var classBars []kvCount
	for k, v := range classCounts {
		classBars = append(classBars, kvCount{Key: k, Val: v})
	}
	sort.Slice(classBars, func(i, j int) bool {
		if classBars[i].Val != classBars[j].Val {
			return classBars[i].Val > classBars[j].Val
		}
		return classBars[i].Key < classBars[j].Key
	})
	if len(classBars) > 6 {
		classBars = classBars[:6]
	}

	var reasonBars []kvCount
	for k, v := range reasonCounts {
		reasonBars = append(reasonBars, kvCount{Key: k, Val: v})
	}
	sort.Slice(reasonBars, func(i, j int) bool {
		if reasonBars[i].Val != reasonBars[j].Val {
			return reasonBars[i].Val > reasonBars[j].Val
		}
		return reasonBars[i].Key < reasonBars[j].Key
	})
	if len(reasonBars) > 6 {
		reasonBars = reasonBars[:6]
	}

	const (
		canvasW    = 1080
		outerPad   = 28
		innerW     = canvasW - outerPad*2 // 1024
		headerH    = 106
		kpiH       = 104
		rowH       = 48
		tableHeadH = 38
		maxRows    = 24
	)

	shownRows := absentRows
	extraRows := 0
	if len(shownRows) > maxRows {
		extraRows = len(shownRows) - maxRows
		shownRows = shownRows[:maxRows]
	}

	chartCardH := 0
	if bulNorm == "" {
		nBars := len(chartBars)
		if nBars < 1 {
			nBars = 1
		}
		chartCardH = 54 + nBars*30 + 18
	} else if len(classBars) > 0 || len(reasonBars) > 0 {
		maxPanelRows := len(classBars)
		if len(reasonBars) > maxPanelRows {
			maxPanelRows = len(reasonBars)
		}
		if maxPanelRows < 2 {
			maxPanelRows = 2
		}
		chartCardH = 54 + maxPanelRows*30 + 18
	}

	tableBodyH := 0
	if len(shownRows) == 0 {
		tableBodyH = 88
	} else {
		tableBodyH = tableHeadH + len(shownRows)*rowH
		if extraRows > 0 {
			tableBodyH += 34
		}
	}
	tableFooterH := 42
	tableCardH := 52 + tableBodyH + tableFooterH + 10

	recentLogRows := scopedLogs
	if len(recentLogRows) > 5 {
		recentLogRows = recentLogRows[:5]
	}
	logsCardH := 0
	if len(recentLogRows) > 0 && bulNorm != "" {
		logsCardH = 50 + len(recentLogRows)*32 + 12
	}

	footerH := 38
	gap := 16

	canvasH := outerPad + headerH + gap + kpiH + gap
	if chartCardH > 0 {
		canvasH += chartCardH + gap
	}
	canvasH += tableCardH + gap
	if logsCardH > 0 {
		canvasH += logsCardH + gap
	}
	canvasH += footerH + outerPad

	img := image.NewRGBA(image.Rect(0, 0, canvasW, canvasH))
	bgCol := color.NRGBA{11, 17, 23, 255}
	draw.Draw(img, img.Bounds(), &image.Uniform{C: bgCol}, image.Point{}, draw.Src)

	fs := newUIFaceSet()
	defer fs.Close()

	// Warna UI konsisten dengan tema gelap WebApp
	cCard := color.NRGBA{19, 28, 38, 255}
	cCardInner := color.NRGBA{26, 38, 52, 255}
	cBorder := color.NRGBA{37, 50, 66, 255}
	cText := color.NRGBA{241, 245, 249, 255}
	cMuted := color.NRGBA{148, 163, 184, 255}
	cPutra := color.NRGBA{56, 189, 248, 255}
	cPutraSoft := color.NRGBA{21, 56, 79, 255}
	cPutri := color.NRGBA{244, 114, 182, 255}
	cPutriSoft := color.NRGBA{75, 32, 59, 255}
	cAccent := color.NRGBA{59, 130, 246, 255}
	cAccentSoft := color.NRGBA{24, 47, 82, 255}
	cRed := color.NRGBA{248, 113, 113, 255}
	cRedBg := color.NRGBA{69, 26, 31, 255}
	cRedBdr := color.NRGBA{185, 56, 62, 255}
	cYellow := color.NRGBA{252, 211, 77, 255}
	cYellowBg := color.NRGBA{66, 42, 18, 255}
	cYellowBdr := color.NRGBA{180, 120, 24, 255}
	cGreen := color.NRGBA{74, 222, 128, 255}
	cGreenBg := color.NRGBA{17, 46, 32, 255}
	cGreenBdr := color.NRGBA{34, 112, 68, 255}

	y := outerPad

	// 1. HEADER CARD
	drawRoundedCard(img, outerPad, y, innerW, headerH, 14, cCard, cBorder)

	// Accent vertical strip on left of header
	accentBarCol := cAccent
	pillBg := cAccentSoft
	pillText := "PUTRA & PUTRI"
	if pelNorm == "PUTRA" {
		accentBarCol = cPutra
		pillBg = cPutraSoft
		pillText = "PELETON PUTRA"
	} else if pelNorm == "PUTRI" {
		accentBarCol = cPutri
		pillBg = cPutriSoft
		pillText = "PELETON PUTRI"
	}
	fillRoundedRect(img, float32(outerPad+16), float32(y+20), 6, float32(headerH-40), 3, accentBarCol)

	drawTextLeft(img, fs.eyebrow, outerPad+36, y+32, accentBarCol, "RINGKASAN ABSENSI PELETON • 2026/2027")

	titleText := "Rekap Tahun Ajaran — Semua Peleton"
	switch {
	case bulNorm != "" && pelNorm != "":
		titleText = fmt.Sprintf("Rekap Bulan %s — Peleton %s", bulNorm, titleCase(pelNorm))
	case bulNorm != "":
		titleText = fmt.Sprintf("Rekap Bulan %s — Semua Peleton", bulNorm)
	case pelNorm != "":
		titleText = fmt.Sprintf("Rekap Tahun Ajaran — Peleton %s", titleCase(pelNorm))
	}
	drawTextLeft(img, fs.title, outerPad+36, y+68, cText, truncateToWidth(fs.title, titleText, 700))

	subLeft := fmt.Sprintf("Diperbarui %s WIB • Data SQLite Terkini", time.Now().Format("02 Jan 2006 15:04"))
	drawTextLeft(img, fs.sub, outerPad+36, y+92, cMuted, subLeft)

	// Right badges on Header
	pillW := float32(measureWidth(fs.eyebrow, pillText)) + 28
	pillX := float32(outerPad+innerW-20) - pillW
	drawRoundedCard(img, int(pillX), y+24, int(pillW), 30, 15, pillBg, accentBarCol)
	drawTextCenter(img, fs.eyebrow, int(pillX+pillW/2), y+44, accentBarCol, pillText)

	scopeMonthText := "SEMUA BULAN"
	if bulNorm != "" {
		scopeMonthText = strings.ToUpper(bulNorm)
	}
	drawTextRight(img, fs.small, outerPad+innerW-22, y+82, cMuted, "Periode: "+scopeMonthText)

	y += headerH + gap

	// 2. KPI CARDS (4 kolom)
	kpiGap := 12
	kpiW := (innerW - kpiGap*3) / 4

	scopeLabel := "Putra & Putri"
	if pelNorm == "PUTRA" {
		scopeLabel = "Peleton Putra"
	} else if pelNorm == "PUTRI" {
		scopeLabel = "Peleton Putri"
	}

	avgPerStudent := 0.0
	if len(scopedStudents) > 0 {
		avgPerStudent = float64(totalAbs) / float64(len(scopedStudents))
	}

	subKPI1 := fmt.Sprintf("%d Pa • %d Pi", ov.Summary.TotalPutraSiswa, ov.Summary.TotalPutriSiswa)
	if pelNorm != "" {
		subKPI1 = scopeLabel
	}
	subKPI2 := fmt.Sprintf("Rata-rata %.1fx / siswa", avgPerStudent)
	if pelNorm == "" {
		subKPI2 = fmt.Sprintf("Pa: %dx • Pi: %dx", putraAbs, putriAbs)
	}
	subKPI3 := fmt.Sprintf("%d siswa hadir penuh", fullAttendCount)

	alertVal := fmt.Sprintf("%d", redCount+yellowCount)
	alertCol := cGreen
	subKPI4 := "Semua aman (< 3x)"
	if redCount > 0 || yellowCount > 0 {
		if redCount > 0 {
			alertCol = cRed
		} else {
			alertCol = cYellow
		}
		subKPI4 = fmt.Sprintf("%d merah (>3) • %d kuning (=3)", redCount, yellowCount)
	}

	kpis := []struct {
		label string
		val   string
		sub   string
		color color.NRGBA
	}{
		{"SISWA TERDAFTAR", fmt.Sprintf("%d", len(scopedStudents)), subKPI1, cText},
		{"TOTAL TIDAK HADIR", fmt.Sprintf("%dx", totalAbs), subKPI2, accentBarCol},
		{"SISWA ABSEN", fmt.Sprintf("%d", absentCount), subKPI3, cText},
		{"PERLU PERHATIAN", alertVal, subKPI4, alertCol},
	}

	for i, k := range kpis {
		kx := outerPad + i*(kpiW+kpiGap)
		drawRoundedCard(img, kx, y, kpiW, kpiH, 12, cCard, cBorder)
		drawTextLeft(img, fs.kpiLabel, kx+16, y+26, cMuted, k.label)
		drawTextLeft(img, fs.kpiVal, kx+16, y+66, k.color, k.val)
		drawTextLeft(img, fs.small, kx+16, y+90, cMuted, truncateToWidth(fs.small, k.sub, float64(kpiW-30)))
	}

	y += kpiH + gap

	// 3. CHART CARD (Bulanan jika tahunan; Kelas + Keterangan jika bulan tertentu)
	if chartCardH > 0 {
		if bulNorm == "" {
			drawRoundedCard(img, outerPad, y, innerW, chartCardH, 14, cCard, cBorder)
			drawTextLeft(img, fs.secTitle, outerPad+18, y+32, cText, "Distribusi Ketidakhadiran Per Bulan")
			if pelNorm == "" {
				drawTextRight(img, fs.small, outerPad+innerW-18, y+31, cMuted, "Biru: Putra  |  Pink: Putri")
			}

			maxBar := 1
			for _, b := range chartBars {
				if b.Total > maxBar {
					maxBar = b.Total
				}
			}

			by := y + 54
			trackX := outerPad + 128
			trackW := innerW - 128 - 118
			for _, b := range chartBars {
				drawTextLeft(img, fs.body, outerPad+18, by+16, cText, b.Label)
				fillRoundedRect(img, float32(trackX), float32(by+4), float32(trackW), 14, 7, cCardInner)

				if b.Total > 0 {
					totalW := float32(b.Total) / float32(maxBar) * float32(trackW)
					if totalW < 8 {
						totalW = 8
					}
					if pelNorm == "" && b.PutraCount > 0 && b.PutriCount > 0 {
						pW := float32(b.PutraCount) / float32(b.Total) * totalW
						wW := totalW - pW
						fillRoundedRect(img, float32(trackX), float32(by+4), pW, 14, 5, cPutra)
						fillRoundedRect(img, float32(trackX)+pW, float32(by+4), wW, 14, 5, cPutri)
					} else {
						barCol := accentBarCol
						if pelNorm == "" && b.PutriCount > 0 && b.PutraCount == 0 {
							barCol = cPutri
						}
						fillRoundedRect(img, float32(trackX), float32(by+4), totalW, 14, 7, barCol)
					}
				}

				valStr := fmt.Sprintf("%dx", b.Total)
				if pelNorm == "" {
					valStr = fmt.Sprintf("%d (%d/%d)", b.Total, b.PutraCount, b.PutriCount)
				}
				drawTextRight(img, fs.small, outerPad+innerW-18, by+16, cMuted, valStr)
				by += 30
			}
		} else {
			halfGap := 14
			panelW := (innerW - halfGap) / 2

			// Left: Sebaran per Kelas
			drawRoundedCard(img, outerPad, y, panelW, chartCardH, 14, cCard, cBorder)
			drawTextLeft(img, fs.secTitle, outerPad+18, y+32, cText, "Sebaran Per Kelas ("+bulNorm+")")
			maxC := 1
			for _, c := range classBars {
				if c.Val > maxC {
					maxC = c.Val
				}
			}
			cy := y + 54
			if len(classBars) == 0 {
				drawTextLeft(img, fs.body, outerPad+18, cy+16, cMuted, "Belum ada ketidakhadiran di bulan ini.")
			} else {
				for _, c := range classBars {
					drawTextLeft(img, fs.body, outerPad+18, cy+16, cText, truncateToWidth(fs.body, c.Key, 100))
					tx := outerPad + 126
					tw := panelW - 126 - 64
					fillRoundedRect(img, float32(tx), float32(cy+4), float32(tw), 14, 7, cCardInner)
					fw := float32(c.Val) / float32(maxC) * float32(tw)
					if fw < 8 {
						fw = 8
					}
					fillRoundedRect(img, float32(tx), float32(cy+4), fw, 14, 7, accentBarCol)
					drawTextRight(img, fs.bodyBold, outerPad+panelW-18, cy+16, cText, fmt.Sprintf("%dx", c.Val))
					cy += 30
				}
			}

			// Right: Sebaran Keterangan
			rx := outerPad + panelW + halfGap
			drawRoundedCard(img, rx, y, panelW, chartCardH, 14, cCard, cBorder)
			drawTextLeft(img, fs.secTitle, rx+18, y+32, cText, "Sebaran Keterangan")
			maxR := 1
			for _, r := range reasonBars {
				if r.Val > maxR {
					maxR = r.Val
				}
			}
			ry := y + 54
			if len(reasonBars) == 0 {
				drawTextLeft(img, fs.body, rx+18, ry+16, cMuted, "Belum ada catatan log.")
			} else {
				for _, r := range reasonBars {
					drawTextLeft(img, fs.body, rx+18, ry+16, cText, truncateToWidth(fs.body, r.Key, 120))
					tx := rx + 144
					tw := panelW - 144 - 64
					fillRoundedRect(img, float32(tx), float32(ry+4), float32(tw), 14, 7, cCardInner)
					fw := float32(r.Val) / float32(maxR) * float32(tw)
					if fw < 8 {
						fw = 8
					}
					fillRoundedRect(img, float32(tx), float32(ry+4), fw, 14, 7, reasonBarColor(r.Key, cRed, cYellow, accentBarCol))
					drawTextRight(img, fs.bodyBold, rx+panelW-18, ry+16, cText, fmt.Sprintf("%dx", r.Val))
					ry += 30
				}
			}
		}

		y += chartCardH + gap
	}

	// 4. TABLE CARD (Siswa Tidak Hadir)
	drawRoundedCard(img, outerPad, y, innerW, tableCardH, 14, cCard, cBorder)

	tableTitle := fmt.Sprintf("Daftar Siswa Tidak Hadir (%d siswa)", len(absentRows))
	if bulNorm != "" {
		tableTitle = fmt.Sprintf("Daftar Tidak Hadir Bulan %s (%d siswa)", bulNorm, len(absentRows))
	}
	drawTextLeft(img, fs.secTitle, outerPad+18, y+32, cText, tableTitle)
	drawTextRight(img, fs.small, outerPad+innerW-18, y+31, cMuted, "Urut: ketidakhadiran terbanyak")

	ty := y + 48
	if len(shownRows) == 0 {
		drawRoundedCard(img, outerPad+16, ty+8, innerW-32, 68, 10, cGreenBg, cGreenBdr)
		drawTextCenter(img, fs.bodyBold, outerPad+innerW/2, ty+38, cGreen, "Nihil Ketidakhadiran!")
		drawTextCenter(img, fs.small, outerPad+innerW/2, ty+59, cText,
			fmt.Sprintf("Seluruh %d siswa pada filter ini tercatat hadir penuh (0 ketidakhadiran).", len(scopedStudents)))
		ty += 88
	} else {
		// Header bar
		fillRoundedRect(img, float32(outerPad+12), float32(ty), float32(innerW-24), float32(tableHeadH-4), 8, cCardInner)
		colNoX := outerPad + 24
		colNameX := outerPad + 64
		colClassX := outerPad + 470
		colPrimaryRight := outerPad + 640
		colTotalRight := outerPad + 738
		colNoteX := outerPad + 762

		drawTextLeft(img, fs.kpiLabel, colNoX, ty+22, cMuted, "NO")
		drawTextLeft(img, fs.kpiLabel, colNameX, ty+22, cMuted, "NAMA PANGGILAN / LENGKAP")
		drawTextLeft(img, fs.kpiLabel, colClassX, ty+22, cMuted, "KELAS")
		if bulNorm != "" {
			drawTextRight(img, fs.kpiLabel, colPrimaryRight, ty+22, cMuted, strings.ToUpper(bulNorm[:3]))
			drawTextRight(img, fs.kpiLabel, colTotalRight, ty+22, cMuted, "THN")
			drawTextLeft(img, fs.kpiLabel, colNoteX, ty+22, cMuted, "RINCIAN")
		} else {
			drawTextRight(img, fs.kpiLabel, colTotalRight, ty+22, cMuted, "TOTAL")
			drawTextLeft(img, fs.kpiLabel, colNoteX, ty+22, cMuted, "BULAN AKTIF")
		}
		ty += tableHeadH

		for idx, r := range shownRows {
			if idx%2 == 1 {
				fillRoundedRect(img, float32(outerPad+12), float32(ty), float32(innerW-24), float32(rowH-2), 6, color.NRGBA{23, 33, 45, 255})
			}

			drawTextLeft(img, fs.body, colNoX, ty+29, cMuted, fmt.Sprintf("%d", idx+1))

			nick := firstNonEmpty(r.Student.NamaLapangan, r.Student.NamaLengkap)
			full := r.Student.NamaLengkap
			drawTextLeft(img, fs.bodyBold, colNameX, ty+21, cText, truncateToWidth(fs.bodyBold, nick, 385))
			drawTextLeft(img, fs.small, colNameX, ty+39, cMuted, truncateToWidth(fs.small, full, 385))

			clsDisplay := r.Student.Kelas
			if pelNorm == "" {
				tag := "PA"
				if r.Student.Peleton == "PUTRI" {
					tag = "PI"
				}
				clsDisplay = fmt.Sprintf("%s (%s)", firstNonEmpty(r.Student.Kelas, "-"), tag)
			}
			drawTextLeft(img, fs.body, colClassX, ty+29, cText, truncateToWidth(fs.body, clsDisplay, 110))

			if bulNorm != "" {
				drawCountBadge(img, fs.bodyBold, colPrimaryRight, ty+29, r.Count, cText, cRed, cRedBg, cRedBdr, cYellow, cYellowBg, cYellowBdr)
				drawCountBadge(img, fs.body, colTotalRight, ty+29, r.Student.Total, cMuted, cRed, cRedBg, cRedBdr, cYellow, cYellowBg, cYellowBdr)
			} else {
				drawCountBadge(img, fs.bodyBold, colTotalRight, ty+29, r.Count, cText, cRed, cRedBg, cRedBdr, cYellow, cYellowBg, cYellowBdr)
			}

			drawTextLeft(img, fs.small, colNoteX, ty+29, color.NRGBA{203, 213, 225, 255},
				truncateToWidth(fs.small, r.NoteSummary, float64(outerPad+innerW-18-colNoteX)))
			ty += rowH
		}

		if extraRows > 0 {
			drawTextLeft(img, fs.small, colNameX, ty+21, cMuted,
				fmt.Sprintf("...dan %d siswa lainnya (buka /gui untuk tabel lengkap)", extraRows))
			ty += 34
		}
	}

	// Table footer: siswa hadir penuh
	drawRoundedCard(img, outerPad+14, ty+6, innerW-28, 32, 8, cGreenBg, cGreenBdr)
	fullMsg := fmt.Sprintf("Hadir Penuh (0 ketidakhadiran): %d dari %d siswa", fullAttendCount, len(scopedStudents))
	drawTextLeft(img, fs.small, outerPad+26, ty+26, cGreen, fullMsg)

	y += tableCardH + gap

	// 5. RECENT LOGS CARD (jika filter bulan aktif dan ada log)
	if logsCardH > 0 {
		drawRoundedCard(img, outerPad, y, innerW, logsCardH, 14, cCard, cBorder)
		drawTextLeft(img, fs.secTitle, outerPad+18, y+30, cText, fmt.Sprintf("Log Terbaru Bulan %s", bulNorm))
		ly := y + 48
		for _, l := range recentLogRows {
			nick := firstNonEmpty(l.NamaLapangan, l.NamaLengkap)
			leftLine := fmt.Sprintf("%s (%s) — %dx %s", nick, l.Kelas, l.Jumlah, l.Keterangan)
			drawTextLeft(img, fs.body, outerPad+18, ly+20, cText, truncateToWidth(fs.body, leftLine, 760))
			drawTextRight(img, fs.small, outerPad+innerW-18, ly+20, cMuted, formatLogTimestamp(l.DicatatAt))
			ly += 32
		}
		y += logsCardH + gap
	}

	// 6. FOOTER LEGEND
	drawRoundedCard(img, outerPad, y, innerW, footerH, 10, cCard, cBorder)
	// Legend dots & labels
	lx := outerPad + 16
	fillRoundedRect(img, float32(lx), float32(y+13), 12, 12, 6, cRed)
	drawTextLeft(img, fs.small, lx+18, y+24, cRed, "> 3x Merah")

	lx += 118
	fillRoundedRect(img, float32(lx), float32(y+13), 12, 12, 6, cYellow)
	drawTextLeft(img, fs.small, lx+18, y+24, cYellow, "= 3x Kuning")

	lx += 118
	fillRoundedRect(img, float32(lx), float32(y+13), 12, 12, 6, cText)
	drawTextLeft(img, fs.small, lx+18, y+24, cText, "< 3x Biasa (tanpa warna)")

	drawTextRight(img, fs.small, outerPad+innerW-16, y+24, cMuted, "Tap tombol di bawah untuk ganti filter atau edit di GUI")

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, "", err
	}

	// Buat caption singkat agar tetap informatif di chat
	captionScope := "Semua Peleton"
	if pelNorm != "" {
		captionScope = "Peleton " + titleCase(pelNorm)
	}
	captionPeriod := "Tahun Ajaran 2026/2027"
	if bulNorm != "" {
		captionPeriod = "Bulan " + bulNorm
	}
	caption := fmt.Sprintf("📊 *%s — %s*\n👥 %d siswa | 📌 %dx tidak hadir (%d siswa absen, %d hadir penuh)",
		captionScope, captionPeriod, len(scopedStudents), totalAbs, absentCount, fullAttendCount)
	if redCount > 0 || yellowCount > 0 {
		caption += fmt.Sprintf("\n🔴 >3x: %d siswa  |  🟡 3x: %d siswa", redCount, yellowCount)
	}

	return buf.Bytes(), caption, nil
}

func buildStudentNoteSummary(s Student, bulNorm string, reasonMap map[string]int) string {
	if bulNorm == "" {
		var mParts []string
		for _, m := range AcademicMonths {
			if v := s.Months[m]; v > 0 {
				mParts = append(mParts, fmt.Sprintf("%s:%d", m[:3], v))
			}
		}
		if len(mParts) == 0 {
			return "-"
		}
		return strings.Join(mParts, ", ")
	}
	if len(reasonMap) == 0 {
		return "-"
	}
	type kv struct {
		k string
		v int
	}
	var list []kv
	for k, v := range reasonMap {
		list = append(list, kv{k, v})
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].v != list[j].v {
			return list[i].v > list[j].v
		}
		return list[i].k < list[j].k
	})
	var parts []string
	for _, it := range list {
		parts = append(parts, fmt.Sprintf("%dx %s", it.v, it.k))
	}
	return strings.Join(parts, ", ")
}

func normalizeReasonLabel(raw string) string {
	low := strings.ToLower(strings.TrimSpace(raw))
	cleaned := strings.Trim(strings.TrimSpace(low), ".,;/ ")
	switch {
	case cleaned == "" || cleaned == "tidak hadir":
		return "Tidak Hadir"
	case cleaned == "s" || strings.Contains(low, "sakit"):
		return "Sakit"
	case cleaned == "i" || cleaned == "i,i" || cleaned == "i, i" || strings.Contains(low, "izin"):
		return "Izin"
	case cleaned == "a" || strings.Contains(low, "alfa") || strings.Contains(low, "alpha"):
		return "Alfa"
	case cleaned == "tm" || strings.Contains(low, "tanpa keterangan"):
		return "TM"
	case strings.Contains(low, "riwayat"):
		return "Riwayat Sheet"
	default:
		return titleCase(low)
	}
}

func formatLogTimestamp(raw string) string {
	s := strings.TrimSpace(raw)
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.Format("02 Jan 2006 15:04")
		}
	}
	return s
}

func reasonBarColor(label string, cRed, cYellow, cDefault color.NRGBA) color.NRGBA {
	low := strings.ToLower(label)
	switch {
	case strings.Contains(low, "alfa") || strings.Contains(low, "tm"):
		return cRed
	case strings.Contains(low, "sakit") || strings.Contains(low, "izin"):
		return cYellow
	default:
		return cDefault
	}
}

func titleCase(s string) string {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return ""
	}
	parts := strings.Fields(s)
	for i, p := range parts {
		r := []rune(p)
		r[0] = []rune(strings.ToUpper(string(r[0])))[0]
		parts[i] = string(r)
	}
	return strings.Join(parts, " ")
}

func drawCountBadge(dst *image.RGBA, face font.Face, rightX, baselineY, val int,
	plainCol, redText, redBg, redBdr, yelText, yelBg, yelBdr color.NRGBA) {
	txt := fmt.Sprintf("%d", val)
	if val <= 0 {
		drawTextRight(dst, face, rightX, baselineY, plainCol, "-")
		return
	}
	if val < 3 {
		drawTextRight(dst, face, rightX, baselineY, plainCol, txt)
		return
	}

	var fg, bg, bdr color.NRGBA
	if val > 3 {
		fg, bg, bdr = redText, redBg, redBdr
	} else {
		fg, bg, bdr = yelText, yelBg, yelBdr
	}
	w := measureWidth(face, txt)
	pillW := int(w) + 20
	if pillW < 34 {
		pillW = 34
	}
	pillH := 26
	pillX := rightX - pillW + 4
	pillY := baselineY - 19
	drawRoundedCard(dst, pillX, pillY, pillW, pillH, 13, bg, bdr)
	drawTextCenter(dst, face, pillX+pillW/2, baselineY, fg, txt)
}

func drawRoundedCard(dst *image.RGBA, x, y, w, h, r int, fill, border color.NRGBA) {
	if w <= 0 || h <= 0 {
		return
	}
	fillRoundedRect(dst, float32(x), float32(y), float32(w), float32(h), float32(r), border)
	if w > 2 && h > 2 {
		ir := r - 1
		if ir < 0 {
			ir = 0
		}
		fillRoundedRect(dst, float32(x+1), float32(y+1), float32(w-2), float32(h-2), float32(ir), fill)
	}
}

func fillRoundedRect(dst *image.RGBA, x, y, w, h, r float32, clr color.NRGBA) {
	if w <= 0 || h <= 0 {
		return
	}
	maxR := w / 2
	if h/2 < maxR {
		maxR = h / 2
	}
	if r > maxR {
		r = maxR
	}
	if r < 0 {
		r = 0
	}

	x0, y0 := int(x), int(y)
	x1, y1 := int(x+w+1), int(y+h+1)
	if x0 < 0 {
		x0 = 0
	}
	if y0 < 0 {
		y0 = 0
	}
	if x1 > dst.Bounds().Max.X {
		x1 = dst.Bounds().Max.X
	}
	if y1 > dst.Bounds().Max.Y {
		y1 = dst.Bounds().Max.Y
	}
	rw, rh := x1-x0, y1-y0
	if rw <= 0 || rh <= 0 {
		return
	}

	lx, ly := x-float32(x0), y-float32(y0)
	var ras vector.Rasterizer
	ras.Reset(rw, rh)
	ras.DrawOp = draw.Over

	ras.MoveTo(lx+r, ly)
	ras.LineTo(lx+w-r, ly)
	if r > 0 {
		ras.QuadTo(lx+w, ly, lx+w, ly+r)
	}
	ras.LineTo(lx+w, ly+h-r)
	if r > 0 {
		ras.QuadTo(lx+w, ly+h, lx+w-r, ly+h)
	}
	ras.LineTo(lx+r, ly+h)
	if r > 0 {
		ras.QuadTo(lx, ly+h, lx, ly+h-r)
	}
	ras.LineTo(lx, ly+r)
	if r > 0 {
		ras.QuadTo(lx, ly, lx+r, ly)
	}
	ras.ClosePath()

	ras.Draw(dst, image.Rect(x0, y0, x1, y1), image.NewUniform(clr), image.Point{})
}

func measureWidth(face font.Face, s string) float64 {
	adv := font.MeasureString(face, s)
	return float64(adv) / 64.0
}

func truncateToWidth(face font.Face, s string, maxW float64) string {
	if measureWidth(face, s) <= maxW {
		return s
	}
	runes := []rune(s)
	for len(runes) > 1 {
		runes = runes[:len(runes)-1]
		cand := string(runes) + "..."
		if measureWidth(face, cand) <= maxW {
			return cand
		}
	}
	return "..."
}

func drawTextLeft(dst *image.RGBA, face font.Face, x, baselineY int, clr color.NRGBA, s string) {
	d := &font.Drawer{
		Dst:  dst,
		Src:  image.NewUniform(clr),
		Face: face,
		Dot:  fixed.P(x, baselineY),
	}
	d.DrawString(s)
}

func drawTextRight(dst *image.RGBA, face font.Face, rightX, baselineY int, clr color.NRGBA, s string) {
	w := measureWidth(face, s)
	drawTextLeft(dst, face, rightX-int(w+0.5), baselineY, clr, s)
}

func drawTextCenter(dst *image.RGBA, face font.Face, centerX, baselineY int, clr color.NRGBA, s string) {
	w := measureWidth(face, s)
	drawTextLeft(dst, face, centerX-int(w/2+0.5), baselineY, clr, s)
}
