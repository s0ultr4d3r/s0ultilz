package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/joho/godotenv"
	xproxy "golang.org/x/net/proxy"
)

// ---------- Types ----------

type Config struct {
	TelegramToken       string
	TelegramSocks5Proxy string

	YtDlpPath             string
	YtDlpCookiesFile      string // generic cookies (used for IG etc.)
	YtDlpProxy            string
	YtDlpJsRuntimes       string
	YtDlpRemoteComponents string // e.g. ejs:github
	YtDlpCacheDir         string // yt-dlp cache dir (important for systemd)

	// YouTube-specific cookies (Netscape). If set, used for YouTube instead of generic cookies.
	YtDlpYouTubeCookiesFile string

	// YouTube tweaks
	YtDlpForceIPv4 bool

	// Base extractor args; we will append youtube:player_client=... automatically during fallback.
	YtDlpYouTubeExtractorArgs string // e.g. "youtube:player_skip=webpage"

	// Client priority lists
	YouTubeClientsAudio string // default: android,tv,web
	YouTubeClientsVideo string // default: android,web,tv

	GalleryDlPath        string
	GalleryDlCookiesFile string

	FfmpegPath string

	DownloadStorageDir     string
	DownloadPublicBaseURL  string
	DownloadListenAddr     string
	DownloadRetention      time.Duration
	DownloadMaxStorageSize int64
}

type PendingAction int

const (
	PendingNone PendingAction = iota
	PendingInstLink
	PendingStoriesUsername
	PendingYtVideoLink
	PendingYtAudioLink
)

const telegramUploadLimit = 48 * 1024 * 1024 // ~48MB

type App struct {
	Bot     *tgbotapi.BotAPI
	Cfg     *Config
	mu      sync.Mutex
	pending map[int64]PendingAction
}

// ---------- main ----------

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	cfg, err := LoadConfig()
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}

	tgHTTPClient, err := newTelegramHTTPClient(cfg.TelegramSocks5Proxy)
	if err != nil {
		log.Fatalf("failed to create telegram http client: %v", err)
	}

	bot, err := tgbotapi.NewBotAPIWithClient(cfg.TelegramToken, tgbotapi.APIEndpoint, tgHTTPClient)
	if err != nil {
		log.Fatalf("failed to create bot: %v", err)
	}
	bot.Debug = false

	log.Printf("Authorized on account %s", bot.Self.UserName)
	log.Printf("Config: tg_socks5_proxy=%q, yt-dlp=%s, proxy=%s, js-runtimes=%q, remote-components=%q, cache-dir=%q, yt_force_ipv4=%v, yt_extractor_args_base=%q, yt_cookies=%q, yt_clients_audio=%q, yt_clients_video=%q, download_dir=%q, download_public_base_url=%q, download_listen_addr=%q, download_retention=%s, download_max_storage=%s",
		cfg.TelegramSocks5Proxy, cfg.YtDlpPath, cfg.YtDlpProxy, cfg.YtDlpJsRuntimes, cfg.YtDlpRemoteComponents, cfg.YtDlpCacheDir, cfg.YtDlpForceIPv4, cfg.YtDlpYouTubeExtractorArgs, cfg.YtDlpYouTubeCookiesFile, cfg.YouTubeClientsAudio, cfg.YouTubeClientsVideo, cfg.DownloadStorageDir, cfg.DownloadPublicBaseURL, cfg.DownloadListenAddr, cfg.DownloadRetention, humanBytes(cfg.DownloadMaxStorageSize))

	if err := os.MkdirAll(cfg.DownloadStorageDir, 0755); err != nil {
		log.Fatalf("failed to create download storage dir %s: %v", cfg.DownloadStorageDir, err)
	}
	cleanupDownloadStorage(cfg.DownloadStorageDir, cfg.DownloadRetention, cfg.DownloadMaxStorageSize)
	go startDownloadCleanupLoop(cfg.DownloadStorageDir, cfg.DownloadRetention, cfg.DownloadMaxStorageSize)
	if cfg.DownloadListenAddr != "" {
		go startDownloadHTTPServer(cfg.DownloadListenAddr, cfg.DownloadStorageDir)
	}

	app := &App{
		Bot:     bot,
		Cfg:     cfg,
		pending: make(map[int64]PendingAction),
	}

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 30

	updates := bot.GetUpdatesChan(u)

	for update := range updates {
		if update.Message == nil {
			continue
		}

		if update.Message.IsCommand() {
			switch update.Message.Command() {
			case "start":
				app.handleStartCommand(update.Message)
			case "inst":
				app.handleInstCommand(update.Message)
			case "inststories":
				app.handleInstStoriesCommand(update.Message)
			case "yt":
				app.handleYtCommand(update.Message)
			case "ytmp3":
				app.handleYtMp3Command(update.Message)
			default:
				msg := tgbotapi.NewMessage(update.Message.Chat.ID, "Не знаю такую команду 😿")
				msg.ReplyMarkup = defaultKeyboard()
				_, _ = bot.Send(msg)
			}
			continue
		}

		if update.Message.Text != "" {
			app.handleNonCommandMessage(update.Message)
		}
	}
}

// ---------- Telegram HTTP client ----------

