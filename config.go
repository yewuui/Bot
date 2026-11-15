package main

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Config holds runtime configuration loaded from environment variables or a local .env file.
type Config struct {
	BotToken       string
	AllowedChatID  int64
	RouterEndpoint string
	RouterAPIKey   string
	RouterModel    string
	LaragonExePath string
	ExcelSeedPath  string
}

// loadDotEnv reads KEY=VALUE pairs from path into the process environment without overriding existing vars.
func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		idx := strings.IndexByte(line, '=')
		if idx <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:idx])
		val := strings.TrimSpace(line[idx+1:])
		if len(val) >= 2 && ((val[0] == '"' && val[len(val)-1] == '"') || (val[0] == '\'' && val[len(val)-1] == '\'')) {
			val = val[1 : len(val)-1]
		}
		if key != "" && os.Getenv(key) == "" {
			_ = os.Setenv(key, val)
		}
	}
}

func loadConfig() (Config, error) {
	loadDotEnv(".env")

	cfg := Config{
		BotToken:       strings.TrimSpace(os.Getenv("BOT_TOKEN")),
		RouterEndpoint: getEnvOrDefault("ROUTER_ENDPOINT", "http://localhost:20128/v1/chat/completions"),
		RouterAPIKey:   strings.TrimSpace(os.Getenv("ROUTER_API_KEY")),
		RouterModel:    getEnvOrDefault("ROUTER_MODEL", "bot"),
		LaragonExePath: getEnvOrDefault("LARAGON_EXE_PATH", `D:\laragon\laragon.exe`),
		ExcelSeedPath:  strings.TrimSpace(os.Getenv("EXCEL_SEED_PATH")),
	}

	if cfg.BotToken == "" {
		return cfg, fmt.Errorf("BOT_TOKEN belum diatur (isi di file .env atau environment variable)")
	}

	rawChatID := strings.TrimSpace(os.Getenv("ALLOWED_CHAT_ID"))
	if rawChatID == "" {
		return cfg, fmt.Errorf("ALLOWED_CHAT_ID belum diatur (isi di file .env atau environment variable)")
	}
	chatID, err := strconv.ParseInt(rawChatID, 10, 64)
	if err != nil || chatID == 0 {
		return cfg, fmt.Errorf("ALLOWED_CHAT_ID tidak valid: %q", rawChatID)
	}
	cfg.AllowedChatID = chatID

	return cfg, nil
}

func getEnvOrDefault(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}
