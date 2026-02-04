package main

import (
	"bytes"
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/joho/godotenv"
)

// Config holds application configuration loaded from environment variables.
type Config struct {
	TelegramToken        string
	YtDlpPath            string
	YtDlpCookiesFile     string
	GalleryDlPath        string
	GalleryDlCookiesFile string
	FfmpegPath           string
}

// PendingAction indicates what the bot is waiting for from this chat.
type PendingAction int

const (
	PendingNone PendingAction = iota
	PendingInstLink
	PendingStoriesUsername
	PendingYtVideoLink
	PendingYtAudioLink
)

// запас чуть меньше официального лимита
const telegramUploadLimit = 48 * 1024 * 1024 // ~48MB

// App holds global application state.
type App struct {
	Bot     *tgbotapi.BotAPI
	Cfg     *Config
	mu      sync.Mutex
	pending map[int64]PendingAction
}

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
		return nil, fmt.Errorf("TELEGRAM_BOT_TOKEN is not set (put it into .env or environment)")
	}

	ytPath := strings.TrimSpace(os.Getenv("YTDLP_PATH"))
	if ytPath == "" {
		ytPath = "yt-dlp"
	}

	galPath := strings.TrimSpace(os.Getenv("GALLERYDL_PATH"))
	if galPath == "" {
		galPath = "gallery-dl"
	}

	cfg := &Config{
		TelegramToken:        token,
		YtDlpPath:            ytPath,
		YtDlpCookiesFile:     strings.TrimSpace(os.Getenv("YTDLP_COOKIES_FILE")),
		GalleryDlPath:        galPath,
		GalleryDlCookiesFile: strings.TrimSpace(os.Getenv("GALLERYDL_COOKIES_FILE")),
		FfmpegPath:           strings.TrimSpace(os.Getenv("FFMPEG_PATH")),
	}
	return cfg, nil
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
	text := "Я могу скачивать контент:\n" +
		"• из Instagram посты / рилсы / сторис\n" +
		"• из Instagram сторис по нику\n" +
		"• видео с YouTube\n" +
		"• аудио MP3 с YouTube\n\n" +
		"Кнопки снизу:\n" +
		"• /inst — пост/риилс/сторис по ссылке\n" +
		"• /inststories — все актуальные сторис по нику\n" +
		"• /yt — видео с YouTube\n" +
		"• /ytmp3 — MP3 с YouTube"

	resp := tgbotapi.NewMessage(msg.Chat.ID, text)
	resp.ReplyMarkup = defaultKeyboard()
	_, _ = a.Bot.Send(resp)
}

// /inst
func (a *App) handleInstCommand(msg *tgbotapi.Message) {
	args := strings.TrimSpace(msg.CommandArguments())
	if args == "" {
		a.setPending(msg.Chat.ID, PendingInstLink)
		resp := tgbotapi.NewMessage(msg.Chat.ID, "Пришли ссылку на пост / риилс / сторис в Instagram.")
		resp.ReplyMarkup = defaultKeyboard()
		_, _ = a.Bot.Send(resp)
		return
	}

	url := strings.Fields(args)[0]
	if !isInstagramURL(url) {
		resp := tgbotapi.NewMessage(msg.Chat.ID, "Похоже, это не ссылка на Instagram.\nПример: https://www.instagram.com/p/XXXXXXXX/")
		resp.ReplyMarkup = defaultKeyboard()
		_, _ = a.Bot.Send(resp)
		return
	}

	a.setPending(msg.Chat.ID, PendingNone)
	a.processInstURL(msg.Chat.ID, url)
}

// /inststories
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

// /yt — YouTube видео
func (a *App) handleYtCommand(msg *tgbotapi.Message) {
	args := strings.TrimSpace(msg.CommandArguments())
	if args == "" {
		a.setPending(msg.Chat.ID, PendingYtVideoLink)
		resp := tgbotapi.NewMessage(msg.Chat.ID, "Пришли ссылку на видео YouTube.")
		resp.ReplyMarkup = defaultKeyboard()
		_, _ = a.Bot.Send(resp)
		return
	}

	url := strings.Fields(args)[0]
	if !isYouTubeURL(url) {
		resp := tgbotapi.NewMessage(msg.Chat.ID, "Похоже, это не ссылка на YouTube.\nПример: https://www.youtube.com/watch?v=...")
		resp.ReplyMarkup = defaultKeyboard()
		_, _ = a.Bot.Send(resp)
		return
	}

	a.setPending(msg.Chat.ID, PendingNone)
	a.processYtURL(msg.Chat.ID, url, false)
}