func newTelegramHTTPClient(proxyRaw string) (*http.Client, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.TLSHandshakeTimeout = 30 * time.Second
	transport.ResponseHeaderTimeout = 60 * time.Second
	transport.ExpectContinueTimeout = 1 * time.Second
	transport.IdleConnTimeout = 90 * time.Second

	client := &http.Client{
		Transport: transport,
		Timeout:   90 * time.Second,
	}

	proxyRaw = strings.TrimSpace(proxyRaw)
	if proxyRaw == "" {
		return client, nil
	}

	proxyAddr, auth, err := parseSOCKS5Proxy(proxyRaw)
	if err != nil {
		return nil, err
	}

	baseDialer := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}

	socksDialer, err := xproxy.SOCKS5("tcp", proxyAddr, auth, baseDialer)
	if err != nil {
		return nil, fmt.Errorf("create socks5 dialer: %w", err)
	}

	transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		type dialResult struct {
			conn net.Conn
			err  error
		}

		ch := make(chan dialResult, 1)

		go func() {
			conn, err := socksDialer.Dial(network, addr)
			ch <- dialResult{conn: conn, err: err}
		}()

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case res := <-ch:
			return res.conn, res.err
		}
	}

	return client, nil
}

func parseSOCKS5Proxy(raw string) (string, *xproxy.Auth, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil, fmt.Errorf("empty telegram socks5 proxy")
	}

	if !strings.Contains(raw, "://") {
		return raw, nil, nil
	}

	u, err := url.Parse(raw)
	if err != nil {
		return "", nil, fmt.Errorf("parse telegram socks5 proxy: %w", err)
	}

	scheme := strings.ToLower(strings.TrimSpace(u.Scheme))
	if scheme != "socks5" && scheme != "socks5h" {
		return "", nil, fmt.Errorf("unsupported telegram proxy scheme: %s", u.Scheme)
	}

	if u.Host == "" {
		return "", nil, fmt.Errorf("empty telegram socks5 proxy host")
	}

	var auth *xproxy.Auth
	if u.User != nil {
		user := u.User.Username()
		pass, _ := u.User.Password()
		if user != "" {
			auth = &xproxy.Auth{
				User:     user,
				Password: pass,
			}
		}
	}

	return u.Host, auth, nil
}

func isSOCKSProxyValue(v string) bool {
	v = strings.TrimSpace(strings.ToLower(v))
	if v == "" {
		return false
	}
	if !strings.Contains(v, "://") {
		return true
	}
	return strings.HasPrefix(v, "socks5://") || strings.HasPrefix(v, "socks5h://")
}

// ---------- Config ----------

func LoadConfig() (*Config, error) {
	_ = godotenv.Load()

	token := strings.TrimSpace(os.Getenv("TELEGRAM_BOT_TOKEN"))
	if token == "" {
		return nil, fmt.Errorf("TELEGRAM_BOT_TOKEN is not set")
	}

	ytPath := strings.TrimSpace(os.Getenv("YTDLP_PATH"))
	if ytPath == "" {
		ytPath = "yt-dlp"
	}

	galPath := strings.TrimSpace(os.Getenv("GALLERYDL_PATH"))
	if galPath == "" {
		galPath = "gallery-dl"
	}

	jsRuntimes := strings.TrimSpace(os.Getenv("YTDLP_JS_RUNTIMES"))
	if jsRuntimes == "" {
		jsRuntimes = strings.TrimSpace(os.Getenv("YTDLP_JS_RUNTIME"))
	}
	if jsRuntimes == "" {
		if _, err := exec.LookPath("deno"); err == nil {
			jsRuntimes = "deno"
		} else if _, err := exec.LookPath("node"); err == nil {
			jsRuntimes = "node"
		} else if _, err := exec.LookPath("nodejs"); err == nil {
			jsRuntimes = "nodejs"
		}
	}

	forceIPv4 := parseBoolEnv("YTDLP_FORCE_IPV4", false)
	ytExtractorArgs := strings.TrimSpace(os.Getenv("YTDLP_YOUTUBE_EXTRACTOR_ARGS"))

	remoteComponents := strings.TrimSpace(os.Getenv("YTDLP_REMOTE_COMPONENTS"))
	cacheDir := strings.TrimSpace(os.Getenv("YTDLP_CACHE_DIR"))

	clientsAudio := strings.TrimSpace(os.Getenv("YTDLP_YOUTUBE_CLIENTS_AUDIO"))
	clientsVideo := strings.TrimSpace(os.Getenv("YTDLP_YOUTUBE_CLIENTS_VIDEO"))
	ytProxy := strings.TrimSpace(os.Getenv("YTDLP_PROXY"))

	tgProxy := strings.TrimSpace(os.Getenv("TELEGRAM_SOCKS5_PROXY"))
	if tgProxy == "" && isSOCKSProxyValue(ytProxy) {
		tgProxy = ytProxy
	}

	// If YouTube cookies are provided, prefer web-like clients for video, because android client does not support cookies in yt-dlp.
	ytCookies := strings.TrimSpace(os.Getenv("YTDLP_YOUTUBE_COOKIES_FILE"))
	if clientsAudio == "" {
		clientsAudio = "android,tv,web"
	}
	if clientsVideo == "" {
		if ytCookies != "" {
			clientsVideo = "web,web_safari,tv"
		} else {
			clientsVideo = "android,web,tv"
		}
	}
	if ytCookies != "" {
		clientsVideo = removeFromCSV(clientsVideo, "android")
	}

	downloadStorageDir := strings.TrimSpace(os.Getenv("DOWNLOAD_STORAGE_DIR"))
	if downloadStorageDir == "" {
		downloadStorageDir = "/var/lib/s0ultilz-bot/downloads"
	}
	downloadPublicBaseURL := strings.TrimRight(strings.TrimSpace(os.Getenv("DOWNLOAD_PUBLIC_BASE_URL")), "/")
	downloadListenAddr := strings.TrimSpace(os.Getenv("DOWNLOAD_LISTEN_ADDR"))
	downloadRetentionDays := parseIntEnv("DOWNLOAD_RETENTION_DAYS", 31)
	if downloadRetentionDays < 1 {
		downloadRetentionDays = 31
	}
	downloadMaxStorageGB := parseIntEnv("DOWNLOAD_MAX_STORAGE_GB", 20)
	if downloadMaxStorageGB < 1 {
		downloadMaxStorageGB = 20
	}

	cfg := &Config{
		TelegramToken:       token,
		TelegramSocks5Proxy: tgProxy,

		YtDlpPath:             ytPath,
		YtDlpCookiesFile:      strings.TrimSpace(os.Getenv("YTDLP_COOKIES_FILE")),
		YtDlpProxy:            ytProxy,
		YtDlpJsRuntimes:       jsRuntimes,
		YtDlpRemoteComponents: remoteComponents,
		YtDlpCacheDir:         cacheDir,

		YtDlpYouTubeCookiesFile: strings.TrimSpace(os.Getenv("YTDLP_YOUTUBE_COOKIES_FILE")),

		YtDlpForceIPv4:            forceIPv4,
		YtDlpYouTubeExtractorArgs: ytExtractorArgs,

		YouTubeClientsAudio: clientsAudio,
		YouTubeClientsVideo: clientsVideo,

		GalleryDlPath:        galPath,
		GalleryDlCookiesFile: strings.TrimSpace(os.Getenv("GALLERYDL_COOKIES_FILE")),

		FfmpegPath: strings.TrimSpace(os.Getenv("FFMPEG_PATH")),

		DownloadStorageDir:     downloadStorageDir,
		DownloadPublicBaseURL:  downloadPublicBaseURL,
		DownloadListenAddr:     downloadListenAddr,
		DownloadRetention:      time.Duration(downloadRetentionDays) * 24 * time.Hour,
		DownloadMaxStorageSize: int64(downloadMaxStorageGB) * 1024 * 1024 * 1024,
	}
	return cfg, nil
}

