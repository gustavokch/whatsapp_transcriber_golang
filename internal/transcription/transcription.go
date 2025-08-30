package transcription

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.mau.fi/whatsmeow"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types/events"
	"go.uber.org/zap"
)

// DownloadableMessage is an interface that represents a message that can be downloaded.
type DownloadableMessage interface {
	GetMediaKey() []byte
	GetFileEncSha256() []byte
	GetFileSha256() []byte
	GetFileLength() uint64
	GetURL() string
	GetDirectPath() string
	GetMimeType() string
	GetFileName() string
}

// Transcriber defines the interface for transcription services.
type Transcriber interface {
	TranscribeAudio(ctx context.Context, audioFilePath string, language string) (string, error)
}

// Job handles the transcription of a single audio message.
type Job struct {
	Client      *whatsmeow.Client
	Message     *events.Message
	Logger      *zap.Logger
	Transcriber Transcriber
	Language    string
}

// NewJob creates a new TranscriptionJob.
func NewJob(cli *whatsmeow.Client, msg *events.Message, logger *zap.Logger, transcriber Transcriber, lang string) *Job {
	return &Job{
		Client:      cli,
		Message:     msg,
		Logger:      logger,
		Transcriber: transcriber,
		Language:    lang,
	}
}

// getContentType returns MIME type based on file extension
func getContentType(filename string) string {
	switch {
	case strings.HasSuffix(filename, ".wav"):
		return "audio/wav"
	case strings.HasSuffix(filename, ".mp3"):
		return "audio/mpeg"
	case strings.HasSuffix(filename, ".ogg"):
		return "audio/ogg"
	case strings.HasSuffix(filename, ".opus"):
		return "audio/opus"
	case strings.HasSuffix(filename, ".flac"):
		return "audio/flac"
	default:
		return "application/octet-stream"
	}
}

// HandleAudioMessage orchestrates the audio processing workflow.
func (j *Job) HandleAudioMessage(ctx context.Context) {
	// Add diagnostic logging to identify nil pointer sources
	if j == nil {
		panic("Job is nil")
	}
	if j.Message == nil {
		panic("Message is nil")
	}
	if j.Client == nil {
		panic("Client is nil")
	}
	if j.Transcriber == nil {
		panic("Transcriber is nil")
	}
	if j.Logger == nil {
		panic("Logger is nil")
	}

	// Check context
	if ctx == nil {
		panic("Context is nil")
	}

	v := j.Message.Info
	j.Logger.Info("Starting audio message processing",
		zap.String("from", v.Sender.User),
		zap.String("job_id", fmt.Sprintf("%p", j)))

	var downloadable whatsmeow.DownloadableMessage
	if j.Message.Message.GetAudioMessage() != nil {
		downloadable = j.Message.Message.GetAudioMessage()
	} else if j.Message.Message.GetDocumentMessage() != nil {
		downloadable = j.Message.Message.GetDocumentMessage()
	} else if j.Message.Message.GetVideoMessage() != nil {
		downloadable = j.Message.Message.GetVideoMessage()
	} else if j.Message.Message.GetImageMessage() != nil {
		downloadable = j.Message.Message.GetImageMessage()
	} else {
		j.Logger.Error("Message is not a downloadable type", zap.String("from", v.Sender.User))
		return
	}

	// Download media
	data, err := j.Client.Download(ctx, downloadable)
	if err != nil {
		j.Logger.Error("Failed to download audio", zap.Error(err), zap.String("from", v.Sender.User))
		// j.replyWithError(ctx, "Failed to download audio.")
		return
	}

	// Save to temporary file
	tempDir := "messages" // Directory to save temporary audio files
	if err := os.MkdirAll(tempDir, 0755); err != nil {
		j.Logger.Error("Failed to create temporary directory", zap.String("path", tempDir), zap.Error(err))
		// j.replyWithError(ctx, "Internal server error: could not create temp directory.")
		return
	}

	tempFileName := filepath.Join(tempDir, fmt.Sprintf("%s-%s.ogg", uuid.New().String(), time.Now().Format("20060102150405")))
	err = os.WriteFile(tempFileName, data, 0644)
	if err != nil {
		j.Logger.Error("Failed to save audio to temporary file", zap.Error(err), zap.String("path", tempFileName))
		// j.replyWithError(ctx, "Internal server error: could not save audio.")
		return
	}
	defer func() {
		if err := os.Remove(tempFileName); err != nil {
			j.Logger.Error("Failed to delete temporary audio file", zap.Error(err), zap.String("path", tempFileName))
		} else {
			j.Logger.Info("Deleted temporary audio file", zap.String("path", tempFileName))
		}
	}()

	j.Logger.Info("Audio saved to temporary file", zap.String("path", tempFileName))

	// Transcribe audio
	j.Logger.Debug("About to call TranscribeAudio",
		zap.String("temp_file", tempFileName),
		zap.String("language", j.Language))

	transcribedText, err := j.Transcriber.TranscribeAudio(ctx, tempFileName, j.Language)
	if err != nil {
		j.Logger.Error("Failed to transcribe audio",
			zap.Error(err),
			zap.String("from", v.Sender.User),
			zap.String("temp_file", tempFileName))
		// j.replyWithError(ctx, "Failed to transcribe audio. Please try again later.")
		return
	}

	j.Logger.Debug("Transcription successful",
		zap.String("from", v.Sender.User),
		zap.String("transcribed_text", transcribedText[:min(100, len(transcribedText))]))

	// Reply with transcribed text
	if len(transcribedText) > 5 {
		j.replyWithText(ctx, transcribedText)
		j.Logger.Info("Successfully transcribed and replied", zap.String("from", v.Sender.User))
	}
}

func (j *Job) replyWithText(ctx context.Context, text string) {
	// Format the message with prefix in bold and transcription in italics
	// Trim whitespace to ensure proper WhatsApp formatting
	trimmedText := strings.TrimSpace(text)
	if len(trimmedText) > 5 {
		formattedText := fmt.Sprintf("*Transcrição automática:* _%s_", trimmedText)
		_, err := j.Client.SendMessage(ctx, j.Message.Info.Chat, &waE2E.Message{
			Conversation: &formattedText,
		})
		if err != nil {
			j.Logger.Error("Failed to send reply message", zap.Error(err), zap.String("to", j.Message.Info.Chat.String()))
		}
	}
}

func (j *Job) replyWithError(ctx context.Context, errorMessage string) {
	_, err := j.Client.SendMessage(ctx, j.Message.Info.Chat, &waE2E.Message{
		Conversation: &errorMessage,
	})
	if err != nil {
		j.Logger.Error("Failed to send error reply message", zap.Error(err), zap.String("to", j.Message.Info.Chat.String()))
	}
}
