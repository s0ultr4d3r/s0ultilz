package main

import (
	"bytes"
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/joho/godotenv"
)

// ---------- Types ----------

type Config struct {
	TelegramToken string

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

	bot, err := tgbotapi.NewBotAPI(cfg.TelegramToken)
	if err != nil {
		log.Fatalf("failed to create bot: %v", err)
	}
	bot.Debug = false

	log.Printf("Authorized on account %s", bot.Self.UserName)
	log.Printf("Config: yt-dlp=%s, proxy=%s, js-runtimes=%q, remote-components=%q, cache-dir=%q, yt_force_ipv4=%v, yt_extractor_args_base=%q, yt_cookies=%q, yt_clients_audio=%q, yt_clients_video=%q",
		cfg.YtDlpPath, cfg.YtDlpProxy, cfg.YtDlpJsRuntimes, cfg.YtDlpRemoteComponents, cfg.YtDlpCacheDir, cfg.YtDlpForceIPv4, cfg.YtDlpYouTubeExtractorArgs, cfg.YtDlpYouTubeCookiesFile, cfg.YouTubeClientsAudio, cfg.YouTubeClientsVideo)

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

	cfg := &Config{
		TelegramToken: token,

		YtDlpPath:             ytPath,
		YtDlpCookiesFile:      strings.TrimSpace(os.Getenv("YTDLP_COOKIES_FILE")),
		YtDlpProxy:            strings.TrimSpace(os.Getenv("YTDLP_PROXY")),
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

	if !audioOnly {
		// enforce Telegram limit
		var filtered []string
		for _, fp := range files {
			info, err := os.Stat(fp)
			if err != nil {
				continue
			}
			if info.Size() <= telegramUploadLimit {
				filtered = append(filtered, fp)
			}
		}
		if len(filtered) == 0 {
			resp := tgbotapi.NewMessage(chatID, "Видео слишком большое для Telegram (~50MB). Попробуй /ytmp3.")
			resp.ReplyMarkup = defaultKeyboard()
			_, _ = a.Bot.Send(resp)
			_ = os.RemoveAll(filepath.Dir(files[0]))
			return
		}
		files = filtered
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
		// For video, avoid android client when cookies are configured (yt-dlp will skip it anyway).
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

		// If YouTube demands "Sign in to confirm you're not a bot" => cookies are needed; do not spam retries.
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
	args := []string{"--no-playlist", "--restrict-filenames", "-o", filepath.Join(tmpDir, "%(title)s.%(ext)s")}

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

	// Prefer YouTube cookies file if set (important!)
	cookies := strings.TrimSpace(a.Cfg.YtDlpYouTubeCookiesFile)
	if cookies == "" {
		cookies = strings.TrimSpace(a.Cfg.YtDlpCookiesFile)
	}
	// Note: yt-dlp currently doesn't support cookies for the android client; it will be skipped.
	// For audio downloads we still want to allow android client attempts, so we simply don't pass cookies there.
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
		// keep small for Telegram
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

	cfg := tgbotapi.NewDocument(chatID, tgbotapi.FilePath(outPath))
	cfg.ReplyMarkup = defaultKeyboard()
	if _, err := a.Bot.Send(cfg); err != nil {
		log.Printf("failed to send document %s: %v", outPath, err)
	}
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