func parseBoolEnv(key string, def bool) bool {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	if v == "1" || strings.EqualFold(v, "true") || strings.EqualFold(v, "yes") || strings.EqualFold(v, "y") {
		return true
	}
	if v == "0" || strings.EqualFold(v, "false") || strings.EqualFold(v, "no") || strings.EqualFold(v, "n") {
		return false
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

func parseIntEnv(key string, def int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

// ---------- Pending state helpers ----------

func (a *App) setPending(chatID int64, action PendingAction) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if action == PendingNone {
		delete(a.pending, chatID)
	} else {
		a.pending[chatID] = action
	}
}

func (a *App) getPending(chatID int64) PendingAction {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.pending[chatID]
}

// ---------- Keyboards ----------

func defaultKeyboard() tgbotapi.ReplyKeyboardMarkup {
	kb := tgbotapi.NewReplyKeyboard(
		tgbotapi.NewKeyboardButtonRow(
			tgbotapi.NewKeyboardButton("/inst"),
			tgbotapi.NewKeyboardButton("/inststories"),
		),
		tgbotapi.NewKeyboardButtonRow(
			tgbotapi.NewKeyboardButton("/yt"),
			tgbotapi.NewKeyboardButton("/ytmp3"),
		),
	)
	kb.ResizeKeyboard = true
	kb.OneTimeKeyboard = false
	return kb
}

// ---------- Command handlers ----------

func (a *App) handleStartCommand(msg *tgbotapi.Message) {
	text := "Команды:\n" +
		"• /inst — Instagram пост/риилс/сторис по ссылке\n" +
		"• /inststories — Instagram сторис по username\n" +
		"• /yt — YouTube видео (как файл)\n" +
		"• /ytmp3 — YouTube MP3\n\n" +
		"Нажми кнопку и просто пришли ссылку/ник 🙂"
	resp := tgbotapi.NewMessage(msg.Chat.ID, text)
	resp.ReplyMarkup = defaultKeyboard()
	_, _ = a.Bot.Send(resp)
}

func (a *App) handleInstCommand(msg *tgbotapi.Message) {
	args := strings.TrimSpace(msg.CommandArguments())
	if args == "" {
		a.setPending(msg.Chat.ID, PendingInstLink)
		resp := tgbotapi.NewMessage(msg.Chat.ID, "Пришли ссылку на Instagram (пост/риилс/сторис).")
		resp.ReplyMarkup = defaultKeyboard()
		_, _ = a.Bot.Send(resp)
		return
	}
	url := strings.Fields(args)[0]
	a.setPending(msg.Chat.ID, PendingNone)
	a.processInstURL(msg.Chat.ID, url)
}

func (a *App) handleInstStoriesCommand(msg *tgbotapi.Message) {
	args := strings.TrimSpace(msg.CommandArguments())
	if args == "" {
		a.setPending(msg.Chat.ID, PendingStoriesUsername)
		resp := tgbotapi.NewMessage(msg.Chat.ID, "Пришли username (без ссылки), например: instagram")
		resp.ReplyMarkup = defaultKeyboard()
		_, _ = a.Bot.Send(resp)
		return
	}
	username := strings.Fields(args)[0]
	a.setPending(msg.Chat.ID, PendingNone)
	a.processStoriesUsername(msg.Chat.ID, username)
}

func (a *App) handleYtCommand(msg *tgbotapi.Message) {
	args := strings.TrimSpace(msg.CommandArguments())
	if args == "" {
		a.setPending(msg.Chat.ID, PendingYtVideoLink)
		resp := tgbotapi.NewMessage(msg.Chat.ID, "Пришли ссылку на YouTube видео.")
		resp.ReplyMarkup = defaultKeyboard()
		_, _ = a.Bot.Send(resp)
		return
	}
	url := strings.Fields(args)[0]
	a.setPending(msg.Chat.ID, PendingNone)
	a.processYtURL(msg.Chat.ID, url, false)
}

func (a *App) handleYtMp3Command(msg *tgbotapi.Message) {
	args := strings.TrimSpace(msg.CommandArguments())
	if args == "" {
		a.setPending(msg.Chat.ID, PendingYtAudioLink)
		resp := tgbotapi.NewMessage(msg.Chat.ID, "Пришли ссылку на YouTube, из которой сделать MP3.")
		resp.ReplyMarkup = defaultKeyboard()
		_, _ = a.Bot.Send(resp)
		return
	}
	url := strings.Fields(args)[0]
	a.setPending(msg.Chat.ID, PendingNone)
	a.processYtURL(msg.Chat.ID, url, true)
}

// ---------- Non-command handler ----------

func (a *App) handleNonCommandMessage(msg *tgbotapi.Message) {
	state := a.getPending(msg.Chat.ID)
	text := strings.TrimSpace(msg.Text)

	switch state {
	case PendingInstLink:
		url := firstMatch(text, isInstagramURL)
		if url == "" {
			resp := tgbotapi.NewMessage(msg.Chat.ID, "Не вижу ссылки на Instagram, пришли URL.")
			resp.ReplyMarkup = defaultKeyboard()
			_, _ = a.Bot.Send(resp)
			return
		}
		a.setPending(msg.Chat.ID, PendingNone)
		a.processInstURL(msg.Chat.ID, url)
		return

	case PendingStoriesUsername:
		username := strings.Fields(text)[0]
		a.setPending(msg.Chat.ID, PendingNone)
		a.processStoriesUsername(msg.Chat.ID, username)
		return

	case PendingYtVideoLink:
		url := firstMatch(text, isYouTubeURL)
		if url == "" {
			resp := tgbotapi.NewMessage(msg.Chat.ID, "Не вижу ссылки на YouTube, пришли URL.")
			resp.ReplyMarkup = defaultKeyboard()
			_, _ = a.Bot.Send(resp)
			return
		}
		a.setPending(msg.Chat.ID, PendingNone)
		a.processYtURL(msg.Chat.ID, url, false)
		return

	case PendingYtAudioLink:
		url := firstMatch(text, isYouTubeURL)
		if url == "" {
			resp := tgbotapi.NewMessage(msg.Chat.ID, "Не вижу ссылки на YouTube, пришли URL.")
			resp.ReplyMarkup = defaultKeyboard()
			_, _ = a.Bot.Send(resp)
			return
		}
		a.setPending(msg.Chat.ID, PendingNone)
		a.processYtURL(msg.Chat.ID, url, true)
		return
	}
}

func firstMatch(text string, pred func(string) bool) string {
	for _, p := range strings.Fields(text) {
		if pred(p) {
			return p
		}
	}
	return ""
}

// ---------- High-level actions ----------

func (a *App) processInstURL(chatID int64, url string) {
	if !isInstagramURL(url) {
		resp := tgbotapi.NewMessage(chatID, "Похоже, это не ссылка на Instagram.")
		resp.ReplyMarkup = defaultKeyboard()
		_, _ = a.Bot.Send(resp)
		return
	}

	files, logOutput, err := a.downloadInstagram(url)
	if err != nil {
		log.Printf("downloadInstagram error: %v\nlogs:\n%s", err, logOutput)
		resp := tgbotapi.NewMessage(chatID, formatUserError("Не удалось скачать: ", err, logOutput))
		resp.ReplyMarkup = defaultKeyboard()
		_, _ = a.Bot.Send(resp)
		return
	}

	for _, fp := range files {
		a.sendAsDocument(chatID, fp)
	}
	if len(files) > 0 {
		_ = os.RemoveAll(filepath.Dir(files[0]))
	}
}

func (a *App) processStoriesUsername(chatID int64, rawUsername string) {
	username := strings.TrimPrefix(strings.TrimSpace(rawUsername), "@")
	if username == "" {
		resp := tgbotapi.NewMessage(chatID, "Пустой username 🙃")
		resp.ReplyMarkup = defaultKeyboard()
		_, _ = a.Bot.Send(resp)
		return
	}

	files, logOutput, err := a.downloadInstagramStories(username)
	if err != nil {
		log.Printf("downloadInstagramStories error: %v\nlogs:\n%s", err, logOutput)
		resp := tgbotapi.NewMessage(chatID, formatUserError("Не удалось скачать сторис: ", err, logOutput))
		resp.ReplyMarkup = defaultKeyboard()
		_, _ = a.Bot.Send(resp)
		return
	}

	for _, fp := range files {
		a.sendAsDocument(chatID, fp)
	}
	if len(files) > 0 {
		_ = os.RemoveAll(filepath.Dir(files[0]))
	}
}

func (a *App) processYtURL(chatID int64, url string, audioOnly bool) {
	if !isYouTubeURL(url) {
		resp := tgbotapi.NewMessage(chatID, "Похоже, это не ссылка на YouTube.")
		resp.ReplyMarkup = defaultKeyboard()
		_, _ = a.Bot.Send(resp)
		return
	}

	files, logOutput, err := a.downloadYouTubeAuto(url, audioOnly)
	if err != nil {
		log.Printf("downloadYouTube error (audioOnly=%v): %v\nlogs:\n%s", audioOnly, err, logOutput)
		resp := tgbotapi.NewMessage(chatID, formatUserError("Не удалось скачать с YouTube: ", err, logOutput))
		resp.ReplyMarkup = defaultKeyboard()
		_, _ = a.Bot.Send(resp)
		return
	}

	for _, fp := range files {
		a.sendAsDocument(chatID, fp)
	}
	_ = os.RemoveAll(filepath.Dir(files[0]))
}

// ---------- Instagram download logic ----------

func (a *App) downloadInstagram(url string) ([]string, string, error) {
	var allLogs strings.Builder

	files1, log1, err1 := a.downloadWithYtDlp(url)
	allLogs.WriteString("[yt-dlp]\n")
	allLogs.WriteString(log1)
	allLogs.WriteString("\n\n")

	if len(files1) > 0 {
		return files1, allLogs.String(), nil
	}

	files2, log2, err2 := a.downloadWithGalleryDl(url)
	allLogs.WriteString("[gallery-dl]\n")
	allLogs.WriteString(log2)
	allLogs.WriteString("\n")

	if len(files2) > 0 {
		return files2, allLogs.String(), nil
	}

	if err2 != nil {
		return nil, allLogs.String(), fmt.Errorf("yt-dlp+gallery-dl both failed: %w", err2)
	}
	if err1 != nil {
		return nil, allLogs.String(), fmt.Errorf("yt-dlp+gallery-dl produced no files: %w", err1)
	}
	return nil, allLogs.String(), fmt.Errorf("yt-dlp+gallery-dl produced no files")
}

func (a *App) downloadInstagramStories(username string) ([]string, string, error) {
	profileURL := fmt.Sprintf("https://www.instagram.com/%s/", username)
	return a.runGalleryDl(profileURL, []string{"-o", "include=stories"})
}

// ---------- yt-dlp (Instagram) ----------

func (a *App) downloadWithYtDlp(url string) ([]string, string, error) {
	tmpDir1, err := os.MkdirTemp("", "s0ultilz_yt_1_*")
	if err != nil {
		return nil, "", fmt.Errorf("yt-dlp temp dir error: %w", err)
	}

	files1, log1, _ := a.runYtDlpOnce(url, tmpDir1, false)
	if len(files1) > 0 {
		return files1, log1, nil
	}

	tmpDir2, err := os.MkdirTemp("", "s0ultilz_yt_2_*")
	if err != nil {
		return nil, log1 + "\n[generic] temp dir error: " + err.Error(), err
	}
	files2, log2, err2 := a.runYtDlpOnce(url, tmpDir2, true)
	combinedLog := log1 + "\n\n[yt-dlp generic attempt]\n" + log2
	if len(files2) > 0 {
		return files2, combinedLog, nil
	}
	if err2 != nil {
		return nil, combinedLog, fmt.Errorf("yt-dlp failed (normal+generic): %w", err2)
	}
	return nil, combinedLog, fmt.Errorf("yt-dlp produced no files")
}

func (a *App) runYtDlpOnce(url, tmpDir string, forceGeneric bool) ([]string, string, error) {
	args := []string{"--no-playlist", "--restrict-filenames", "-o", filepath.Join(tmpDir, "%(id)s.%(ext)s")}

	if a.Cfg.YtDlpJsRuntimes != "" {
		args = append(args, "--js-runtimes", a.Cfg.YtDlpJsRuntimes)
	}
	if a.Cfg.YtDlpCookiesFile != "" {
		args = append(args, "--cookies", a.Cfg.YtDlpCookiesFile)
	}
	if a.Cfg.YtDlpProxy != "" {
		args = append(args, "--proxy", a.Cfg.YtDlpProxy)
	}
	if forceGeneric {
		args = append(args, "--force-generic-extractor")
	}

	args = append(args, url)

	cmd := exec.Command(a.Cfg.YtDlpPath, args...)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	runErr := cmd.Run()

	logOutput := outBuf.String() + "\n" + errBuf.String()
	files := collectFiles(tmpDir)
	if runErr != nil && len(files) == 0 {
		return nil, logOutput, fmt.Errorf("yt-dlp failed: %w", runErr)
	}
	return files, logOutput, nil
}

// ---------- gallery-dl (Instagram) ----------

func (a *App) downloadWithGalleryDl(url string) ([]string, string, error) {
	return a.runGalleryDl(url, nil)
}

func (a *App) runGalleryDl(url string, extraArgs []string) ([]string, string, error) {
	tmpDir, err := os.MkdirTemp("", "s0ultilz_gal_*")
	if err != nil {
		return nil, "", fmt.Errorf("gallery-dl temp dir error: %w", err)
	}

	args := []string{"-d", tmpDir}
	if len(extraArgs) > 0 {
		args = append(args, extraArgs...)
	}
	if a.Cfg.GalleryDlCookiesFile != "" {
		args = append(args, "--cookies", a.Cfg.GalleryDlCookiesFile)
	}
	args = append(args, url)

	cmd := exec.Command(a.Cfg.GalleryDlPath, args...)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	runErr := cmd.Run()

	logOutput := outBuf.String() + "\n" + errBuf.String()
	files := collectFiles(tmpDir)

	if runErr != nil && len(files) == 0 {
		return nil, logOutput, fmt.Errorf("gallery-dl failed: %w", runErr)
	}
	return files, logOutput, nil
}

// ---------- YouTube with auto-fallback ----------

func (a *App) downloadYouTubeAuto(url string, audioOnly bool) ([]string, string, error) {
	cookies := strings.TrimSpace(a.Cfg.YtDlpYouTubeCookiesFile)
	if cookies == "" {
		cookies = strings.TrimSpace(a.Cfg.YtDlpCookiesFile)
	}

	var clients []string
	if audioOnly {
		clients = parseCSV(a.Cfg.YouTubeClientsAudio)
	} else {
		clients = parseCSV(a.Cfg.YouTubeClientsVideo)
		if cookies != "" {
			var filtered []string
			for _, c := range clients {
				if strings.EqualFold(c, "android") {
					continue
				}
				filtered = append(filtered, c)
			}
			clients = filtered
		}
	}
	if len(clients) == 0 {
		if audioOnly {
			clients = []string{"android", "tv", "web"}
		} else {
			clients = []string{"android", "web", "tv"}
		}
	}

	var allLogs strings.Builder
	var lastErr error

	for i, client := range clients {
		tmpDir, err := os.MkdirTemp("", "s0ultilz_youtube_*")
		if err != nil {
			return nil, "", fmt.Errorf("yt-dlp temp dir error: %w", err)
		}

		files, logs, err := a.downloadYouTubeOnce(url, audioOnly, client, tmpDir)
		allLogs.WriteString(fmt.Sprintf("[attempt %d/%d client=%s]\n", i+1, len(clients), client))
		allLogs.WriteString(logs)
		allLogs.WriteString("\n\n")

		if len(files) > 0 && err == nil {
			return files, allLogs.String(), nil
		}
		if len(files) > 0 {
			return files, allLogs.String(), nil
		}

		lastErr = err

		if isYouTubeSignInBotCheck(logs) {
			_ = os.RemoveAll(tmpDir)
			return nil, allLogs.String(), fmt.Errorf("youtube bot-check: cookies required")
		}

		if !shouldRetryWithNextClient(logs, err) {
			_ = os.RemoveAll(tmpDir)
			return nil, allLogs.String(), err
		}

		_ = os.RemoveAll(tmpDir)
	}

	if lastErr == nil {
		lastErr = fmt.Errorf("yt-dlp produced no files")
	}
	return nil, allLogs.String(), lastErr
}

func (a *App) downloadYouTubeOnce(url string, audioOnly bool, client string, tmpDir string) ([]string, string, error) {
	args := []string{"--no-playlist", "-o", filepath.Join(tmpDir, "%(title).180B.%(ext)s")}

	if a.Cfg.YtDlpCacheDir != "" {
		args = append(args, "--cache-dir", a.Cfg.YtDlpCacheDir)
	}
	if a.Cfg.YtDlpRemoteComponents != "" {
		args = append(args, "--remote-components", a.Cfg.YtDlpRemoteComponents)
	}

	if a.Cfg.YtDlpJsRuntimes != "" {
		args = append(args, "--js-runtimes", a.Cfg.YtDlpJsRuntimes)
	}
	if a.Cfg.YtDlpForceIPv4 {
		args = append(args, "--force-ipv4")
	}

	cookies := strings.TrimSpace(a.Cfg.YtDlpYouTubeCookiesFile)
	if cookies == "" {
		cookies = strings.TrimSpace(a.Cfg.YtDlpCookiesFile)
	}
	if cookies != "" && !strings.EqualFold(client, "android") {
		args = append(args, "--cookies", cookies)
	}
	if a.Cfg.YtDlpProxy != "" {
		args = append(args, "--proxy", a.Cfg.YtDlpProxy)
	}

	extractorArgs := mergeYouTubeExtractorArgs(a.Cfg.YtDlpYouTubeExtractorArgs, client)
	if extractorArgs != "" {
		args = append(args, "--extractor-args", extractorArgs)
	}

	if audioOnly {
		args = append(args, "-x", "--audio-format", "mp3", "--audio-quality", "0")
	} else {
		args = append(args, "-f", "bv*[height<=480]+ba/best[height<=480]/b[height<=480]/best", "--merge-output-format", "mp4")
	}

	args = append(args, url)

	cmd := exec.Command(a.Cfg.YtDlpPath, args...)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	runErr := cmd.Run()

	logOutput := outBuf.String() + "\n" + errBuf.String()
	files := collectFiles(tmpDir)

	if runErr != nil && len(files) == 0 {
		return nil, logOutput, fmt.Errorf("yt-dlp failed: %w", runErr)
	}
	return files, logOutput, nil
}

func parseCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func removeFromCSV(csv string, value string) string {
	parts := parseCSV(csv)
	var out []string
	for _, p := range parts {
		if strings.EqualFold(p, value) {
			continue
		}
		out = append(out, p)
	}
	return strings.Join(out, ",")
}

func mergeYouTubeExtractorArgs(base string, client string) string {
	base = strings.TrimSpace(base)
	if base == "" {
		return fmt.Sprintf("youtube:player_client=%s", client)
	}

	parts := strings.Split(base, ";")
	var hasYouTube bool
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
		if strings.HasPrefix(parts[i], "youtube:") {
			hasYouTube = true
			parts[i] = replaceOrAppendKey(parts[i], "player_client", client)
		}
	}
	if !hasYouTube {
		parts = append(parts, fmt.Sprintf("youtube:player_client=%s", client))
	}

	sort.Strings(parts)
	return strings.Join(parts, ";")
}

func replaceOrAppendKey(section string, key string, value string) string {
	prefix := "youtube:"
	if !strings.HasPrefix(section, prefix) {
		return section
	}
	payload := strings.TrimSpace(strings.TrimPrefix(section, prefix))
	if payload == "" {
		return prefix + fmt.Sprintf("%s=%s", key, value)
	}

	kvs := strings.Split(payload, ",")
	found := false
	for i := range kvs {
		kv := strings.TrimSpace(kvs[i])
		if strings.HasPrefix(kv, key+"=") {
			kvs[i] = fmt.Sprintf("%s=%s", key, value)
			found = true
		}
	}
	if !found {
		kvs = append(kvs, fmt.Sprintf("%s=%s", key, value))
	}
	sort.Strings(kvs)
	return prefix + strings.Join(kvs, ",")
}

func isYouTubeSignInBotCheck(logs string) bool {
	l := strings.ToLower(logs)
	return strings.Contains(l, "sign in to confirm you") || strings.Contains(l, "confirm you’re not a bot") || strings.Contains(l, "confirm you're not a bot")
}

func shouldRetryWithNextClient(logs string, err error) bool {
	l := strings.ToLower(logs)
	if strings.Contains(l, "http error 403") {
		return true
	}
	if strings.Contains(l, "forcing sabr") || strings.Contains(l, "missing a url") {
		return true
	}
	if strings.Contains(l, "po token") {
		return true
	}
	if strings.Contains(l, "unable to download video data") {
		return true
	}
	if strings.Contains(l, "handshake operation timed out") || strings.Contains(l, "timed out") {
		return true
	}
	if err != nil && strings.TrimSpace(logs) == "" {
		return true
	}
	return false
}

// ---------- Sending ----------

func (a *App) sendAsDocument(chatID int64, path string) {
	outPath := path

	if isVideo(path) && a.Cfg.FfmpegPath != "" {
		normalized, err := normalizeVideoForTelegram(a.Cfg.FfmpegPath, path)
		if err == nil {
			outPath = normalized
		} else {
			log.Printf("ffmpeg normalize failed for %s: %v (sending original)", path, err)
		}
	}

	info, statErr := os.Stat(outPath)
	if statErr == nil && info.Size() > telegramUploadLimit {
		a.sendPublicDownloadLink(chatID, outPath, info.Size())
		return
	}

	cfg := tgbotapi.NewDocument(chatID, tgbotapi.FilePath(outPath))
	cfg.ReplyMarkup = defaultKeyboard()
	if _, err := a.Bot.Send(cfg); err != nil {
		log.Printf("failed to send document %s: %v", outPath, err)
		if statErr == nil {
			a.sendPublicDownloadLink(chatID, outPath, info.Size())
		}
	}
}

func (a *App) sendPublicDownloadLink(chatID int64, path string, size int64) {
	link, err := storePublicDownload(a.Cfg.DownloadStorageDir, a.Cfg.DownloadPublicBaseURL, path)
	if err != nil {
		log.Printf("failed to store public download %s: %v", path, err)
		resp := tgbotapi.NewMessage(chatID, "Файл больше лимита Telegram, и не удалось положить его на сервер 😿")
		resp.ReplyMarkup = defaultKeyboard()
		_, _ = a.Bot.Send(resp)
		return
	}

	cleanupDownloadStorage(a.Cfg.DownloadStorageDir, a.Cfg.DownloadRetention, a.Cfg.DownloadMaxStorageSize)

	resp := tgbotapi.NewMessage(chatID, fmt.Sprintf("Файл больше лимита Telegram (%s). Ссылка на скачивание:\n%s", humanBytes(size), link))
	resp.ReplyMarkup = defaultKeyboard()
	_, _ = a.Bot.Send(resp)
}

func normalizeVideoForTelegram(ffmpegPath, inPath string) (string, error) {
	if strings.HasSuffix(inPath, "_tg.mp4") {
		return inPath, nil
	}
	dir := filepath.Dir(inPath)
	base := strings.TrimSuffix(filepath.Base(inPath), filepath.Ext(inPath))
	outPath := filepath.Join(dir, base+"_tg.mp4")

	args := []string{
		"-y",
		"-i", inPath,
		"-c:v", "libx264",
		"-preset", "veryfast",
		"-vf", "setsar=1",
		"-pix_fmt", "yuv420p",
		"-c:a", "aac",
		"-b:a", "128k",
		"-movflags", "+faststart",
		outPath,
	}

	cmd := exec.Command(ffmpegPath, args...)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("ffmpeg failed: %w", err)
	}
	return outPath, nil
}