// /ytmp3 — YouTube MP3
func (a *App) handleYtMp3Command(msg *tgbotapi.Message) {
	args := strings.TrimSpace(msg.CommandArguments())
	if args == "" {
		a.setPending(msg.Chat.ID, PendingYtAudioLink)
		resp := tgbotapi.NewMessage(msg.Chat.ID, "Пришли ссылку на видео YouTube, из которого сделать MP3.")
		resp.ReplyMarkup = defaultKeyboard()
		_, _ = a.Bot.Send(resp)
		return
	}

	url := strings.Fields(args)[0]
	if !isYouTubeURL(url) {
		resp := tgbotapi.NewMessage(msg.Chat.ID, "Похоже, это не ссылка на YouTube.\nПример: https://www.youtube.com/watch?v=...")
		resp.ReplyMarkup = defaultKeyboard()
		_, _ = a.Bot.Send(resp)
		return
	}

	a.setPending(msg.Chat.ID, PendingNone)
	a.processYtURL(msg.Chat.ID, url, true)
}

// ---------- Non-command handler ----------

func (a *App) handleNonCommandMessage(msg *tgbotapi.Message) {
	state := a.getPending(msg.Chat.ID)
	text := strings.TrimSpace(msg.Text)

	switch state {

	case PendingInstLink:
		parts := strings.Fields(text)
		var url string
		for _, p := range parts {
			if isInstagramURL(p) {
				url = p
				break
			}
		}
		if url == "" {
			resp := tgbotapi.NewMessage(msg.Chat.ID, "Не вижу в сообщении ссылки на Instagram, пришли URL.")
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
		parts := strings.Fields(text)
		var url string
		for _, p := range parts {
			if isYouTubeURL(p) {
				url = p
				break
			}
		}
		if url == "" {
			resp := tgbotapi.NewMessage(msg.Chat.ID, "Не вижу в сообщении ссылки на YouTube, пришли URL.")
			resp.ReplyMarkup = defaultKeyboard()
			_, _ = a.Bot.Send(resp)
			return
		}
		a.setPending(msg.Chat.ID, PendingNone)
		a.processYtURL(msg.Chat.ID, url, false)
		return

	case PendingYtAudioLink:
		parts := strings.Fields(text)
		var url string
		for _, p := range parts {
			if isYouTubeURL(p) {
				url = p
				break
			}
		}
		if url == "" {
			resp := tgbotapi.NewMessage(msg.Chat.ID, "Не вижу в сообщении ссылки на YouTube, пришли URL.")
			resp.ReplyMarkup = defaultKeyboard()
			_, _ = a.Bot.Send(resp)
			return
		}
		a.setPending(msg.Chat.ID, PendingNone)
		a.processYtURL(msg.Chat.ID, url, true)
		return
	}

	// No pending state: игнорируем произвольный текст
}

// ---------- High-level actions ----------

func (a *App) processInstURL(chatID int64, url string) {
	files, logOutput, err := a.downloadInstagram(url)
	if err != nil {
		log.Printf("downloadInstagram error: %v\nlogs:\n%s", err, logOutput)

		userMsg := formatUserError("Не удалось скачать: ", err, logOutput)
		resp := tgbotapi.NewMessage(chatID, userMsg)
		resp.ReplyMarkup = defaultKeyboard()
		_, _ = a.Bot.Send(resp)
		return
	}
	if len(files) == 0 {
		resp := tgbotapi.NewMessage(chatID,
			"Ни один бэкенд не смог скачать ни одного файла. Возможно, формат поста поменялся или он пустой.")
		resp.ReplyMarkup = defaultKeyboard()
		_, _ = a.Bot.Send(resp)
		return
	}

	for _, fp := range files {
		a.sendMediaFile(chatID, fp)
	}

	if len(files) > 0 {
		tmpDir := filepath.Dir(files[0])
		_ = os.RemoveAll(tmpDir)
	}
}

func (a *App) processStoriesUsername(chatID int64, rawUsername string) {
	username := strings.TrimSpace(rawUsername)
	username = strings.TrimPrefix(username, "@")

	if username == "" || strings.ContainsAny(username, " /?&") {
		resp := tgbotapi.NewMessage(chatID, "Кажется, это не похоже на валидный username.\nПример: instagram")
		resp.ReplyMarkup = defaultKeyboard()
		_, _ = a.Bot.Send(resp)
		return
	}

	files, logOutput, err := a.downloadInstagramStories(username)
	if err != nil {
		log.Printf("downloadInstagramStories error: %v\nlogs:\n%s", err, logOutput)

		prefix := fmt.Sprintf("Не удалось скачать сторис @%s: ", username)
		userMsg := formatUserError(prefix, err, logOutput)
		resp := tgbotapi.NewMessage(chatID, userMsg)
		resp.ReplyMarkup = defaultKeyboard()
		_, _ = a.Bot.Send(resp)
		return
	}
	if len(files) == 0 {
		resp := tgbotapi.NewMessage(chatID,
			fmt.Sprintf("Сторис у @%s не найдено (или не получилось их скачать).", username))
		resp.ReplyMarkup = defaultKeyboard()
		_, _ = a.Bot.Send(resp)
		return
	}

	for _, fp := range files {
		a.sendMediaFile(chatID, fp)
	}

	if len(files) > 0 {
		tmpDir := filepath.Dir(files[0])
		_ = os.RemoveAll(tmpDir)
	}
}

// YouTube: audioOnly=false => видео, true => mp3
func (a *App) processYtURL(chatID int64, url string, audioOnly bool) {
	files, logOutput, err := a.downloadYouTube(url, audioOnly)
	if err != nil {
		log.Printf("downloadYouTube error (audioOnly=%v): %v\nlogs:\n%s", audioOnly, err, logOutput)
		prefix := "Не удалось скачать с YouTube: "
		userMsg := formatUserError(prefix, err, logOutput)
		resp := tgbotapi.NewMessage(chatID, userMsg)
		resp.ReplyMarkup = defaultKeyboard()
		_, _ = a.Bot.Send(resp)
		return
	}
	if len(files) == 0 {
		text := "Не получилось сохранить файл с YouTube. Возможно, видео недоступно или формат не поддерживается."
		resp := tgbotapi.NewMessage(chatID, text)
		resp.ReplyMarkup = defaultKeyboard()
		_, _ = a.Bot.Send(resp)
		return
	}

	// Для YouTube видео проверяем лимит Telegram
	if !audioOnly {
		var filtered []string
		for _, fp := range files {
			info, err := os.Stat(fp)
			if err != nil {
				log.Printf("stat failed for %s: %v", fp, err)
				continue
			}
			if info.Size() > telegramUploadLimit {
				log.Printf("file %s (size=%d) exceeds Telegram upload limit, skipping", fp, info.Size())
				continue
			}
			filtered = append(filtered, fp)
		}

		if len(filtered) == 0 {
			resp := tgbotapi.NewMessage(chatID,
				"Видео скачалось, но размер файла больше лимита Telegram (~50 МБ). Попробуй более короткий ролик или /ytmp3.")
			resp.ReplyMarkup = defaultKeyboard()
			_, _ = a.Bot.Send(resp)

			if len(files) > 0 {
				tmpDir := filepath.Dir(files[0])
				_ = os.RemoveAll(tmpDir)
			}
			return
		}
		files = filtered
	}

	for _, fp := range files {
		a.sendMediaFile(chatID, fp)
	}

	if len(files) > 0 {
		tmpDir := filepath.Dir(files[0])
		_ = os.RemoveAll(tmpDir)
	}
}

// ---------- Instagram download logic ----------

func (a *App) downloadInstagram(url string) ([]string, string, error) {
	var allLogs strings.Builder

	files1, log1, err1 := a.downloadWithYtDlp(url)
	allLogs.WriteString("[yt-dlp]\n")
	allLogs.WriteString(log1)
	allLogs.WriteString("\n\n")

	if len(files1) > 0 {
		if err1 != nil {
			log.Printf("yt-dlp exited with error but produced %d file(s); using them", len(files1))
		}
		return files1, allLogs.String(), nil
	}

	log.Printf("yt-dlp did not produce files (err=%v), trying gallery-dl", err1)

	files2, log2, err2 := a.downloadWithGalleryDl(url)
	allLogs.WriteString("[gallery-dl]\n")
	allLogs.WriteString(log2)
	allLogs.WriteString("\n")

	if len(files2) > 0 {
		if err2 != nil {
			log.Printf("gallery-dl exited with error but produced %d file(s); using them", len(files2))
		}
		return files2, allLogs.String(), nil
	}

	if err2 != nil {
		return nil, allLogs.String(), fmt.Errorf("yt-dlp+gallery-dl both failed: %w", err2)
	}
	return nil, allLogs.String(), fmt.Errorf("yt-dlp+gallery-dl produced no files")
}

func (a *App) downloadInstagramStories(username string) ([]string, string, error) {
	profileURL := fmt.Sprintf("https://www.instagram.com/%s/", username)
	extraArgs := []string{"-o", "include=stories"}

	files, logOutput, err := a.runGalleryDl(profileURL, extraArgs)
	if len(files) == 0 {
		if err != nil {
			return nil, logOutput, fmt.Errorf("gallery-dl failed: %w", err)
		}
		return nil, logOutput, fmt.Errorf("gallery-dl produced no files for stories")
	}
	return files, logOutput, nil
}

func (a *App) downloadWithYtDlp(url string) ([]string, string, error) {
	tmpDir1, err := os.MkdirTemp("", "s0ultilz_yt_1_*")
	if err != nil {
		return nil, "", fmt.Errorf("yt-dlp temp dir error: %w", err)
	}

	files1, log1, err1 := a.runYtDlpOnce(url, tmpDir1, false)

	if len(files1) > 0 {
		if err1 != nil {
			log.Printf("yt-dlp (normal) exited with error but produced %d file(s)", len(files1))
		}
		return files1, log1, nil
	}

	if err1 == nil && len(files1) == 0 {
		return nil, log1, fmt.Errorf("yt-dlp (normal) finished without error but produced no files")
	}

	log.Printf("yt-dlp normal extractor failed with no files, trying generic extractor")

	tmpDir2, err := os.MkdirTemp("", "s0ultilz_yt_2_*")
	if err != nil {
		return nil, log1 + "\n[generic] temp dir error: " + err.Error(), err1
	}
	files2, log2, err2 := a.runYtDlpOnce(url, tmpDir2, true)

	combinedLog := log1 + "\n\n[yt-dlp generic attempt]\n" + log2

	if len(files2) > 0 {
		if err2 != nil {
			log.Printf("yt-dlp (generic) exited with error but produced %d file(s)", len(files2))
		}
		return files2, combinedLog, nil
	}

	if err2 != nil {
		return nil, combinedLog, fmt.Errorf("yt-dlp failed (normal+generic): %w", err2)
	}
	return nil, combinedLog, fmt.Errorf("yt-dlp generic extractor produced no files")
}

func (a *App) runYtDlpOnce(url, tmpDir string, forceGeneric bool) ([]string, string, error) {
	args := []string{
		"--no-playlist",
		"--restrict-filenames",
		"-o", filepath.Join(tmpDir, "%(id)s.%(ext)s"),
		url,
	}

	if forceGeneric {
		args = append([]string{"--force-generic-extractor"}, args...)
	}

	if a.Cfg.YtDlpCookiesFile != "" {
		args = append(args, "--cookies", a.Cfg.YtDlpCookiesFile)
	}

	cmd := exec.Command(a.Cfg.YtDlpPath, args...)

	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf

	runErr := cmd.Run()
	logOutput := outBuf.String() + "\n" + errBuf.String()

	var files []string
	_ = filepath.WalkDir(tmpDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if strings.HasSuffix(d.Name(), ".part") {
			return nil
		}
		files = append(files, path)
		return nil
	})

	if runErr != nil && len(files) == 0 {
		return nil, logOutput, fmt.Errorf("yt-dlp failed: %w", runErr)
	}

	if runErr != nil && len(files) > 0 {
		log.Printf("yt-dlp exited with error (%v) but produced %d file(s)", runErr, len(files))
	}

	return files, logOutput, nil
}

