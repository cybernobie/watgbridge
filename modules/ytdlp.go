package modules

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"watgbridge/state"
	"watgbridge/utils"

	"github.com/PaulSonOfLars/gotgbot/v2"
	"github.com/PaulSonOfLars/gotgbot/v2/ext"
	"github.com/PaulSonOfLars/gotgbot/v2/ext/handlers"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types/events"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"
)

func init() {
	TelegramHandlers[GetNewTelegramHandlerGroup()] = []ext.Handler{
		handlers.NewCommand("ytdlp", ytdlpVideoHandler),
		handlers.NewCommand("ytdlpmp3", ytdlpMp3Handler),
	}

	WhatsAppHandlers = append(WhatsAppHandlers, ytdlpWhatsAppHandler)
}

/* ================= HANDLERS ================= */

func ytdlpVideoHandler(b *gotgbot.Bot, c *ext.Context) error {
	return handleYtDlp(b, c, "video")
}

func ytdlpMp3Handler(b *gotgbot.Bot, c *ext.Context) error {
	return handleYtDlp(b, c, "mp3")
}

func ytdlpWhatsAppHandler(evt interface{}) {
	v, ok := evt.(*events.Message)
	if !ok {
		return
	}

	var text string
	if extendedMessageText := v.Message.GetExtendedTextMessage().GetText(); extendedMessageText != "" {
		text = extendedMessageText
	} else {
		text = v.Message.GetConversation()
	}

	if text == "" {
		return
	}

	parts := strings.Fields(text)
	if len(parts) == 0 {
		return
	}
	command := strings.ToLower(parts[0])

	if command == "/ytdlp" {
		handleYtDlpWhatsApp(v, parts, "video")
	} else if command == "/ytdlpmp3" {
		handleYtDlpWhatsApp(v, parts, "mp3")
	}
}

/* ================= CORE ================= */