// ---------- Public download storage ----------

func startDownloadCleanupLoop(storageDir string, retention time.Duration, maxStorageSize int64) {
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()

	for range ticker.C {
		cleanupDownloadStorage(storageDir, retention, maxStorageSize)
	}
}

func startDownloadHTTPServer(addr string, storageDir string) {
	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.Dir(storageDir)))

	server := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	log.Printf("download HTTP server listening on %s, dir=%s", addr, storageDir)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Printf("download HTTP server error: %v", err)
	}
}

type downloadFileInfo struct {
	path    string
	size    int64
	modTime time.Time
}

func cleanupDownloadStorage(storageDir string, retention time.Duration, maxStorageSize int64) {
	if strings.TrimSpace(storageDir) == "" {
		return
	}

	now := time.Now()
	var files []downloadFileInfo
	var total int64

	_ = filepath.WalkDir(storageDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if retention > 0 && now.Sub(info.ModTime()) > retention {
			if err := os.Remove(path); err != nil {
				log.Printf("failed to remove expired download %s: %v", path, err)
			} else {
				log.Printf("removed expired download %s", path)
			}
			return nil
		}
		files = append(files, downloadFileInfo{path: path, size: info.Size(), modTime: info.ModTime()})
		total += info.Size()
		return nil
	})

	if maxStorageSize > 0 && total > maxStorageSize {
		sort.Slice(files, func(i, j int) bool {
			return files[i].modTime.Before(files[j].modTime)
		})
		for _, f := range files {
			if total <= maxStorageSize {
				break
			}
			if err := os.Remove(f.path); err != nil {
				log.Printf("failed to remove old download %s: %v", f.path, err)
				continue
			}
			total -= f.size
			log.Printf("removed old download %s to keep storage under %s", f.path, humanBytes(maxStorageSize))
		}
	}

	removeEmptyDirs(storageDir)
}