// ---------- YouTube download logic (yt-dlp only) ----------

func (a *App) downloadYouTube(url string, audioOnly bool) ([]string, string, error) {
	tmpDir, err := os.MkdirTemp("", "s0ultilz_youtube_*")
	if err != nil {
		return nil, "", fmt.Errorf("yt-dlp temp dir error: %w", err)
	}

	args := []string{
		"--no-playlist",
		"--restrict-filenames",
		"-o", filepath.Join(tmpDir, "%(title)s.%(ext)s"),
	}

	if audioOnly {
		args = append(args,
			"-x",
			"--audio-format", "mp3",
			"--audio-quality", "0",
		)
	} else {
		// ограничиваемся максимум 480p, чтобы файл не раздувался
		args = append(args,
			"-f", "bv*[height<=480]+ba/best[height<=480]/b[height<=480]",
			"--merge-output-format", "mp4",
		)
	}

	args = append(args, url)

	cmd := exec.Command(a.Cfg.YtDlpPath, args...)

	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf

	runErr := cmd.Run()
	logOutput := outBuf.String() + "\n" + errBuf.String()

	var files []string
	_ = filepath.WalkDir(tmpDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if strings.HasSuffix(d.Name(), ".part") {
			return nil
		}
		files = append(files, path)
		return nil
	})

	if runErr != nil && len(files) == 0 {
		return nil, logOutput, fmt.Errorf("yt-dlp failed: %w", runErr)
	}

	if runErr != nil && len(files) > 0 {
		log.Printf("yt-dlp (YouTube, audioOnly=%v) exited with error (%v) but produced %d file(s)", audioOnly, runErr, len(files))
	}

	return files, logOutput, nil
}

