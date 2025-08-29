package transcription

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"go.uber.org/zap"
)

// DeepgramAITranscriber implements the Transcriber interface for Deepgram AI.
type DeepgramAITranscriber struct {
	APIKey string
	Logger *zap.Logger
}

// NewDeepgramAITranscriber creates a new DeepgramAITranscriber.
func NewDeepgramAITranscriber(apiKey string, logger *zap.Logger) *DeepgramAITranscriber {
	return &DeepgramAITranscriber{
		APIKey: apiKey,
		Logger: logger,
	}
}

// TranscribeAudio sends an audio file to Deepgram AI for transcription.
func (d *DeepgramAITranscriber) TranscribeAudio(ctx context.Context, audioFilePath string, language string) (string, error) {
	d.Logger.Debug("Starting Deepgram transcription",
		zap.String("audio_file", audioFilePath),
		zap.String("language", language))

	contentType := getContentType(audioFilePath)
	d.Logger.Debug("Detected content type", zap.String("content_type", contentType))

	apiURL := "https://api.deepgram.com/v1/listen?model=nova-3&smart_format=true"
	if language != "" {
		apiURL += "&language=" + language
	}
	d.Logger.Debug("Using Deepgram API URL", zap.String("url", apiURL))

	audioFile, err := os.Open(audioFilePath)
	if err != nil {
		return "", fmt.Errorf("failed to open audio file: %w", err)
	}
	defer audioFile.Close()

	req, err := http.NewRequestWithContext(ctx, "POST", apiURL, audioFile)
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Authorization", "Token "+d.APIKey)
	req.Header.Set("Content-Type", contentType)

	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to send request to Deepgram AI: %w", err)
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("deepgram api returned non-200 status: %d, body: %s", resp.StatusCode, string(bodyBytes))
	}

	var response struct {
		Metadata struct {
			RequestID string `json:"request_id"`
		} `json:"metadata"`
		Results struct {
			Channels []struct {
				Alternatives []struct {
					Transcript string `json:"transcript"`
				} `json:"alternatives"`
			} `json:"channels"`
		} `json:"results"`
	}

	d.Logger.Debug("Deepgram API response parsing",
		zap.Int("body_length", len(bodyBytes)),
		zap.String("body_preview", string(bodyBytes)[:min(200, len(bodyBytes))]))

	if err := json.Unmarshal(bodyBytes, &response); err != nil {
		d.Logger.Error("Failed to parse JSON response",
			zap.Error(err),
			zap.String("body_preview", string(bodyBytes)[:min(500, len(bodyBytes))]))
		return "", fmt.Errorf("failed to parse response: %w", err)
	}

	d.Logger.Debug("Parsed response structure",
		zap.Int("channels_count", len(response.Results.Channels)))

	if len(response.Results.Channels) == 0 {
		d.Logger.Error("No channels in response",
			zap.Any("full_response", response))
		return "", fmt.Errorf("no channels found in response")
	}

	if len(response.Results.Channels[0].Alternatives) == 0 {
		d.Logger.Error("No alternatives in first channel",
			zap.Any("first_channel", response.Results.Channels[0]))
		return "", fmt.Errorf("no alternatives found in first channel")
	}

	transcript := response.Results.Channels[0].Alternatives[0].Transcript
	d.Logger.Debug("Extracted transcript",
		zap.String("transcript_preview", transcript[:min(100, len(transcript))]))

	return transcript, nil
}