func removeEmptyDirs(root string) {
	var dirs []string
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() && path != root {
			dirs = append(dirs, path)
		}
		return nil
	})
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i]) > len(dirs[j]) })
	for _, dir := range dirs {
		_ = os.Remove(dir)
	}
}

func storePublicDownload(storageDir string, publicBaseURL string, srcPath string) (string, error) {
	if strings.TrimSpace(publicBaseURL) == "" {
		return "", fmt.Errorf("DOWNLOAD_PUBLIC_BASE_URL is not set")
	}

	token, err := randomHex(16)
	if err != nil {
		return "", err
	}

	fileName := safeDownloadFileName(srcPath)
	dstDir := filepath.Join(storageDir, token)
	if err := os.MkdirAll(dstDir, 0755); err != nil {
		return "", err
	}

	dstPath := filepath.Join(dstDir, fileName)
	if err := copyFile(srcPath, dstPath); err != nil {
		return "", err
	}
	if err := os.Chmod(dstPath, 0644); err != nil {
		return "", err
	}

	return strings.TrimRight(publicBaseURL, "/") + "/" + url.PathEscape(token) + "/" + url.PathEscape(fileName), nil
}

func safeDownloadFileName(path string) string {
	base := filepath.Base(path)
	ext := filepath.Ext(base)
	name := strings.TrimSuffix(base, ext)
	name = strings.TrimSuffix(name, "_tg")
	name = strings.TrimSuffix(name, "-tg")
	name = strings.TrimSuffix(name, ".tg")
	name = normalizeFileNamePart(name)
	if name == "" {
		name = "download"
	}
	ext = normalizeFileExt(ext)
	return name + ext
}

