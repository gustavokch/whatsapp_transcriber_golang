package transcription

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"go.uber.org/zap"
)

const groqAPIURL = "https://api.groq.com/openai/v1/audio/transcriptions"

// GroqTranscriber implements the Transcriber interface for Groq API.
type GroqTranscriber struct {
	APIKey string
	Model  string
	Logger *zap.Logger
}

// NewGroqTranscriber creates a new GroqTranscriber.
func NewGroqTranscriber(apiKey, model string, logger *zap.Logger) *GroqTranscriber {
	return &GroqTranscriber{
		APIKey: apiKey,
		Model:  model,
		Logger: logger,
	}
}

// TranscribeAudio sends an audio file to Groq API for transcription.
func (g *GroqTranscriber) TranscribeAudio(ctx context.Context, audioFilePath string, language string) (string, error) {
	file, err := os.Open(audioFilePath)
	if err != nil {
		return "", fmt.Errorf("failed to open audio file: %w", err)
	}
	defer file.Close()

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)

	// Add model field
	_ = writer.WriteField("model", g.Model)
	// Add language field if provided
	if language != "" {
		_ = writer.WriteField("language", language)
	}

	// Add audio file
	part, err := writer.CreateFormFile("file", filepath.Base(audioFilePath))
	if err != nil {
		return "", fmt.Errorf("failed to create form file: %w", err)
	}
	_, err = io.Copy(part, file)
	if err != nil {
		return "", fmt.Errorf("failed to copy file data: %w", err)
	}
	writer.Close() // Close the multipart writer to write the trailing boundary

	req, err := http.NewRequestWithContext(ctx, "POST", groqAPIURL, body)
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+g.APIKey)

	client := &http.Client{Timeout: 60 * time.Second} // 60 seconds timeout for transcription
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to send request to Groq API: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("Groq API returned non-200 status: %d, body: %s", resp.StatusCode, respBody)
	}

	var result struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("failed to decode Groq API response: %w", err)
	}

	return result.Text, nil
}