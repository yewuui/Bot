# Asisten Laptop & Bot Absensi Telegram

Bot Telegram berbasis Go untuk mengontrol laptop Windows dari jarak jauh, berinteraksi dengan asisten AI (via endpoint OpenAI-compatible seperti 9router), serta mengelola sistem absensi berbasis SQLite lengkap dengan Web App GUI dan generator gambar rekap.

## Fitur Utama

- **Kontrol Laptop via Telegram**: Screenshot layar (`/ss`), info sistem (`/sysinfo`), jalankan perintah PowerShell dengan konfirmasi keamanan, kirim/terima file, kunci layar, hingga kontrol Laragon.
- **Asisten AI**: Terhubung ke endpoint OpenAI-compatible (`ROUTER_ENDPOINT`) untuk percakapan alami dan eksekusi aksi terstruktur.
- **Sistem Absensi (SQLite)**: Pencatatan kehadiran per peleton/bulan, import/seed dari file Excel (`.xlsx`), export data, serta filter cepat.
- **Web App GUI & Rekap Gambar**: Panel web lokal (`web/index.html`) yang bisa diakses langsung di localhost atau melalui Telegram Web App via `cloudflared` tunnel, ditambah render gambar rekap absensi PNG (`recap_image.go`).

## Prasyarat

- **Go** (1.22+ / sesuai `go.mod`)
- **Token Bot Telegram** dari `@BotFather` dan **Chat ID** pemilik bot
- *(Opsional)* **9router** atau server OpenAI-compatible untuk fitur chat AI
- *(Opsional)* **cloudflared** (`C:\Program Files (x86)\cloudflared\cloudflared.exe`) untuk mengekspos Web App GUI sebagai Telegram Mini App via HTTPS tunnel
- *(Opsional)* **Laragon** jika menggunakan perintah kontrol Laragon

## Instalasi & Konfigurasi

1. Clone repositori ini:
   ```bash
   git clone <url-repo-github-anda>
   cd Bot
   ```

2. Salin `.env.example` menjadi `.env`:
   ```bash
   copy .env.example .env
   ```

3. Edit `.env` dan isi nilai konfigurasi:
   - `BOT_TOKEN` — Token bot Telegram dari `@BotFather`
   - `ALLOWED_CHAT_ID` — ID Telegram milikmu (hanya ID ini yang diizinkan mengakses bot)
   - `ROUTER_ENDPOINT`, `ROUTER_API_KEY`, `ROUTER_MODEL` — Konfigurasi server AI
   - `LARAGON_EXE_PATH` & `EXCEL_SEED_PATH` — Opsional

4. Jalankan atau build:
   ```bash
   go run .
   ```
   atau build menjadi executable:
   ```bash
   go build -o asisten-laptop.exe .
   ```

## Menjalankan Test

```bash
go test ./...
```
