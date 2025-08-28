package transcription

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
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

// getContentType returns MIME type based on file extension
func getContentType(filename string) string {
	switch {
	case strings.HasSuffix(filename, ".wav"):
		return "audio/wav"
	case strings.HasSuffix(filename, ".mp3"):
		return "audio/mpeg"
	case strings.HasSuffix(filename, ".ogg"):
		return "audio/ogg"
	case strings.HasSuffix(filename, ".flac"):
		return "audio/flac"
	default:
		return "application/octet-stream"
	}
}

// TranscribeAudio sends an audio file to Cloudflare AI for transcription.
func (c *CloudflareAITranscriber) TranscribeAudio(ctx context.Context, audioFilePath string, language string) (string, error) {
	audioBytes, err := os.ReadFile(audioFilePath)
	if err != nil {
		return "", fmt.Errorf("failed to read audio file: %w", err)
	}

	apiURL := fmt.Sprintf("https://api.cloudflare.com/client/v4/accounts/%s/ai/run/%s", c.AccountID, c.Model)
	c.Logger.Debug("Calling Cloudflare AI API", zap.String("url", apiURL))

	// Different models have different API requirements
	// Whisper models expect JSON format with audio as base64
	// Deepgram models expect audio as array
	if strings.Contains(c.Model, "whisper") {
		return c.transcribeWithWhisperAPI(ctx, apiURL, audioBytes)
	} else {
		return c.transcribeWithDeepgramAPI(ctx, apiURL, audioBytes, audioFilePath)
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
		return "", fmt.Errorf("cloudflare ai whisper api returned non-200 status: %d, body: %s", resp.StatusCode, string(respBody))
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
		errMsg := "unknown cloudflare ai error"
		if len(result.Errors) > 0 {
			errMsg = result.Errors[0].Message
		}
		return "", fmt.Errorf("cloudflare ai whisper api transcription failed: %s", errMsg)
	}

	return result.Result.Text, nil
}

// transcribeWithDeepgramAPI uses the Deepgram API format (JSON with audio object)
func (c *CloudflareAITranscriber) transcribeWithDeepgramAPI(ctx context.Context, apiURL string, audioBytes []byte, audioFilePath string) (string, error) {
	// Determine content type from file extension
	contentType := getContentType(audioFilePath)

	// Encode audio bytes to base64 for JSON format
	base64Audio := base64.StdEncoding.EncodeToString(audioBytes)

	// Create request body matching Cloudflare AI API schema
	requestBody := map[string]interface{}{
		"audio": map[string]interface{}{
			"contentType": contentType,
			"body":        base64Audio,
		},
	}

	jsonData, err := json.Marshal(requestBody)
	if err != nil {
		return "", fmt.Errorf("failed to marshal request body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", apiURL, bytes.NewBuffer(jsonData))
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	// Set headers for JSON data
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.APIKey)

	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to send request to Cloudflare AI: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("cloudflare ai deepgram api returned non-200 status: %d, body: %s", resp.StatusCode, string(respBody))
	}

	// Decode response matching Deepgram format
	var result struct {
		Result struct {
			Results struct {
				Channels []struct {
					Alternatives []struct {
						Transcript string `json:"transcript"`
						Words      []struct {
							Word       string  `json:"word"`
							Start      float64 `json:"start"`
							End        float64 `json:"end"`
							Confidence float64 `json:"confidence"`
						} `json:"words"`
					} `json:"alternatives"`
				} `json:"channels"`
			} `json:"results"`
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
		errMsg := "unknown cloudflare ai error"
		if len(result.Errors) > 0 {
			errMsg = result.Errors[0].Message
		}
		return "", fmt.Errorf("cloudflare ai deepgram api transcription failed: %s", errMsg)
	}

	// Extract transcript from Deepgram response
	if len(result.Result.Results.Channels) == 0 || len(result.Result.Results.Channels[0].Alternatives) == 0 {
		return "", fmt.Errorf("no transcription results in response")
	}

	return result.Result.Results.Channels[0].Alternatives[0].Transcript, nil
}
