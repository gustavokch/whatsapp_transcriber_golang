package transcription

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"go.uber.org/zap"
)

// CloudflareAITranscriber implements the Transcriber interface for Cloudflare AI.
type CloudflareAITranscriber struct {
	AccountID string
	APIKey    string
	Model     string
	Logger    *zap.Logger
}

// NewCloudflareAITranscriber creates a new CloudflareAITranscriber.
func NewCloudflareAITranscriber(accountID, apiKey, model string, logger *zap.Logger) *CloudflareAITranscriber {
	return &CloudflareAITranscriber{
		AccountID: accountID,
		APIKey:    apiKey,
		Model:     model,
		Logger:    logger,
	}
}

// TranscribeAudio sends an audio file to Cloudflare AI for transcription.
func (c *CloudflareAITranscriber) TranscribeAudio(ctx context.Context, audioFilePath string, language string) (string, error) {
	// Cloudflare AI Whisper API typically expects audio as raw bytes or base64 encoded.
	// For simplicity, we'll read the file directly and send it as multipart/form-data,
	// similar to Groq, assuming the endpoint supports it.
	// If not, further audio encoding (e.g., base64) would be required.

	audioBytes, err := os.ReadFile(audioFilePath)
	if err != nil {
		return "", fmt.Errorf("failed to read audio file: %w", err)
	}

	// Cloudflare AI expects a JSON body with the audio data.
	// The Python example used base64 encoding for Cloudflare.
	// Let's assume for now we send raw bytes and the model handles it,
	// or we'll need to add base64 encoding here.
	// For Whisper, Cloudflare's API usually takes raw audio bytes.

	// Construct the request body as a JSON object
	requestBody, err := json.Marshal(map[string]interface{}{
		"audio": audioBytes,
	})
	if err != nil {
		return "", fmt.Errorf("failed to marshal request body: %w", err)
	}

	apiURL := fmt.Sprintf("https://api.cloudflare.com/client/v4/accounts/%s/ai/run/%s", c.AccountID, c.Model)
	req, err := http.NewRequestWithContext(ctx, "POST", apiURL, bytes.NewBuffer(requestBody))
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.APIKey)

	client := &http.Client{Timeout: 60 * time.Second} // 60 seconds timeout for transcription
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to send request to Cloudflare AI: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("Cloudflare AI returned non-200 status: %d, body: %s", resp.StatusCode, respBody)
	}

	var result struct {
		Result struct {
			Text string `json:"text"`
		} `json:"result"`
		Success bool `json:"success"`
		Errors  []struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("failed to decode Cloudflare AI response: %w", err)
	}

	if !result.Success {
		errMsg := "unknown Cloudflare AI error"
		if len(result.Errors) > 0 {
			errMsg = result.Errors[0].Message
		}
		return "", fmt.Errorf("Cloudflare AI transcription failed: %s", errMsg)
	}

	return result.Result.Text, nil
}