func handleYtDlp(b *gotgbot.Bot, c *ext.Context, mode string) error {
	if !utils.TgUpdateIsAuthorized(b, c) {
		return nil
	}

	args := c.Args()
	if len(args) < 2 {
		utils.TgReplyTextByContext(b, c, "Usage: /ytdlp <url> or /ytdlpmp3 <url>", nil, false)
		return nil
	}

	url := args[1]
	cfg := state.State.Config
	logger := state.State.Logger

	statusMsg, _ := utils.TgReplyTextByContext(b, c, "Downloading… 0%", nil, false)

	tempDir := "temp"
	_ = os.MkdirAll(tempDir, 0755)

	base := fmt.Sprintf("ytdlp_%d", c.EffectiveMessage.MessageId)
	outputTemplate := filepath.Join(tempDir, base+".%(ext)s")

	var ytArgs []string

	if mode == "mp3" {
		ytArgs = []string{
			"-x",
			"--audio-format", "mp3",
			"--newline",
			"-o", outputTemplate,
			url,
		}
	} else {
		ytArgs = []string{
			"-f", "bestvideo[ext=mp4]+bestaudio[ext=m4a]/best",
			"--merge-output-format", "mp4",
			"--newline",
			"-o", outputTemplate,
			url,
		}
	}

	cmd := exec.Command(cfg.YtDlpExecutable, ytArgs...)

	stdout, _ := cmd.StdoutPipe()
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		return err
	}

	// ===== Progress reader =====
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.Contains(line, "%") {
				if idx := strings.Index(line, "%"); idx >= 2 {
					p := line[idx-2 : idx+1]
					b.EditMessageText("Downloading… "+p, &gotgbot.EditMessageTextOpts{
						ChatId:    c.EffectiveChat.Id,
						MessageId: statusMsg.MessageId,
					})
				}
			}
		}
	}()

	if err := cmd.Wait(); err != nil {
		logger.Error("yt-dlp failed", zap.Error(err))
		b.EditMessageText(fmt.Sprintf("Download failed.\n%s", stderrBuf.String()), &gotgbot.EditMessageTextOpts{
			ChatId: c.EffectiveChat.Id, MessageId: statusMsg.MessageId,
		})
		return nil
	}

	files, _ := filepath.Glob(filepath.Join(tempDir, base+"*"))
	if len(files) == 0 {
		b.EditMessageText("File not found.", &gotgbot.EditMessageTextOpts{
			ChatId: c.EffectiveChat.Id, MessageId: statusMsg.MessageId,
		})
		return nil
	}

	filePath := files[0]
	defer os.Remove(filePath)

	info, _ := os.Stat(filePath)
	f, _ := os.Open(filePath)
	defer f.Close()

	b.EditMessageText("Uploading…", &gotgbot.EditMessageTextOpts{
		ChatId: c.EffectiveChat.Id, MessageId: statusMsg.MessageId,
	})

	/* ===== MP3 ===== */
	if mode == "mp3" {
		_, err := b.SendAudio(
			c.EffectiveChat.Id,
			&gotgbot.FileReader{
				Name: filepath.Base(filePath),
				Data: f,
			},
			&gotgbot.SendAudioOpts{
				Caption: "Downloaded MP3",
			},
		)
		b.DeleteMessage(c.EffectiveChat.Id, statusMsg.MessageId, nil)
		return err
	}

	/* ===== VIDEO ===== */
	if !cfg.Telegram.SelfHostedAPI && info.Size() > int64(utils.UploadSizeLimit) {
		// 🔥 FALLBACK
		_, _ = b.SendDocument(
			c.EffectiveChat.Id,
			&gotgbot.FileReader{
				Name: filepath.Base(filePath),
				Data: f,
			},
			&gotgbot.SendDocumentOpts{
				Caption: "Video too large – sent as document",
			},
		)
	} else {
		_, _ = b.SendVideo(
			c.EffectiveChat.Id,
			&gotgbot.FileReader{
				Name: filepath.Base(filePath),
				Data: f,
			},
			&gotgbot.SendVideoOpts{
				Caption: "Downloaded via yt-dlp",
			},
		)
	}

	b.DeleteMessage(c.EffectiveChat.Id, statusMsg.MessageId, nil)
	return nil
}