func normalizeFileNamePart(s string) string {
	s = strings.TrimSpace(s)
	var b strings.Builder
	prevUnderscore := false
	runeCount := 0
	for _, r := range s {
		var out rune
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			out = r
		case r == ' ' || r == '_' || r == '-' || r == '.' || r == ',' || r == ':' || r == ';' || r == '(' || r == ')' || r == '[' || r == ']':
			out = '_'
		default:
			out = '_'
		}
		if out == '_' {
			if prevUnderscore {
				continue
			}
			prevUnderscore = true
		} else {
			prevUnderscore = false
		}
		b.WriteRune(out)
		runeCount++
		if runeCount >= 160 {
			break
		}
	}
	return strings.Trim(b.String(), "_.-")
}

func normalizeFileExt(ext string) string {
	ext = strings.ToLower(strings.TrimSpace(ext))
	if ext == "" {
		return ""
	}
	var b strings.Builder
	b.WriteByte('.')
	for _, r := range strings.TrimPrefix(ext, ".") {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	if b.Len() == 1 {
		return ""
	}
	return b.String()
}

func copyFile(src string, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}

	_, copyErr := io.Copy(out, in)
	syncErr := out.Sync()
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}

func randomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// ---------- Utils ----------

func collectFiles(dir string) []string {
	var files []string
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if strings.HasSuffix(d.Name(), ".part") {
			return nil
		}
		files = append(files, path)
		return nil
	})
	sort.Strings(files)
	return files
}