// ---------- gallery-dl wrapper ----------

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

	var files []string
	_ = filepath.WalkDir(tmpDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if strings.HasSuffix(d.Name(), ".part") {
			return nil
		}
		files = append(files, path)
		return nil
	})

	if runErr != nil && len(files) == 0 {
		return nil, logOutput, fmt.Errorf("gallery-dl failed: %w", runErr)
	}

	if runErr != nil && len(files) > 0 {
		log.Printf("gallery-dl exited with error (%v) but produced %d file(s)", runErr, len(files))
	}

	return files, logOutput, nil
}

// ---------- Sending ----------

func (a *App) sendMediaFile(chatID int64, path string) {
	outPath := path
	if isVideo(path) && a.Cfg.FfmpegPath != "" {
		normalized, err := normalizeVideoForTelegram(a.Cfg.FfmpegPath, path)
		if err != nil {
			log.Printf("ffmpeg normalize failed for %s: %v, sending original as document", path, err)
		} else {
			outPath = normalized
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

	err := cmd.Run()
	if err != nil {
		log.Printf("ffmpeg error for %s -> %s: %v\nstdout:\n%s\nstderr:\n%s",
			inPath, outPath, err, outBuf.String(), errBuf.String())
		return "", fmt.Errorf("ffmpeg failed: %w", err)
	}

	return outPath, nil
}

// ---------- Utils ----------

func isInstagramURL(u string) bool {
	l := strings.ToLower(u)
	return strings.Contains(l, "instagram.com") || strings.Contains(l, "instagr.am")
}

func isYouTubeURL(u string) bool {
	l := strings.ToLower(u)
	return strings.Contains(l, "youtube.com") || strings.Contains(l, "youtu.be")
}

func formatUserError(prefix string, err error, logs string) string {
	l := strings.ToLower(logs)

	switch {
	case strings.Contains(l, "read timed out"):
		return prefix + "Источник не отдал файл (таймаут CDN/HTTP). Скорее всего, проблема на их стороне или с маршрутом с этого сервера. Попробуй позже или с другого IP."
	case strings.Contains(l, "429 too many requests"):
		return prefix + "Сервис отвечает 429 (слишком много запросов). Подожди немного, уменьшай частоту запросов или попробуй другие cookies/другой IP."
	case strings.Contains(l, "login required"):
		return prefix + "Сервис требует логин/доступ. Проверь, что cookies.txt актуален и аккаунт реально видит нужный контент."
	case strings.Contains(l, "no video formats found"):
		return prefix + "Видео не отдалось (форматы не найдены). Такое бывает на специфичных постах/каруселях. Попробуй обновить yt-dlp или использовать другую ссылку на этот же контент."
	default:
		return prefix + fmt.Sprintf("%v", err)
	}
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
