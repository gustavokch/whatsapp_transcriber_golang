package transcription

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"strings"
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
	audioBytes, err := os.ReadFile(audioFilePath)
	if err != nil {
		return "", fmt.Errorf("failed to read audio file: %w", err)
	}

	apiURL := fmt.Sprintf("https://api.cloudflare.com/client/v4/accounts/%s/ai/run/%s", c.AccountID, c.Model)

	// Different models have different API requirements
	// Whisper models expect JSON format with audio as base64
	// Deepgram models expect multipart form data
	if strings.Contains(c.Model, "whisper") {
		return c.transcribeWithWhisperAPI(ctx, apiURL, audioBytes)
	} else {
		return c.transcribeWithDeepgramAPI(ctx, apiURL, audioBytes)
	}
}

// transcribeWithWhisperAPI uses the Whisper API format (JSON with base64 audio)
func (c *CloudflareAITranscriber) transcribeWithWhisperAPI(ctx context.Context, apiURL string, audioBytes []byte) (string, error) {
	// Encode audio to base64 for Whisper API
	base64Audio := base64.StdEncoding.EncodeToString(audioBytes)

	// Construct the request body as a JSON object
	requestBody, err := json.Marshal(map[string]interface{}{
		"audio": base64Audio,
	})
	if err != nil {
		return "", fmt.Errorf("failed to marshal request body: %w", err)
	}

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

// transcribeWithDeepgramAPI uses the Deepgram API format (multipart form data)
func (c *CloudflareAITranscriber) transcribeWithDeepgramAPI(ctx context.Context, apiURL string, audioBytes []byte) (string, error) {
	// Create a multipart form body
	var requestBody bytes.Buffer
	writer := multipart.NewWriter(&requestBody)

	// Add audio file part
	part, err := writer.CreateFormFile("audio", "audio.ogg")
	if err != nil {
		return "", fmt.Errorf("failed to create form file: %w", err)
	}
	_, err = part.Write(audioBytes)
	if err != nil {
		return "", fmt.Errorf("failed to write audio data: %w", err)
	}

	// Close the writer to finalize the multipart form
	err = writer.Close()
	if err != nil {
		return "", fmt.Errorf("failed to close multipart writer: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", apiURL, &requestBody)
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", writer.FormDataContentType())
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