func isInstagramURL(u string) bool {
	l := strings.ToLower(u)
	return strings.Contains(l, "instagram.com") || strings.Contains(l, "instagr.am")
}

func isYouTubeURL(u string) bool {
	l := strings.ToLower(u)
	return strings.Contains(l, "youtube.com") || strings.Contains(l, "youtu.be")
}

func isVideo(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".mp4", ".mov", ".m4v", ".webm":
		return true
	default:
		return false
	}
}

func formatUserError(prefix string, err error, logs string) string {
	l := strings.ToLower(logs)
	switch {
	case strings.Contains(l, "sign in to confirm you"):
		return prefix + "YouTube просит подтверждение (anti-bot). Нужны YouTube cookies (Netscape) для скачивания видео."
	case strings.Contains(l, "only images are available") || strings.Contains(l, "signature solving failed"):
		return prefix + "Похоже, YouTube включил JS-challenge. Для видео обычно помогает YTDLP_REMOTE_COMPONENTS=ejs:github и JS runtime (deno/node)."
	case strings.Contains(l, "http error 403"):
		return prefix + "YouTube отвечает 403. Попробовал разные клиенты — не вышло 😿"
	case strings.Contains(l, "429 too many requests"):
		return prefix + "429 Too Many Requests. Подожди и попробуй позже."
	case err != nil && strings.Contains(err.Error(), "youtube bot-check"):
		return prefix + "YouTube anti-bot: нужны cookies."
	default:
		return prefix + fmt.Sprintf("%v", err)
	}
}