func handleYtDlpWhatsApp(v *events.Message, args []string, mode string) {
	waClient := state.State.WhatsAppClient
	logger := state.State.Logger
	cfg := state.State.Config

	if len(args) < 2 {
		utils.WaSendText(v.Info.Chat, "Usage: /ytdlp <url> or /ytdlpmp3 <url>", v.Info.ID, v.Info.MessageSource.Sender.String(), v.Message, true)
		return
	}
	url := args[1]

	// Send initial message and capture response to get ID for editing
	resp, err := utils.WaSendText(v.Info.Chat, "Downloading... 0%", v.Info.ID, v.Info.MessageSource.Sender.String(), v.Message, true)
	if err != nil {
		logger.Error("Failed to send status message", zap.Error(err))
		return
	}
	statusMsgID := resp.ID

	tempDir := filepath.Join("temp", fmt.Sprintf("wa_%s", v.Info.ID))
	_ = os.MkdirAll(tempDir, 0755)
	defer os.RemoveAll(tempDir)

	// Use %(title)s in filename to capture the title from yt-dlp
	outputTemplate := filepath.Join(tempDir, "%(title)s.%(ext)s")

	var ytArgs []string
	if mode == "mp3" {
		ytArgs = []string{
			"-x",
			"--audio-format", "mp3",
			"--newline",
			"-o", outputTemplate,
			url,
		}
	} else {
		ytArgs = []string{
			"-f", "bestvideo[ext=mp4]+bestaudio[ext=m4a]/best",
			"--merge-output-format", "mp4",
			"--newline",
			"-o", outputTemplate,
			url,
		}
	}

	cmd := exec.Command(cfg.YtDlpExecutable, ytArgs...)

	stdout, _ := cmd.StdoutPipe()
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		logger.Error("yt-dlp failed to start", zap.Error(err))
		utils.WaEditText(v.Info.Chat, statusMsgID, fmt.Sprintf("Download failed to start: %s", err.Error()))
		return
	}

	// ===== Progress reader =====
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.Contains(line, "%") {
				if idx := strings.Index(line, "%"); idx >= 2 {
					p := line[idx-2 : idx+1]
					// Update progress on WhatsApp
					// Note: WhatsApp has rate limits/spam detection, so frequent edits might be risky.
					// However, for single user usage it should be fine.
					utils.WaEditText(v.Info.Chat, statusMsgID, "Downloading... "+p)
				}
			}
		}
	}()

	if err := cmd.Wait(); err != nil {
		logger.Error("yt-dlp failed (WhatsApp)", zap.Error(err))
		utils.WaEditText(v.Info.Chat, statusMsgID, fmt.Sprintf("Download failed: %s\n%s", err.Error(), stderrBuf.String()))
		return
	}

	files, _ := filepath.Glob(filepath.Join(tempDir, "*"))
	if len(files) == 0 {
		utils.WaEditText(v.Info.Chat, statusMsgID, "File not found.")
		return
	}

	filePath := files[0]
	fileName := filepath.Base(filePath)

	fileData, err := os.ReadFile(filePath)
	if err != nil {
		logger.Error("Failed to read file", zap.Error(err))
		utils.WaEditText(v.Info.Chat, statusMsgID, fmt.Sprintf("Failed to read downloaded file: %s", err.Error()))
		return
	}

	utils.WaEditText(v.Info.Chat, statusMsgID, "Uploading...")

	uploadedBytes, err := waClient.Upload(context.Background(), fileData, whatsmeow.MediaDocument)
	if err != nil {
		logger.Error("Failed to upload media to WhatsApp", zap.Error(err))
		utils.WaEditText(v.Info.Chat, statusMsgID, fmt.Sprintf("Failed to upload media: %s", err.Error()))
		return
	}

	mimeType := "video/mp4"
	if mode == "mp3" {
		mimeType = "audio/mpeg"
	}

	msg := &waE2E.Message{
		DocumentMessage: &waE2E.DocumentMessage{
			URL:           proto.String(uploadedBytes.URL),
			DirectPath:    proto.String(uploadedBytes.DirectPath),
			MediaKey:      uploadedBytes.MediaKey,
			Mimetype:      proto.String(mimeType),
			FileEncSHA256: uploadedBytes.FileEncSHA256,
			FileSHA256:    uploadedBytes.FileSHA256,
			FileLength:    proto.Uint64(uint64(len(fileData))),
			FileName:      proto.String(fileName),
			Caption:       proto.String(fileName),
			ContextInfo: &waE2E.ContextInfo{
				StanzaID:      proto.String(v.Info.ID),
				Participant:   proto.String(v.Info.MessageSource.Sender.String()),
				QuotedMessage: v.Message,
			},
		},
	}
	_, err = waClient.SendMessage(context.Background(), v.Info.Chat, msg)

	if err != nil {
		logger.Error("Failed to send message", zap.Error(err))
	} else {
		// Delete the status message after successful upload
		// To delete a message for everyone, we use REVOKE.
		// Constructing a revoke message:
		waClient.SendMessage(context.Background(), v.Info.Chat, &waE2E.Message{
			ProtocolMessage: &waE2E.ProtocolMessage{
				Key: &waCommon.MessageKey{
					FromMe:    proto.Bool(true),
					ID:        proto.String(statusMsgID),
					RemoteJID: proto.String(v.Info.Chat.String()),
				},
				Type: waE2E.ProtocolMessage_REVOKE.Enum(),
			},
		})
		logger.Info("Sent yt-dlp media to WhatsApp", zap.String("file", fileName))
	}
}
